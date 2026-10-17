package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func openCursorStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestSyncCursorRoundTrip(t *testing.T) {
	s := openCursorStore(t)
	ctx := context.Background()

	// A never-synced chat reports a pending backlog and no watermark.
	got, err := s.SyncCursor(ctx, 0, 42)
	if err != nil {
		t.Fatal(err)
	}
	if got.BacklogState != BacklogPending || got.LastMsgID != 0 || got.BacklogOldestMsgID != 0 {
		t.Fatalf("fresh cursor = %+v", got)
	}

	if err := s.SaveSyncCursor(ctx, SyncCursor{
		ChatID: 42, BacklogState: BacklogRunning, BacklogOldestMsgID: 10, LastMsgID: 900, LastSeenAt: 1234,
	}); err != nil {
		t.Fatal(err)
	}
	got, err = s.SyncCursor(ctx, 0, 42)
	if err != nil {
		t.Fatal(err)
	}
	if got.BacklogState != BacklogRunning || got.BacklogOldestMsgID != 10 || got.LastMsgID != 900 || got.LastSeenAt != 1234 {
		t.Fatalf("cursor = %+v", got)
	}

	// last_msg_id only ever moves forward, so a stale writer cannot rewind it.
	if err := s.SaveSyncCursor(ctx, SyncCursor{ChatID: 42, BacklogState: BacklogDone, LastMsgID: 5}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.SyncCursor(ctx, 0, 42)
	if got.LastMsgID != 900 {
		t.Fatalf("last_msg_id regressed to %d", got.LastMsgID)
	}
	if got.BacklogState != BacklogDone {
		t.Fatalf("state = %q", got.BacklogState)
	}
}

func TestMessageIDRangeAndChatWatermark(t *testing.T) {
	s := openCursorStore(t)
	ctx := context.Background()
	for _, id := range []int{5, 9, 7} {
		if err := s.UpsertMessage(ctx, Message{ChatID: 1, MessageID: id, Date: time.Unix(int64(id), 0), Text: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	oldest, newest, err := s.MessageIDRange(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if oldest != 5 || newest != 9 {
		t.Fatalf("range = %d..%d", oldest, newest)
	}

	// An unknown chat has no watermark and no error.
	if got, err := s.ChatLastMsgID(ctx, 999); err != nil || got != 0 {
		t.Fatalf("unknown chat watermark = %d, %v", got, err)
	}
	if err := s.SetChatLastMsgID(ctx, 1, 9); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.ChatLastMsgID(ctx, 1); got != 9 {
		t.Fatalf("watermark = %d", got)
	}
}

func TestUpdateTargetRulesPartialUpdate(t *testing.T) {
	s := openCursorStore(t)
	ctx := context.Background()
	if err := s.AddTarget(ctx, Target{ChatID: 7, Username: "chan", Enabled: true, ArchiveText: true, ArchiveMedia: true, DownloadMedia: true}); err != nil {
		t.Fatal(err)
	}

	on := false
	max := int64(1024)
	if err := s.UpdateTargetRules(ctx, 7, TargetRules{
		Enabled:         &on,
		MediaMaxSize:    &max,
		Keywords:        []string{"go", "sqlite"},
		ExcludeKeywords: []string{"ads"},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Target(ctx, 7)
	if err != nil {
		t.Fatal(err)
	}
	if got.Enabled || got.MediaMaxSize != 1024 || !got.DownloadMedia {
		t.Fatalf("target = %+v", got)
	}
	if len(got.Keywords) != 2 || got.Keywords[0] != "go" || len(got.ExcludeKeywords) != 1 {
		t.Fatalf("rules = %+v / %+v", got.Keywords, got.ExcludeKeywords)
	}

	// A nil field is left untouched: download_media must survive this call.
	if err := s.UpdateTargetRules(ctx, 7, TargetRules{Keywords: []string{}}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Target(ctx, 7)
	if !got.DownloadMedia || len(got.Keywords) != 0 {
		t.Fatalf("after keyword clear: %+v", got)
	}

	if err := s.UpdateTargetRules(ctx, 12345, TargetRules{Keywords: []string{"x"}}); err == nil {
		t.Fatal("expected an error for an unmonitored chat")
	}
}

func TestMediaPauseGate(t *testing.T) {
	s := openCursorStore(t)
	ctx := context.Background()

	if until := s.MediaPausedUntil(ctx); !until.IsZero() {
		t.Fatalf("a fresh archive is not paused, got %s", until)
	}
	want := time.Now().Add(10 * time.Minute).Truncate(time.Second)
	if err := s.SetMediaPausedUntil(ctx, want); err != nil {
		t.Fatal(err)
	}
	if got := s.MediaPausedUntil(ctx); !got.Equal(want) {
		t.Fatalf("paused until %s, want %s", got, want)
	}
}
