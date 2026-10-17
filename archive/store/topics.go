package store

import (
	"context"
	"encoding/json"
	"strconv"
)

// TopicSummary is one forum topic of a chat as shown in the topic bar.
type TopicSummary struct {
	TopicID   int    `json:"topic_id"`
	Title     string `json:"title"`
	Count     int    `json:"count"`
	FirstDate int64  `json:"first_date"`
	LastDate  int64  `json:"last_date"`
}

// ChatIsForum reports whether the chat is a forum (topic-enabled) group.
// Unknown chats report false: the topic bar simply stays hidden.
func (s *Store) ChatIsForum(ctx context.Context, chatID int64) bool {
	var forum int
	if err := s.db.QueryRowContext(ctx, "SELECT is_forum FROM chats WHERE id=?", chatID).Scan(&forum); err != nil {
		return false
	}
	return forum == 1
}

// Topics aggregates the forum topics of a chat: one row per topic id, with the
// reply/message count and the newest activity. Titles come from the
// topic_created service event; when the root was not archived the earliest
// message text stands in, and the last resort is the raw topic id.
func (s *Store) Topics(ctx context.Context, chatID int64, limit int) ([]TopicSummary, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT tid, SUM(n), MIN(d), MAX(d) FROM (
	SELECT COALESCE(NULLIF(thread_id,0),NULLIF(reply_to_top_id,0)) AS tid, 1 AS n, date AS d
		FROM messages WHERE chat_id=? AND deleted=0
	UNION ALL
	SELECT e.message_id AS tid, 0 AS n, e.date AS d
		FROM message_events e WHERE e.chat_id=? AND e.event_type='topic_created'
) WHERE tid>0 GROUP BY tid ORDER BY MAX(d) DESC LIMIT ?`, chatID, chatID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TopicSummary{}
	for rows.Next() {
		var t TopicSummary
		if err := rows.Scan(&t.TopicID, &t.Count, &t.FirstDate, &t.LastDate); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	s.fillTopicTitles(ctx, chatID, out)
	return out, nil
}

// fillTopicTitles resolves a display title per topic: the topic_created
// payload title first, then the earliest message text, then "#<id>".
func (s *Store) fillTopicTitles(ctx context.Context, chatID int64, topics []TopicSummary) {
	titles := map[int]string{}
	rows, err := s.db.QueryContext(ctx,
		"SELECT message_id, payload_json FROM message_events WHERE chat_id=? AND event_type='topic_created'", chatID)
	if err == nil {
		for rows.Next() {
			var id int
			var payload string
			if rows.Scan(&id, &payload) == nil {
				var p map[string]any
				if json.Unmarshal([]byte(payload), &p) == nil {
					if title, ok := p["title"].(string); ok && title != "" {
						titles[id] = title
					}
				}
			}
		}
		rows.Close()
	}
	for i := range topics {
		t := &topics[i]
		if title, ok := titles[t.TopicID]; ok {
			t.Title = title
			continue
		}
		var text string
		_ = s.db.QueryRowContext(ctx, `SELECT COALESCE(NULLIF(text,''),NULLIF(caption,''),'')
 FROM messages WHERE chat_id=? AND COALESCE(NULLIF(thread_id,0),NULLIF(reply_to_top_id,0))=? AND deleted=0
 ORDER BY date ASC, message_id ASC LIMIT 1`, chatID, t.TopicID).Scan(&text)
		if len(text) > 60 {
			text = text[:60] + "…"
		}
		if text != "" {
			t.Title = text
		} else {
			t.Title = "话题 #" + strconv.Itoa(t.TopicID)
		}
	}
}

// TopicMessageFilterSQL is the shared WHERE fragment matching every message of
// one topic: replies carry the topic id in thread_id/reply_to_top_id, and the
// topic root itself is the topic_created service message with that id.
const TopicMessageFilterSQL = `(COALESCE(NULLIF(m.thread_id,0),NULLIF(m.reply_to_top_id,0))=? OR (m.message_id=? AND EXISTS(SELECT 1 FROM message_events te WHERE te.chat_id=m.chat_id AND te.message_id=m.message_id AND te.event_type='topic_created')))`
