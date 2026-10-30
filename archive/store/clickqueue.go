package store

import (
	"context"
	"database/sql"
	"strings"
	"time"
)

// Auto-click queue states. The queue exists because "click every buttoned
// message" and "never trip flood limits" are in direct conflict. A bot that
// posts a hundred buttoned messages in a minute must be pressed at a human
// pace, and the ones that do not fit in this minute have to survive a restart
// instead of being dropped on the floor.
const (
	ClickPending = "pending"
	ClickDone    = "done"
	ClickFailed  = "failed"
	ClickEmpty   = "empty"
	ClickGone    = "gone"
)

// ClickJob is one queued message waiting for its button to be pressed.
type ClickJob struct {
	ChatID    int64
	MessageID int
	Attempts  int
}

// EnqueueClicks adds message ids to the click queue. Rows that already exist
// are left untouched, so re-scanning a chat never re-clicks a handled message
// and never resurrects one that already reached a terminal state.
func (s *Store) EnqueueClicks(ctx context.Context, chatID int64, msgIDs []int, nextAt int64) (int, error) {
	if len(msgIDs) == 0 {
		return 0, nil
	}
	now := time.Now().Unix()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	added := 0
	for _, id := range msgIDs {
		res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO autoclick_queue(chat_id,message_id,state,attempts,next_at,created_at,updated_at)
 VALUES(?,?,?,0,?,?,?)`, chatID, id, ClickPending, nextAt, now, now)
		if err != nil {
			return added, err
		}
		if n, err := res.RowsAffected(); err == nil {
			added += int(n)
		}
	}
	return added, tx.Commit()
}

// DueClicks returns pending jobs whose backoff has elapsed, oldest first. The
// ordering keeps a burst in arrival order instead of letting the queue reshuffle.
func (s *Store) DueClicks(ctx context.Context, now int64, limit int) ([]ClickJob, error) {
	if limit <= 0 {
		limit = 8
	}
	rows, err := s.db.QueryContext(ctx, `SELECT chat_id,message_id,attempts FROM autoclick_queue
 WHERE state=? AND next_at<=? ORDER BY next_at,rowid LIMIT ?`, ClickPending, now, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ClickJob
	for rows.Next() {
		var j ClickJob
		if err := rows.Scan(&j.ChatID, &j.MessageID, &j.Attempts); err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// MarkClick puts a job into a terminal state. done/empty/gone never retry.
func (s *Store) MarkClick(ctx context.Context, chatID int64, msgID int, state, errText string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE autoclick_queue SET state=?,last_error=?,updated_at=? WHERE chat_id=? AND message_id=?`,
		state, errText, time.Now().Unix(), chatID, msgID)
	return err
}

// RescheduleClick keeps a job pending and pushes it into the future. attempts
// is incremented so a permanently broken message cannot spin forever.
func (s *Store) RescheduleClick(ctx context.Context, chatID int64, msgID int, nextAt int64, errText string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE autoclick_queue SET attempts=attempts+1,next_at=?,last_error=?,updated_at=? WHERE chat_id=? AND message_id=?`,
		nextAt, errText, time.Now().Unix(), chatID, msgID)
	return err
}

// ClickQueueStats counts rows per state.
func (s *Store) ClickQueueStats(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT state,COUNT(*) FROM autoclick_queue GROUP BY state`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var st string
		var n int
		if err := rows.Scan(&st, &n); err != nil {
			return nil, err
		}
		out[st] = n
	}
	return out, rows.Err()
}

