package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestAutoClickOutletRoundtrip(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	o, err := s.AutoClickOutlet(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if o.BotToken != "" || o.BotChatID != "" {
		t.Fatalf("default outlet should be empty, got %+v", o)
	}

	if err := s.SaveAutoClickOutlet(ctx, AutoClickOutlet{BotToken: "12345:AA-secret", BotChatID: "-100200"}); err != nil {
		t.Fatal(err)
	}
	o, err = s.AutoClickOutlet(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if o.BotToken != "12345:AA-secret" || o.BotChatID != "-100200" {
		t.Fatalf("outlet roundtrip mismatch: %+v", o)
	}

	// clearing the chat id clears the whole outlet
	if err := s.SaveAutoClickOutlet(ctx, AutoClickOutlet{BotToken: "", BotChatID: ""}); err != nil {
		t.Fatal(err)
	}
	o, _ = s.AutoClickOutlet(ctx)
	if o.BotToken != "" || o.BotChatID != "" {
		t.Fatalf("outlet should be cleared, got %+v", o)
	}
}

func TestAutoClickLogAndPrune(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	for i := 0; i < 8; i++ {
		if err := s.AddAutoClickLog(ctx, AutoClickLogEntry{
			ChatID: 100, MessageID: i, ButtonText: "领取", Kind: "callback",
			Status: "clicked", Detail: "",
		}); err != nil {
			t.Fatal(err)
		}
	}
	log, err := s.ListAutoClickLog(ctx, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(log) != 5 {
		t.Fatalf("want 5 newest entries, got %d", len(log))
	}
	if log[0].MessageID != 7 || log[4].MessageID != 3 {
		t.Fatalf("log not ordered newest first: %v", log)
	}
	if log[0].ButtonText != "领取" || log[0].Status != "clicked" {
		t.Fatalf("entry fields lost: %+v", log[0])
	}

	if err := s.PruneAutoClickLog(ctx, 3); err != nil {
		t.Fatal(err)
	}
	log, _ = s.ListAutoClickLog(ctx, 50)
	if len(log) != 3 {
		t.Fatalf("prune should keep 3, got %d", len(log))
	}
}

func TestTargetAutoClickFields(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	if err := s.AddTarget(ctx, Target{
		ChatID: 555, Username: "durov", Enabled: true,
		AutoClick: true, AutoClickKeywords: []string{"领取", "福利"},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Target(ctx, 555)
	if err != nil {
		t.Fatal(err)
	}
	if !got.AutoClick {
		t.Fatal("AutoClick flag lost")
	}
	if len(got.AutoClickKeywords) != 2 || got.AutoClickKeywords[0] != "领取" {
		t.Fatalf("AutoClickKeywords lost: %v", got.AutoClickKeywords)
	}

	off := false
	if err := s.UpdateTargetRules(ctx, 555, TargetRules{AutoClick: &off, AutoClickKeywords: nil}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Target(ctx, 555)
	if got.AutoClick {
		t.Fatal("AutoClick should be disabled")
	}
	if len(got.AutoClickKeywords) != 2 {
		t.Fatalf("keywords should survive a partial update, got %v", got.AutoClickKeywords)
	}

	list, err := s.Targets(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("Targets: %v %v", list, err)
	}
	if list[0].AutoClickKeywords == nil {
		// rows written before the columns existed scan back as nil; that is fine
		t.Log("nil keywords on legacy row")
	}
}

func TestAutoClickTargetMemory(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	key := "bot:bianbaoyang1_bot|/start SW1561"
	seen, err := s.AutoClickTargetSeen(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if seen {
		t.Fatal("a destination that never fired must report unseen")
	}

	if err := s.RecordAutoClickTarget(ctx, key); err != nil {
		t.Fatal(err)
	}
	seen, err = s.AutoClickTargetSeen(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if !seen {
		t.Fatal("destination must be remembered after the first click")
	}

	// Repeating the same destination must not create a second row: the loop
	// guard keys on the destination, so a duplicate row would silently disable
	// it for one of the two spellings.
	if err := s.RecordAutoClickTarget(ctx, key); err != nil {
		t.Fatal(err)
	}
	distinct, hits, err := s.CountAutoClickTargets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if distinct != 1 || hits != 2 {
		t.Fatalf("distinct=%d hits=%d, want distinct=1 hits=2", distinct, hits)
	}

	// A different payload is a different destination: the next coupon code must
	// still be clickable.
	other := "bot:bianbaoyang1_bot|/start SW1562"
	seen, err = s.AutoClickTargetSeen(ctx, other)
	if err != nil {
		t.Fatal(err)
	}
	if seen {
		t.Fatal("a new payload must not be treated as already clicked")
	}
	// An empty key is never remembered, so a malformed button cannot poison a
	// later one.
	if err := s.RecordAutoClickTarget(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if seen, _ := s.AutoClickTargetSeen(ctx, ""); seen {
		t.Fatal("empty destination must stay unseen")
	}
}
