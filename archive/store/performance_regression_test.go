package store

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func TestListLinksMatchesSearchAdvancedCandidates(t *testing.T) {
	s := newLinksTestStore(t)
	ctx := context.Background()
	base := time.Unix(1700000000, 0)
	for i := 1; i <= 120; i++ {
		insertLinkTestMessage(t, s, i, 42, base.Add(time.Duration(i)*time.Minute), fmt.Sprintf("https://example.com/%d", i), "example.com")
	}
	for page := 1; page <= 3; page++ {
		const pageSize = 50
		candidates, err := s.SearchAdvanced(ctx, "from:@alice", SearchOptions{HasLink: true, Limit: pageSize, Offset: (page - 1) * pageSize})
		if err != nil {
			t.Fatal(err)
		}
		got, err := s.ListLinks(ctx, LinkFilter{Query: "from:@alice", Page: page, PageSize: pageSize})
		if err != nil {
			t.Fatal(err)
		}
		if got.Total != 120 || len(candidates) != len(got.Items) {
			t.Fatalf("page=%d candidates=%d links=%d total=%d", page, len(candidates), len(got.Items), got.Total)
		}
		for i := range candidates {
			if candidates[i].RowID != got.Items[i].MessageRowID {
				t.Fatalf("page=%d index=%d candidate=%d link=%d", page, i, candidates[i].RowID, got.Items[i].MessageRowID)
			}
		}
	}
}

func TestListLinksLargeCandidateSet(t *testing.T) {
	s := newLinksTestStore(t)
	ctx := context.Background()
	tx, err := s.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	msgStmt, err := tx.PrepareContext(ctx, "INSERT INTO messages(chat_id,message_id,sender_id,date,text,has_media,deleted,edit_date,created_at,updated_at,fts_text) VALUES(?,?,?,?,?,?,?,?,?,?,?)")
	if err != nil {
		t.Fatal(err)
	}
	linkStmt, err := tx.PrepareContext(ctx, "INSERT INTO message_links(message_rowid,url,domain) VALUES(?,?,?)")
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 5000; i++ {
		if _, err := msgStmt.Exec(200, i, 42, now-int64(i), fmt.Sprintf("large link %d", i), 0, 0, 0, now, now, fmt.Sprintf("large link %d", i)); err != nil {
			t.Fatal(err)
		}
		if _, err := linkStmt.Exec(i, fmt.Sprintf("https://example.com/%d", i), "example.com"); err != nil {
			t.Fatal(err)
		}
	}
	msgStmt.Close()
	linkStmt.Close()
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	page, err := s.ListLinks(ctx, LinkFilter{Page: 1, PageSize: 200})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 5000 || len(page.Items) != 200 {
		t.Fatalf("large links total=%d items=%d", page.Total, len(page.Items))
	}
}

func TestListEditedMessagesEditCountMatches(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	if err := s.UpsertMessage(ctx, Message{ChatID: 1, MessageID: 1, Date: time.Unix(1, 0), Text: "edited"}); err != nil {
		t.Fatal(err)
	}
	rowID, err := s.RowID(ctx, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	for version := 1; version <= 3; version++ {
		if _, err := s.DB().ExecContext(ctx, "INSERT INTO message_edits(message_rowid,version,text,edit_date,archived_at) VALUES(?,?,?,?,?)", rowID, version, fmt.Sprintf("v%d", version), int64(version), int64(version)); err != nil {
			t.Fatal(err)
		}
	}
	page, err := s.ListEditedMessages(ctx, EditedFilter{Page: 1, PageSize: 50})
	if err != nil {
		t.Fatal(err)
	}
	timeline, err := s.GetMessageEditTimeline(ctx, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].EditCount != len(timeline)-1 {
		t.Fatalf("edit count=%d timeline=%d", page.Items[0].EditCount, len(timeline))
	}
}

func TestListEditedMessagesLargeSet(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	tx, err := s.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	msgStmt, err := tx.PrepareContext(ctx, "INSERT INTO messages(chat_id,message_id,date,text,has_media,deleted,edit_date,created_at,updated_at,fts_text) VALUES(?,?,?,?,?,?,?,?,?,?)")
	if err != nil {
		t.Fatal(err)
	}
	editStmt, err := tx.PrepareContext(ctx, "INSERT INTO message_edits(message_rowid,version,text,edit_date,archived_at) VALUES(?,?,?,?,?)")
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 5000; i++ {
		if _, err := msgStmt.Exec(1, i, now-int64(i), fmt.Sprintf("edited %d", i), 0, 0, 0, now, now, fmt.Sprintf("edited %d", i)); err != nil {
			t.Fatal(err)
		}
		if _, err := editStmt.Exec(int64(i), 1, "v1", now-int64(i), now); err != nil {
			t.Fatal(err)
		}
	}
	msgStmt.Close()
	editStmt.Close()
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	page, err := s.ListEditedMessages(ctx, EditedFilter{Page: 1, PageSize: 200})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 5000 || len(page.Items) != 200 {
		t.Fatalf("large edited total=%d items=%d", page.Total, len(page.Items))
	}
}
