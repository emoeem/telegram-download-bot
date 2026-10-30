package store

import (
	"context"
	"database/sql"
	"time"
)

// Settings keys holding the dedicated auto-click delivery outlet (Telegram Bot
// API). Stored in the archive_settings KV like other single-value config.
const (
	AutoClickBotTokenKey  = "autoclick_bot_token"
	AutoClickBotChatIDKey = "autoclick_bot_chat_id"
)

// AutoClickOutlet is the dedicated delivery outlet for auto-click results.
type AutoClickOutlet struct {
	BotToken  string
	BotChatID string
}

// SaveAutoClickOutlet upserts the delivery outlet. Empty values clear it.
func (s *Store) SaveAutoClickOutlet(ctx context.Context, o AutoClickOutlet) error {
	if err := s.SetSetting(ctx, AutoClickBotTokenKey, o.BotToken); err != nil {
		return err
	}
	return s.SetSetting(ctx, AutoClickBotChatIDKey, o.BotChatID)
}

// AutoClickOutlet loads the delivery outlet. A missing config yields zero
// values: the clicker still runs and logs, it just cannot deliver.
func (s *Store) AutoClickOutlet(ctx context.Context) (AutoClickOutlet, error) {
	var o AutoClickOutlet
	token, err := s.Setting(ctx, AutoClickBotTokenKey)
	if err != nil {
		return o, nil
	}
	chat, err := s.Setting(ctx, AutoClickBotChatIDKey)
	if err != nil {
		return o, nil
	}
	o.BotToken, o.BotChatID = token, chat
	return o, nil
}

// AutoClickLogEntry records one auto-click attempt or delivery outcome so the
// web UI can show what the clicker did and why.
type AutoClickLogEntry struct {
	ID         int64     `json:"id"`
	ChatID     int64     `json:"chat_id"`
	MessageID  int       `json:"message_id"`
	ButtonText string    `json:"button_text"`
	Kind       string    `json:"kind"`
	Status     string    `json:"status"`
	Detail     string    `json:"detail"`
	CreatedAt  time.Time `json:"created_at"`
}

func (s *Store) AddAutoClickLog(ctx context.Context, e AutoClickLogEntry) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO autoclick_log(chat_id,message_id,button_text,button_kind,status,detail,created_at)
 VALUES(?,?,?,?,?,?,?)`, e.ChatID, e.MessageID, e.ButtonText, e.Kind, e.Status, e.Detail, time.Now().Unix())
	return err
}

// HasAutoClickLog reports whether any click activity was already recorded for
// this chat+message, so restarts and gap replays do not click twice.
func (s *Store) HasAutoClickLog(ctx context.Context, chatID int64, msgID int) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx, "SELECT 1 FROM autoclick_log WHERE chat_id=? AND message_id=? LIMIT 1", chatID, msgID).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// AutoClickTargetSeen reports whether this click destination already fired at
// least once. The key is the destination itself (bot username plus /start
// payload, or the raw URL), never the message id: a reward bot echoes the same
// /start back as a brand-new message, so message-level deduplication is blind
// to exactly the case that loops.
func (s *Store) AutoClickTargetSeen(ctx context.Context, key string) (bool, error) {
	if key == "" {
		return false, nil
	}
	var one int
	err := s.db.QueryRowContext(ctx, "SELECT 1 FROM autoclick_targets WHERE key=? LIMIT 1", key).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// RecordAutoClickTarget marks a click destination as fired. Repeated calls only
// bump the hit counter; the first timestamp is preserved so the log can tell
// "clicked once, echoed six times" apart from "clicked six times".
func (s *Store) RecordAutoClickTarget(ctx context.Context, key string) error {
	if key == "" {
		return nil
	}
	now := time.Now().Unix()
	if _, err := s.db.ExecContext(ctx, `INSERT INTO autoclick_targets(key,first_clicked_at,last_clicked_at,hits)
 VALUES(?,?,?,1) ON CONFLICT(key) DO UPDATE SET last_clicked_at=excluded.last_clicked_at, hits=hits+1`, key, now, now); err != nil {
		return err
	}
	return nil
}

// CountAutoClickTargets reports how many distinct destinations were clicked and
// how many click attempts in total they account for.
func (s *Store) CountAutoClickTargets(ctx context.Context) (distinct int, hits int, err error) {
	row := s.db.QueryRowContext(ctx, "SELECT COUNT(*), COALESCE(SUM(hits),0) FROM autoclick_targets")
	if err := row.Scan(&distinct, &hits); err != nil {
		return 0, 0, err
	}
	return distinct, hits, nil
}

func (s *Store) ListAutoClickLog(ctx context.Context, limit int) ([]AutoClickLogEntry, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, "SELECT id,chat_id,message_id,button_text,button_kind,status,detail,created_at FROM autoclick_log ORDER BY id DESC LIMIT ?", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AutoClickLogEntry
	for rows.Next() {
		var e AutoClickLogEntry
		var ts int64
		if err := rows.Scan(&e.ID, &e.ChatID, &e.MessageID, &e.ButtonText, &e.Kind, &e.Status, &e.Detail, &ts); err != nil {
			return nil, err
		}
		e.CreatedAt = time.Unix(ts, 0)
		out = append(out, e)
	}
	return out, rows.Err()
}

// PruneAutoClickLog keeps only the newest keep entries.
func (s *Store) PruneAutoClickLog(ctx context.Context, keep int) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM autoclick_log WHERE id NOT IN
 (SELECT id FROM autoclick_log ORDER BY id DESC LIMIT ?)`, keep)
	return err
}
