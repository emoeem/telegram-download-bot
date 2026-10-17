package archive

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iyear/tdl/archive/store"
)

func gcTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// seedObject writes a fake CAS object and its files row.
func seedObject(t *testing.T, s *store.Store, root, sha string, size int64) string {
	t.Helper()
	p := ObjectPath(root, sha)
	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(strings.Repeat("x", int(size))), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec("INSERT INTO files(sha256,size,mime_type,state,ref_count,created_at) VALUES(?,?,'image/png','ready',1,1)", sha, size); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestGarbageCollectKeepsReferencedObjects(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	s := gcTestStore(t)

	liveSHA := strings.Repeat("ab", 32)
	orphanSHA := strings.Repeat("cd", 32)
	live := seedObject(t, s, root, liveSHA, 10)
	orphan := seedObject(t, s, root, orphanSHA, 20)

	if err := s.UpsertMessage(ctx, store.Message{ChatID: 1, MessageID: 1, Date: time.Unix(1, 0), Text: "img", HasMedia: true}); err != nil {
		t.Fatal(err)
	}
	row, err := s.RowID(ctx, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec("INSERT INTO media(message_rowid,type,status,sha256,created_at,updated_at) VALUES(?,'photo','completed',?,1,1)", row, liveSHA); err != nil {
		t.Fatal(err)
	}

	// Dry run: reports the orphan, deletes nothing.
	rep, err := GarbageCollect(ctx, s, root, GCOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Applied || len(rep.OrphanObjects) != 1 || rep.OrphanObjects[0] != orphanSHA {
		t.Fatalf("dry run report = %+v", rep)
	}
	if rep.OrphanBytes != 20 {
		t.Fatalf("orphan bytes = %d", rep.OrphanBytes)
	}
	if _, err := os.Stat(orphan); err != nil {
		t.Fatalf("dry run removed the orphan: %v", err)
	}

	// Apply: the orphan goes, the referenced object stays.
	rep, err = GarbageCollect(ctx, s, root, GCOptions{Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Applied || rep.FilesRowsPruned != 1 {
		t.Fatalf("apply report = %+v", rep)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatal("orphan object survived --apply")
	}
	if _, err := os.Stat(live); err != nil {
		t.Fatalf("referenced object was removed: %v", err)
	}
	orphans, err := s.OrphanFiles(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(orphans) != 0 {
		t.Fatalf("files rows left = %+v", orphans)
	}
}

func TestGarbageCollectRequeuesMissingObjects(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	s := gcTestStore(t)

	sha := strings.Repeat("ef", 32)
	if err := s.UpsertMessage(ctx, store.Message{ChatID: 1, MessageID: 1, Date: time.Unix(1, 0), Text: "img", HasMedia: true}); err != nil {
		t.Fatal(err)
	}
	row, err := s.RowID(ctx, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	// completed media whose object file does not exist on disk
	if _, err := s.DB().Exec("INSERT INTO media(message_rowid,type,status,sha256,created_at,updated_at) VALUES(?,'photo','completed',?,1,1)", row, sha); err != nil {
		t.Fatal(err)
	}

	rep, err := GarbageCollect(ctx, s, root, GCOptions{Apply: true, Repair: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.RequeuedMedia) != 1 || rep.RequeuedMedia[0] != sha {
		t.Fatalf("repair report = %+v", rep)
	}
	var status string
	if err := s.DB().QueryRow("SELECT status FROM media WHERE message_rowid=?", row).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "pending" {
		t.Fatalf("status after repair = %q", status)
	}
}

func TestCleanStagingOnlyRemovesOldPartials(t *testing.T) {
	root := t.TempDir()
	staging := filepath.Join(root, "staging")
	if err := os.MkdirAll(staging, 0755); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(staging, "old.tmp")
	fresh := filepath.Join(staging, "fresh.tmp")
	for _, p := range []string{old, fresh} {
		if err := os.WriteFile(p, []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}

	n, err := CleanStaging(root, DefaultStagingMaxAge)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("removed = %d", n)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("stale partial survived")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("in-flight partial was removed: %v", err)
	}
}
