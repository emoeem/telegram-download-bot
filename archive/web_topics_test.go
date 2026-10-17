package archive

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/iyear/tdl/archive/store"
)

// The feed must honor the chat and topic query params: picking a source in the
// sidebar filters messagesPage to that chat (this used to be silently ignored).
func TestMessagesPageChatAndTopicFilters(t *testing.T) {
	s, ts := newNotifyWebServer(t, true)
	seedTopicFixture(t, s)

	// chat filter
	resp, err := http.Get(ts.URL + "/api/messages/page?page=1&page_size=50&chat=1001")
	if err != nil {
		t.Fatal(err)
	}
	var page struct {
		Total int `json:"total"`
		Items []store.MessageView
	}
	_ = json.NewDecoder(resp.Body).Decode(&page)
	resp.Body.Close()
	if page.Total != 6 {
		t.Fatalf("chat=1001 total=%d", page.Total)
	}

	// topic filter includes the root and the replies
	resp, err = http.Get(ts.URL + "/api/messages/page?page=1&page_size=50&chat=1001&topic=500")
	if err != nil {
		t.Fatal(err)
	}
	_ = json.NewDecoder(resp.Body).Decode(&page)
	resp.Body.Close()
	if page.Total != 3 {
		t.Fatalf("topic=500 total=%d", page.Total)
	}

	// an unrelated chat stays empty
	resp, err = http.Get(ts.URL + "/api/messages/page?page=1&page_size=50&chat=4242")
	if err != nil {
		t.Fatal(err)
	}
	_ = json.NewDecoder(resp.Body).Decode(&page)
	resp.Body.Close()
	if page.Total != 0 {
		t.Fatalf("chat=4242 total=%d", page.Total)
	}
}

func TestTopicsEndpoint(t *testing.T) {
	s, ts := newNotifyWebServer(t, true)
	seedTopicFixture(t, s)

	resp, err := http.Get(ts.URL + "/api/topics?chat=1001")
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Forum  bool                 `json:"forum"`
		Topics []store.TopicSummary `json:"topics"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if !out.Forum || len(out.Topics) != 2 {
		t.Fatalf("topics response = %+v", out)
	}
	byID := map[int]store.TopicSummary{}
	for _, tp := range out.Topics {
		byID[tp.TopicID] = tp
	}
	if byID[500].Title != "故障反馈" || byID[501].Title != "资源分享" {
		t.Fatalf("titles = %+v", out.Topics)
	}

	// Non-forum chats answer with forum=false and an empty list.
	resp, err = http.Get(ts.URL + "/api/topics?chat=8888")
	if err != nil {
		t.Fatal(err)
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if out.Forum || len(out.Topics) != 0 {
		t.Fatalf("non-forum response = %+v", out)
	}

	// chat is required.
	resp, err = http.Get(ts.URL + "/api/topics")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing chat status %d", resp.StatusCode)
	}
}

// seedTopicFixture mirrors the store-level fixture through public APIs.
func seedTopicFixture(t *testing.T, s *store.Store) {
	t.Helper()
	ctx := t.Context()
	if _, err := s.DB().ExecContext(ctx, "INSERT INTO chats(id,type,title,is_forum,created_at,updated_at) VALUES(1001,'channel','论坛群',1,0,0) ON CONFLICT(id) DO UPDATE SET is_forum=1"); err != nil {
		t.Fatal(err)
	}
	msgs := []store.Message{
		{ChatID: 1001, MessageID: 500, Date: time.Unix(99, 0)},
		{ChatID: 1001, MessageID: 501, Date: time.Unix(99, 0)},
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
	for _, tc := range []struct {
		id    int
		title string
	}{{500, "故障反馈"}, {501, "资源分享"}} {
		rowID, err := s.RowID(ctx, 1001, tc.id)
		if err != nil {
			t.Fatal(err)
		}
		payload, _ := json.Marshal(map[string]any{"title": tc.title})
		if _, err := s.DB().ExecContext(ctx, `INSERT INTO message_events(message_rowid,chat_id,message_id,event_type,payload_json,date,created_at) VALUES(?,1001,?,'topic_created',?,?,?)`, rowID, tc.id, string(payload), 99, 99); err != nil {
			t.Fatal(err)
		}
	}
}
