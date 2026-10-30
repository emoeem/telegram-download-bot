package archive

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/iyear/tdl/archive/store"
)

func newSerialArchive(t *testing.T) (*store.Store, string) {
	t.Helper()
	root := t.TempDir()
	s, err := store.Open(filepath.Join(t.TempDir(), "archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, root
}

// seedCompleted archives one message whose media is already on disk, and returns
// the media id, serial and object path.
func seedCompleted(t *testing.T, s *store.Store, root string, msgID int, kind, mime string, payload []byte) (int64, int64, string) {
	t.Helper()
	ctx := context.Background()
	m := store.Message{ChatID: -1001, MessageID: msgID, Date: time.Unix(1700000000+int64(msgID), 0), HasMedia: true, Caption: "hello"}
	if err := s.UpsertMessage(ctx, m); err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureMedia(ctx, m, store.MediaMeta{Kind: kind, Name: kind + ".dat", MIME: mime, Size: int64(len(payload))}); err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := s.DB().QueryRow(`SELECT media.id FROM media JOIN messages ON messages.id=media.message_rowid
 WHERE messages.chat_id=? AND messages.message_id=?`, m.ChatID, msgID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	serial, err := s.MediaSerial(ctx, id)
	if err != nil || serial == 0 {
		t.Fatalf("serial=%d err=%v", serial, err)
	}
	obj := filepath.Join(root, "objects", "aa", "bb", fmt.Sprintf("%d-obj", msgID))
	if err := os.MkdirAll(filepath.Dir(obj), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(obj, payload, 0644); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkMediaCompleted(ctx, id, "sha"+fmt.Sprint(msgID), int64(len(payload)), mime, obj, "", ""); err != nil {
		t.Fatal(err)
	}
	return id, serial, obj
}

func TestSerialViewPathIsBucketedAndNumbered(t *testing.T) {
	root := "/archive"
	cases := []struct {
		serial int64
		kind   string
		mime   string
		want   string
	}{
		{1, "photo", "image/jpeg", "/archive/objects/by-serial/000/000001-photo.jpg"},
		{999, "video", "video/mp4", "/archive/objects/by-serial/000/000999-video.mp4"},
		{1000, "document", "application/pdf", "/archive/objects/by-serial/001/001000-document.pdf"},
		{1337, "photo", "image/webp", "/archive/objects/by-serial/001/001337-photo.webp"},
		{0, "photo", "image/jpeg", ""},
	}
	for _, c := range cases {
		if got := SerialViewPath(root, c.serial, c.kind, c.mime); got != c.want {
			t.Fatalf("serial %d: got %q want %q", c.serial, got, c.want)
		}
	}
}

func TestLinkSerialViewUsesHardlink(t *testing.T) {
	_, root := newSerialArchive(t)
	obj := filepath.Join(root, "objects", "aa", "bb", "obj")
	if err := os.MkdirAll(filepath.Dir(obj), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(obj, []byte("payload"), 0644); err != nil {
		t.Fatal(err)
	}
	view, err := LinkSerialView(root, 42, "photo", "image/jpeg", obj)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(root, "objects", "by-serial", "000", "000042-photo.jpg"); view != want {
		t.Fatalf("view=%q want %q", view, want)
	}
	src, _ := os.Stat(obj)
	dst, err := os.Stat(view)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(src, dst) {
		t.Fatal("view is not a hard link to the object")
	}
	// Idempotent.
	if _, err := LinkSerialView(root, 42, "photo", "image/jpeg", obj); err != nil {
		t.Fatal(err)
	}
}

func TestRelinkAllRebuildsViewForCompletedMedia(t *testing.T) {
	s, root := newSerialArchive(t)
	for i := 1; i <= 3; i++ {
		seedCompleted(t, s, root, i, "photo", "image/jpeg", []byte("payload"))
	}
	// Simulate an archive that predates the view.
	if err := os.RemoveAll(filepath.Join(root, "objects", "by-serial")); err != nil {
		t.Fatal(err)
	}
	examined, created, err := RelinkAll(context.Background(), s, root, 2)
	if err != nil {
		t.Fatal(err)
	}
	if examined != 3 || created != 3 {
		t.Fatalf("examined=%d created=%d", examined, created)
	}
	for i := 1; i <= 3; i++ {
		view := SerialViewPath(root, int64(i), "photo", "image/jpeg")
		if _, err := os.Stat(view); err != nil {
			t.Fatalf("serial %d view missing: %v", i, err)
		}
	}
	// Second pass examines the same rows but creates nothing.
	examined, created, err = RelinkAll(context.Background(), s, root, 10)
	if err != nil {
		t.Fatal(err)
	}
	if examined != 3 || created != 0 {
		t.Fatalf("second pass examined=%d created=%d", examined, created)
	}
}

func TestApplyPurgeRemovesObjectAndView(t *testing.T) {
	s, root := newSerialArchive(t)
	id, serial, obj := seedCompleted(t, s, root, 1, "photo", "image/jpeg", []byte("payload"))
	view := SerialViewPath(root, serial, "photo", "image/jpeg")
	if _, err := LinkSerialView(root, serial, "photo", "image/jpeg", obj); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(view); err != nil {
		t.Fatalf("view not linked: %v", err)
	}
	rep, err := s.Purge(context.Background(), store.PurgeSelector{Serial: serial, Messages: true})
	if err != nil {
		t.Fatal(err)
	}
	removed, freed := ApplyPurge(root, rep)
	if removed != 1 || freed != int64(len("payload")) {
		t.Fatalf("removed=%d freed=%d", removed, freed)
	}
	if _, err := os.Stat(obj); !os.IsNotExist(err) {
		t.Fatalf("object still present: %v", err)
	}
	if _, err := os.Stat(view); !os.IsNotExist(err) {
		t.Fatalf("serial view still present: %v", err)
	}
	_ = id
}
