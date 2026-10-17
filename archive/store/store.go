package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	_ "modernc.org/sqlite"
	"strings"
	"time"
)

type Store struct {
	db   *sql.DB
	path string
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db, path: path}
	if _, err = db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	for _, q := range []string{
		"ALTER TABLE chats ADD COLUMN photo_id INTEGER",
		"ALTER TABLE chats ADD COLUMN avatar_path TEXT",
		"ALTER TABLE chats ADD COLUMN is_forum INTEGER NOT NULL DEFAULT 0",
		"ALTER TABLE users ADD COLUMN photo_id INTEGER",
		"ALTER TABLE users ADD COLUMN avatar_path TEXT",
		"ALTER TABLE archive_exports ADD COLUMN destination_id INTEGER",
		"ALTER TABLE archive_exports ADD COLUMN grouped_id INTEGER",
		"ALTER TABLE archive_exports ADD COLUMN fallback_reason TEXT",
		"ALTER TABLE monitor_targets ADD COLUMN archive_mode TEXT NOT NULL DEFAULT 'local'",
		"ALTER TABLE monitor_targets ADD COLUMN destination_id INTEGER",
		"ALTER TABLE media ADD COLUMN original_path TEXT",
		"ALTER TABLE media ADD COLUMN thumbnail_path TEXT",
		"ALTER TABLE media ADD COLUMN preview_path TEXT",
		"ALTER TABLE media ADD COLUMN error_kind TEXT",
		"ALTER TABLE media ADD COLUMN error_detail TEXT",
		"ALTER TABLE media ADD COLUMN last_error_at INTEGER",
	} {

		if _, e := db.Exec(q); e != nil && !strings.Contains(strings.ToLower(e.Error()), "duplicate column") {
			db.Close()
			return nil, e
		}
	}
	if _, err := db.Exec("CREATE INDEX IF NOT EXISTS idx_media_file_id_unique ON media(file_id_unique)"); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.Exec("CREATE INDEX IF NOT EXISTS idx_exports_grouped ON archive_exports(destination_id,grouped_id)"); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}
func (s *Store) Close() error { return s.db.Close() }
func (s *Store) DB() *sql.DB  { return s.db }
func (s *Store) Path() string { return s.path }

type Target struct {
	ChatID                                                          int64
	Username                                                        string
	Enabled, ArchiveText, ArchiveMedia, ArchiveFiles, DownloadMedia bool
	MediaMaxSize                                                    int64
	ArchiveMode                                                     string
	DestinationID                                                   sql.NullInt64
	Keywords, ExcludeKeywords                                       []string
	Since                                                           *time.Time
}

func (s *Store) AddTarget(ctx context.Context, t Target) error {
	now := time.Now().Unix()
	var since any
	if t.Since != nil {
		since = t.Since.Unix()
	}
	k, _ := json.Marshal(t.Keywords)
	e, _ := json.Marshal(t.ExcludeKeywords)
	_, err := s.db.ExecContext(ctx, `INSERT INTO monitor_targets(chat_id,username,enabled,archive_text,archive_media,archive_files,media_max_size,download_media,archive_mode,destination_id,keywords_json,exclude_keywords_json,since_date,created_at,updated_at)
 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(chat_id) DO UPDATE SET username=excluded.username,enabled=excluded.enabled,archive_text=excluded.archive_text,archive_media=excluded.archive_media,archive_files=excluded.archive_files,media_max_size=excluded.media_max_size,download_media=excluded.download_media,archive_mode=excluded.archive_mode,destination_id=excluded.destination_id,keywords_json=excluded.keywords_json,exclude_keywords_json=excluded.exclude_keywords_json,since_date=excluded.since_date,updated_at=excluded.updated_at`,
		t.ChatID, t.Username, t.Enabled, t.ArchiveText, t.ArchiveMedia, t.ArchiveFiles, t.MediaMaxSize, t.DownloadMedia, t.ArchiveMode, t.DestinationID, string(k), string(e), since, now, now)
	return err
}

