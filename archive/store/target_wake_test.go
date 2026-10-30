package store

import (
	"context"
	"testing"
	"time"
)

// parkMedia marks one media row the way the worker does when the chat has no
// monitor_targets row.
func parkMedia(t *testing.T, s *Store, id int64) {
	t.Helper()
	if err := s.MarkMediaTargetMissing(context.Background(), id, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("MarkMediaTargetMissing: %v", err)
	}
}

func mediaID(t *testing.T, s *Store, chatID int64, msgID int) int64 {
	t.Helper()
	var id int64
	err := s.DB().QueryRow(
		`SELECT media.id FROM media JOIN messages m ON m.id=media.message_rowid
		 WHERE m.chat_id=? AND m.message_id=?`, chatID, msgID).Scan(&id)
	if err != nil {
		t.Fatalf("media row for %d/%d: %v", chatID, msgID, err)
	}
	return id
}

func mediaStatus(t *testing.T, s *Store, id int64) (status string, kind string, nextRetry int64) {
	t.Helper()
	var k *string
	var nr *int64
	err := s.DB().QueryRow(
		`SELECT status, error_kind, next_retry_at FROM media WHERE id=?`, id).Scan(&status, &k, &nr)
	if err != nil {
		t.Fatalf("media status %d: %v", id, err)
	}
	if k != nil {
		kind = *k
	}
	if nr != nil {
		nextRetry = *nr
	}
	return status, kind, nextRetry
}

