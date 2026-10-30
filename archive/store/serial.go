package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// The serial number is the receive ledger: one monotonically increasing number
// per archived media row, assigned the moment the message lands, never reused.
//
// Why not just use message_id? Because a message id is scoped to one chat, so
// "the 42nd thing my account received" is not expressible in it — every bot and
// every channel restarts its own sequence. Why not use the media rowid? Because
// rowid is an implementation detail that jumps when a row is rebuilt (the media
// table has already been rebuilt once, see migrate_media_link_uniqueness), and a
// number the user quotes in a filename must not move.
//
// Assignment happens at ingest (EnsureMedia) rather than at download completion
// so that a media item is numbered even when the download is skipped, fails, or
// is still queued. The file view in objects/by-serial/ appears later, when the
// bytes do.

// nextSerialExpr yields MAX(serial)+1. It is evaluated against the table before
// the UPDATE touches the row, and stays correct because Store serializes all
// access on a single connection (db.SetMaxOpenConns(1)).
const nextSerialExpr = `(SELECT COALESCE(MAX(serial),0)+1 FROM media)`

// AssignSerial numbers one media row if it has no number yet. Idempotent: a row
// that already has a serial keeps it, so calling it from a retry path is safe.
func (s *Store) AssignSerial(ctx context.Context, mediaID int64) (int64, error) {
	if _, err := s.db.ExecContext(ctx,
		`UPDATE media SET serial=`+nextSerialExpr+` WHERE id=? AND serial IS NULL`, mediaID); err != nil {
		return 0, err
	}
	return s.MediaSerial(ctx, mediaID)
}

// assignSerialTx is the transactional form used by writers that already hold a
// tx (MarkMediaCompleted), so a completed file can never exist without a number.
func assignSerialTx(ctx context.Context, tx *sql.Tx, mediaID int64) error {
	// The subquery reads the table being updated, which SQLite allows: the
	// target row set is fixed before the SET expressions run, so MAX(serial)
	// here is the pre-update maximum.
	_, err := tx.ExecContext(ctx,
		`UPDATE media SET serial=`+nextSerialExpr+` WHERE id=? AND serial IS NULL`, mediaID)
	return err
}

