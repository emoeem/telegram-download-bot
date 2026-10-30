package store

import (
	"context"
	"encoding/json"
)

// ExportChat is one dialog described the way Telegram's own exporter describes
// it, so the file this feeds can be opened by tools that already read Telegram
// Desktop exports.
//
// Type is not read from chats.type: that column only ever holds "channel" or
// "chat" because ingest cannot know better at that point. The real distinction
// (broadcast vs supergroup) lives in raw_json, which holds the marshalled
// tg.Channel. Losing it would silently label every supergroup a channel.
type ExportChat struct {
	ID       int64
	Type     string
	Title    string
	Username string
	Messages int64
}

// channelFlags is the tiny shape needed out of raw_json. Decoding the whole
// tg.Channel would drag in bin.Fields, whose JSON form is not a plain object;
// asking for two bools keeps this robust against that representation.
type channelFlags struct {
	Broadcast bool
	Megagroup bool
}

// ExportChatType maps one archived chat to the type string Telegram's exporter
// emits. The names come from Telegram Desktop's own export writer, not from
// MTProto constructor names, so a consumer sees the vocabulary it expects.
func ExportChatType(rawJSON []byte, username string) string {
	public := username != ""
	var f channelFlags
	if len(rawJSON) > 0 && json.Unmarshal(rawJSON, &f) == nil {
		switch {
		case f.Broadcast && public:
			return "public_channel"
		case f.Broadcast:
			return "private_channel"
		case f.Megagroup && public:
			return "public_supergroup"
		case f.Megagroup:
			return "private_supergroup"
		}
	}
	// No raw_json (a chat known only from a sync cursor) or no flags recorded:
	// fall back to the one signal that is always present.
	if public {
		return "public_channel"
	}
	return "private_channel"
}

// ListExportChats returns every chat that still has at least one live message.
// A chat with nothing to write would produce an empty dialog in the export,
// which is worse than omitting it: importers create a channel for it.
func (s *Store) ListExportChats(ctx context.Context) ([]ExportChat, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT c.id, COALESCE(c.title,''), COALESCE(c.username,''), COALESCE(c.raw_json,''), COUNT(m.id)
FROM chats c
JOIN messages m ON m.chat_id=c.id AND m.deleted=0
GROUP BY c.id
HAVING COUNT(m.id) > 0
ORDER BY c.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]ExportChat, 0, 8)
	for rows.Next() {
		var c ExportChat
		var raw []byte
		if err := rows.Scan(&c.ID, &c.Title, &c.Username, &raw, &c.Messages); err != nil {
			return nil, err
		}
		c.Type = ExportChatType(raw, c.Username)
		out = append(out, c)
	}
	return out, rows.Err()
}

// ChatMessagesAsc walks one chat forward by message_id. A keyset cursor rather
// than OFFSET: an export reads the whole chat, and OFFSET makes SQLite rescan
// everything already emitted, which turns a linear export quadratic.
//
// Deleted rows are skipped: a deleted message is not part of the chat any more,
// and Telegram's exporter does not resurrect them either.
func (s *Store) ChatMessagesAsc(ctx context.Context, chatID int64, afterMessageID int, limit int) ([]MessageView, error) {
	if limit <= 0 || limit > 1000 {
		limit = 500
	}
	rows, err := s.db.QueryContext(ctx, messageViewSelect+
		" WHERE m.chat_id=? AND m.deleted=0 AND m.message_id>? ORDER BY m.message_id ASC LIMIT ?",
		chatID, afterMessageID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]MessageView, 0, limit)
	for rows.Next() {
		v, err := scanMessageView(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
