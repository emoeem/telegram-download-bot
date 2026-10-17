package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/gotd/td/tg"
)

func reactionTestMessage(t *testing.T, s *Store, ctx context.Context, chatID, msgID int64, text string) {
	t.Helper()
	if err := s.UpsertMessage(ctx, Message{ChatID: chatID, MessageID: int(msgID), Date: time.Unix(100, 0), Text: text}); err != nil {
		t.Fatal(err)
	}
}

func TestRefreshMetadataPersistsReactions(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	reactionTestMessage(t, s, ctx, 10, 1, "hello")

	m := Message{ChatID: 10, MessageID: 1, Date: time.Unix(100, 0), Text: "hello", Reactions: []Reaction{
		{Emoji: "👍", Count: 3}, {Emoji: "custom:1234", Count: 1},
	}}
	if err := s.RefreshMetadata(ctx, m); err != nil {
		t.Fatal(err)
	}
	rowID, _ := s.RowID(ctx, 10, 1)
	got, err := s.ReactionsByRowIDs(ctx, []int64{rowID})
	if err != nil {
		t.Fatal(err)
	}
	rs := got[rowID]
	if len(rs) != 2 || rs[0].Emoji != "👍" || rs[0].Count != 3 || rs[1].Emoji != "custom:1234" {
		t.Fatalf("reactions = %+v", rs)
	}

	// A later delivery with fewer counters must not leave stale rows behind.
	m.Reactions = []Reaction{{Emoji: "👍", Count: 5}}
	if err := s.RefreshMetadata(ctx, m); err != nil {
		t.Fatal(err)
	}
	got, err = s.ReactionsByRowIDs(ctx, []int64{rowID})
	if err != nil {
		t.Fatal(err)
	}
	rs = got[rowID]
	if len(rs) != 1 || rs[0].Count != 5 {
		t.Fatalf("reactions after replace = %+v", rs)
	}

	// Views must survive RefreshMetadata and stay reachable through the view.
	v, err := s.MessageView(ctx, 10, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Reactions) != 1 || v.Reactions[0].Count != 5 {
		t.Fatalf("MessageView.Reactions = %+v", v.Reactions)
	}
}

func TestSetMessageReactionsForUnarchivedMessageFails(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.SetMessageReactions(context.Background(), 99, 5, []Reaction{{Emoji: "👍", Count: 1}}); err == nil {
		t.Fatal("expected an error for a message that is not archived")
	}
}

func TestFromTGExtractsReactions(t *testing.T) {
	m := &tg.Message{ID: 1, PeerID: &tg.PeerChannel{ChannelID: 7}, Date: 100}
	// gotd conditional fields need the flag set, so use the setter.
	m.SetReactions(tg.MessageReactions{Results: []tg.ReactionCount{
		{Reaction: &tg.ReactionEmoji{Emoticon: "🔥"}, Count: 9},
		{Reaction: &tg.ReactionCustomEmoji{DocumentID: 42}, Count: 2},
		{Reaction: &tg.ReactionEmoji{Emoticon: "💀"}, Count: 0},
	}})
	got := FromTG(m)
	if len(got.Reactions) != 2 {
		t.Fatalf("reactions = %+v", got.Reactions)
	}
	if got.Reactions[0].Emoji != "🔥" || got.Reactions[0].Count != 9 {
		t.Fatalf("reaction[0] = %+v", got.Reactions[0])
	}
	if got.Reactions[1].Emoji != "custom:42" || got.Reactions[1].Count != 2 {
		t.Fatalf("reaction[1] = %+v", got.Reactions[1])
	}
}

func TestApplyMessageViews(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	for i := 1; i <= 3; i++ {
		if err := s.UpsertMessage(ctx, Message{ChatID: 5, MessageID: i, Date: time.Unix(int64(100+i), 0), Text: "m"}); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.ApplyMessageViews(ctx, 5, []MessageViewStatsRow{
		{MessageID: 1, Views: 100, Forwards: 7},
		{MessageID: 999, Views: 1, Forwards: 1}, // unknown id: ignored
	})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("updated %d rows", n)
	}
	v, err := s.MessageView(ctx, 5, 1)
	if err != nil {
		t.Fatal(err)
	}
	if v.Views != 100 || v.Forwards != 7 {
		t.Fatalf("views=%d forwards=%d", v.Views, v.Forwards)
	}
}

func TestRecentMessageIDsDescending(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	for i := 1; i <= 5; i++ {
		if err := s.UpsertMessage(ctx, Message{ChatID: 5, MessageID: i, Date: time.Unix(int64(100+i), 0), Text: "m"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SoftDelete(ctx, 5, []int{5}); err != nil {
		t.Fatal(err)
	}
	ids, err := s.RecentMessageIDs(ctx, 5, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 4 || ids[0] != 4 || ids[len(ids)-1] != 1 {
		t.Fatalf("ids = %v", ids)
	}
}
