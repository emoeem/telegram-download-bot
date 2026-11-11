package store

import (
	"context"
	"testing"
	"time"
)

// A full wipe is the one operation that can destroy the archive, so the test
// pins both halves of the promise: content goes, configuration stays.
func TestPurgeEverythingClearsContentAndKeepsConfig(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.AddTarget(ctx, Target{ChatID: 100, Username: "chan"}); err != nil {
		t.Fatalf("AddTarget: %v", err)
	}
	if err := s.UpsertMessage(ctx, Message{ChatID: 100, MessageID: 1, Date: time.Unix(0, 0), Text: "hello"}); err != nil {
		t.Fatalf("UpsertMessage: %v", err)
	}

	dry, err := s.Purge(ctx, PurgeSelector{All: true, Messages: true, DryRun: true})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if !dry.Everything {
		t.Fatalf("dry run report = %+v, want Everything", dry)
	}
	if dry.Messages != 1 {
		t.Fatalf("dry run messages = %d, want 1", dry.Messages)
	}

	rep, err := s.Purge(ctx, PurgeSelector{All: true, Messages: true})
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if rep.Messages != 1 {
		t.Fatalf("messages removed = %d, want 1", rep.Messages)
	}

	var n int
	if err := s.DB().QueryRowContext(ctx, "SELECT count(*) FROM messages").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("messages left = %d, want 0", n)
	}
	targets, err := s.Targets(ctx)
	if err != nil {
		t.Fatalf("Targets: %v", err)
	}
	if len(targets) != 1 || targets[0].ChatID != 100 {
		t.Fatalf("targets = %+v, want the monitored source to survive the wipe", targets)
	}
}

// The empty-selector guard is the reason a wipe has to be asked for by name;
// it must keep rejecting a selector that only forgot its flags.
func TestPurgeRejectsEmptySelector(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Purge(context.Background(), PurgeSelector{}); err == nil {
		t.Fatal("empty selector should be rejected")
	}
}
