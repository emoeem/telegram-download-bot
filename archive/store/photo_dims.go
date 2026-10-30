package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// This file backfills media.width/height for photo rows archived before
// MediaInfo read a photo's renditions. The data was never lost: messages
// .raw_json holds the whole tg.Message, so the dimensions can be recovered
// locally instead of re-fetching thousands of messages from Telegram.
//
// The re-fetch route was rejected on purpose. A history walk per chat is the
// expensive part of a backfill (one request per batch per chat, paced to avoid
// FLOOD_WAIT), and it would still have to be matched back to rows we already
// have. Reading raw_json costs one query over an indexed column and no network
// at all — the same reasoning as clickablesFromRaw.

// rawPhotoShape is the slice of a tg.Message JSON payload needed to recover a
// photo's dimensions.
//
// It is deliberately a purpose-built shape rather than tg.Message. The Media and
// Photo fields are interface values, and encoding/json cannot unmarshal into an
// interface without a type tag — tg.Message's own marshal has none, because the
// struct Flattening happens at the Go level. Unknown fields (FileReference, the
// stripped "Bytes" blobs, DCID) are simply ignored by encoding/json, which keeps
// this cheap: no base64 decode of thumbnail payloads we would immediately drop.
type rawPhotoShape struct {
	Media *struct {
		Photo *struct {
			Sizes []struct {
				Type string `json:"Type"`
				W    int    `json:"W"`
				H    int    `json:"H"`
			} `json:"Sizes"`
		} `json:"Photo"`
	} `json:"Media"`
}

// photoDimensionsFromRaw recovers a photo's largest rendition size from an
// archived raw JSON payload. It returns (0, 0) rather than an error when the
// payload carries no usable rendition: a message predating the media column, a
// stripped-only photo, or a row whose raw_json is empty are all "nothing to
// backfill", not failures that should abort a batch over thousands of rows.
func photoDimensionsFromRaw(raw string) (int, int) {
	if raw == "" {
		return 0, 0
	}
	var shape rawPhotoShape
	if err := json.Unmarshal([]byte(raw), &shape); err != nil {
		return 0, 0
	}
	if shape.Media == nil || shape.Media.Photo == nil {
		return 0, 0
	}
	w, h := 0, 0
	for _, s := range shape.Media.Photo.Sizes {
		// A stripped size ("i") carries a decodable placeholder, not a
		// rendition, and reports no W/H — it never wins this comparison.
		if s.W <= 0 || s.H <= 0 {
			continue
		}
		if s.W*s.H > w*h {
			w, h = s.W, s.H
		}
	}
	return w, h
}

// CountPhotosMissingDimensions reports how many photo rows still lack a
// dimension. It exists so --dry-run can say how much work there is without
// writing anything.
func (s *Store) CountPhotosMissingDimensions(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM media
WHERE type = 'photo' AND (width IS NULL OR height IS NULL)`).Scan(&n)
	return n, err
}

// BackfillPhotoDimensions fills width/height on photo media rows that are
// missing them. It reports how many rows it filled and how many it examined.
//
// The read and the write are deliberately separated, and that is not a style
// choice: the store runs on a single connection (db.SetMaxOpenConns(1)), so
// issuing an UPDATE while a *sql.Rows is still open would block until the
// context deadline — the writer would wait for the only connection, which the
// unfinished read is holding. Each batch is therefore drained into memory,
// closed, and only then written.
func (s *Store) BackfillPhotoDimensions(ctx context.Context) (filled, scanned int, err error) {
	const batch = 500
	var cursor int64
	now := time.Now().Unix()

	for {
		type candidate struct {
			id   int64
			w, h int
		}
		var candidates []candidate
		inBatch := 0

		rows, queryErr := s.db.QueryContext(ctx, `
SELECT m.id, COALESCE(msg.raw_json, '')
FROM media m
JOIN messages msg ON msg.id = m.message_rowid
WHERE m.type = 'photo'
  AND (m.width IS NULL OR m.height IS NULL)
  AND m.id > ?
ORDER BY m.id
LIMIT ?`, cursor, batch)
		if queryErr != nil {
			return filled, scanned, queryErr
		}
		for rows.Next() {
			var id int64
			var raw string
			if scanErr := rows.Scan(&id, &raw); scanErr != nil {
				rows.Close()
				return filled, scanned, scanErr
			}
			inBatch++
			cursor = id
			if w, h := photoDimensionsFromRaw(raw); w > 0 && h > 0 {
				candidates = append(candidates, candidate{id: id, w: w, h: h})
			}
		}
		if iterErr := rows.Err(); iterErr != nil {
			rows.Close()
			return filled, scanned, iterErr
		}
		// Must precede the writes: see the single-connection note above.
		rows.Close()
		scanned += inBatch

		for _, c := range candidates {
			if _, execErr := s.db.ExecContext(ctx,
				`UPDATE media SET width = ?, height = ?, updated_at = ? WHERE id = ?`,
				c.w, c.h, now, c.id); execErr != nil {
				return filled, scanned, fmt.Errorf("backfill photo dimensions: media id %d: %w", c.id, execErr)
			}
			filled++
		}

		// Stop on a short batch. Progress is guaranteed even when nothing in the
		// batch was backfillable, because cursor advanced with the batch.
		if inBatch < batch {
			break
		}
	}
	return filled, scanned, nil
}
