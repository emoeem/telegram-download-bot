package store

import (
	"context"
	"database/sql"
	"time"
)

// Backlog states recorded in sync_cursors.backlog_state.
const (
	BacklogPending = "pending"
	BacklogRunning = "running"
	BacklogDone    = "done"
)

// SyncCursor is the persisted per-chat sync watermark: how far the archive has
// caught up (LastMsgID) and how far a deep/backwards backfill has reached
// (BacklogOldestMsgID). It survives restarts, which is what makes an interrupted
// deep backfill resumable instead of restarting from the newest page.
type SyncCursor struct {
	AccountID          int64
	ChatID             int64
	ChannelPTS, PTS    int
	BacklogState       string
	BacklogOldestMsgID int
	LastMsgID          int
	LastSeenAt         int64
}

// SyncCursor returns the stored cursor, or a zero cursor with state "pending"
// when the chat has never been synced.
func (s *Store) SyncCursor(ctx context.Context, accountID, chatID int64) (SyncCursor, error) {
	c := SyncCursor{AccountID: accountID, ChatID: chatID, BacklogState: BacklogPending}
	var oldest, last sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT channel_pts,pts,backlog_state,backlog_oldest_msg_id,last_msg_id,last_seen_at
FROM sync_cursors WHERE account_id=? AND chat_id=?`, accountID, chatID).
		Scan(&c.ChannelPTS, &c.PTS, &c.BacklogState, &oldest, &last, &c.LastSeenAt)
	if err == sql.ErrNoRows {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	c.BacklogOldestMsgID = int(oldest.Int64)
	c.LastMsgID = int(last.Int64)
	return c, nil
}

// SaveSyncCursor upserts a cursor row.
func (s *Store) SaveSyncCursor(ctx context.Context, c SyncCursor) error {
	if c.BacklogState == "" {
		c.BacklogState = BacklogPending
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO sync_cursors(account_id,chat_id,channel_pts,pts,backlog_state,backlog_oldest_msg_id,last_msg_id,last_seen_at,updated_at)
VALUES(?,?,?,?,?,?,?,?,?)
ON CONFLICT(account_id,chat_id) DO UPDATE SET
 channel_pts=excluded.channel_pts,
 pts=excluded.pts,
 backlog_state=excluded.backlog_state,
 backlog_oldest_msg_id=COALESCE(excluded.backlog_oldest_msg_id,sync_cursors.backlog_oldest_msg_id),
 last_msg_id=MAX(COALESCE(excluded.last_msg_id,0),sync_cursors.last_msg_id),
 last_seen_at=excluded.last_seen_at,
 updated_at=excluded.updated_at`,
		c.AccountID, c.ChatID, c.ChannelPTS, c.PTS, c.BacklogState,
		nullableInt64(int64(c.BacklogOldestMsgID)), nullableInt64(int64(c.LastMsgID)), c.LastSeenAt, time.Now().Unix())
	return err
}

// MessageIDRange reports the oldest and newest archived message ids of a chat.
func (s *Store) MessageIDRange(ctx context.Context, chatID int64) (oldest, newest int, err error) {
	var lo, hi sql.NullInt64
	if err = s.db.QueryRowContext(ctx, "SELECT MIN(message_id),MAX(message_id) FROM messages WHERE chat_id=?", chatID).Scan(&lo, &hi); err != nil {
		return 0, 0, err
	}
	return int(lo.Int64), int(hi.Int64), nil
}

// SetChatLastMsgID records the newest archived message id on the chat row, which
// is what the gap check compares against the server's newest message. It upserts:
// a chat can be backfilled before its metadata row was ever written, and a silent
// no-op there would leave the gap check permanently blind.
func (s *Store) SetChatLastMsgID(ctx context.Context, chatID int64, id int) error {
	now := time.Now().Unix()
	_, err := s.db.ExecContext(ctx, `INSERT INTO chats(id,type,last_msg_id,created_at,updated_at) VALUES(?,'unknown',?,?,?)
ON CONFLICT(id) DO UPDATE SET last_msg_id=MAX(excluded.last_msg_id,chats.last_msg_id),updated_at=excluded.updated_at`,
		chatID, id, now, now)
	return err
}

// ChatLastMsgID reads the newest archived message id recorded for a chat.
func (s *Store) ChatLastMsgID(ctx context.Context, chatID int64) (int, error) {
	var id sql.NullInt64
	if err := s.db.QueryRowContext(ctx, "SELECT last_msg_id FROM chats WHERE id=?", chatID).Scan(&id); err != nil {
		if err == sql.ErrNoRows {
			return 0, nil
		}
		return 0, err
	}
	return int(id.Int64), nil
}
