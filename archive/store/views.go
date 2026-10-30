package store

import (
	"context"
	"encoding/json"
	"sort"
)

type MessageView struct {
	RowID, ChatID, GroupedID                                      int64
	ReplyToMessageID, ReplyToTopID, ThreadID                      int
	ReplyToPeerID, ReplyToRowID                                   int64
	ForwardFromID, ForwardFromChatID                              int64
	ForwardFromMessageID                                          int
	ForwardFromName, ForwardPostAuthor                            string
	ReplyToText, ReplyToSenderName                                string
	MessageID                                                     int
	SenderID                                                      int64
	SenderName, SenderUsername, AvatarPath                        string
	ChatTitle                                                     string
	Date, EditDate, DeletedAt                                     int64
	Text, Caption                                                 string
	MediaSHA256, MediaMime, MediaPath, ThumbnailPath, PreviewPath string
	MediaStatus                                                   string
	Width, Height                                                 int
	// Duration is video/voice length in seconds and FileName is the original
	// uploaded name. Both are carried because the Telegram-format export needs
	// them to describe a media message the way the client does; the web views
	// simply ignore them.
	Duration  int
	FileName  string
	MediaSize int64
	// MediaType is the ingest-time kind (photo/video/voice/sticker/...). The
	// Telegram-format export needs it to pick both the target directory and the
	// media_type field; mime alone cannot tell a voice note from a music track.
	MediaType                   string
	Deleted, Pinned             bool
	Views, Forwards, ReplyCount int
	Reactions                   []ReactionView `json:"reactions,omitempty"`
	Buttons                     []InlineButton `json:"buttons,omitempty"`
	EventType                   string
}

type MessageFilter struct {
	Query    string
	ChatID   int64
	Topic    int64
	Page     int
	PageSize int
}

type MessagePage struct {
	Items    []MessageView
	Total    int
	Page     int
	PageSize int
}

// messageViewColumns is the single definition of the "render one archived
// message" projection. Every read path shares it so the column order and the
// scan order below can never drift apart.
const messageViewColumns = `m.id,m.chat_id,COALESCE(m.grouped_id,0),COALESCE(m.reply_to_message_id,0),COALESCE(m.reply_to_top_id,0),COALESCE(m.thread_id,0),COALESCE(m.reply_to_peer_id,0),COALESCE((SELECT r.id FROM messages r WHERE r.chat_id=COALESCE(m.reply_to_peer_id,m.chat_id) AND r.message_id=m.reply_to_message_id LIMIT 1),0),COALESCE(m.forward_from_id,0),COALESCE(m.forward_from_chat_id,0),COALESCE(m.forward_from_message_id,0),COALESCE(m.forward_from_name,''),COALESCE(m.forward_post_author,''),COALESCE((SELECT COALESCE(NULLIF(r.text,''),NULLIF(r.caption,''),'') FROM messages r WHERE r.chat_id=COALESCE(m.reply_to_peer_id,m.chat_id) AND r.message_id=m.reply_to_message_id LIMIT 1),''),COALESCE((SELECT COALESCE(u2.first_name||' '||u2.last_name,u2.username,'') FROM messages r2 LEFT JOIN users u2 ON u2.id=r2.sender_id WHERE r2.chat_id=COALESCE(m.reply_to_peer_id,m.chat_id) AND r2.message_id=m.reply_to_message_id LIMIT 1),''),m.message_id,COALESCE(m.sender_id,0),COALESCE(u.first_name||' '||u.last_name,u.username,''),COALESCE(u.username,''),COALESCE(u.avatar_path,''),COALESCE(c.title,c.username,''),m.date,COALESCE(m.edit_date,0),COALESCE(m.deleted_at,0),COALESCE(NULLIF(m.text,''),NULLIF(m.caption,''),''),COALESCE(me.sha256,''),COALESCE(me.mime_type,''),COALESCE(me.original_path,''),COALESCE(me.thumbnail_path,''),COALESCE(me.preview_path,''),COALESCE(me.status,''),COALESCE(me.size,0),COALESCE(me.width,0),COALESCE(me.height,0),m.deleted,m.pinned,COALESCE(m.views,0),COALESCE(m.forwards,0),COALESCE(m.reply_count,0),COALESCE((SELECT event_type FROM message_events WHERE message_rowid=m.id ORDER BY id DESC LIMIT 1),''),COALESCE(m.buttons_json,''),COALESCE(me.duration,0),COALESCE(me.file_name,''),COALESCE(me.type,'')`

