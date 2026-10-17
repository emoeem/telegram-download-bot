package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"
)

// NotifyRule is a keyword-triggered notification outlet: when a newly archived
// realtime message matches the rule, the Notifier pushes it to the configured
// channels (webhook and/or Telegram Bot API chat).
type NotifyRule struct {
	ID              int64
	Name            string
	Keywords        []string
	ExcludeKeywords []string
	ChatIDs         []int64 // empty = all monitored chats
	WebhookURL      string
	BotToken        string
	BotChatID       string
	Enabled         bool
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// Matches reports whether an archived message triggers this rule. Matching
// mirrors the monitor keyword semantics: case-insensitive substring over
// text+caption, exclude wins, chat scope optional.
func (r NotifyRule) Matches(m Message) bool {
	if !r.Enabled {
		return false
	}
	if len(r.ChatIDs) > 0 {
		found := false
		for _, id := range r.ChatIDs {
			if id == m.ChatID {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	text := lowerASCII(m.Text + " " + m.Caption)
	for _, k := range r.ExcludeKeywords {
		if len(k) > 0 && containsSubstring(text, lowerASCII(k)) {
			return false
		}
	}
	if len(r.Keywords) == 0 {
		return true
	}
	for _, k := range r.Keywords {
		if len(k) > 0 && containsSubstring(text, lowerASCII(k)) {
			return true
		}
	}
	return false
}

func lowerASCII(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 'a' - 'A'
		}
	}
	return string(b)
}

func containsSubstring(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

func (s *Store) AddNotifyRule(ctx context.Context, r NotifyRule) (int64, error) {
	now := time.Now().Unix()
	k, _ := json.Marshal(r.Keywords)
	e, _ := json.Marshal(r.ExcludeKeywords)
	c, _ := json.Marshal(r.ChatIDs)
	res, err := s.db.ExecContext(ctx, `INSERT INTO notify_rules(name,keywords_json,exclude_keywords_json,chat_ids_json,webhook_url,bot_token,bot_chat_id,enabled,created_at,updated_at)
 VALUES(?,?,?,?,?,?,?,?,?,?)`, r.Name, string(k), string(e), string(c), r.WebhookURL, r.BotToken, r.BotChatID, r.Enabled, now, now)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func scanNotifyRule(row rowScanner) (NotifyRule, error) {
	var r NotifyRule
	var k, e, c string
	var created, updated int64
	if err := row.Scan(&r.ID, &r.Name, &k, &e, &c, &r.WebhookURL, &r.BotToken, &r.BotChatID, &r.Enabled, &created, &updated); err != nil {
		return r, err
	}
	_ = json.Unmarshal([]byte(k), &r.Keywords)
	_ = json.Unmarshal([]byte(e), &r.ExcludeKeywords)
	_ = json.Unmarshal([]byte(c), &r.ChatIDs)
	r.CreatedAt, r.UpdatedAt = time.Unix(created, 0), time.Unix(updated, 0)
	return r, nil
}

const notifyRuleColumns = "id,name,keywords_json,exclude_keywords_json,chat_ids_json,webhook_url,bot_token,bot_chat_id,enabled,created_at,updated_at"

func (s *Store) NotifyRules(ctx context.Context) ([]NotifyRule, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+notifyRuleColumns+" FROM notify_rules ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NotifyRule
	for rows.Next() {
		r, err := scanNotifyRule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) NotifyRule(ctx context.Context, id int64) (NotifyRule, error) {
	return scanNotifyRule(s.db.QueryRowContext(ctx, "SELECT "+notifyRuleColumns+" FROM notify_rules WHERE id=?", id))
}

// UpdateNotifyRule applies a partial update. Keyword lists are replaced only
// when non-nil; credential fields are deliberately immutable here so an edit
// that only changes keywords cannot wipe them — recreate the rule instead.
func (s *Store) UpdateNotifyRule(ctx context.Context, id int64, r NotifyRule) error {
	sets, args := []string{}, []any{}
	if r.Name != "" {
		sets = append(sets, "name=?")
		args = append(args, r.Name)
	}
	if r.Keywords != nil {
		k, _ := json.Marshal(r.Keywords)
		sets = append(sets, "keywords_json=?")
		args = append(args, string(k))
	}
	if r.ExcludeKeywords != nil {
		e, _ := json.Marshal(r.ExcludeKeywords)
		sets = append(sets, "exclude_keywords_json=?")
		args = append(args, string(e))
	}
	if r.ChatIDs != nil {
		c, _ := json.Marshal(r.ChatIDs)
		sets = append(sets, "chat_ids_json=?")
		args = append(args, string(c))
	}
	sets = append(sets, "enabled=?", "updated_at=?")
	args = append(args, r.Enabled, time.Now().Unix(), id)
	// SET lists join with commas — joinAnd is only for WHERE clauses; using it
	// here silently turns the assignment into a boolean expression.
	res, err := s.db.ExecContext(ctx, "UPDATE notify_rules SET "+strings.Join(sets, ", ")+" WHERE id=?", args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) RemoveNotifyRule(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM notify_rules WHERE id=?", id)
	return err
}

// NotifyDelivery records the outcome of one notification attempt.
type NotifyDelivery struct {
	RuleID    int64
	ChatID    int64
	MessageID int
	Channel   string // "webhook" | "telegram"
	Status    string // "sent" | "failed" | "skipped"
	Error     string
	CreatedAt int64
}

func (s *Store) RecordNotifyDelivery(ctx context.Context, d NotifyDelivery) error {
	_, err := s.db.ExecContext(ctx, "INSERT INTO notify_log(rule_id,chat_id,message_id,channel,status,error,created_at) VALUES(?,?,?,?,?,?,?)",
		d.RuleID, nullableInt64(d.ChatID), nullableInt64(int64(d.MessageID)), d.Channel, d.Status, nullIfEmpty(d.Error), time.Now().Unix())
	return err
}

// RecentNotifyDeliveries returns the newest delivery attempts for display,
// newest first.
func (s *Store) RecentNotifyDeliveries(ctx context.Context, limit int) ([]NotifyDelivery, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, "SELECT rule_id,COALESCE(chat_id,0),COALESCE(message_id,0),channel,status,COALESCE(error,''),created_at FROM notify_log ORDER BY id DESC LIMIT ?", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NotifyDelivery
	for rows.Next() {
		var d NotifyDelivery
		if err := rows.Scan(&d.RuleID, &d.ChatID, &d.MessageID, &d.Channel, &d.Status, &d.Error, &d.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// PruneNotifyLog keeps only the newest keep entries; called opportunistically
// after writes so the log cannot grow without bound.
func (s *Store) PruneNotifyLog(ctx context.Context, keep int) error {
	if keep <= 0 {
		keep = 500
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM notify_log WHERE id NOT IN (SELECT id FROM notify_log ORDER BY id DESC LIMIT ?)`, keep)
	return err
}
