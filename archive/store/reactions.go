package store

import (
	"context"
	"time"
)

// ReactionView is one reaction counter as exposed to the web/API layer.
type ReactionView struct {
	Emoji string `json:"emoji"`
	Count int    `json:"count"`
}

// SetMessageReactions replaces the stored reactions of one message without
// touching the rest of the row. It is the entry point for realtime
// updateMessageReactions deliveries, which carry the full aggregate.
func (s *Store) SetMessageReactions(ctx context.Context, chatID int64, msgID int, reactions []Reaction) error {
	row, err := s.RowID(ctx, chatID, msgID)
	if err != nil {
		return err // not archived (yet): nothing to attach reactions to
	}
	now := time.Now().Unix()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "DELETE FROM message_reactions WHERE message_rowid=?", row); err != nil {
		return err
	}
	for _, rc := range reactions {
		if _, err = tx.ExecContext(ctx, "INSERT INTO message_reactions(message_rowid,emoji,count,updated_at) VALUES(?,?,?,?) ON CONFLICT(message_rowid,emoji) DO UPDATE SET count=excluded.count,updated_at=excluded.updated_at", row, rc.Emoji, rc.Count, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ReactionsByRowIDs loads the reaction counters of many messages in one query
// and groups them by rowid.
func (s *Store) ReactionsByRowIDs(ctx context.Context, ids []int64) (map[int64][]ReactionView, error) {
	out := make(map[int64][]ReactionView, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	placeholders := make([]byte, 0, len(ids)*2)
	args := make([]any, 0, len(ids))
	for i, id := range ids {
		if i > 0 {
			placeholders = append(placeholders, ',')
		}
		placeholders = append(placeholders, '?')
		args = append(args, id)
	}
	rows, err := s.db.QueryContext(ctx, "SELECT message_rowid,emoji,count FROM message_reactions WHERE message_rowid IN ("+string(placeholders)+") ORDER BY id", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var rowID int64
		var r ReactionView
		if err := rows.Scan(&rowID, &r.Emoji, &r.Count); err != nil {
			return nil, err
		}
		out[rowID] = append(out[rowID], r)
	}
	return out, rows.Err()
}

// MessageViewStatsRow is one (message_id, views, forwards) triple as reported
// by messages.getMessagesViews.
type MessageViewStatsRow struct {
	MessageID int
	Views     int
	Forwards  int
}

// ApplyMessageViews persists refreshed view/forward counters for a chat. Only
// the given rows are touched, so messages the server no longer reports keep
// their last known numbers.
func (s *Store) ApplyMessageViews(ctx context.Context, chatID int64, rows []MessageViewStatsRow) (int, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	now := time.Now().Unix()
	n := 0
	for _, r := range rows {
		res, err := tx.ExecContext(ctx, "UPDATE messages SET views=?, forwards=?, updated_at=? WHERE chat_id=? AND message_id=?", r.Views, r.Forwards, now, chatID, r.MessageID)
		if err != nil {
			return n, err
		}
		if affected, _ := res.RowsAffected(); affected > 0 {
			n++
		}
	}
	return n, tx.Commit()
}

// RecentMessageIDs returns the newest archived (non-deleted) message ids of a
// chat, descending. It feeds the periodic views refresh.
func (s *Store) RecentMessageIDs(ctx context.Context, chatID int64, limit int) ([]int, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	rows, err := s.db.QueryContext(ctx, "SELECT message_id FROM messages WHERE chat_id=? AND deleted=0 ORDER BY message_id DESC LIMIT ?", chatID, limit)
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