// messageViewFrom is the matching FROM clause for messageViewColumns. The media
// join is pinned to one row: media is unique per (message_rowid, source_url), so
// a message carrying both an attachment and a t.me link has two rows and a bare
// join would repeat the post in every list. The ingested attachment (non-empty
// sha256) wins over a link row, and MIN(id) keeps the choice deterministic.
const messageViewFrom = ` FROM messages m LEFT JOIN users u ON u.id=m.sender_id LEFT JOIN chats c ON c.id=m.chat_id LEFT JOIN media me ON me.id=COALESCE((SELECT MIN(pm.id) FROM media pm WHERE pm.message_rowid=m.id AND pm.sha256 IS NOT NULL AND pm.sha256<>''),(SELECT MIN(pm.id) FROM media pm WHERE pm.message_rowid=m.id))`

const messageViewSelect = "SELECT " + messageViewColumns + messageViewFrom

// rowScanner is satisfied by both *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanMessageView(sc rowScanner) (MessageView, error) {
	var v MessageView
	var buttons string
	err := sc.Scan(&v.RowID, &v.ChatID, &v.GroupedID, &v.ReplyToMessageID, &v.ReplyToTopID, &v.ThreadID, &v.ReplyToPeerID, &v.ReplyToRowID, &v.ForwardFromID, &v.ForwardFromChatID, &v.ForwardFromMessageID, &v.ForwardFromName, &v.ForwardPostAuthor, &v.ReplyToText, &v.ReplyToSenderName, &v.MessageID, &v.SenderID, &v.SenderName, &v.SenderUsername, &v.AvatarPath, &v.ChatTitle, &v.Date, &v.EditDate, &v.DeletedAt, &v.Text, &v.MediaSHA256, &v.MediaMime, &v.MediaPath, &v.ThumbnailPath, &v.PreviewPath, &v.MediaStatus, &v.MediaSize, &v.Width, &v.Height, &v.Deleted, &v.Pinned, &v.Views, &v.Forwards, &v.ReplyCount, &v.EventType, &buttons, &v.Duration, &v.FileName, &v.MediaType)
	if err == nil && buttons != "" && buttons != "[]" {
		_ = json.Unmarshal([]byte(buttons), &v.Buttons)
	}
	return v, err
}

func (s *Store) ListMessages(ctx context.Context, f MessageFilter) (MessagePage, error) {
	if f.Page < 1 {
		f.Page = 1
	}
	if f.PageSize <= 0 || f.PageSize > 200 {
		f.PageSize = 50
	}
	opt := SearchOptions{ChatID: f.ChatID, Topic: f.Topic, Limit: f.PageSize, Offset: (f.Page - 1) * f.PageSize}
	results, err := s.SearchAdvanced(ctx, f.Query, opt)
	if err != nil {
		return MessagePage{}, err
	}
	total, err := s.CountSearch(ctx, f.Query, opt)
	if err != nil {
		return MessagePage{}, err
	}
	ids := make([]int64, 0, len(results))
	for _, r := range results {
		ids = append(ids, r.RowID)
	}
	views, err := s.MessageViewsByRowIDs(ctx, ids)
	if err != nil {
		return MessagePage{}, err
	}
	items, err := s.expandGroupBoundaries(ctx, orderMessageViews(views, ids))
	if err != nil {
		return MessagePage{}, err
	}
	return MessagePage{Items: items, Total: total, Page: f.Page, PageSize: f.PageSize}, nil
}

// expandGroupBoundaries pulls in the rest of any album sliced by the page
// window. One media group is one logical post: when the page boundary falls
// inside it, the two halves render as two half-empty cards, each missing part
// of the artwork and (worse) the half that carries the caption may land on the
// other page, so the visible half looks like a picture with no text at all.
//
// Only the first and last rows can be cut, so at most two groups get expanded.
func (s *Store) expandGroupBoundaries(ctx context.Context, items []MessageView) ([]MessageView, error) {
	if len(items) < 2 {
		return items, nil
	}
	seen := make(map[int64]bool, len(items))
	for _, v := range items {
		seen[v.RowID] = true
	}
	groups := make(map[int64]bool, 2)
	for _, v := range []MessageView{items[0], items[len(items)-1]} {
		if v.GroupedID != 0 {
			groups[v.GroupedID] = true
		}
	}
	if len(groups) == 0 {
		return items, nil
	}
	extra := make([]int64, 0, 16)
	for gid := range groups {
		rows, err := s.db.QueryContext(ctx, "SELECT id FROM messages WHERE grouped_id=? AND deleted=0", gid)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, err
			}
			if !seen[id] {
				extra = append(extra, id)
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	if len(extra) == 0 {
		return items, nil
	}
	more, err := s.MessageViewsByRowIDs(ctx, extra)
	if err != nil {
		return nil, err
	}
	out := append(items, more...)
	// Same order the page query uses, so the expanded rows land next to their
	// siblings instead of at the bottom of the list.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Date != out[j].Date {
			return out[i].Date > out[j].Date
		}
		return out[i].RowID > out[j].RowID
	})
	return out, nil
}

