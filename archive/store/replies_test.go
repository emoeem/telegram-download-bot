package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/gotd/td/tg"
)

func openReplyTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestFromTGPopulatesReplyFields(t *testing.T) {
	h := &tg.MessageReplyHeader{}
	h.SetReplyToMsgID(10)
	h.SetReplyToTopID(8)
	h.SetReplyToPeerID(&tg.PeerChannel{ChannelID: 55})
	m := &tg.Message{ID: 11, PeerID: &tg.PeerChannel{ChannelID: 55}, Date: 1}
	m.SetReplyTo(h)
	got := FromTG(m)
	if got.ReplyTo != 10 || got.ReplyToTopID != 8 || got.ThreadID != 8 || got.ReplyToPeerID != 55 {
		t.Fatalf("reply fields=%+v", got)
	}
}

func TestFromTGPopulatesForwardFields(t *testing.T) {
	m := &tg.Message{ID: 20, PeerID: &tg.PeerChannel{ChannelID: 55}, Date: 1}
	m.SetFwdFrom(tg.MessageFwdHeader{
		FromID:      &tg.PeerUser{UserID: 99},
		FromName:    "Alice",
		ChannelPost: 123,
		PostAuthor:  "Alice Channel",
	})
	got := FromTG(m)
	if got.ForwardFromID != 99 || got.ForwardFromMessageID != 123 || got.ForwardFromName != "Alice" || got.ForwardPostAuthor != "Alice Channel" {
		t.Fatalf("forward fields=%+v", got)
	}
}

func TestListRepliesByTopID(t *testing.T) {
	s := openReplyTestStore(t)
	ctx := context.Background()
	now := time.Now()
	for _, m := range []Message{
		{ChatID: 77, MessageID: 1, Date: now, Text: "root"},
		{ChatID: 77, MessageID: 2, Date: now.Add(time.Second), Text: "reply 1", ReplyTo: 1, ReplyToTopID: 1, ThreadID: 1},
		{ChatID: 77, MessageID: 3, Date: now.Add(2 * time.Second), Text: "reply 2", ReplyTo: 2, ReplyToTopID: 1, ThreadID: 1},
		{ChatID: 77, MessageID: 4, Date: now.Add(3 * time.Second), Text: "other", ReplyTo: 9},
	} {
		if err := s.UpsertMessage(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.ListReplies(ctx, 77, 1, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].MessageID != 2 || got[1].MessageID != 3 {
		t.Fatalf("replies=%+v", got)
	}
}
