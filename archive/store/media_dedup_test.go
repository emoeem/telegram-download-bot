package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMediaDedupByFileIDUniqueSkipsDownload(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	if err := s.UpsertMessage(ctx, Message{ChatID: 1, MessageID: 1, Date: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertMessage(ctx, Message{ChatID: 1, MessageID: 2, Date: time.Now()}); err != nil {
		t.Fatal(err)
	}
	meta := MediaMeta{Kind: "document", FileUniqueID: "tg-document-42", MIME: "video/mp4", Size: 4}
	if err := s.EnsureMedia(ctx, Message{ChatID: 1, MessageID: 1}, meta); err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureMedia(ctx, Message{ChatID: 1, MessageID: 2}, meta); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	sha := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	obj := filepath.Join(root, "objects", sha[:2], sha[2:4], sha)
	if err := os.MkdirAll(filepath.Dir(obj), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(obj, []byte("data"), 0644); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	if _, err := s.DB().ExecContext(ctx, "UPDATE media SET status='completed',sha256=?,original_path=?,size=4,mime_type='video/mp4',downloaded_at=?,updated_at=? WHERE message_rowid=(SELECT id FROM messages WHERE chat_id=1 AND message_id=1)", sha, obj, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().ExecContext(ctx, "INSERT INTO files(sha256,size,mime_type,state,ref_count,created_at) VALUES(?,?,?,'ready',1,?)", sha, 4, "video/mp4", now); err != nil {
		t.Fatal(err)
	}
	r, ok, err := s.FindReusableMedia(ctx, meta.FileUniqueID)
	if err != nil || !ok {
		t.Fatalf("reuse ok=%v err=%v", ok, err)
	}
	if r.SHA256 != sha {
		t.Fatalf("sha=%s", r.SHA256)
	}
	if err := s.MarkMediaReused(ctx, 2, r); err != nil {
		t.Fatal(err)
	}
	var status string
	var gotSHA string
	s.DB().QueryRowContext(ctx, "SELECT status,sha256 FROM media WHERE message_rowid=(SELECT id FROM messages WHERE chat_id=1 AND message_id=2)").Scan(&status, &gotSHA)
	if status != "completed" || gotSHA != sha {
		t.Fatalf("status=%s sha=%s", status, gotSHA)
	}
}
func TestMediaDedupNotTriggeredWhenFileIDEmpty(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	r, ok, err := s.FindReusableMedia(context.Background(), "")
	if err != nil || ok || r.SHA256 != "" {
		t.Fatalf("r=%+v ok=%v err=%v", r, ok, err)
	}
}
func TestMediaDedupFallsBackWhenCASMissing(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	if err := s.UpsertMessage(ctx, Message{ChatID: 1, MessageID: 1, Date: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureMedia(ctx, Message{ChatID: 1, MessageID: 1}, MediaMeta{Kind: "document", FileUniqueID: "missing", MIME: "video/mp4"}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	_, err = s.DB().ExecContext(ctx, "UPDATE media SET status='completed',sha256=?,updated_at=? WHERE message_rowid=(SELECT id FROM messages WHERE chat_id=1 AND message_id=1)", "abcdef", now)
	if err != nil {
		t.Fatal(err)
	}
	_, ok, err := s.FindReusableMedia(ctx, "missing")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("expected no reusable media without files/CAS")
	}
}
