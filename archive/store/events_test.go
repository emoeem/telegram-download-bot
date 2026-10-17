package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/gotd/td/tg"
)

func openEventTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func serviceMessage(action tg.MessageActionClass, id int, chat int64, actor int64) *tg.MessageService {
	return &tg.MessageService{ID: id, PeerID: &tg.PeerChat{ChatID: chat}, FromID: &tg.PeerUser{UserID: actor}, Date: int(time.Now().Unix()), Action: action}
}

func TestParseServiceMessageCreatesEvent(t *testing.T) {
	s := openEventTestStore(t)
	m := serviceMessage(&tg.MessageActionChatEditTitle{Title: "New Title"}, 1, 123, 42)
	if err := s.SaveMessageService(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.DB().QueryRow("SELECT COUNT(*) FROM message_events").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("events=%d", count)
	}
	e, err := s.GetMessageEventByMessageRowID(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if e.EventType != "title_changed" || e.ChatID != 123 || e.ActorID != 42 {
		t.Fatalf("event=%+v", e)
	}
}

func TestListMessageEventsByChatAndType(t *testing.T) {
	s := openEventTestStore(t)
	ctx := context.Background()
	for i, action := range []tg.MessageActionClass{
		&tg.MessageActionChatEditTitle{Title: "Title"},
		&tg.MessageActionChatAddUser{Users: []int64{99}},
		&tg.MessageActionPinMessage{},
	} {
		if err := s.SaveMessageService(ctx, serviceMessage(action, i+1, 77, 42)); err != nil {
			t.Fatal(err)
		}
	}
	events, err := s.ListMessageEvents(ctx, 77, nil, nil, 20)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, e := range events {
		got[e.EventType] = true
	}
	for _, want := range []string{"title_changed", "user_added", "pinned"} {
		if !got[want] {
			t.Fatalf("missing %s in %+v", want, got)
		}
	}
}

func TestServiceMessageEventTypes(t *testing.T) {
	s := openEventTestStore(t)
	ctx := context.Background()
	cases := []struct {
		action tg.MessageActionClass
		want   string
	}{
		{&tg.MessageActionChatEditTitle{Title: "x"}, "title_changed"},
		{&tg.MessageActionChatJoinedByLink{InviterID: 7}, "user_joined"},
		{&tg.MessageActionPinMessage{}, "pinned"},
	}
	for i, tc := range cases {
		m := serviceMessage(tc.action, i+1, 88, 42)
		if err := s.SaveMessageService(ctx, m); err != nil {
			t.Fatal(err)
		}
		e, err := s.GetMessageEventByMessageRowID(ctx, int64(i+1))
		if err != nil {
			t.Fatal(err)
		}
		if e.EventType != tc.want {
			t.Fatalf("want=%s got=%s", tc.want, e.EventType)
		}
	}
}
