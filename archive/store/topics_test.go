package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/gotd/td/tg"
)

func seedForumChat(t *testing.T, s *Store, ctx context.Context) {
	t.Helper()
	// Mark the chat as a forum and upsert the topic-create service events.
	if _, err := s.DB().ExecContext(ctx, "INSERT INTO chats(id,type,title,is_forum,created_at,updated_at) VALUES(1001,'channel','论坛群',1,0,0) ON CONFLICT(id) DO UPDATE SET is_forum=1"); err != nil {
		t.Fatal(err)
	}
	eventDate := int64(99)
	for _, tc := range []struct {
		id  int
		peg string
	}{{500, "故障反馈"}, {501, "资源分享"}} {
		// The topic root is archived as the service message row; the event
		// needs its real rowid because message_events carries an FK.
		if err := s.UpsertMessage(ctx, Message{ChatID: 1001, MessageID: tc.id, Date: time.Unix(eventDate, 0)}); err != nil {
			t.Fatal(err)
		}
		rowID, err := s.RowID(ctx, 1001, tc.id)
		if err != nil {
			t.Fatal(err)
		}
		// Same payload shape ParseMessageService produces for topic_created.
		raw, _ := json.Marshal(map[string]any{"type": "messageActionTopicCreate", "title": tc.peg})
		if _, err := s.DB().ExecContext(ctx, `INSERT INTO message_events(message_rowid,chat_id,message_id,event_type,payload_json,date,created_at) VALUES(?,1001,?,'topic_created',?,?,?)`, rowID, tc.id, string(raw), eventDate, eventDate); err != nil {
			t.Fatal(err)
		}
	}
	// Messages: topic 500 has 2 replies, topic 501 has 1, plus untopic'd chatter.
	msgs := []Message{
		{ChatID: 1001, MessageID: 1, Date: time.Unix(100, 0), Text: "general chat"},
		{ChatID: 1001, MessageID: 2, Date: time.Unix(101, 0), Text: "第一条故障", ReplyToTopID: 500, ThreadID: 500},
		{ChatID: 1001, MessageID: 3, Date: time.Unix(102, 0), Text: "第二条故障", ReplyToTopID: 500, ThreadID: 500},
		{ChatID: 1001, MessageID: 4, Date: time.Unix(103, 0), Text: "分享一个资源", ReplyToTopID: 501, ThreadID: 501},
	}
	for _, m := range msgs {
		if err := s.UpsertMessage(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
}

func TestTopicsAggregation(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	seedForumChat(t, s, ctx)

	if !s.ChatIsForum(ctx, 1001) {
		t.Fatal("chat should be forum")
	}
	if s.ChatIsForum(ctx, 9999) {
		t.Fatal("unknown chat must not be forum")
	}
	topics, err := s.Topics(ctx, 1001, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(topics) != 2 {
		t.Fatalf("topics = %+v", topics)
	}
	// Ordered by newest activity: topic 501 (date 103) before 500 (date 102).
	if topics[0].TopicID != 501 || topics[0].Title != "资源分享" || topics[0].Count != 1 {
		t.Fatalf("topic[0] = %+v", topics[0])
	}
	if topics[1].TopicID != 500 || topics[1].Title != "故障反馈" || topics[1].Count != 2 {
		t.Fatalf("topic[1] = %+v", topics[1])
	}

	// A non-forum chat reports no topics.
	topics, err = s.Topics(ctx, 4242, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(topics) != 0 {
		t.Fatalf("non-forum chat topics = %+v", topics)
	}
}

func TestTopicTitleFallbackUsesFirstMessage(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	if _, err := s.DB().ExecContext(ctx, "INSERT INTO chats(id,type,title,is_forum,created_at,updated_at) VALUES(7,'channel','g',1,0,0)"); err != nil {
		t.Fatal(err)
	}
	// No topic_created event archived: the earliest in-topic message text
	// stands in as the title.
	msgs := []Message{
		{ChatID: 7, MessageID: 11, Date: time.Unix(2, 0), Text: "话题里的第一条消息", ReplyToTopID: 42, ThreadID: 42},
		{ChatID: 7, MessageID: 12, Date: time.Unix(3, 0), Text: "后续", ReplyToTopID: 42, ThreadID: 42},
	}
	for _, m := range msgs {
		if err := s.UpsertMessage(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	topics, err := s.Topics(ctx, 7, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(topics) != 1 || topics[0].TopicID != 42 || topics[0].Title != "话题里的第一条消息" || topics[0].Count != 2 {
		t.Fatalf("topics = %+v", topics)
	}
}

func TestListMessagesTopicFilter(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	seedForumChat(t, s, ctx)

	page, err := s.ListMessages(ctx, MessageFilter{ChatID: 1001, Topic: 500, Page: 1, PageSize: 50})
	if err != nil {
		t.Fatal(err)
	}
	// The topic root (service message) plus its two replies.
	if page.Total != 3 || len(page.Items) != 3 {
		t.Fatalf("topic 500 page = %+v", page)
	}
	for _, it := range page.Items {
		if it.ThreadID != 500 && it.MessageID != 500 {
			t.Fatalf("foreign message in topic 500: %+v", it)
		}
	}

	// Topic 0 means no topic filter: everything comes back, including the two
	// topic root service messages.
	page, err = s.ListMessages(ctx, MessageFilter{ChatID: 1001, Page: 1, PageSize: 50})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 6 {
		t.Fatalf("unfiltered total = %d", page.Total)
	}

	// Chat filter alone must work (the web feed regression).
	page, err = s.ListMessages(ctx, MessageFilter{ChatID: 1001, Page: 1, PageSize: 50})
	if err != nil || page.Total != 6 {
		t.Fatalf("chat filter page=%+v err=%v", page, err)
	}
	other, err := s.ListMessages(ctx, MessageFilter{ChatID: 2002, Page: 1, PageSize: 50})
	if err != nil || other.Total != 0 {
		t.Fatalf("other chat total=%d err=%v", other.Total, err)
	}
}

func TestParseTopicCreateEvent(t *testing.T) {
	ev, err := ParseMessageService(&tg.MessageService{
		ID: 500, PeerID: &tg.PeerChannel{ChannelID: 1001},
		FromID: &tg.PeerUser{UserID: 2}, Date: 100,
		Action: &tg.MessageActionTopicCreate{Title: "新话题"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if ev.EventType != "topic_created" || ev.MessageID != 500 {
		t.Fatalf("event = %+v", ev)
	}
	if !containsSubstring(ev.PayloadJSON, "新话题") {
		t.Fatalf("payload missing title: %s", ev.PayloadJSON)
	}
}