// MessageViewsByGroup renders every member of one album, in the order Telegram
// sent them. The detail view needs the whole post, not one frame: the caption
// lives on exactly one member, so opening any other member would show a bare
// picture and look like the text was lost.
func (s *Store) MessageViewsByGroup(ctx context.Context, chatID, groupedID int64) ([]MessageView, error) {
	if groupedID == 0 {
		return nil, nil
	}
	// The chat predicate is appended conditionally: "(?=0 OR m.chat_id=?)" makes
	// the planner skip idx_messages_chat_msg and scan the grouped_id index alone.
	q := messageViewSelect + " WHERE m.grouped_id=? AND m.deleted=0"
	args := []any{groupedID}
	if chatID != 0 {
		q += " AND m.chat_id=?"
		args = append(args, chatID)
	}
	q += " ORDER BY m.id ASC"
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]MessageView, 0, 10)
	for rows.Next() {
		v, err := scanMessageView(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// GetMessageByRowID renders one message plus its media by rowid.
func (s *Store) GetMessageByRowID(ctx context.Context, rowid int64) (MessageView, error) {
	v, err := scanMessageView(s.db.QueryRowContext(ctx, messageViewSelect+" WHERE m.id=?", rowid))
	if err != nil {
		return v, err
	}
	if reactions, e := s.ReactionsByRowIDs(ctx, []int64{rowid}); e == nil {
		v.Reactions = reactions[rowid]
	}
	return v, nil
}

// MessageViewsByRowIDs renders many messages in one round trip instead of one
// query per row, and returns them in the caller's order.
func (s *Store) MessageViewsByRowIDs(ctx context.Context, ids []int64) ([]MessageView, error) {
	if len(ids) == 0 {
		return nil, nil
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
	rows, err := s.db.QueryContext(ctx, messageViewSelect+" WHERE m.id IN ("+string(placeholders)+")", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]MessageView, 0, len(ids))
	for rows.Next() {
		v, err := scanMessageView(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	reactions, err := s.ReactionsByRowIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Reactions = reactions[out[i].RowID]
	}
	return out, nil
}

// orderMessageViews restores the order of ids (an IN (...) query does not
// guarantee result order) and drops rows that disappeared meanwhile.
func orderMessageViews(views []MessageView, ids []int64) []MessageView {
	if len(views) <= 1 {
		return views
	}
	byID := make(map[int64]MessageView, len(views))
	for _, v := range views {
		byID[v.RowID] = v
	}
	out := make([]MessageView, 0, len(views))
	for _, id := range ids {
		if v, ok := byID[id]; ok {
			out = append(out, v)
		}
	}
	return out
}

func (s *Store) RecentMessages(ctx context.Context, chatID int64, limit int) ([]MessageView, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	// Conditional predicate so the planner can use idx_messages_chat_date; the
	// "(?=0 OR chat_id=?)" form cannot use it.
	q := messageViewSelect
	args := []any{}
	if chatID != 0 {
		q += " WHERE m.chat_id=?"
		args = append(args, chatID)
	}
	q += " ORDER BY m.date DESC LIMIT ?"
	args = append(args, limit)
	rows, e := s.db.QueryContext(ctx, q, args...)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var out []MessageView
	for rows.Next() {
		v, e := scanMessageView(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *Store) MessageView(ctx context.Context, chatID int64, msgID int) (MessageView, error) {
	v, err := scanMessageView(s.db.QueryRowContext(ctx, messageViewSelect+" WHERE m.chat_id=? AND m.message_id=?", chatID, msgID))
	if err != nil {
		return v, err
	}
	if reactions, e := s.ReactionsByRowIDs(ctx, []int64{v.RowID}); e == nil {
		v.Reactions = reactions[v.RowID]
	}
	return v, nil
}

// ListReplies returns the messages of a thread (comment/reply chain) in
// chronological order. The thread root itself is not included: it is matched by
// reply_to_top_id/thread_id, which the root does not carry.
func (s *Store) ListReplies(ctx context.Context, chatID int64, topID int, limit int) ([]MessageView, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, messageViewSelect+
		" WHERE m.chat_id=? AND (m.reply_to_top_id=? OR m.thread_id=?) ORDER BY m.date ASC, m.message_id ASC LIMIT ?",
		chatID, topID, topID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MessageView
	for rows.Next() {
		v, err := scanMessageView(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
