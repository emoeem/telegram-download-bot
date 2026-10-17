package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestListEditedMessagesOnlyReturnsEdited(t *testing.T) {
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
	rowID, err := s.RowID(ctx, 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().ExecContext(ctx, "INSERT INTO message_edits(message_rowid,version,text,edit_date,archived_at) VALUES(?,?,?,?,?)", rowID, 1, "edited", 10, 11); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListEditedMessages(ctx, EditedFilter{Page: 1, PageSize: 50})
	if err != nil {
		t.Fatal(err)
	}
	if got.Total != 1 || len(got.Items) != 1 || got.Items[0].MessageID != 2 || got.Items[0].EditCount != 1 {
		t.Fatalf("page=%+v", got)
	}
}

func TestListEditedMessagesPagination(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	for i := 1; i <= 120; i++ {
		if err := s.UpsertMessage(ctx, Message{ChatID: 1, MessageID: i, Date: time.Unix(int64(i), 0), Text: "message"}); err != nil {
			t.Fatal(err)
		}
		rowID, err := s.RowID(ctx, 1, i)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.DB().ExecContext(ctx, "INSERT INTO message_edits(message_rowid,version,text,edit_date,archived_at) VALUES(?,?,?,?,?)", rowID, 1, "edited", int64(i), int64(i)+1); err != nil {
			t.Fatal(err)
		}
	}
	for page, want := range map[int]int{1: 50, 2: 50, 3: 20} {
		got, err := s.ListEditedMessages(ctx, EditedFilter{Page: page, PageSize: 50})
		if err != nil {
			t.Fatal(err)
		}
		if got.Total != 120 || len(got.Items) != want || got.Page != page || got.PageSize != 50 {
			t.Fatalf("page %d: %+v want items=%d total=120", page, got, want)
		}
	}
}

func TestListEditedMessagesWithSearchSyntax(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	if _, err = s.DB().ExecContext(ctx, "INSERT INTO users(id,username,created_at,updated_at) VALUES(42,'alice',1,1)"); err != nil {
		t.Fatal(err)
	}
	a := Message{ChatID: 1, MessageID: 1, SenderID: 42, Date: time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC), Text: "alice image"}
	b := Message{ChatID: 1, MessageID: 2, SenderID: 43, Date: time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC), Text: "bob image"}
	for _, m := range []Message{a, b} {
		m.HasMedia = true
		if err := s.UpsertMessage(ctx, m); err != nil {
			t.Fatal(err)
		}
		rowID, err := s.RowID(ctx, m.ChatID, m.MessageID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.DB().ExecContext(ctx, "INSERT INTO media(message_rowid,type,mime_type,status,created_at,updated_at) VALUES(?,'photo','image/jpeg','ready',1,1)", rowID); err != nil {
			t.Fatal(err)
		}
	}
	rowID, err := s.RowID(ctx, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB().ExecContext(ctx, "INSERT INTO message_edits(message_rowid,version,text,edit_date,archived_at) VALUES(?,?,?,?,?)", rowID, 1, "edited", time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC).Unix(), 1); err != nil {
		t.Fatal(err)
	}
	rowID, err = s.RowID(ctx, 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB().ExecContext(ctx, "INSERT INTO message_edits(message_rowid,version,text,edit_date,archived_at) VALUES(?,?,?,?,?)", rowID, 1, "edited", time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC).Unix(), 1); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListEditedMessages(ctx, EditedFilter{Query: "from:@alice has:image after:2026-10-01 before:2026-10-04", Page: 1, PageSize: 50})
	if err != nil {
		t.Fatal(err)
	}
	if got.Total != 1 || len(got.Items) != 1 || got.Items[0].MessageID != 1 {
		t.Fatalf("filtered=%+v", got)
	}
}

func TestGetMessageEditTimeline(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	editAt := []int64{100, 200, 300}
	if err := s.UpsertMessage(ctx, Message{ChatID: 7, MessageID: 9, Date: time.Unix(50, 0), EditDate: timePtr(time.Unix(400, 0)), Text: "current"}); err != nil {
		t.Fatal(err)
	}
	rowID, err := s.RowID(ctx, 7, 9)
	if err != nil {
		t.Fatal(err)
	}
	for i, ts := range editAt {
		if _, err = s.DB().ExecContext(ctx, "INSERT INTO message_edits(message_rowid,version,text,edit_date,archived_at) VALUES(?,?,?,?,?)", rowID, i+1, "v"+string(rune('1'+i)), ts, ts+1); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.GetMessageEditTimeline(ctx, 7, 9)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 || got[0].EditedAt != 100 || got[1].EditedAt != 200 || got[2].EditedAt != 300 || !got[3].IsCurrent || got[3].Text != "current" {
		t.Fatalf("timeline=%+v", got)
	}
	if err = s.UpsertMessage(ctx, Message{ChatID: 8, MessageID: 1, Date: time.Unix(50, 0), Text: "never edited"}); err != nil {
		t.Fatal(err)
	}
	empty, err := s.GetMessageEditTimeline(ctx, 8, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(empty) != 0 {
		t.Fatalf("expected no timeline without history, got=%+v", empty)
	}
}

func timePtr(t time.Time) *time.Time { return &t }
