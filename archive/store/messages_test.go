package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func newMessageListTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	if err := s.SetChat(ctx, 123, "Source", "source", "channel"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().ExecContext(ctx, "INSERT INTO users(id,username,first_name,created_at,updated_at) VALUES(42,'alice','Alice',1,1)"); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestListMessagesBasic(t *testing.T) {
	s := newMessageListTestStore(t)
	ctx := context.Background()
	base := time.Unix(1700000000, 0)
	for i := 1; i <= 3; i++ {
		if err := s.UpsertMessage(ctx, Message{ChatID: 123, MessageID: i, SenderID: 42, Date: base.Add(time.Duration(i) * time.Minute), Text: "message"}); err != nil {
			t.Fatal(err)
		}
	}
	page, err := s.ListMessages(ctx, MessageFilter{Page: 1, PageSize: 50})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 3 || len(page.Items) != 3 {
		t.Fatalf("total=%d items=%d", page.Total, len(page.Items))
	}
	if page.Items[0].MessageID != 3 || page.Items[2].MessageID != 1 {
		t.Fatalf("unexpected order: %+v", page.Items)
	}
}

func TestListMessagesPagination(t *testing.T) {
	s := newMessageListTestStore(t)
	ctx := context.Background()
	base := time.Unix(1700000000, 0)
	for i := 1; i <= 120; i++ {
		if err := s.UpsertMessage(ctx, Message{ChatID: 123, MessageID: i, Date: base.Add(time.Duration(i) * time.Minute), Text: "message"}); err != nil {
			t.Fatal(err)
		}
	}
	for pageNo, want := range map[int]int{1: 50, 2: 50, 3: 20} {
		page, err := s.ListMessages(ctx, MessageFilter{Page: pageNo, PageSize: 50})
		if err != nil {
			t.Fatal(err)
		}
		if page.Total != 120 || len(page.Items) != want {
			t.Fatalf("page=%d total=%d items=%d want=%d", pageNo, page.Total, len(page.Items), want)
		}
	}
}

func TestListMessagesWithSearchSyntax(t *testing.T) {
	s := newMessageListTestStore(t)
	ctx := context.Background()
	base := time.Unix(1700000000, 0)
	if err := s.UpsertMessage(ctx, Message{ChatID: 123, MessageID: 1, SenderID: 42, Date: base, Text: "plain"}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertMessage(ctx, Message{ChatID: 123, MessageID: 2, SenderID: 42, Date: base.Add(24 * time.Hour), Text: "target"}); err != nil {
		t.Fatal(err)
	}
	rowID, err := s.RowID(ctx, 123, 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().ExecContext(ctx, "INSERT INTO media(message_rowid,type,mime_type,status,created_at,updated_at) VALUES(?,?,?,?,?,?)", rowID, "photo", "image/jpeg", "completed", base.Unix(), base.Unix()); err != nil {
		t.Fatal(err)
	}
	page, err := s.ListMessages(ctx, MessageFilter{
		Query:    "from:@alice has:image after:2023-11-14 before:2023-11-16",
		PageSize: 50,
	})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 1 || len(page.Items) != 1 || page.Items[0].MessageID != 2 {
		t.Fatalf("result=%+v total=%d", page.Items, page.Total)
	}
}

func TestGetMessageByRowID(t *testing.T) {
	s := newMessageListTestStore(t)
	ctx := context.Background()
	if err := s.UpsertMessage(ctx, Message{ChatID: 123, MessageID: 7, SenderID: 42, Date: time.Unix(1700000000, 0), Text: "focus target"}); err != nil {
		t.Fatal(err)
	}
	rowID, err := s.RowID(ctx, 123, 7)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.GetMessageByRowID(ctx, rowID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RowID != rowID || got.MessageID != 7 || got.Text != "focus target" {
		t.Fatalf("got=%+v", got)
	}
	if _, err := s.GetMessageByRowID(ctx, rowID+999); err == nil {
		t.Fatal("expected missing rowid error")
	}
}

// TestRecordEditSnapshotsPreviousContent guards the proposal §8 semantics: the
// edit timeline must archive the content that existed BEFORE the edit, not the
// new content (which the main row will hold anyway).
func TestRecordEditSnapshotsPreviousContent(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	orig := Message{ChatID: 7, MessageID: 1, Date: time.Unix(1000, 0), Text: "ORIGINAL"}
	if err = s.UpsertMessage(ctx, orig); err != nil {
		t.Fatal(err)
	}

	e1 := Message{ChatID: 7, MessageID: 1, Date: time.Unix(1100, 0), Text: "EDITED", EditDate: ptrTime(1100)}
	if err = s.RecordEdit(ctx, e1); err != nil {
		t.Fatal(err)
	}
	edits, err := s.Edits(ctx, 7, 1)
	if err != nil {
		t.Fatal(err)
	}
	// Edits() appends the live row as an is_current entry after the versions.
	if len(edits) != 2 || edits[0].Text != "ORIGINAL" {
		t.Fatalf("v1 must snapshot the original text, got %+v", edits)
	}

	// Applying the edit must not add another version...
	if err = s.UpsertMessage(ctx, e1); err != nil {
		t.Fatal(err)
	}
	// ...and re-delivering the same edit must be a no-op.
	if err = s.RecordEdit(ctx, e1); err != nil {
		t.Fatal(err)
	}
	if edits, _ = s.Edits(ctx, 7, 1); len(edits) != 2 {
		t.Fatalf("re-delivered edit created a duplicate version: %+v", edits)
	}

	e2 := Message{ChatID: 7, MessageID: 1, Date: time.Unix(1200, 0), Text: "EDITED2", EditDate: ptrTime(1200)}
	if err = s.RecordEdit(ctx, e2); err != nil {
		t.Fatal(err)
	}
	if err = s.UpsertMessage(ctx, e2); err != nil {
		t.Fatal(err)
	}
	edits, _ = s.Edits(ctx, 7, 1)
	if len(edits) != 3 {
		t.Fatalf("want 2 versions + current, got %d: %+v", len(edits), edits)
	}
	if edits[0].Text != "ORIGINAL" || edits[1].Text != "EDITED" || edits[2].Text != "EDITED2" {
		t.Fatalf("timeline must be [ORIGINAL, EDITED, current=EDITED2], got %+v", edits)
	}
}

func ptrTime(unix int64) *time.Time { t := time.Unix(unix, 0); return &t }
