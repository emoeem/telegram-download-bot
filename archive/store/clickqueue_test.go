package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/gotd/td/tg"
)

func openClickStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "archive.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// insertMessage writes one message row so the queue has something to resolve.
func insertClickMessage(t *testing.T, s *Store, chatID int64, msgID int, buttons string, out bool) {
	t.Helper()
	now := time.Now()
	m := Message{ChatID: chatID, MessageID: msgID, Date: now, Text: "t", Out: out}
	if buttons != "" {
		m.Buttons = []InlineButton{{Text: "查询", Kind: "callback"}}
	}
	if err := s.UpsertMessage(context.Background(), m); err != nil {
		t.Fatalf("upsert message %d: %v", msgID, err)
	}
	if buttons == "" {
		// UpsertMessage always writes the (empty) button list; force the column
		// to the shape an archive written before button capture would have.
		if _, err := s.DB().Exec(`UPDATE messages SET buttons_json=? WHERE chat_id=? AND message_id=?`, buttons, chatID, msgID); err != nil {
			t.Fatalf("clear buttons: %v", err)
		}
	}
}

func TestClickQueueEnqueueIsIdempotent(t *testing.T) {
	ctx := context.Background()
	s := openClickStore(t)

	n, err := s.EnqueueClicks(ctx, 7, []int{1, 2, 3}, 0)
	if err != nil || n != 3 {
		t.Fatalf("first enqueue = %d, %v; want 3, nil", n, err)
	}
	n, err = s.EnqueueClicks(ctx, 7, []int{2, 3, 4}, 0)
	if err != nil || n != 1 {
		t.Fatalf("second enqueue = %d, %v; want 1 (only id 4 is new)", n, err)
	}
	stats, err := s.ClickQueueStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats[ClickPending] != 4 {
		t.Fatalf("pending = %d, want 4", stats[ClickPending])
	}
}

func TestDueClicksRespectsBackoffAndOrder(t *testing.T) {
	ctx := context.Background()
	s := openClickStore(t)
	now := time.Now().Unix()

	if _, err := s.EnqueueClicks(ctx, 9, []int{10}, now+3600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnqueueClicks(ctx, 9, []int{11}, 0); err != nil {
		t.Fatal(err)
	}
	jobs, err := s.DueClicks(ctx, now, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].MessageID != 11 {
		t.Fatalf("due = %+v, want only message 11", jobs)
	}
}

func TestMarkClickIsTerminal(t *testing.T) {
	ctx := context.Background()
	s := openClickStore(t)
	if _, err := s.EnqueueClicks(ctx, 3, []int{5}, 0); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkClick(ctx, 3, 5, ClickDone, ""); err != nil {
		t.Fatal(err)
	}
	jobs, err := s.DueClicks(ctx, time.Now().Unix()+10, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 0 {
		t.Fatalf("a done job must never come due again, got %+v", jobs)
	}
	// Enqueueing the same message again must not resurrect it either.
	if n, err := s.EnqueueClicks(ctx, 3, []int{5}, 0); err != nil || n != 0 {
		t.Fatalf("re-enqueue = %d, %v; want 0", n, err)
	}
}

func TestRequeueClicksSkipsDone(t *testing.T) {
	ctx := context.Background()
	s := openClickStore(t)
	for _, id := range []int{1, 2, 3} {
		if _, err := s.EnqueueClicks(ctx, 4, []int{id}, 0); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.MarkClick(ctx, 4, 1, ClickDone, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkClick(ctx, 4, 2, ClickFailed, "boom"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkClick(ctx, 4, 3, ClickGone, "deleted"); err != nil {
		t.Fatal(err)
	}
	n, err := s.RequeueClicks(ctx, nil)
	if err != nil || n != 2 {
		t.Fatalf("requeue = %d, %v; want 2 (done stays done)", n, err)
	}
	stats, err := s.ClickQueueStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats[ClickPending] != 2 || stats[ClickDone] != 1 {
		t.Fatalf("stats = %+v, want pending 2 / done 1", stats)
	}
}

func TestButtonedMessageIDsFilters(t *testing.T) {
	ctx := context.Background()
	s := openClickStore(t)
	insertClickMessage(t, s, 100, 1, "", false)         // no keyboard
	insertClickMessage(t, s, 100, 2, "callback", false) // clickable
	insertClickMessage(t, s, 100, 3, "callback", true)  // our own message: skipped
	insertClickMessage(t, s, 100, 4, "callback", false) // clickable

	ids, err := s.ButtonedMessageIDs(ctx, 100, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 || ids[0] != 4 || ids[1] != 2 {
		t.Fatalf("ids = %v, want [4 2] (newest first, out messages excluded)", ids)
	}
}

// The entity URL is the only record of where a text-link pill points; losing it
// at ingest makes the button permanently unclickable.
func TestFromTGPersistsTextLinkURL(t *testing.T) {
	m := &tg.Message{
		ID: 9, PeerID: &tg.PeerChannel{ChannelID: 5}, Message: "按钮",
		Entities: []tg.MessageEntityClass{
			&tg.MessageEntityTextURL{Offset: 0, Length: 2, URL: "https://t.me/bot?start=X"},
			&tg.MessageEntityBold{Offset: 0, Length: 2},
		},
	}
	x := FromTG(m)
	var found string
	for _, e := range x.Entities {
		if e.Value != "" {
			found = e.Value
		}
	}
	if found != "https://t.me/bot?start=X" {
		t.Fatalf("text_link URL not persisted: %+v", x.Entities)
	}
}
