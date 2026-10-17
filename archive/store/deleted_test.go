package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestListDeletedMessagesOnlyReturnsDeleted(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	for i := 1; i <= 3; i++ {
		if err := s.UpsertMessage(ctx, Message{ChatID: 1, MessageID: i, Date: time.Unix(int64(i), 0), Text: "message"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SoftDelete(ctx, 1, []int{2}); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListDeletedMessages(ctx, DeletedFilter{Page: 1, PageSize: 50})
	if err != nil {
		t.Fatal(err)
	}
	if got.Total != 1 || len(got.Items) != 1 || got.Items[0].MessageID != 2 || !got.Items[0].Deleted {
		t.Fatalf("page=%+v", got)
	}
	if got.Items[0].DeletedAt == 0 {
		t.Fatal("deleted_at was not returned")
	}
}

func TestListDeletedMessagesPagination(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	for i := 1; i <= 120; i++ {
		if err := s.UpsertMessage(ctx, Message{ChatID: 1, MessageID: i, Date: time.Unix(int64(i), 0), Text: "deleted"}); err != nil {
			t.Fatal(err)
		}
	}
	ids := make([]int, 120)
	for i := range ids {
		ids[i] = i + 1
	}
	if err := s.SoftDelete(ctx, 1, ids); err != nil {
		t.Fatal(err)
	}
	for page, want := range map[int]int{1: 50, 2: 50, 3: 20} {
		got, err := s.ListDeletedMessages(ctx, DeletedFilter{Page: page, PageSize: 50})
		if err != nil {
			t.Fatal(err)
		}
		if got.Total != 120 || len(got.Items) != want || got.Page != page || got.PageSize != 50 {
			t.Fatalf("page %d: %+v want items=%d total=120", page, got, want)
		}
	}
}

func TestListDeletedMessagesWithSearchSyntax(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	if _, err = s.DB().ExecContext(ctx, "INSERT INTO users(id,username,created_at,updated_at) VALUES(42,'alice',1,1)"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB().ExecContext(ctx, "INSERT INTO users(id,username,created_at,updated_at) VALUES(43,'bob',1,1)"); err != nil {
		t.Fatal(err)
	}
	a := Message{ChatID: 1, MessageID: 1, SenderID: 42, Date: time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC), Text: "alice image"}
	b := Message{ChatID: 1, MessageID: 2, SenderID: 43, Date: time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC), Text: "bob image"}
	for _, m := range []Message{a, b} {
		m.HasMedia = true
		if err := s.UpsertMessage(ctx, m); err != nil {
			t.Fatal(err)
		}
		if _, err := s.DB().ExecContext(ctx, "INSERT INTO media(message_rowid,type,mime_type,status,created_at,updated_at) SELECT id,'photo','image/jpeg','ready',1,1 FROM messages WHERE chat_id=? AND message_id=?", m.ChatID, m.MessageID); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SoftDelete(ctx, 1, []int{1, 2}); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListDeletedMessages(ctx, DeletedFilter{Query: "from:@alice has:image after:2026-10-01 before:2026-10-04", Page: 1, PageSize: 50})
	if err != nil {
		t.Fatal(err)
	}
	if got.Total != 1 || len(got.Items) != 1 || got.Items[0].MessageID != 1 {
		t.Fatalf("filtered=%+v", got)
	}
}
