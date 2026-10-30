package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/gotd/td/tg"
)

func TestLinkMedias(t *testing.T) {
	msg := &tg.Message{Message: "看这个 https://www.youtube.com/watch?v=abc，还有这个 https://youtu.be/xyz。" +
		" 重复一次 https://youtu.be/xyz 再挂个普通页 https://example.com/post/1 和 B 站 https://www.bilibili.com/video/BV1xx"}
	got := LinkMedias(msg)
	// Canonicalised: "www." is dropped and a trailing slash is removed, so the
	// two spellings further down count as one video.
	want := []string{
		"https://youtube.com/watch?v=abc",
		"https://youtu.be/xyz",
		"https://bilibili.com/video/BV1xx",
	}
	if len(got) != len(want) {
		t.Fatalf("LinkMedias returned %d entries (%v), want %d", len(got), got, len(want))
	}
	for i, u := range want {
		if got[i].URL != u {
			t.Errorf("entry %d: URL=%q, want %q", i, got[i].URL, u)
		}
		if got[i].Kind != "link" {
			t.Errorf("entry %d: Kind=%q, want link", i, got[i].Kind)
		}
	}
}

// A trailing full stop is part of the sentence, not the URL. Feeding it to
// yt-dlp turns a valid link into a 404 that looks like a dead video.
func TestLinkMediasTrimsProsePunctuation(t *testing.T) {
	msg := &tg.Message{Message: "直达：https://youtu.be/abc）。"}
	got := LinkMedias(msg)
	if len(got) != 1 {
		t.Fatalf("got %d entries, want 1", len(got))
	}
	if got[0].URL != "https://youtu.be/abc" {
		t.Errorf("URL=%q, want https://youtu.be/abc", got[0].URL)
	}
}

// Covers the case MediaInfo structurally cannot see: Telegram built no preview,
// so the media field is empty and the URL exists only in the text.
func TestLinkMediasWithoutTelegramPreview(t *testing.T) {
	msg := &tg.Message{Message: "https://youtu.be/abc"}
	if _, ok := MediaInfo(msg); ok {
		t.Fatal("MediaInfo unexpectedly reported media for a text-only message")
	}
	if got := LinkMedias(msg); len(got) != 1 {
		t.Fatalf("LinkMedias returned %d entries, want 1", len(got))
	}
}

func TestLinkMediasIgnoresNonVideoHosts(t *testing.T) {
	// A t.me message link is the one exception to "only known video hosts",
	// because its bytes are served by Telegram itself rather than by a site an
	// external extractor knows. It is covered by TestLinkMediasTMe.
	msg := &tg.Message{Message: "https://example.com/a.mp4 https://someblog.dev/post/1"}
	if got := LinkMedias(msg); len(got) != 0 {
		t.Fatalf("LinkMedias returned %v, want none", got)
	}
}

// One post may name several videos. They must coexist, not overwrite each
// other — that overwrite is exactly what the media unique key used to cause.
func TestEnsureMediaKeepsEveryLinkOfOneMessage(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	saveMsg(t, s, 100, 1)
	m := Message{ChatID: 100, MessageID: 1}

	for _, u := range []string{"https://youtu.be/one", "https://youtu.be/two"} {
		if err := s.EnsureMedia(ctx, m, MediaMeta{Kind: "link", URL: u, Name: u}); err != nil {
			t.Fatalf("EnsureMedia(%s): %v", u, err)
		}
	}
	// Re-queueing the same URL must update in place, not stack a second row.
	if err := s.EnsureMedia(ctx, m, MediaMeta{Kind: "link", URL: "https://youtu.be/one"}); err != nil {
		t.Fatalf("EnsureMedia repeat: %v", err)
	}

	var n int
	if err := s.DB().QueryRow("SELECT COUNT(*) FROM media").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("media rows=%d, want 2", n)
	}
	jobs, err := s.PendingMedia(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	urls := map[string]bool{}
	for _, j := range jobs {
		if j.Type != "link" {
			t.Errorf("job type=%q, want link", j.Type)
		}
		urls[j.URL] = true
	}
	if !urls["https://youtu.be/one"] || !urls["https://youtu.be/two"] {
		t.Fatalf("pending link URLs=%v, want both", urls)
	}
}

