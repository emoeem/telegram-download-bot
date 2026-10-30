package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

func (s *Store) UpsertMessage(ctx context.Context, m Message) error {
	now := time.Now().Unix()
	fts := m.SearchText()
	buttons, _ := json.Marshal(m.Buttons)
	if string(buttons) == "null" {
		buttons = []byte("[]")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO messages(chat_id,message_id,sender_id,date,edit_date,text,caption,grouped_id,reply_to_message_id,reply_to_top_id,reply_to_peer_id,thread_id,forward_from_id,forward_from_chat_id,forward_from_message_id,forward_from_name,forward_post_author,views,forwards,reply_count,has_media,out,pinned,raw_json,fts_text,buttons_json,created_at,updated_at)
 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(chat_id,message_id) DO UPDATE SET sender_id=excluded.sender_id,date=excluded.date,edit_date=excluded.edit_date,text=excluded.text,caption=excluded.caption,grouped_id=excluded.grouped_id,reply_to_message_id=excluded.reply_to_message_id,reply_to_top_id=excluded.reply_to_top_id,reply_to_peer_id=excluded.reply_to_peer_id,thread_id=excluded.thread_id,forward_from_id=excluded.forward_from_id,forward_from_chat_id=excluded.forward_from_chat_id,forward_from_message_id=excluded.forward_from_message_id,forward_from_name=excluded.forward_from_name,forward_post_author=excluded.forward_post_author,views=excluded.views,forwards=excluded.forwards,reply_count=excluded.reply_count,has_media=excluded.has_media,out=excluded.out,pinned=excluded.pinned,raw_json=excluded.raw_json,fts_text=excluded.fts_text,buttons_json=excluded.buttons_json,updated_at=excluded.updated_at`,
		m.ChatID, m.MessageID, m.SenderID, m.Date.Unix(), nullableTime(m.EditDate), m.Text, m.Caption, nullableInt64(m.GroupedID), nullableInt64(int64(m.ReplyTo)), nullableInt64(int64(m.ReplyToTopID)), nullableInt64(m.ReplyToPeerID), nullableInt64(int64(m.ThreadID)), nullableInt64(m.ForwardFromID), nullableInt64(m.ForwardFromChatID), nullableInt64(int64(m.ForwardFromMessageID)), m.ForwardFromName, m.ForwardPostAuthor, m.Views, m.Forwards, m.ReplyCount, m.HasMedia, m.Out, m.Pinned, m.Raw, fts, string(buttons), now, now)
	return err
}
func nullableTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.Unix()
}
func nullableInt64(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}
func (s *Store) RowID(ctx context.Context, chatID int64, msgID int) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, "SELECT id FROM messages WHERE chat_id=? AND message_id=?", chatID, msgID).Scan(&id)
	return id, err
}

// RecordEdit snapshots the CURRENTLY stored content of the message into the
// edit timeline BEFORE the caller overwrites the row with the edited version
// (proposal §8: "当前版本快照进 message_edits，再更新主表"). Storing m.Text
// here would archive the new content and lose the original forever.
func (s *Store) RecordEdit(ctx context.Context, m Message) error {
	if m.EditDate == nil {
		return nil
	}
	id, err := s.RowID(ctx, m.ChatID, m.MessageID)
	if err != nil {
		return nil // not archived yet — nothing to snapshot
	}
	var curText, curCaption, curRaw string
	var curEdit int64
	err = s.db.QueryRowContext(ctx, "SELECT COALESCE(text,''),COALESCE(caption,''),COALESCE(raw_json,''),COALESCE(edit_date,0) FROM messages WHERE id=?", id).Scan(&curText, &curCaption, &curRaw, &curEdit)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	// Re-delivered edit (already applied) or content unchanged: no new version.
	if curEdit == m.EditDate.Unix() || (curText == m.Text && curCaption == m.Caption) {
		return nil
	}
	var n int
	_ = s.db.QueryRowContext(ctx, "SELECT COALESCE(MAX(version),0) FROM message_edits WHERE message_rowid=?", id).Scan(&n)
	_, err = s.db.ExecContext(ctx, "INSERT INTO message_edits(message_rowid,version,text,caption,raw_json,edit_date,archived_at) VALUES(?,?,?,?,?,?,?)", id, n+1, curText, curCaption, curRaw, curEdit, time.Now().Unix())
	return err
}
func (s *Store) SoftDelete(ctx context.Context, chatID int64, ids []int) error {
	if len(ids) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, id := range ids {
		var rowID, date int64
		if err = tx.QueryRowContext(ctx, "SELECT id,date FROM messages WHERE chat_id=? AND message_id=?", chatID, id).Scan(&rowID, &date); err != nil {
			if err == sql.ErrNoRows {
				continue
			}
			return err
		}
		now := time.Now().Unix()
		if _, err = tx.ExecContext(ctx, "UPDATE messages SET deleted=1,deleted_at=?,updated_at=? WHERE chat_id=? AND message_id=?", now, now, chatID, id); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "DELETE FROM message_events WHERE message_rowid=? AND event_type='message_deleted'", rowID); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO message_events(message_rowid,chat_id,message_id,event_type,date,created_at) VALUES(?,?,?,?,?,?)", rowID, chatID, id, "message_deleted", date, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}
func (s *Store) SetChat(ctx context.Context, id int64, title, username, kind string) error {
	now := time.Now().Unix()
	_, err := s.db.ExecContext(ctx, `INSERT INTO chats(id,type,title,username,created_at,updated_at) VALUES(?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET type=excluded.type,title=excluded.title,username=excluded.username,updated_at=excluded.updated_at`, id, kind, title, username, now, now)
	return err
}
func (s *Store) LastMessageID(ctx context.Context, chatID int64) (int, error) {
	var id sql.NullInt64
	err := s.db.QueryRowContext(ctx, "SELECT MAX(message_id) FROM messages WHERE chat_id=? AND deleted=0", chatID).Scan(&id)
	if err != nil {
		return 0, err
	}
	if !id.Valid {
		return 0, nil
	}
	return int(id.Int64), nil
}
func (s *Store) FirstMessageID(ctx context.Context, chatID int64) (int, error) {
	var id sql.NullInt64
	err := s.db.QueryRowContext(ctx, "SELECT MIN(message_id) FROM messages WHERE chat_id=? AND deleted=0", chatID).Scan(&id)
	if err != nil {
		return 0, err
	}
	if !id.Valid {
		return 0, nil
	}
	return int(id.Int64), nil
}
func (s *Store) Message(ctx context.Context, chatID int64, msgID int) (string, error) {
	var raw []byte
	var text string
	var date int64
	var deleted int
	err := s.db.QueryRowContext(ctx, "SELECT COALESCE(raw_json,''),COALESCE(text,caption,''),date,deleted FROM messages WHERE chat_id=? AND message_id=?", chatID, msgID).Scan(&raw, &text, &date, &deleted)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("chat=%d message=%d date=%s deleted=%t\n%s\nraw=%s", chatID, msgID, time.Unix(date, 0).Format(time.RFC3339), deleted == 1, text, string(raw)), nil
}
func (s *Store) RefreshMetadata(ctx context.Context, m Message) error {
	row, err := s.RowID(ctx, m.ChatID, m.MessageID)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "DELETE FROM message_links WHERE message_rowid=?", row); err != nil {
		return err
	}
	for _, u := range ExtractLinks(m.Text + " " + m.Caption) {
		if _, err = tx.ExecContext(ctx, "INSERT INTO message_links(message_rowid,url,domain) VALUES(?,?,?)", row, u, linkDomain(u)); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM message_entities WHERE message_rowid=?", row); err != nil {
		return err
	}
	for _, e := range m.Entities {
		if _, err = tx.ExecContext(ctx, "INSERT INTO message_entities(message_rowid,type,offset,length,value) VALUES(?,?,?,?,?)", row, e.Type, e.Offset, e.Length, e.Value); err != nil {
			return err
		}
	}
	// Reactions are rewritten wholesale: the incoming list is the full current
	// counter set (Telegram sends aggregates, not deltas), so replace instead of
	// merge to keep removed reactions from lingering.
	now := time.Now().Unix()
	if _, err = tx.ExecContext(ctx, "DELETE FROM message_reactions WHERE message_rowid=?", row); err != nil {
		return err
	}
	for _, rc := range m.Reactions {
		if _, err = tx.ExecContext(ctx, "INSERT INTO message_reactions(message_rowid,emoji,count,updated_at) VALUES(?,?,?,?) ON CONFLICT(message_rowid,emoji) DO UPDATE SET count=excluded.count,updated_at=excluded.updated_at", row, rc.Emoji, rc.Count, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}
func linkDomain(u string) string {
	for i := 0; i < len(u); i++ {
		if u[i] == '/' && i+2 < len(u) && u[i+1] == '/' {
			start := i + 2
			end := start
			for end < len(u) && u[end] != '/' && u[end] != '?' && u[end] != '#' {
				end++
			}
			return u[start:end]
		}
	}
	return ""
}

type EditView struct {
	Version    int    `json:"version"`
	Text       string `json:"text"`
	Caption    string `json:"caption"`
	EditDate   int64  `json:"edit_date"`
	ArchivedAt int64  `json:"archived_at"`
	IsCurrent  bool   `json:"is_current,omitempty"`
}

func (s *Store) Edits(ctx context.Context, chatID int64, msgID int) ([]EditView, error) {
	timeline, err := s.GetMessageEditTimeline(ctx, chatID, msgID)
	if err != nil {
		return nil, err
	}
	if len(timeline) == 0 {
		return nil, nil
	}
	rowID, err := s.RowID(ctx, chatID, msgID)
	if err != nil {
		return nil, err
	}
	var out []EditView
	for _, v := range timeline {
		view := EditView{Version: v.Version, Text: v.Text, Caption: v.Caption, EditDate: v.EditedAt, IsCurrent: v.IsCurrent}
		if !v.IsCurrent {
			if err := s.db.QueryRowContext(ctx, "SELECT archived_at FROM message_edits WHERE message_rowid=? AND version=? ORDER BY id LIMIT 1", rowID, v.Version).Scan(&view.ArchivedAt); err != nil {
				return nil, err
			}
		}
		out = append(out, view)
	}
	return out, nil
}

// SoftDeleteRowIDs marks messages deleted in the local archive only. Telegram
// is untouched: this is the "hide it from my views" operation an operator
// reaches for when the remote copy must stay where it is.
//
// The row id is the archive's own identity, so callers do not have to pair a
// chat id with a message id they may no longer have.
func (s *Store) SoftDeleteRowIDs(ctx context.Context, rowIDs []int64) (int64, error) {
	if len(rowIDs) == 0 {
		return 0, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	now := time.Now().Unix()
	var n int64
	for _, rowID := range rowIDs {
		if rowID <= 0 {
			continue
		}
		res, err := tx.ExecContext(ctx, "UPDATE messages SET deleted=1,deleted_at=?,updated_at=? WHERE id=?", now, now, rowID)
		if err != nil {
			return n, err
		}
		// A row that was already deleted still counts as success: the caller
		// asked for an end state, not for a transition.
		if affected, _ := res.RowsAffected(); affected > 0 {
			n++
		}
	}
	return n, tx.Commit()
}

// RestoreRowIDs undoes SoftDeleteRowIDs.
func (s *Store) RestoreRowIDs(ctx context.Context, rowIDs []int64) (int64, error) {
	if len(rowIDs) == 0 {
		return 0, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	now := time.Now().Unix()
	var n int64
	for _, rowID := range rowIDs {
		if rowID <= 0 {
			continue
		}
		res, err := tx.ExecContext(ctx, "UPDATE messages SET deleted=0,deleted_at=NULL,updated_at=? WHERE id=?", now, rowID)
		if err != nil {
			return n, err
		}
		if affected, _ := res.RowsAffected(); affected > 0 {
			n++
		}
	}
	return n, tx.Commit()
}

// DeleteMessageHard removes one message row plus its media rows and reports the
// object files that lost their last reference. The caller unlinks them through
// ApplyPurge, which keeps the file layer in one place.
//
// The whole removal is one transaction so a crash cannot leave a message row
// pointing at already-released objects. Never-numbered media goes through the
// same path: it has no object yet, so only the row disappears.
func (s *Store) DeleteMessageHard(ctx context.Context, rowID int64) (PurgeReport, error) {
	rep := PurgeReport{}
	if rowID <= 0 {
		return rep, fmt.Errorf("row id is required")
	}
	items, err := s.purgeRowsForMessage(ctx, rowID)
	if err != nil {
		return rep, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return rep, err
	}
	defer tx.Rollback()
	files, err := purgeItemsTx(ctx, tx, items, []int64{rowID})
	if err != nil {
		return rep, err
	}
	if err := tx.Commit(); err != nil {
		return rep, err
	}
	for _, it := range items {
		rep.Media++
		rep.Bytes += it.size
		if it.serial > 0 {
			rep.Serials = append(rep.Serials, it.serial)
		}
	}
	rep.Messages = 1
	rep.Files = files
	return rep, nil
}
