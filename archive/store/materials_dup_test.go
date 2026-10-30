package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// materials.go had no test file at all, which is how a status CASE that said
// "keep the duplicate flag" kept clearing it instead. These tests pin the three
// behaviours the dedupe pass depends on: flagging, surviving a re-materialize,
// and recovering rows left half-written by the broken build.

func newMaterialStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func materialAt(chatID int64, msgID int, hash string, date int64) Material {
	return Material{
		ChatID:          chatID,
		AnchorRowID:     int64(msgID),
		AnchorMessageID: msgID,
		Kind:            "photo",
		ContentHash:     hash,
		MediaCount:      1,
		MediaKind:       "photo",
		Date:            date,
		Status:          "active",
	}
}

func TestMarkDuplicateMaterialsFlagsLaterCopies(t *testing.T) {
	s := newMaterialStore(t)
	ctx := context.Background()

	// Same picture posted three times: earliest stays active, the rest point at it.
	for i, msgID := range []int{100, 200, 300} {
		if _, err := s.ReplaceMaterial(ctx, materialAt(10, msgID, "hash-same", int64(1700000000+i)), nil); err != nil {
			t.Fatal(err)
		}
	}

	n, err := s.MarkDuplicateMaterials(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("marked = %d, want 2 duplicates", n)
	}

	rows, err := s.DB().QueryContext(ctx, "SELECT anchor_message_id, status, coalesce(duplicate_of,0) FROM materials ORDER BY anchor_message_id")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	type row struct {
		msgID   int
		status  string
		dupOfID int64
	}
	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.msgID, &r.status, &r.dupOfID); err != nil {
			t.Fatal(err)
		}
		got = append(got, r)
	}
	if len(got) != 3 {
		t.Fatalf("rows = %d, want 3", len(got))
	}
	if got[0].status != "active" {
		t.Fatalf("earliest copy status = %q, want active", got[0].status)
	}
	for _, r := range got[1:] {
		if r.status != "duplicate" {
			t.Fatalf("msg %d status = %q, want duplicate", r.msgID, r.status)
		}
		if r.dupOfID == 0 {
			t.Fatalf("msg %d duplicate_of is empty", r.msgID)
		}
	}
}

// The regression that mattered: the collector re-materializes a message on every
// edit, download completion and Touch pass. The old CASE wrote excluded.status
// (always "active") back over a duplicate flag, so a repeated picture silently
// lost its flag minutes after being marked.
func TestReplaceMaterialKeepsDuplicateFlag(t *testing.T) {
	s := newMaterialStore(t)
	ctx := context.Background()

	if _, err := s.ReplaceMaterial(ctx, materialAt(10, 100, "hash-keep", 1700000000), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReplaceMaterial(ctx, materialAt(10, 200, "hash-keep", 1700000001), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkDuplicateMaterials(ctx); err != nil {
		t.Fatal(err)
	}

	// Re-materialize the flagged copy, exactly like Touch does.
	if _, err := s.ReplaceMaterial(ctx, materialAt(10, 200, "hash-keep", 1700000001), nil); err != nil {
		t.Fatal(err)
	}

	var status string
	var dupOf int64
	if err := s.DB().QueryRowContext(ctx,
		"SELECT status, coalesce(duplicate_of,0) FROM materials WHERE anchor_message_id=200").Scan(&status, &dupOf); err != nil {
		t.Fatal(err)
	}
	if status != "duplicate" || dupOf == 0 {
		t.Fatalf("after re-materialize status=%q duplicate_of=%d, want duplicate with a pointer", status, dupOf)
	}
}

// A content edit makes the old duplicate verdict wrong: the post is no longer the
// same picture, so the flag has to drop instead of being preserved.
func TestReplaceMaterialDropsFlagWhenContentChanges(t *testing.T) {
	s := newMaterialStore(t)
	ctx := context.Background()

	if _, err := s.ReplaceMaterial(ctx, materialAt(10, 100, "hash-a", 1700000000), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReplaceMaterial(ctx, materialAt(10, 200, "hash-a", 1700000001), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkDuplicateMaterials(ctx); err != nil {
		t.Fatal(err)
	}

	changed := materialAt(10, 200, "hash-edited", 1700000001)
	if _, err := s.ReplaceMaterial(ctx, changed, nil); err != nil {
		t.Fatal(err)
	}

	var status string
	var dupOf int64
	if err := s.DB().QueryRowContext(ctx,
		"SELECT status, coalesce(duplicate_of,0) FROM materials WHERE anchor_message_id=200").Scan(&status, &dupOf); err != nil {
		t.Fatal(err)
	}
	if status != "active" || dupOf != 0 {
		t.Fatalf("edited material status=%q duplicate_of=%d, want active with no pointer", status, dupOf)
	}
}

// Rows written by the broken build carry status='active' together with a
// non-empty duplicate_of. The old reset only looked at status='duplicate', and
// the old mark guarded on "duplicate_of IS NULL", so those rows could never be
// repaired: every later pass skipped them and --stats reported 0 duplicates
// while 93 content hashes appeared more than once.
func TestMarkDuplicateRecoversHalfWrittenRow(t *testing.T) {
	s := newMaterialStore(t)
	ctx := context.Background()

	if _, err := s.ReplaceMaterial(ctx, materialAt(10, 100, "hash-half", 1700000000), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReplaceMaterial(ctx, materialAt(10, 200, "hash-half", 1700000001), nil); err != nil {
		t.Fatal(err)
	}
	// Recreate the exact damaged shape.
	if _, err := s.DB().ExecContext(ctx,
		"UPDATE materials SET status='active', duplicate_of=1 WHERE anchor_message_id=200"); err != nil {
		t.Fatal(err)
	}

	n, err := s.MarkDuplicateMaterials(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("marked = %d, want 1", n)
	}
	var status string
	var dupOf int64
	if err := s.DB().QueryRowContext(ctx,
		"SELECT status, coalesce(duplicate_of,0) FROM materials WHERE anchor_message_id=200").Scan(&status, &dupOf); err != nil {
		t.Fatal(err)
	}
	if status != "duplicate" || dupOf == 0 {
		t.Fatalf("half-written row status=%q duplicate_of=%d, still not repaired", status, dupOf)
	}
}

// A dedupe pass must be repeatable: running it twice cannot invent new marks or
// chain a duplicate onto another duplicate.
func TestMarkDuplicateMaterialsIsStable(t *testing.T) {
	s := newMaterialStore(t)
	ctx := context.Background()

	for i, msgID := range []int{100, 200, 300, 400} {
		if _, err := s.ReplaceMaterial(ctx, materialAt(10, msgID, "hash-stable", int64(1700000000+i)), nil); err != nil {
			t.Fatal(err)
		}
	}
	first, err := s.MarkDuplicateMaterials(ctx)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.MarkDuplicateMaterials(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if first != 3 || second != 3 {
		t.Fatalf("marked first=%d second=%d, want 3 and 3", first, second)
	}

	// Every duplicate must point at the same active owner, never at another duplicate.
	var chained int
	if err := s.DB().QueryRowContext(ctx, `
SELECT count(*) FROM materials d
JOIN materials o ON o.id=d.duplicate_of
WHERE d.status='duplicate' AND o.status<>'active'`).Scan(&chained); err != nil {
		t.Fatal(err)
	}
	if chained != 0 {
		t.Fatalf("%d duplicates point at a non-active owner", chained)
	}
}

var _ = time.Now
