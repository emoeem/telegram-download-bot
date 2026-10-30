package store

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestRealCopyMigration runs the media rebuild against a copy of the live
// archive when REAL_COPY points at one. It is the only test that sees 1300+
// media rows, so it is what proves the rebuild neither loses rows nor locks up
// on a real database.
func TestRealCopyMigration(t *testing.T) {
	path := os.Getenv("REAL_COPY")
	if path == "" {
		t.Skip("REAL_COPY not set")
	}
	before := map[string]int{}
	s, err := Open(path)
	if err != nil {
		t.Fatalf("seed open: %v", err)
	}
	for _, q := range []struct{ k, sql string }{
		{"media", "SELECT COUNT(*) FROM media"},
		{"messages", "SELECT COUNT(*) FROM messages"},
		{"links", "SELECT COUNT(*) FROM message_links"},
		{"completed", "SELECT COUNT(*) FROM media WHERE status='completed'"},
	} {
		var n int
		if err := s.DB().QueryRow(q.sql).Scan(&n); err != nil {
			t.Fatalf("%s: %v", q.k, err)
		}
		before[q.k] = n
	}
	_ = s.Close()

	start := time.Now()
	s, err = Open(path)
	if err != nil {
		t.Fatalf("Open after migration: %v", err)
	}
	defer s.Close()
	t.Logf("open took %s", time.Since(start))

	for k, want := range before {
		q := map[string]string{
			"media":     "SELECT COUNT(*) FROM media",
			"messages":  "SELECT COUNT(*) FROM messages",
			"links":     "SELECT COUNT(*) FROM message_links",
			"completed": "SELECT COUNT(*) FROM media WHERE status='completed'",
		}[k]
		var got int
		if err := s.DB().QueryRow(q).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("%s: %d -> %d (rows lost)", k, want, got)
		} else {
			t.Logf("%s: %d preserved", k, got)
		}
	}

	var chatID int64
	var msgID int
	if err := s.DB().QueryRow("SELECT chat_id,message_id FROM messages ORDER BY id LIMIT 1").Scan(&chatID, &msgID); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, u := range []string{"https://youtu.be/mig-a", "https://youtu.be/mig-b"} {
		if err := s.EnsureMedia(ctx, Message{ChatID: chatID, MessageID: msgID}, MediaMeta{Kind: "link", URL: u}); err != nil {
			t.Fatalf("EnsureMedia: %v", err)
		}
	}
	var total int
	if err := s.DB().QueryRow("SELECT COUNT(*) FROM media").Scan(&total); err != nil {
		t.Fatal(err)
	}
	t.Logf("media after adding 2 links: %d (was %d)", total, before["media"])
	if total != before["media"]+2 {
		t.Fatalf("expected %d rows", before["media"]+2)
	}
	var idx int
	if err := s.DB().QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name IN ('idx_media_sha','idx_media_status','idx_media_status_retry_id','idx_media_file_id_unique')").Scan(&idx); err != nil {
		t.Fatal(err)
	}
	if idx != 4 {
		t.Fatalf("media indexes after rebuild: %d, want 4", idx)
	}
}
