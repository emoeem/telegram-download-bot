package archive

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/iyear/tdl/archive/store"
)

func newQueueTestClicker(t *testing.T) (*AutoClicker, *store.Store) {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "archive.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return NewAutoClicker(s, nil, func() *Runtime { return nil }), s
}

// The pacing gate is what keeps a burst of buttoned messages from being turned
// into a burst of clicks: the second click in the same chat inside the window
// must be refused, while another chat is unaffected.
func TestClickPaceDefersSecondClickInSameChat(t *testing.T) {
	a, _ := newQueueTestClicker(t)

	if !a.paceOK(11) {
		t.Fatal("first click in a chat must be allowed")
	}
	if a.paceOK(11) {
		t.Fatal("second click inside clickPace must be refused")
	}
	if !a.paceOK(22) {
		t.Fatal("pacing must be per chat, not global")
	}

	// Once the window has passed the same chat is allowed again.
	a.lastClickMu.Lock()
	a.lastClick[11] = time.Now().Add(-2 * clickPace)
	a.lastClickMu.Unlock()
	if !a.paceOK(11) {
		t.Fatal("click after the pacing window must be allowed")
	}
}

// A queued message whose row vanished from the archive has nothing to resolve:
// it must be retired as gone, not retried forever.
func TestClickQueuedMarksGoneForMissingRow(t *testing.T) {
	a, s := newQueueTestClicker(t)
	ctx := context.Background()
	job := store.ClickJob{ChatID: 5, MessageID: 99}
	if _, err := s.EnqueueClicks(ctx, job.ChatID, []int{job.MessageID}, 0); err != nil {
		t.Fatal(err)
	}

	a.clickQueued(ctx, nil, job)

	stats, err := s.ClickQueueStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats[store.ClickGone] != 1 {
		t.Fatalf("stats = %+v, want one gone row", stats)
	}
}

// A canceled context is not evidence that the message is gone: a click that
// overlaps a shutdown must stay queued for the next drain.
func TestClickQueuedCancellationKeepsJobPending(t *testing.T) {
	a, s := newQueueTestClicker(t)
	ctx := context.Background()
	job := store.ClickJob{ChatID: 5, MessageID: 99}
	if _, err := s.EnqueueClicks(ctx, job.ChatID, []int{job.MessageID}, 0); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertMessage(ctx, store.Message{ChatID: job.ChatID, MessageID: job.MessageID, Date: time.Unix(1700000000, 0), Text: "btn"}); err != nil {
		t.Fatal(err)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	a.clickQueued(canceled, nil, job)

	stats, err := s.ClickQueueStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats[store.ClickGone] != 0 {
		t.Fatalf("shutdown marked the click gone: %+v", stats)
	}
	if stats[store.ClickPending] != 1 {
		t.Fatalf("stats = %+v, want the row still pending", stats)
	}
}

// Retry budget: the last attempt is terminal so a permanently broken message
// cannot spin forever.
func TestRescheduleGivesUpAfterMaxAttempts(t *testing.T) {
	a, s := newQueueTestClicker(t)
	ctx := context.Background()

	// Below the budget: stays pending and is pushed into the future.
	if _, err := s.EnqueueClicks(ctx, 5, []int{1}, 0); err != nil {
		t.Fatal(err)
	}
	a.reschedule(ctx, store.ClickJob{ChatID: 5, MessageID: 1, Attempts: 0}, "boom")
	due, err := s.DueClicks(ctx, time.Now().Unix(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 0 {
		t.Fatalf("a rescheduled job must not be due immediately, got %+v", due)
	}

	// At the budget: terminal.
	if _, err := s.EnqueueClicks(ctx, 5, []int{2}, 0); err != nil {
		t.Fatal(err)
	}
	a.reschedule(ctx, store.ClickJob{ChatID: 5, MessageID: 2, Attempts: clickQueueMaxAttempt - 1}, "boom")
	stats, err := s.ClickQueueStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats[store.ClickFailed] != 1 {
		t.Fatalf("stats = %+v, want one failed row", stats)
	}
}

// Scan backfills from the archive: only messages whose keyboard carried a
// callback button are queued by default, so a wide history scan does not spend
// an API call per message that has nothing to click.
func TestScanChatQueuesOnlyButtonedMessages(t *testing.T) {
	a, s := newQueueTestClicker(t)
	ctx := context.Background()
	now := time.Now()

	plain := store.Message{ChatID: 42, MessageID: 1, Date: now, Text: "hello"}
	if err := s.UpsertMessage(ctx, plain); err != nil {
		t.Fatal(err)
	}
	buttoned := store.Message{ChatID: 42, MessageID: 2, Date: now, Text: "query",
		Buttons: []store.InlineButton{{Text: "点击查询", Kind: "callback"}}}
	if err := s.UpsertMessage(ctx, buttoned); err != nil {
		t.Fatal(err)
	}

	n, err := a.ScanChat(ctx, 42, 100, false)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("scan queued %d, want 1 (only the buttoned message)", n)
	}
	stats, _ := s.ClickQueueStats(ctx)
	if stats[store.ClickPending] != 1 {
		t.Fatalf("stats = %+v, want one pending", stats)
	}

	// Re-scanning must not duplicate.
	if n, err := a.ScanChat(ctx, 42, 100, false); err != nil || n != 0 {
		t.Fatalf("second scan = %d, %v; want 0", n, err)
	}
}