// An attached file still collapses to a single row: its source_url is "", so
// the widened key cannot multiply it.
func TestEnsureMediaAttachedFileStaysUniquePerMessage(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	saveMsg(t, s, 100, 1)
	m := Message{ChatID: 100, MessageID: 1}

	for i := 0; i < 3; i++ {
		if err := s.EnsureMedia(ctx, m, MediaMeta{Kind: "photo", FileUniqueID: "tg-photo-9"}); err != nil {
			t.Fatalf("EnsureMedia: %v", err)
		}
	}
	var n int
	if err := s.DB().QueryRow("SELECT COUNT(*) FROM media").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("media rows=%d, want 1", n)
	}
}

// A photo plus two named videos is three downloads. The old one-file-per-message
// key made this impossible to represent.
func TestEnsureMediaPhotoPlusLinks(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	saveMsg(t, s, 100, 1)
	m := Message{ChatID: 100, MessageID: 1}

	if err := s.EnsureMedia(ctx, m, MediaMeta{Kind: "photo", FileUniqueID: "tg-photo-9"}); err != nil {
		t.Fatal(err)
	}
	for _, u := range []string{"https://youtu.be/a", "https://youtu.be/b"} {
		if err := s.EnsureMedia(ctx, m, MediaMeta{Kind: "link", URL: u}); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	if err := s.DB().QueryRow("SELECT COUNT(*) FROM media").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("media rows=%d, want 3", n)
	}
}