// A chat is regularly archived before anyone adds it as a target: a rescan, a
// sync or an autoclick reply all ingest messages for chats that are not
// monitored. The download must wait for the target instead of being written off.
//
// Production lost 1153 rows this way — the old code called a missing target a
// permanent failure, and 900 of them belonged to a chat that was added as a
// target 13 hours later.
func TestParkedMediaWaitsForItsTarget(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	saveMsg(t, s, 100, 1)
	m := Message{ChatID: 100, MessageID: 1}
	if err := s.EnsureMedia(ctx, m, MediaMeta{Kind: "photo", FileUniqueID: "u1", Size: 10}); err != nil {
		t.Fatalf("EnsureMedia: %v", err)
	}
	id := mediaID(t, s, 100, 1)

	parkMedia(t, s, id)
	status, kind, next := mediaStatus(t, s, id)
	if status != "pending" {
		t.Fatalf("status=%q, want pending (a missing target is a wait, not a verdict)", status)
	}
	if kind != "target_missing" {
		t.Fatalf("error_kind=%q, want target_missing", kind)
	}
	if next <= time.Now().Unix() {
		t.Fatalf("next_retry_at=%d, want a future backoff", next)
	}
	// The whole point of the long backoff: a parked row must not be picked up on
	// every pass while the chat stays unmonitored.
	jobs, err := s.PendingMedia(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 0 {
		t.Fatalf("PendingMedia returned %d parked job(s), want 0", len(jobs))
	}

	// Adding the target is the event that un-parks it.
	n, err := s.WakeTargetMedia(ctx, 100)
	if err != nil {
		t.Fatalf("WakeTargetMedia: %v", err)
	}
	if n != 1 {
		t.Fatalf("woke %d row(s), want 1", n)
	}
	status, kind, next = mediaStatus(t, s, id)
	if status != "pending" || kind != "" || next != 0 {
		t.Fatalf("after wake: status=%q kind=%q next_retry_at=%d, want pending/empty/0", status, kind, next)
	}
	jobs, err = s.PendingMedia(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].ID != id {
		t.Fatalf("PendingMedia after wake=%+v, want the woken row", jobs)
	}
}

// The archive already contains rows written by the old behaviour, which stored
// them as skipped with the sql.ErrNoRows text. A one-shot repair has to find
// those too, otherwise every existing archive stays short by however many it
// lost.
func TestWakeRepairsRowsSkippedByTheOldBehaviour(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	saveMsg(t, s, 200, 1)
	m := Message{ChatID: 200, MessageID: 1}
	if err := s.EnsureMedia(ctx, m, MediaMeta{Kind: "photo", FileUniqueID: "u2", Size: 10}); err != nil {
		t.Fatalf("EnsureMedia: %v", err)
	}
	id := mediaID(t, s, 200, 1)
	if err := s.AddTarget(ctx, Target{ChatID: 200, Username: "chan", Enabled: true, DownloadMedia: true}); err != nil {
		t.Fatalf("AddTarget: %v", err)
	}

	// Exactly what the old worker wrote: permanent skip, kind unknown.
	if _, err := s.DB().Exec(
		`UPDATE media SET status='skipped', error='giving up after 1 attempt(s): sql: no rows in result set',
		 error_kind='unknown', attempts=1 WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}

	n, err := s.WakeTargetMedia(ctx, 200)
	if err != nil {
		t.Fatalf("WakeTargetMedia: %v", err)
	}
	if n != 1 {
		t.Fatalf("woke %d legacy row(s), want 1", n)
	}
	if status, _, _ := mediaStatus(t, s, id); status != "pending" {
		t.Fatalf("status=%q after repair, want pending", status)
	}
}

// Both entry points must be scoped to one chat: waking every parked row because
// one target was added would retry chats that are still unmonitored.
func TestWakeIsScopedToItsChat(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	for _, chat := range []int64{300, 400} {
		saveMsg(t, s, chat, 1)
		if err := s.EnsureMedia(ctx, Message{ChatID: chat, MessageID: 1},
			MediaMeta{Kind: "photo", FileUniqueID: "u", Size: 10}); err != nil {
			t.Fatalf("EnsureMedia(%d): %v", chat, err)
		}
		parkMedia(t, s, mediaID(t, s, chat, 1))
	}

	if n, err := s.WakeTargetMedia(ctx, 300); err != nil || n != 1 {
		t.Fatalf("WakeTargetMedia(300) = %d, %v; want 1, nil", n, err)
	}
	if status, _, _ := mediaStatus(t, s, mediaID(t, s, 300, 1)); status != "pending" {
		t.Fatalf("chat 300 status=%q, want pending", status)
	}
	if status, kind, _ := mediaStatus(t, s, mediaID(t, s, 400, 1)); status != "pending" || kind != "target_missing" {
		t.Fatalf("chat 400 status=%q kind=%q, want parked and untouched", status, kind)
	}
}

// Adding a target must heal the archive on its own. Requiring a manual retry
// per row is how the 1153 rows stayed lost.
func TestAddTargetWakesParkedMedia(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	saveMsg(t, s, 500, 1)
	if err := s.EnsureMedia(ctx, Message{ChatID: 500, MessageID: 1},
		MediaMeta{Kind: "photo", FileUniqueID: "u3", Size: 10}); err != nil {
		t.Fatalf("EnsureMedia: %v", err)
	}
	id := mediaID(t, s, 500, 1)
	parkMedia(t, s, id)

	if err := s.AddTarget(ctx, Target{ChatID: 500, Username: "chan", Enabled: true, DownloadMedia: true}); err != nil {
		t.Fatalf("AddTarget: %v", err)
	}
	status, kind, next := mediaStatus(t, s, id)
	if status != "pending" || kind != "" || next != 0 {
		t.Fatalf("after AddTarget: status=%q kind=%q next=%d, want woken", status, kind, next)
	}
}

// The bulk repair touches monitored chats only. A chat that was never added as
// a target has nothing to retry against, so its rows stay parked at the long
// backoff rather than burning a pass every tick.
func TestWakeAllSkipsUnmonitoredChats(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	for _, chat := range []int64{600, 700} {
		saveMsg(t, s, chat, 1)
		if err := s.EnsureMedia(ctx, Message{ChatID: chat, MessageID: 1},
			MediaMeta{Kind: "photo", FileUniqueID: "u", Size: 10}); err != nil {
			t.Fatalf("EnsureMedia(%d): %v", chat, err)
		}
		parkMedia(t, s, mediaID(t, s, chat, 1))
	}
	if err := s.AddTarget(ctx, Target{ChatID: 600, Username: "chan", Enabled: true, DownloadMedia: true}); err != nil {
		t.Fatalf("AddTarget: %v", err)
	}
	// AddTarget already woke 600; park it again to prove the bulk path does the
	// same work rather than piggybacking on the insert.
	parkMedia(t, s, mediaID(t, s, 600, 1))

	n, err := s.WakeAllMonitoredMedia(ctx)
	if err != nil {
		t.Fatalf("WakeAllMonitoredMedia: %v", err)
	}
	if n != 1 {
		t.Fatalf("woke %d row(s), want 1 (only the monitored chat)", n)
	}
	if status, _, _ := mediaStatus(t, s, mediaID(t, s, 600, 1)); status != "pending" {
		t.Fatalf("monitored chat status=%q, want pending", status)
	}
	if _, kind, _ := mediaStatus(t, s, mediaID(t, s, 700, 1)); kind != "target_missing" {
		t.Fatalf("unmonitored chat kind=%q, want target_missing", kind)
	}
}
