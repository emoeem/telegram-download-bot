package store

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func benchmarkStore(b *testing.B, n int, withMedia, withLinks, withEdits, withDeleted bool) *Store {
	b.Helper()
	s, err := Open(filepath.Join(b.TempDir(), fmt.Sprintf("bench-%d.db", n)))
	if err != nil {
		b.Fatal(err)
	}
	ctx := context.Background()
	now := time.Now().Unix()
	tx, err := s.DB().BeginTx(ctx, nil)
	if err != nil {
		s.Close()
		b.Fatal(err)
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO users(id,username,first_name,created_at,updated_at) VALUES(42,'alice','Alice',?,?)", now, now); err != nil {
		tx.Rollback()
		s.Close()
		b.Fatal(err)
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO chats(id,type,username,title,created_at,updated_at) VALUES(123,'channel','source','Source',?,?)", now, now); err != nil {
		tx.Rollback()
		s.Close()
		b.Fatal(err)
	}
	msgStmt, err := tx.PrepareContext(ctx, "INSERT INTO messages(chat_id,message_id,sender_id,date,text,has_media,deleted,edit_date,created_at,updated_at,fts_text) VALUES(?,?,?,?,?,?,?,?,?,?,?)")
	if err != nil {
		tx.Rollback()
		s.Close()
		b.Fatal(err)
	}
	for i := 1; i <= n; i++ {
		deleted := 0
		if withDeleted && i <= n/10 {
			deleted = 1
		}
		hasMedia := 0
		if withMedia {
			hasMedia = 1
		}
		if _, err = msgStmt.Exec(123, i, 42, now-int64(i), fmt.Sprintf("benchmark message %d", i), hasMedia, deleted, 0, now, now, fmt.Sprintf("benchmark message %d", i)); err != nil {
			msgStmt.Close()
			tx.Rollback()
			s.Close()
			b.Fatal(err)
		}
	}
	msgStmt.Close()
	if withMedia {
		stmt, err := tx.PrepareContext(ctx, "INSERT INTO media(message_rowid,type,file_id_unique,mime_type,size,status,created_at,updated_at) VALUES(?,?,?,?,?,'completed',?,?)")
		if err != nil {
			tx.Rollback()
			s.Close()
			b.Fatal(err)
		}
		for i := 1; i <= n; i++ {
			if _, err = stmt.Exec(i, "photo", fmt.Sprintf("bench-file-%d", i), "image/jpeg", 1024, now, now); err != nil {
				stmt.Close()
				tx.Rollback()
				s.Close()
				b.Fatal(err)
			}
		}
		stmt.Close()
	}
	if withLinks {
		stmt, err := tx.PrepareContext(ctx, "INSERT INTO message_links(message_rowid,url,domain) VALUES(?,?,?)")
		if err != nil {
			tx.Rollback()
			s.Close()
			b.Fatal(err)
		}
		for i := 1; i <= n; i++ {
			if _, err = stmt.Exec(i, fmt.Sprintf("https://example.com/%d", i), "example.com"); err != nil {
				stmt.Close()
				tx.Rollback()
				s.Close()
				b.Fatal(err)
			}
		}
		stmt.Close()
	}
	if withEdits {
		stmt, err := tx.PrepareContext(ctx, "INSERT INTO message_edits(message_rowid,version,text,edit_date,archived_at) VALUES(?,?,?,?,?)")
		if err != nil {
			tx.Rollback()
			s.Close()
			b.Fatal(err)
		}
		for i := 1; i <= n/10; i++ {
			if _, err = stmt.Exec(i, 1, fmt.Sprintf("edited message %d", i), now, now); err != nil {
				stmt.Close()
				tx.Rollback()
				s.Close()
				b.Fatal(err)
			}
		}
		stmt.Close()
	}
	if err = tx.Commit(); err != nil {
		s.Close()
		b.Fatal(err)
	}
	b.Cleanup(func() { s.Close() })
	return s
}

func BenchmarkListMessages(b *testing.B) {
	for _, n := range []int{10000, 100000} {
		b.Run(fmt.Sprintf("%dk", n/1000), func(b *testing.B) {
			s := benchmarkStore(b, n, false, false, false, false)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := s.ListMessages(context.Background(), MessageFilter{Page: 1, PageSize: 50}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkSearchAdvanced(b *testing.B) {
	for _, n := range []int{10000, 100000} {
		b.Run(fmt.Sprintf("%dk", n/1000), func(b *testing.B) {
			s := benchmarkStore(b, n, true, false, false, false)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := s.SearchAdvanced(context.Background(), "benchmark has:image from:@alice after:2020-01-01", SearchOptions{Limit: 50}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkListMediaMessages(b *testing.B) {
	for _, n := range []int{10000, 100000} {
		b.Run(fmt.Sprintf("%dk", n/1000), func(b *testing.B) {
			s := benchmarkStore(b, n, true, false, false, false)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := s.ListMediaMessages(context.Background(), MediaFilter{Kind: MediaKindImage, Page: 1, PageSize: 50}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkListLinks(b *testing.B) {
	for _, n := range []int{10000, 100000} {
		b.Run(fmt.Sprintf("%dk", n/1000), func(b *testing.B) {
			s := benchmarkStore(b, n, false, true, false, false)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := s.ListLinks(context.Background(), LinkFilter{Page: 1, PageSize: 50}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkListDeletedMessages(b *testing.B) {
	for _, n := range []int{10000, 100000} {
		b.Run(fmt.Sprintf("%dk", n/1000), func(b *testing.B) {
			s := benchmarkStore(b, n, false, false, false, true)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := s.ListDeletedMessages(context.Background(), DeletedFilter{Page: 1, PageSize: 50}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkListEditedMessages(b *testing.B) {
	for _, n := range []int{10000, 100000} {
		b.Run(fmt.Sprintf("%dk", n/1000), func(b *testing.B) {
			s := benchmarkStore(b, n, false, false, true, false)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := s.ListEditedMessages(context.Background(), EditedFilter{Page: 1, PageSize: 50}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkInsertMessage(b *testing.B) {
	s := benchmarkStore(b, 1, false, false, false, false)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := s.UpsertMessage(ctx, Message{ChatID: 999, MessageID: i + 1, SenderID: 42, Date: time.Now(), Text: "benchmark insert"}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkEnsureMedia(b *testing.B) {
	s := benchmarkStore(b, 1, false, false, false, false)
	ctx := context.Background()
	meta := MediaMeta{Kind: "photo", FileUniqueID: "benchmark-media", MIME: "image/jpeg", Size: 1024, Width: 100, Height: 100}
	m := Message{ChatID: 123, MessageID: 1}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := s.EnsureMedia(ctx, m, meta); err != nil {
			b.Fatal(err)
		}
	}
}