// TargetRules carries the per-target switches and filters that can be changed
// after the target was created. Nil fields are left untouched.
type TargetRules struct {
	Enabled         *bool
	DownloadMedia   *bool
	MediaMaxSize    *int64
	Keywords        []string
	ExcludeKeywords []string
}

// UpdateTargetRules applies a partial update to a monitored chat and reports
// whether such a target exists.
func (s *Store) UpdateTargetRules(ctx context.Context, chatID int64, r TargetRules) error {
	sets := []string{}
	args := []any{}
	if r.Enabled != nil {
		sets = append(sets, "enabled=?")
		args = append(args, *r.Enabled)
	}
	if r.DownloadMedia != nil {
		sets = append(sets, "download_media=?")
		args = append(args, *r.DownloadMedia)
	}
	if r.MediaMaxSize != nil {
		sets = append(sets, "media_max_size=?")
		args = append(args, *r.MediaMaxSize)
	}
	if r.Keywords != nil {
		k, _ := json.Marshal(r.Keywords)
		sets = append(sets, "keywords_json=?")
		args = append(args, string(k))
	}
	if r.ExcludeKeywords != nil {
		e, _ := json.Marshal(r.ExcludeKeywords)
		sets = append(sets, "exclude_keywords_json=?")
		args = append(args, string(e))
	}
	if len(sets) == 0 {
		return nil
	}
	sets = append(sets, "updated_at=?")
	args = append(args, time.Now().Unix(), chatID)
	res, err := s.db.ExecContext(ctx, "UPDATE monitor_targets SET "+strings.Join(sets, ",")+" WHERE chat_id=?", args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("chat %d is not monitored", chatID)
	}
	return nil
}

