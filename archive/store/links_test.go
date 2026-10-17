package store

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func newLinksTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	ctx := context.Background()
	if err := s.SetChat(ctx, 200, "Link Source", "source", "channel"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().ExecContext(ctx, "INSERT INTO users(id,username,first_name,created_at,updated_at) VALUES(42,'alice','Alice',1,1)"); err != nil {
		t.Fatal(err)
	}
	return s
}

func insertLinkTestMessage(t *testing.T, s *Store, id int, sender int64, date time.Time, rawURL, domain string) {
	t.Helper()
	ctx := context.Background()
	m := Message{ChatID: 200, MessageID: id, SenderID: sender, Date: date, Text: fmt.Sprintf("link %d %s", id, rawURL)}
	if err := s.UpsertMessage(ctx, m); err != nil {
		t.Fatal(err)
	}
	rowID, err := s.RowID(ctx, 200, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().ExecContext(ctx, "INSERT INTO message_links(message_rowid,url,domain) VALUES(?,?,?)", rowID, rawURL, domain); err != nil {
		t.Fatal(err)
	}
}

func TestListLinksBasic(t *testing.T) {
	s := newLinksTestStore(t)
	insertLinkTestMessage(t, s, 1, 42, time.Unix(1700000000, 0), "https://example.com/a", "example.com")
	page, err := s.ListLinks(context.Background(), LinkFilter{PageSize: 50})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 1 || len(page.Items) != 1 {
		t.Fatalf("page=%+v", page)
	}
	got := page.Items[0]
	if got.URL != "https://example.com/a" || got.Domain != "example.com" || got.ChatTitle != "Link Source" || got.SenderName != "alice" || got.SenderUsername != "alice" || got.MessageID != 1 {
		t.Fatalf("item=%+v", got)
	}
}

func TestListLinksPagination(t *testing.T) {
	s := newLinksTestStore(t)
	base := time.Unix(1700000000, 0)
	for i := 1; i <= 120; i++ {
		insertLinkTestMessage(t, s, i, 42, base.Add(time.Duration(i)*time.Minute), fmt.Sprintf("https://example.com/%d", i), "example.com")
	}
	for pageNo, want := range map[int]int{1: 50, 2: 50, 3: 20} {
		page, err := s.ListLinks(context.Background(), LinkFilter{Page: pageNo, PageSize: 50})
		if err != nil {
			t.Fatal(err)
		}
		if page.Total != 120 || len(page.Items) != want {
			t.Fatalf("page=%d total=%d items=%d want=%d", pageNo, page.Total, len(page.Items), want)
		}
	}
}

func TestListLinksByDomain(t *testing.T) {
	s := newLinksTestStore(t)
	base := time.Unix(1700000000, 0)
	insertLinkTestMessage(t, s, 1, 42, base, "https://example.com/a", "example.com")
	insertLinkTestMessage(t, s, 2, 42, base.Add(time.Minute), "https://example.org/b", "example.org")
	page, err := s.ListLinks(context.Background(), LinkFilter{Domain: "example.com", PageSize: 50})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 1 || len(page.Items) != 1 || page.Items[0].Domain != "example.com" {
		t.Fatalf("domain page=%+v total=%d", page.Items, page.Total)
	}
}

func TestListLinksWithSearchSyntax(t *testing.T) {
	s := newLinksTestStore(t)
	base := time.Unix(1700000000, 0)
	insertLinkTestMessage(t, s, 1, 42, base.AddDate(0, 0, -2), "https://example.com/old", "example.com")
	insertLinkTestMessage(t, s, 2, 42, base, "https://example.com/match", "example.com")
	insertLinkTestMessage(t, s, 3, 42, base.AddDate(0, 0, 2), "https://example.com/new", "example.com")
	page, err := s.ListLinks(context.Background(), LinkFilter{Query: "from:@alice after:2023-11-14 before:2023-11-15", PageSize: 50})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 1 || len(page.Items) != 1 || page.Items[0].MessageID != 2 {
		t.Fatalf("search page=%+v total=%d", page.Items, page.Total)
	}
}

func TestListLinkDomains(t *testing.T) {
	s := newLinksTestStore(t)
	base := time.Unix(1700000000, 0)
	insertLinkTestMessage(t, s, 1, 42, base, "https://example.com/a", "example.com")
	insertLinkTestMessage(t, s, 2, 42, base.Add(time.Minute), "https://example.org/b", "example.org")
	insertLinkTestMessage(t, s, 3, 42, base.Add(2*time.Minute), "https://example.com/c", "example.com")
	insertLinkTestMessage(t, s, 4, 42, base.Add(3*time.Minute), "https://example.net/d", "example.net")
	domains, err := s.ListLinkDomains(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(domains) != 3 || domains[0].Domain != "example.com" || domains[0].Count != 2 {
		t.Fatalf("domains=%+v", domains)
	}
}