// ButtonedMessageIDs lists archived messages in a chat that carried an inline
// callback button. Ingest already records the display form of every keyboard,
// so a backfill can be built from the local archive without re-reading Telegram
// for messages that have nothing to click.
func (s *Store) ButtonedMessageIDs(ctx context.Context, chatID int64, limit int) ([]int, error) {
	if limit <= 0 {
		limit = 500
	}
	// Two sources, because a button reaches the archive two ways: an inline
	// keyboard lands in buttons_json, while the pills this archive sees most
	// often are text links whose destination lives in the raw entity array.
	// Selecting on the raw JSON as well is what makes a backfill possible
	// without one network fetch per message. instr+CAST, not LIKE: raw_json is
	// stored as a BLOB, and SQLite's LIKE never matches a BLOB value.
	rows, err := s.db.QueryContext(ctx, `SELECT message_id FROM messages
 WHERE chat_id=? AND deleted=0 AND out=0
   AND (
     (buttons_json IS NOT NULL AND buttons_json NOT IN ('','[]','null'))
     OR instr(CAST(raw_json AS TEXT),'t.me/')>0
   )
 ORDER BY message_id DESC LIMIT ?`, chatID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// InboundMessageIDs lists the newest inbound message ids of a chat, used when a
// chat was archived before buttons were recorded and the local archive cannot
// say which messages are clickable.
func (s *Store) InboundMessageIDs(ctx context.Context, chatID int64, limit int) ([]int, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := s.db.QueryContext(ctx, `SELECT message_id FROM messages
 WHERE chat_id=? AND deleted=0 AND out=0 ORDER BY message_id DESC LIMIT ?`, chatID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// QueuedMessage loads the slice of a message row that a queued click needs:
// text, caption, media flag and direction.
func (s *Store) QueuedMessage(ctx context.Context, chatID int64, msgID int) (Message, error) {
	var m Message
	var hasMedia, out, deleted int
	var raw []byte
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(text,''),COALESCE(caption,''),has_media,out,deleted,COALESCE(raw_json,'')
 FROM messages WHERE chat_id=? AND message_id=?`, chatID, msgID).Scan(&m.Text, &m.Caption, &hasMedia, &out, &deleted, &raw)
	if err != nil {
		return m, err
	}
	m.ChatID, m.MessageID = chatID, msgID
	m.HasMedia, m.Out, m.Deleted = hasMedia == 1, out == 1, deleted == 1
	m.Raw = raw
	return m, nil
}

// RequeueClicks puts finished rows back to pending. Only non-done states are
// eligible: a message that was already clicked must never be clicked twice.
func (s *Store) RequeueClicks(ctx context.Context, states []string) (int, error) {
	if len(states) == 0 {
		states = []string{ClickFailed, ClickGone, ClickEmpty}
	}
	place := make([]string, 0, len(states))
	args := []any{ClickPending, time.Now().Unix()}
	for _, st := range states {
		if st == ClickPending || st == ClickDone {
			continue
		}
		place = append(place, "?")
		args = append(args, st)
	}
	if len(place) == 0 {
		return 0, nil
	}
	res, err := s.db.ExecContext(ctx, `UPDATE autoclick_queue SET state=?,attempts=0,next_at=0,last_error=NULL,updated_at=?
 WHERE state IN (`+strings.Join(place, ",")+`)`, args...)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// PruneClickQueue trims finished rows so the table cannot grow without bound.
func (s *Store) PruneClickQueue(ctx context.Context, keep int) error {
	if keep <= 0 {
		keep = 2000
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM autoclick_queue WHERE rowid IN (
 SELECT rowid FROM autoclick_queue WHERE state<>? ORDER BY rowid DESC LIMIT -1 OFFSET ?)`, ClickPending, keep)
	return err
}

// ErrClickEmpty reports that a message carries nothing to click any more.
var ErrClickEmpty = sql.ErrNoRows

// normalizeIDs drops duplicates while keeping order, so a scan that overlaps
// with realtime enqueueing does not enqueue the same message twice.
func normalizeIDs(ids []int) []int {
	seen := make(map[int]struct{}, len(ids))
	out := make([]int, 0, len(ids))
	for _, id := range ids {
		if id <= 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

// CleanClickError trims an error string so the queue table stays readable.
func CleanClickError(err error) string {
	if err == nil {
		return ""
	}
	s := strings.TrimSpace(err.Error())
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}