// MediaSerial returns the number assigned to one media row, or 0 when it has
// none yet.
func (s *Store) MediaSerial(ctx context.Context, mediaID int64) (int64, error) {
	var n sql.NullInt64
	err := s.db.QueryRowContext(ctx, "SELECT serial FROM media WHERE id=?", mediaID).Scan(&n)
	if err == sql.ErrNoRows || !n.Valid {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return n.Int64, nil
}

// backfillMediaSerials numbers legacy rows (serial IS NULL), oldest message
// first, then by row id. It runs once per upgraded database and is a no-op
// afterwards. Ordering by the message date — not by rowid — means the number
// reflects when the account received the item, not when this code first saw it.
func (s *Store) backfillMediaSerials(ctx context.Context) error {
	var pending int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM media WHERE serial IS NULL").Scan(&pending); err != nil {
		return err
	}
	if pending == 0 {
		return nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT media.id FROM media
 LEFT JOIN messages m ON m.id=media.message_rowid
 WHERE media.serial IS NULL
 ORDER BY COALESCE(m.date,0) ASC, media.id ASC`)
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, id := range ids {
		if err := assignSerialTx(ctx, tx, id); err != nil {
			return fmt.Errorf("backfill media serial %d: %w", id, err)
		}
	}
	return tx.Commit()
}

// BackfillMediaSerials is the exported entry point for the same repair, so an
// operator can renumber after restoring an older backup without restarting.
func (s *Store) BackfillMediaSerials(ctx context.Context) (int, error) {
	var before int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM media WHERE serial IS NULL").Scan(&before); err != nil {
		return 0, err
	}
	if err := s.backfillMediaSerials(ctx); err != nil {
		return 0, err
	}
	var after int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM media WHERE serial IS NULL").Scan(&after); err != nil {
		return 0, err
	}
	return before - after, nil
}

// SerialItem is one row of the receive ledger, joined with the message it came
// from so the ledger is readable without a second query.
type SerialItem struct {
	Serial       int64
	Kind         string
	Size         int64
	Status       string
	FileName     string
	MIME         string
	Path         string
	DownloadedAt time.Time
	ChatID       int64
	MessageID    int
	Date         time.Time
	// Out reports the direction: false = arrived from the peer (what the
	// receive ledger is for), true = sent by this account.
	Out     bool
	Caption string
	// ChatLabel is the peer's username or title, resolved in the same query so
	// a ledger row names the bot/channel instead of a bare numeric id.
	ChatLabel string
}

// SerialFilter narrows a ledger read. Zero values mean "no restriction".
type SerialFilter struct {
	Limit       int
	Kind        string
	Status      string
	InboundOnly bool
	// MaxSerial, when > 0, returns numbers at or below it — the "everything up
	// to N" view used when citing a range.
	MaxSerial int64
	Since     *time.Time
}

// Serials returns ledger rows, newest number first.
func (s *Store) Serials(ctx context.Context, f SerialFilter) ([]SerialItem, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 1000 {
		limit = 1000
	}
	var (
		where []string
		args  []any
	)
	where = append(where, "media.serial IS NOT NULL")
	if f.Kind != "" {
		where = append(where, "media.type=?")
		args = append(args, f.Kind)
	}
	if f.Status != "" {
		where = append(where, "media.status=?")
		args = append(args, f.Status)
	}
	if f.InboundOnly {
		where = append(where, "m.out=0")
	}
	if f.MaxSerial > 0 {
		where = append(where, "media.serial<=?")
		args = append(args, f.MaxSerial)
	}
	if f.Since != nil {
		where = append(where, "m.date>=?")
		args = append(args, f.Since.Unix())
	}
	q := `SELECT media.serial, media.type, COALESCE(media.size,0), media.status,
       COALESCE(media.file_name,''), COALESCE(media.mime_type,''), COALESCE(media.original_path,''),
       COALESCE(media.downloaded_at,0), m.chat_id, m.message_id, COALESCE(m.date,0), COALESCE(m.out,0),
       COALESCE(NULLIF(m.caption,''), m.text, ''),
       COALESCE(NULLIF(c.username,''), NULLIF(c.title,''), '')
 FROM media JOIN messages m ON m.id=media.message_rowid
 LEFT JOIN chats c ON c.id=m.chat_id
 WHERE ` + strings.Join(where, " AND ") + `
 ORDER BY media.serial DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SerialItem
	for rows.Next() {
		var (
			it      SerialItem
			dl      int64
			date    int64
			caption string
			outFlag int
		)
		if err := rows.Scan(&it.Serial, &it.Kind, &it.Size, &it.Status, &it.FileName, &it.MIME,
			&it.Path, &dl, &it.ChatID, &it.MessageID, &date, &outFlag, &caption, &it.ChatLabel); err != nil {
			return nil, err
		}
		if dl > 0 {
			it.DownloadedAt = time.Unix(dl, 0)
		}
		if date > 0 {
			it.Date = time.Unix(date, 0)
		}
		it.Out = outFlag != 0
		it.Caption = oneLine(caption, 80)
		out = append(out, it)
	}
	return out, rows.Err()
}

// SerialByNumber resolves one receive number to its ledger row.
func (s *Store) SerialByNumber(ctx context.Context, serial int64) (SerialItem, bool, error) {
	list, err := s.Serials(ctx, SerialFilter{Limit: 1, MaxSerial: serial})
	if err != nil {
		return SerialItem{}, false, err
	}
	for _, it := range list {
		if it.Serial == serial {
			return it, true, nil
		}
	}
	return SerialItem{}, false, nil
}

// SerialSummary is the headline of the ledger: how many items are numbered, the
// highest number, and how many still await bytes.
type SerialSummary struct {
	Total     int64
	Max       int64
	Inbound   int64
	Pending   int64
	Completed int64
}

func (s *Store) SerialSummary(ctx context.Context) (SerialSummary, error) {
	var sum SerialSummary
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(MAX(media.serial),0),
 COALESCE(SUM(CASE WHEN m.out=0 THEN 1 ELSE 0 END),0),
 COALESCE(SUM(CASE WHEN media.status IN ('pending','failed') THEN 1 ELSE 0 END),0),
 COALESCE(SUM(CASE WHEN media.status='completed' THEN 1 ELSE 0 END),0)
 FROM media JOIN messages m ON m.id=media.message_rowid WHERE media.serial IS NOT NULL`).
		Scan(&sum.Total, &sum.Max, &sum.Inbound, &sum.Pending, &sum.Completed)
	return sum, err
}

// oneLine flattens a caption to a single truncated line so a ledger row stays a
// row. Real captions carry newlines and full prompts; printing them raw turns a
// 50-row table into an unreadable wall.
func oneLine(s string, limit int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	return string(r[:limit]) + "…"
}

// LinkTarget is one completed media item that should be reachable under its
// receive number. It exists so the relink pass can rebuild the by-serial view
// for rows archived before the view existed.
type LinkTarget struct {
	MediaID int64
	Serial  int64
	Kind    string
	MIME    string
	Path    string
}

// CompletedForLink pages completed media that carry both a number and a stored
// path, ordered by media id so a caller can resume after a failure.
func (s *Store) CompletedForLink(ctx context.Context, afterID int64, limit int) ([]LinkTarget, error) {
	if limit <= 0 || limit > 2000 {
		limit = 500
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, serial, type, COALESCE(mime_type,''), COALESCE(original_path,'')
 FROM media
 WHERE status='completed' AND serial IS NOT NULL AND COALESCE(original_path,'')<>'' AND id>?
 ORDER BY id LIMIT ?`, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LinkTarget
	for rows.Next() {
		var t LinkTarget
		if err := rows.Scan(&t.MediaID, &t.Serial, &t.Kind, &t.MIME, &t.Path); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
