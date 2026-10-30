package archive

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iyear/tdl/archive/store"
)

// tmeJobStore builds a store with a target and one pending t.me job, which is
// the minimum processTMe needs to reach its own branches without a live client.
func tmeJobStore(t *testing.T, url string) (*store.Store, store.MediaJob) {
	t.Helper()
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	if err := s.AddTarget(ctx, store.Target{ChatID: 42, Username: "chan", Enabled: true, ArchiveMedia: true, ArchiveMode: "local"}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertMessage(ctx, store.Message{ChatID: 42, MessageID: 7, Date: time.Unix(1, 0), Text: "quoted " + url}); err != nil {
		t.Fatal(err)
	}
	row, err := s.RowID(ctx, 42, 7)
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.DB().Exec("INSERT INTO media(message_rowid,type,status,source_url,created_at,updated_at) VALUES(?,'tme','pending',?,1,1)", row, url)
	if err != nil {
		t.Fatal(err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return s, store.MediaJob{ID: id, ChatID: 42, MessageID: 7, Type: store.KindTMe, URL: url}
}

// A job whose URL never parsed as a t.me message link must park with a reason
// an operator can act on, not retry forever against an unresolvable target.
func TestProcessTMeParksMalformedURL(t *testing.T) {
	s, j := tmeJobStore(t, "https://www.youtube.com/watch?v=abc")
	w := &MediaWorker{Store: s, Root: t.TempDir()}
	if err := w.processTMe(context.Background(), j); err != nil {
		t.Fatalf("processTMe returned %v, want a parked job", err)
	}
	status, _ := mediaStatus(t, s, j.ID)
	if status != "skipped" {
		t.Fatalf("status = %q, want skipped", status)
	}
	var kind, detail string
	if err := s.DB().QueryRow("SELECT COALESCE(error_kind,''),COALESCE(error,'') FROM media WHERE id=?", j.ID).Scan(&kind, &detail); err != nil {
		t.Fatal(err)
	}
	if kind != "unsupported" {
		t.Errorf("error_kind = %q, want unsupported", kind)
	}
	if !strings.Contains(detail, "not a t.me message link") {
		t.Errorf("error = %q, want it to name the reason", detail)
	}
}

// The target gate still applies: a chat the archive no longer monitors has no
// business resolving third-party peers on its behalf.
func TestProcessTMeHonoursTargetRemoval(t *testing.T) {
	s, j := tmeJobStore(t, "https://t.me/other/5")
	ctx := context.Background()
	if _, err := s.DB().Exec("DELETE FROM monitor_targets WHERE chat_id=42"); err != nil {
		t.Fatal(err)
	}
	w := &MediaWorker{Store: s, Root: t.TempDir()}
	if err := w.processTMe(ctx, j); err != nil {
		t.Fatalf("processTMe returned %v, want a parked job", err)
	}
	status, _ := mediaStatus(t, s, j.ID)
	if status != "skipped" {
		t.Fatalf("status = %q, want skipped", status)
	}
}

// ingestObject is the shared tail of every download; a quoted video must land in
// the CAS exactly like an uploaded one — same hash, same object path, same
// completed status.
func TestIngestObjectStoresAndHashe(t *testing.T) {
	s, j := tmeJobStore(t, "https://t.me/other/5")
	root := t.TempDir()
	w := &MediaWorker{Store: s, Root: root}

	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for x := 0; x < 8; x++ {
		for y := 0; y < 8; y++ {
			img.Set(x, y, color.RGBA{R: uint8(x * 30), G: uint8(y * 30), B: 120, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(root, "staging", "job.tmp")
	if err := os.MkdirAll(filepath.Dir(tmp), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tmp, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := w.ingestObject(context.Background(), j, tmp, "image/png", 0, nil); err != nil {
		t.Fatalf("ingestObject: %v", err)
	}
	var sha, original, status string
	var size int64
	if err := s.DB().QueryRow("SELECT sha256,original_path,status,size FROM media WHERE id=?", j.ID).
		Scan(&sha, &original, &status, &size); err != nil {
		t.Fatal(err)
	}
	if status != "completed" {
		t.Fatalf("status = %q, want completed", status)
	}
	if size != int64(buf.Len()) {
		t.Errorf("size = %d, want %d", size, buf.Len())
	}
	want := filepath.Join(root, "objects", sha[:2], sha[2:4], sha)
	if original != want {
		t.Errorf("original_path = %q, want %q", original, want)
	}
	if _, err := os.Stat(original); err != nil {
		t.Errorf("object missing from CAS: %v", err)
	}
	// The staging copy must be gone: it was moved, not copied.
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Errorf("staging file still present after ingest")
	}
}

// The per-target size cap has to be enforced for a quoted post too, otherwise
// one linked 4GB file ignores the limit that protects an in-chat download.
func TestIngestObjectEnforcesMaxSize(t *testing.T) {
	s, j := tmeJobStore(t, "https://t.me/other/5")
	root := t.TempDir()
	w := &MediaWorker{Store: s, Root: root}
	tmp := filepath.Join(root, "staging", "big.tmp")
	if err := os.MkdirAll(filepath.Dir(tmp), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tmp, bytes.Repeat([]byte("x"), 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := w.ingestObject(context.Background(), j, tmp, "video/mp4", 1024, nil); err != nil {
		t.Fatalf("ingestObject: %v", err)
	}
	status, _ := mediaStatus(t, s, j.ID)
	if status != "skipped" {
		t.Fatalf("status = %q, want skipped for an oversized object", status)
	}
}