// A message carrying an attached photo plus external video links has several
// media rows. The message projection must return the post once and show the
// attachment, not one copy per link.
func TestMessageViewWithAttachedFileAndLinks(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	saveMsg(t, s, 100, 1)
	m := Message{ChatID: 100, MessageID: 1}
	if err := s.EnsureMedia(ctx, m, MediaMeta{Kind: "photo", FileUniqueID: "tg-photo-1", MIME: "image/jpeg"}); err != nil {
		t.Fatal(err)
	}
	for _, u := range []string{"https://youtu.be/a", "https://youtu.be/b"} {
		if err := s.EnsureMedia(ctx, m, MediaMeta{Kind: "link", URL: u}); err != nil {
			t.Fatal(err)
		}
	}
	var photoID int64
	if err := s.DB().QueryRow("SELECT id FROM media WHERE type='photo'").Scan(&photoID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec("UPDATE media SET status='completed',sha256=?,original_path=? WHERE id=?", "ee", "/tmp/obj/ee", photoID); err != nil {
		t.Fatal(err)
	}

	page, err := s.ListMessages(ctx, MessageFilter{Page: 1, PageSize: 10})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 1 || len(page.Items) != 1 {
		t.Fatalf("total=%d items=%d, want one post", page.Total, len(page.Items))
	}
	got := page.Items[0]
	if got.MediaMime != "image/jpeg" || got.MediaSHA256 != "ee" {
		t.Fatalf("projection picked media mime=%q sha=%q, want the attachment", got.MediaMime, got.MediaSHA256)
	}
}

// Existing databases were built with the old key. The realistic shape is a
// complete schema whose media table still carries the single-column UNIQUE, so
// the test builds a current database and then downgrades just that table.
func TestOpenMigratesMediaLinkUniqueness(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	first, err := Open(path)
	if err != nil {
		t.Fatalf("seed Open: %v", err)
	}
	ctx := context.Background()
	saveMsg(t, first, 100, 1)
	if err := first.EnsureMedia(ctx, Message{ChatID: 100, MessageID: 1},
		MediaMeta{Kind: "photo", FileUniqueID: "tg-photo-9"}); err != nil {
		t.Fatalf("seed EnsureMedia: %v", err)
	}
	if _, err := first.DB().Exec("UPDATE media SET status='completed',sha256='deadbeef'"); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	raw := openRawDB(t, path)
	for _, q := range []string{
		`PRAGMA foreign_keys=OFF`,
		// serial is present here because Open() adds it with ALTER before the
		// link-uniqueness rebuild runs; this fixture reproduces that exact state
		// (old constraint, new column), not a pre-serial database.
		`CREATE TABLE media_legacy (
 id INTEGER PRIMARY KEY AUTOINCREMENT, message_rowid INTEGER NOT NULL UNIQUE,
 serial INTEGER,
 type TEXT NOT NULL, file_id_unique TEXT, file_name TEXT, mime_type TEXT,
 size INTEGER, width INTEGER, height INTEGER, duration INTEGER,
 sha256 TEXT, original_path TEXT, thumbnail_path TEXT, preview_path TEXT,
 source_url TEXT NOT NULL DEFAULT '',
 ref_count INTEGER NOT NULL DEFAULT 1,
 status TEXT NOT NULL DEFAULT 'pending', error TEXT, attempts INTEGER NOT NULL DEFAULT 0,
 next_retry_at INTEGER, downloaded_at INTEGER, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL,
 error_kind TEXT, error_detail TEXT, last_error_at INTEGER,
 FOREIGN KEY(message_rowid) REFERENCES messages(id) ON DELETE CASCADE)`,
		`INSERT INTO media_legacy (` + mediaColumns + `) SELECT ` + mediaColumns + ` FROM media`,
		`DROP TABLE media`,
		`ALTER TABLE media_legacy RENAME TO media`,
	} {
		if _, err := raw.Exec(q); err != nil {
			t.Fatalf("downgrade media: %v", err)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	var kept string
	if err := s.DB().QueryRow("SELECT COALESCE(sha256,'') FROM media").Scan(&kept); err != nil {
		t.Fatal(err)
	}
	if kept != "deadbeef" {
		t.Fatalf("migrated row lost its sha256: %q", kept)
	}

	for _, u := range []string{"https://youtu.be/one", "https://youtu.be/two"} {
		if err := s.EnsureMedia(ctx, Message{ChatID: 100, MessageID: 1}, MediaMeta{Kind: "link", URL: u}); err != nil {
			t.Fatalf("EnsureMedia after migration: %v", err)
		}
	}
	var n int
	if err := s.DB().QueryRow("SELECT COUNT(*) FROM media").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("media rows=%d, want 3 (1 legacy + 2 links)", n)
	}

	// Indexes dropped with the old table must be back, or every media query
	// degrades into a full scan.
	var idx int
	if err := s.DB().QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_media_sha'").Scan(&idx); err != nil {
		t.Fatal(err)
	}
	if idx != 1 {
		t.Fatal("idx_media_sha missing after rebuild")
	}
}

func saveMsg(t *testing.T, s *Store, chatID int64, msgID int) {
	t.Helper()
	if err := s.UpsertMessage(context.Background(), Message{
		ChatID: chatID, MessageID: msgID, Date: time.Unix(0, 0), Text: "hello",
	}); err != nil {
		t.Fatalf("UpsertMessage: %v", err)
	}
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "archive.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// openRawDB opens a bare connection so a test can lay down a schema that
// predates the migrations Open runs.
func openRawDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	db.SetMaxOpenConns(1)
	return db
}

// The same page pasted in different spellings must queue once. Real archive
// data had one B站 video queued twice ("bilibili.com/x" and "www.bilibili.com/x/"),
// so the archive downloaded it twice and stored two rows for one file.
func TestLinkMediasDedupesURLSpellings(t *testing.T) {
	msg := &tg.Message{Message: "https://bilibili.com/video/BV1CzrNBxEzh 和 https://www.bilibili.com/video/BV1CzrNBxEzh/ 还有 https://www.bilibili.com/video/BV1CzrNBxEzh/"}
	got := LinkMedias(msg)
	if len(got) != 1 {
		t.Fatalf("got %d entries (%+v), want 1", len(got), got)
	}
	if got[0].URL != "https://bilibili.com/video/BV1CzrNBxEzh" {
		t.Errorf("URL = %q, want the canonical form", got[0].URL)
	}
}

func TestCanonicalLinkURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://www.bilibili.com/video/BV1xx/", "https://bilibili.com/video/BV1xx"},
		{"https://bilibili.com/video/BV1xx", "https://bilibili.com/video/BV1xx"},
		{"http://example.com", "https://example.com/"},
		{"https://WWW.YouTube.com/watch?v=abc", "https://youtube.com/watch?v=abc"},
		// A query string is part of the identity of a YouTube watch link.
		{"https://youtu.be/abc?t=30", "https://youtu.be/abc?t=30"},
	}
	for _, c := range cases {
		if got := canonicalLinkURL(c.in); got != c.want {
			t.Errorf("canonicalLinkURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