func (s *Store) RemoveTarget(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM monitor_targets WHERE chat_id=?", id)
	return err
}
func (s *Store) Target(ctx context.Context, id int64) (Target, error) {
	var t Target
	var k, e string
	var since sql.NullInt64
	err := s.db.QueryRowContext(ctx, "SELECT chat_id,username,enabled,archive_text,archive_media,archive_files,media_max_size,download_media,archive_mode,destination_id,keywords_json,exclude_keywords_json,since_date FROM monitor_targets WHERE chat_id=?", id).Scan(&t.ChatID, &t.Username, &t.Enabled, &t.ArchiveText, &t.ArchiveMedia, &t.ArchiveFiles, &t.MediaMaxSize, &t.DownloadMedia, &t.ArchiveMode, &t.DestinationID, &k, &e, &since)
	if err != nil {
		return t, err
	}
	_ = json.Unmarshal([]byte(k), &t.Keywords)
	_ = json.Unmarshal([]byte(e), &t.ExcludeKeywords)
	if since.Valid {
		x := time.Unix(since.Int64, 0)
		t.Since = &x
	}
	return t, nil
}
func (s *Store) Targets(ctx context.Context) ([]Target, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT chat_id,username,enabled,archive_text,archive_media,archive_files,media_max_size,download_media,archive_mode,destination_id,keywords_json,exclude_keywords_json,since_date FROM monitor_targets ORDER BY chat_id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Target
	for rows.Next() {
		var t Target
		var k, e string
		var since sql.NullInt64
		if err := rows.Scan(&t.ChatID, &t.Username, &t.Enabled, &t.ArchiveText, &t.ArchiveMedia, &t.ArchiveFiles, &t.MediaMaxSize, &t.DownloadMedia, &t.ArchiveMode, &t.DestinationID, &k, &e, &since); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(k), &t.Keywords)
		_ = json.Unmarshal([]byte(e), &t.ExcludeKeywords)
		if since.Valid {
			x := time.Unix(since.Int64, 0)
			t.Since = &x
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
func (s *Store) Stats(ctx context.Context) (messages, media, targets int64, err error) {
	err = s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM messages").Scan(&messages)
	if err != nil {
		return
	}
	err = s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM media").Scan(&media)
	if err != nil {
		return
	}
	err = s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM monitor_targets WHERE enabled=1").Scan(&targets)
	return
}
func (s *Store) Search(ctx context.Context, q string, chatID int64, limit int) ([]SearchResult, error) {
	return s.SearchAdvanced(ctx, q, SearchOptions{ChatID: chatID, Limit: limit})
}

type SearchResult struct {
	RowID, ChatID int64
	MessageID     int
	Date          int64
	Text          string
	Rank          float64
}

type SearchOptions struct {
	ChatID, SenderID  int64
	After, Before     *time.Time
	MediaType         string
	HasMedia, HasLink bool
	MediaReadyOnly    bool
	DeletedOnly       bool
	EditedOnly        bool
	Topic             int64 // forum topic id; 0 = all topics
	Limit, Offset     int
}

// CountSearch counts the messages a SearchAdvanced call with the same input
// and options would return, reusing one shared filter builder instead of
// paging through the whole result set.
func (s *Store) CountSearch(ctx context.Context, input string, opt SearchOptions) (int, error) {
	where, args, _, err := s.buildSearchFilter(ctx, input, opt)
	if err != nil {
		return 0, err
	}
	q := "SELECT COUNT(*) FROM messages m WHERE " + strings.Join(where, " AND ")
	var n int
	if err := s.db.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// buildSearchFilter parses the search input (from:/in:/has:/before:/after:)
// and options into a WHERE clause over messages m. It also returns the raw
// FTS5 MATCH expression when the input carries search terms.
func (s *Store) buildSearchFilter(ctx context.Context, input string, opt SearchOptions) ([]string, []any, string, error) {
	parts := strings.Fields(strings.TrimSpace(input))
	var terms []string
	for _, p := range parts {
		switch {
		case strings.HasPrefix(p, "from:@"):
			var id int64
			if e := s.db.QueryRowContext(ctx, "SELECT id FROM users WHERE lower(username)=lower(?)", strings.TrimPrefix(p, "from:@")).Scan(&id); e != nil {
				opt.SenderID = -1
			} else {
				opt.SenderID = id
			}
		case strings.HasPrefix(p, "in:@"):
			var id int64
			if e := s.db.QueryRowContext(ctx, "SELECT id FROM chats WHERE lower(username)=lower(?)", strings.TrimPrefix(p, "in:@")).Scan(&id); e != nil {
				opt.ChatID = -1
			} else {
				opt.ChatID = id
			}
		case strings.HasPrefix(p, "has:"):
			switch strings.TrimPrefix(p, "has:") {
			case "image":
				opt.MediaType = "image"
			case "video":
				opt.MediaType = "video"
			case "file":
				opt.MediaType = "file"
			case "audio":
				opt.MediaType = "audio"
			case "link":
				opt.HasLink = true
			default:
				terms = append(terms, p)
			}
		case strings.HasPrefix(p, "before:"):
			if t, e := time.Parse("2006-01-02", strings.TrimPrefix(p, "before:")); e == nil {
				opt.Before = &t
			}
		case strings.HasPrefix(p, "after:"):
			if t, e := time.Parse("2006-01-02", strings.TrimPrefix(p, "after:")); e == nil {
				opt.After = &t
			}
		default:
			terms = append(terms, p)
		}
	}
	if opt.Limit <= 0 || opt.Limit > 500 {
		opt.Limit = 50
	}
	if opt.Offset < 0 {
		opt.Offset = 0
	}
	where := []string{"1=1"}
	args := []any{}
	ftsQuery := ""
	if len(terms) > 0 {
		tokens := segmenter.CutSearch(strings.Join(terms, " "), true)
		qparts := make([]string, 0, len(tokens))
		for _, token := range tokens {
			qparts = append(qparts, "\""+strings.ReplaceAll(token, "\"", "\"\"")+"\"")
		}
		if len(qparts) > 0 {
			where = append(where, "m.id IN (SELECT rowid FROM messages_fts WHERE messages_fts MATCH ?)")
			args = append(args, strings.Join(qparts, " AND "))
			ftsQuery = strings.Join(qparts, " AND ")
		}
	}
	if opt.ChatID != 0 {
		where = append(where, "m.chat_id=?")
		args = append(args, opt.ChatID)
	}
	if opt.Topic > 0 {
		where = append(where, TopicMessageFilterSQL)
		args = append(args, opt.Topic, opt.Topic)
	}
	if opt.SenderID != 0 {
		where = append(where, "m.sender_id=?")
		args = append(args, opt.SenderID)
	}
	if opt.After != nil {
		where = append(where, "m.date>=?")
		args = append(args, opt.After.Unix())
	}
	if opt.Before != nil {
		where = append(where, "m.date<?")
		args = append(args, opt.Before.Unix())
	}
	if opt.HasMedia {
		where = append(where, "m.has_media=1")
	}
	if opt.HasLink {
		where = append(where, "EXISTS(SELECT 1 FROM message_links ml WHERE ml.message_rowid=m.id)")
	}
	if opt.MediaReadyOnly {
		where = append(where, "EXISTS(SELECT 1 FROM media mr WHERE mr.message_rowid=m.id AND mr.status='completed')")
	}
	if opt.DeletedOnly {
		where = append(where, "m.deleted=1")
	}
	if opt.EditedOnly {
		where = append(where, "EXISTS(SELECT 1 FROM message_edits me WHERE me.message_rowid=m.id)")
	}
	if opt.MediaType != "" {
		switch opt.MediaType {
		case "image":
			where = append(where, "EXISTS(SELECT 1 FROM media mx WHERE mx.message_rowid=m.id AND (mx.type='photo' OR mx.mime_type LIKE 'image/%'))")
		case "video":
			where = append(where, "EXISTS(SELECT 1 FROM media mx WHERE mx.message_rowid=m.id AND (mx.type IN ('video','animation') OR mx.mime_type LIKE 'video/%'))")
		case "audio":
			where = append(where, "EXISTS(SELECT 1 FROM media mx WHERE mx.message_rowid=m.id AND (mx.type IN ('audio','voice') OR mx.mime_type LIKE 'audio/%'))")
		case "file":
			where = append(where, "EXISTS(SELECT 1 FROM media mx WHERE mx.message_rowid=m.id AND mx.type NOT IN ('photo','video','animation','audio','voice','sticker') AND (mx.mime_type IS NULL OR (mx.mime_type NOT LIKE 'image/%' AND mx.mime_type NOT LIKE 'video/%' AND mx.mime_type NOT LIKE 'audio/%')))")
		}
	}
	return where, args, ftsQuery, nil
}

// SearchAdvanced parses the search input and runs the paged query.
func (s *Store) SearchAdvanced(ctx context.Context, input string, opt SearchOptions) ([]SearchResult, error) {
	where, args, ftsQuery, err := s.buildSearchFilter(ctx, input, opt)
	if err != nil {
		return nil, err
	}
	return s.searchWhere(ctx, where, args, opt, ftsQuery)
}

// searchWhere runs the assembled WHERE clause with SearchOptions paging and
// ordering semantics.
func (s *Store) searchWhere(ctx context.Context, where []string, args []any, opt SearchOptions, ftsQuery string) ([]SearchResult, error) {
	order := "m.date DESC"
	join := ""
	rankExpr := "0.0"
	if ftsQuery != "" {
		// JOIN the FTS table with MATCH so bm25() actually computes: the bare
		// `rank` column is only meaningful inside a MATCH query. Better matches
		// score smaller (negative) — ascending order is best-first relevance.
		join = "JOIN messages_fts ON messages_fts.rowid=m.id"
		where = append([]string{"messages_fts MATCH ?"}, where...)
		args = append([]any{ftsQuery}, args...)
		rankExpr = "bm25(messages_fts)"
		if !opt.DeletedOnly && !opt.EditedOnly {
			order = rankExpr + ", m.date DESC"
		}
	}
	q := "SELECT m.id,m.chat_id,m.message_id,m.date,COALESCE(m.text,m.caption,'')," + rankExpr + " FROM messages m " + join + " WHERE " + strings.Join(where, " AND ") + " ORDER BY " + order + " LIMIT ? OFFSET ?"
	args = append(args, opt.Limit, opt.Offset)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SearchResult
	for rows.Next() {
		var r SearchResult
		if err := rows.Scan(&r.RowID, &r.ChatID, &r.MessageID, &r.Date, &r.Text, &r.Rank); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
