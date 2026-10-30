package store

import (
	"context"
	"fmt"
	"strings"
)

type MediaKind string

const (
	MediaKindImage MediaKind = "image"
	MediaKindVideo MediaKind = "video"
	MediaKindFile  MediaKind = "file"
)

type MediaFilter struct {
	Kind     MediaKind
	Query    string
	ChatID   int64
	Topic    int64
	Page     int
	PageSize int
	// Dedupe collapses messages carrying the same file into one entry. A reward
	// bot reposts the same video on every /start, so the raw count says "301
	// videos" while the library holds 190 distinct files — a gallery that shows
	// the same frame thirteen times reads as a bug, not as a library.
	Dedupe bool
}

type MediaItem struct {
	MessageView
	// DupCount is how many messages reference this exact file, dedupe mode only.
	DupCount int `json:"dup_count,omitempty"`
}

type MediaPage struct {
	Items    []MediaItem
	Total    int
	Page     int
	PageSize int
}

func (s *Store) ListMediaMessages(ctx context.Context, f MediaFilter) (MediaPage, error) {
	if f.Page < 1 {
		f.Page = 1
	}
	if f.PageSize <= 0 || f.PageSize > 200 {
		f.PageSize = 50
	}
	if f.Kind != MediaKindImage && f.Kind != MediaKindVideo && f.Kind != MediaKindFile {
		return MediaPage{}, fmt.Errorf("invalid media kind %q", f.Kind)
	}
	offset := (f.Page - 1) * f.PageSize
	query := strings.TrimSpace(f.Query)
	if f.Dedupe && query == "" {
		return s.listMediaDeduped(ctx, f)
	}
	if query != "" {
		query += " "
	}
	query += "has:" + string(f.Kind)

	results, err := s.SearchAdvanced(ctx, query, SearchOptions{
		MediaReadyOnly: true,
		ChatID:         f.ChatID,
		Topic:          f.Topic,
		Limit:          f.PageSize,
		Offset:         offset,
	})
	if err != nil {
		return MediaPage{}, err
	}
	total, err := s.CountSearch(ctx, query, SearchOptions{MediaReadyOnly: true, ChatID: f.ChatID, Topic: f.Topic})
	if err != nil {
		return MediaPage{}, err
	}
	ids := make([]int64, 0, len(results))
	for _, result := range results {
		ids = append(ids, result.RowID)
	}
	views, err := s.MessageViewsByRowIDs(ctx, ids)
	if err != nil {
		return MediaPage{}, err
	}
	views = orderMessageViews(views, ids)
	items := make([]MediaItem, 0, len(views))
	for _, view := range views {
		items = append(items, MediaItem{MessageView: view})
	}
	return MediaPage{Items: items, Total: total, Page: f.Page, PageSize: f.PageSize}, nil
}

// mediaKindCond returns the per-kind predicate used by both the FTS path and the
// dedupe path. Kept as one function so the two views can never disagree about
// what counts as a video.
func mediaKindCond(kind MediaKind) (string, bool) {
	switch kind {
	case MediaKindImage:
		return "(m.type='photo' OR m.mime_type LIKE 'image/%')", true
	case MediaKindVideo:
		return "(m.type IN ('video','animation') OR m.mime_type LIKE 'video/%')", true
	case MediaKindFile:
		return "(m.type NOT IN ('photo','video','animation','audio','voice','sticker') AND (m.mime_type IS NULL OR (m.mime_type NOT LIKE 'image/%' AND m.mime_type NOT LIKE 'video/%' AND m.mime_type NOT LIKE 'audio/%')))", true
	}
	return "", false
}

// listMediaDeduped lists one entry per distinct file. Grouping happens in SQL
// rather than in Go so that paging stays honest: filtering duplicate rows out of
// a page after the fact would return short pages and shift entries between them.
//
// The representative row is the OLDEST message carrying the file (MIN rowid), so
// the entry keeps the original caption — the first post is the one with the text,
// the reposts are bare media.
func (s *Store) listMediaDeduped(ctx context.Context, f MediaFilter) (MediaPage, error) {
	kindCond, ok := mediaKindCond(f.Kind)
	if !ok {
		return MediaPage{}, fmt.Errorf("invalid media kind %q", f.Kind)
	}
	where := []string{
		"m.sha256 IS NOT NULL AND m.sha256 <> ''",
		"m.status = 'completed'",
		"mm.deleted = 0",
		kindCond,
	}
	args := []any{}
	if f.ChatID != 0 {
		where = append(where, "mm.chat_id = ?")
		args = append(args, f.ChatID)
	}
	if f.Topic != 0 {
		where = append(where, "mm.thread_id = ?")
		args = append(args, f.Topic)
	}
	cond := strings.Join(where, " AND ")

	var total int
	if err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(DISTINCT m.sha256) FROM media m JOIN messages mm ON mm.id=m.message_rowid WHERE "+cond,
		args...).Scan(&total); err != nil {
		return MediaPage{}, err
	}

	// Order the groups by their newest member, then pick the oldest row as the
	// representative: newest-first browsing, oldest caption.
	rows, err := s.db.QueryContext(ctx, `
SELECT g.rowid, g.dup_count FROM (
  SELECT MIN(m.message_rowid) AS rowid, COUNT(*) AS dup_count, MAX(mm.date) AS latest
  FROM media m JOIN messages mm ON mm.id=m.message_rowid
  WHERE `+cond+`
  GROUP BY m.sha256
) g ORDER BY g.latest DESC, g.rowid DESC LIMIT ? OFFSET ?`,
		append(args, f.PageSize, (f.Page-1)*f.PageSize)...)
	if err != nil {
		return MediaPage{}, err
	}
	defer rows.Close()
	type pick struct {
		rowid int64
		dup   int
	}
	var picks []pick
	ids := make([]int64, 0, f.PageSize)
	for rows.Next() {
		var p pick
		if err := rows.Scan(&p.rowid, &p.dup); err != nil {
			return MediaPage{}, err
		}
		picks = append(picks, p)
		ids = append(ids, p.rowid)
	}
	if err := rows.Err(); err != nil {
		return MediaPage{}, err
	}
	if len(ids) == 0 {
		return MediaPage{Items: []MediaItem{}, Total: total, Page: f.Page, PageSize: f.PageSize}, nil
	}
	views, err := s.MessageViewsByRowIDs(ctx, ids)
	if err != nil {
		return MediaPage{}, err
	}
	views = orderMessageViews(views, ids)
	dup := make(map[int64]int, len(picks))
	for _, p := range picks {
		dup[p.rowid] = p.dup
	}
	items := make([]MediaItem, 0, len(views))
	for _, view := range views {
		items = append(items, MediaItem{MessageView: view, DupCount: dup[view.RowID]})
	}
	return MediaPage{Items: items, Total: total, Page: f.Page, PageSize: f.PageSize}, nil
}
