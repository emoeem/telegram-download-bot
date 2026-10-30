package store

import (
	"context"
	"strings"
	"time"
)

// Material is one normalized archive asset: a single post, or one album folded
// from every message sharing a grouped_id. It is derived data — rebuilding from
// the raw messages table must always reproduce an identical row.
type Material struct {
	ID              int64
	ChatID          int64
	AnchorRowID     int64
	AnchorMessageID int
	GroupedID       int64
	Kind            string
	Title           string
	Body            string
	Caption         string
	ContentHash     string
	TextHash        string
	MediaCount      int
	MediaKind       string
	CharCount       int
	SourceURL       string
	SourceName      string
	Date            int64
	Tags            string
	Status          string
	DuplicateOf     int64
	CreatedAt       int64
	UpdatedAt       int64
}

// MaterialSource is one raw message as consumed by the materializer: the fields
// needed to build a material, plus the media identity that feeds content_hash.
type MaterialSource struct {
	RowID       int64
	ChatID      int64
	MessageID   int
	GroupedID   int64
	Date        int64
	Text        string
	Caption     string
	Views       int
	Forwards    int
	HasMedia    bool
	MediaFileID string
	MediaType   string
	MediaMIME   string
	MediaSHA    string
	MediaPath   string
	ThumbPath   string
	PreviewPath string
	MediaWidth  int
	MediaHeight int
	Duration    int
	MediaSize   int64
}

const materialSourceColumns = `m.id,m.chat_id,m.message_id,COALESCE(m.grouped_id,0),m.date,COALESCE(m.text,''),COALESCE(m.caption,''),COALESCE(m.views,0),COALESCE(m.forwards,0),m.has_media,COALESCE(md.file_id_unique,''),COALESCE(md.type,''),COALESCE(md.mime_type,''),COALESCE(md.sha256,''),COALESCE(md.original_path,''),COALESCE(md.thumbnail_path,''),COALESCE(md.preview_path,''),COALESCE(md.width,0),COALESCE(md.height,0),COALESCE(md.duration,0),COALESCE(md.size,0)`

const materialSourceFrom = ` FROM messages m LEFT JOIN media md ON md.message_rowid=m.id`

func scanMaterialSource(sc rowScanner) (MaterialSource, error) {
	var s MaterialSource
	err := sc.Scan(&s.RowID, &s.ChatID, &s.MessageID, &s.GroupedID, &s.Date, &s.Text, &s.Caption,
		&s.Views, &s.Forwards, &s.HasMedia, &s.MediaFileID, &s.MediaType, &s.MediaMIME, &s.MediaSHA,
		&s.MediaPath, &s.ThumbPath, &s.PreviewPath, &s.MediaWidth, &s.MediaHeight, &s.Duration, &s.MediaSize)
	return s, err
}

// MaterialSources pages the raw messages of one chat (or of every chat when
// chatID is 0) in stable rowid order. afterRowID is exclusive, so a rebuild can
// walk a large archive without holding every row in memory.
func (s *Store) MaterialSources(ctx context.Context, chatID, afterRowID int64, limit int) ([]MaterialSource, error) {
	if limit <= 0 || limit > 2000 {
		limit = 500
	}
	// paging is by m.id, so the rows come back in key order regardless of the
	// media join; the chat predicate is conditional so the index is usable.
	q := "SELECT " + materialSourceColumns + materialSourceFrom + " WHERE m.id>? AND m.deleted=0"
	args := []any{afterRowID}
	if chatID != 0 {
		q += " AND m.chat_id=?"
		args = append(args, chatID)
	}
	q += " ORDER BY m.id LIMIT ?"
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]MaterialSource, 0, limit)
	for rows.Next() {
		v, err := scanMaterialSource(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// MaterialSourcesByGrouped returns every message of one album in message order.
func (s *Store) MaterialSourcesByGrouped(ctx context.Context, chatID, groupedID int64) ([]MaterialSource, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+materialSourceColumns+materialSourceFrom+
		" WHERE m.chat_id=? AND m.grouped_id=? AND m.deleted=0 ORDER BY m.message_id", chatID, groupedID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MaterialSource
	for rows.Next() {
		v, err := scanMaterialSource(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// MaterialSourceByRowID reads one message as materializer input.
func (s *Store) MaterialSourceByRowID(ctx context.Context, rowID int64) (MaterialSource, error) {
	return scanMaterialSource(s.db.QueryRowContext(ctx, "SELECT "+materialSourceColumns+materialSourceFrom+" WHERE m.id=?", rowID))
}

// ReplaceMaterial writes one material and its message membership atomically.
// The upsert key is (chat_id, anchor_message_id): re-materializing the same
// post after an edit updates the row instead of duplicating it.
func (s *Store) ReplaceMaterial(ctx context.Context, m Material, rowIDs []int64) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	now := time.Now().Unix()
	if m.CreatedAt == 0 {
		m.CreatedAt = now
	}
	var groupID any
	if m.GroupedID != 0 {
		groupID = m.GroupedID
	}
	var dup any
	if m.DuplicateOf != 0 {
		dup = m.DuplicateOf
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO materials(chat_id,anchor_rowid,anchor_message_id,grouped_id,kind,title,body,caption,content_hash,text_hash,media_count,media_kind,char_count,source_url,source_name,date,tags,status,duplicate_of,created_at,updated_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(chat_id,anchor_message_id) DO UPDATE SET
 anchor_rowid=excluded.anchor_rowid,grouped_id=excluded.grouped_id,kind=excluded.kind,title=excluded.title,
 body=excluded.body,caption=excluded.caption,content_hash=excluded.content_hash,text_hash=excluded.text_hash,
 media_count=excluded.media_count,media_kind=excluded.media_kind,char_count=excluded.char_count,
 source_url=excluded.source_url,source_name=excluded.source_name,date=excluded.date,
 status=CASE WHEN materials.status='duplicate' AND materials.content_hash=excluded.content_hash THEN 'duplicate' ELSE excluded.status END,
 duplicate_of=CASE WHEN materials.status='duplicate' AND materials.content_hash=excluded.content_hash THEN materials.duplicate_of ELSE NULL END,
 updated_at=excluded.updated_at`,
		m.ChatID, m.AnchorRowID, m.AnchorMessageID, groupID, m.Kind, m.Title, m.Body, m.Caption,
		m.ContentHash, m.TextHash, m.MediaCount, m.MediaKind, m.CharCount, m.SourceURL, m.SourceName,
		m.Date, m.Tags, m.Status, dup, m.CreatedAt, now); err != nil {
		return 0, err
	}
	var id int64
	if err = tx.QueryRowContext(ctx, "SELECT id FROM materials WHERE chat_id=? AND anchor_message_id=?", m.ChatID, m.AnchorMessageID).Scan(&id); err != nil {
		return 0, err
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM material_messages WHERE material_id=?", id); err != nil {
		return 0, err
	}
	for i, row := range rowIDs {
		if _, err = tx.ExecContext(ctx, "INSERT INTO material_messages(material_id,message_rowid,position) VALUES(?,?,?)", id, row, i); err != nil {
			return 0, err
		}
	}
	return id, tx.Commit()
}

// DeleteMaterialByAnchor removes the material built from one post, used when the
// post turns out to be excluded (deleted, filtered out by a target edit).
func (s *Store) DeleteMaterialByAnchor(ctx context.Context, chatID int64, anchorMessageID int) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM materials WHERE chat_id=? AND anchor_message_id=?", chatID, anchorMessageID)
	return err
}

// DeleteMaterials drops every material of one chat, or all of them when chatID
// is 0. It is the first step of a full rebuild.
func (s *Store) DeleteMaterials(ctx context.Context, chatID int64) (int64, error) {
	res, err := s.db.ExecContext(ctx, "DELETE FROM materials WHERE (?=0 OR chat_id=?)", chatID, chatID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// MaterialFilter selects materials for listing and export.
type MaterialFilter struct {
	ChatID   int64
	Kind     string
	Query    string
	Status   string // "", "active", "duplicate", "all"
	MinChars int
	HasMedia *bool
	Since    int64
	Until    int64
	Page     int
	PageSize int
}

func (f MaterialFilter) where() (string, []any) {
	var cond []string
	var args []any
	if f.ChatID != 0 {
		cond = append(cond, "mt.chat_id=?")
		args = append(args, f.ChatID)
	}
	if f.Kind != "" && f.Kind != "all" {
		cond = append(cond, "mt.kind=?")
		args = append(args, f.Kind)
	}
	switch f.Status {
	case "all", "":
		// no status restriction
	case "duplicate", "active":
		cond = append(cond, "mt.status=?")
		args = append(args, f.Status)
	default:
		cond = append(cond, "mt.status='active'")
	}
	if f.Query != "" {
		like := "%" + strings.ToLower(f.Query) + "%"
		cond = append(cond, "(LOWER(mt.body) LIKE ? OR LOWER(mt.title) LIKE ? OR LOWER(mt.source_name) LIKE ?)")
		args = append(args, like, like, like)
	}
	if f.MinChars > 0 {
		cond = append(cond, "mt.char_count>=?")
		args = append(args, f.MinChars)
	}
	if f.HasMedia != nil {
		if *f.HasMedia {
			cond = append(cond, "mt.media_count>0")
		} else {
			cond = append(cond, "mt.media_count=0")
		}
	}
	if f.Since > 0 {
		cond = append(cond, "mt.date>=?")
		args = append(args, f.Since)
	}
	if f.Until > 0 {
		cond = append(cond, "mt.date<=?")
		args = append(args, f.Until)
	}
	if len(cond) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(cond, " AND "), args
}

// MaterialView is one material joined with its chat name and its anchor media,
// i.e. everything an export row or a publish template can address.
type MaterialView struct {
	Material
	ChatTitle    string
	ChatUsername string
	MediaType    string
	MediaMIME    string
	MediaSHA     string
	// MediaURL is the external page a link material points at (Kind=="link").
	// It is separate from Material.SourceURL, which is the t.me post.
	MediaURL      string
	MediaPath     string
	ThumbnailPath string
	PreviewPath   string
	Width         int
	Height        int
	Duration      int
	MediaSize     int64
	MessageIDs    string
	Views         int
	Forwards      int
	// Serial is the receive number of the material's first media item — the
	// number a person cites ("post #001337") and the anchor for
	// serial → material → publish. 0 means the material has no numbered media
	// (text-only material, or media archived before numbering was introduced
	// and not yet backfilled).
	Serial int64
	// anchorSerial carries the second column of the two-COALESCE read: an
	// album's number comes from all its messages, a single post's from its
	// anchor. Private because it is a scan artifact, not part of the view.
	anchorSerial int64
}

const materialViewColumns = `mt.id,mt.chat_id,mt.anchor_rowid,mt.anchor_message_id,COALESCE(mt.grouped_id,0),mt.kind,mt.title,mt.body,mt.caption,mt.content_hash,mt.text_hash,mt.media_count,mt.media_kind,mt.char_count,mt.source_url,mt.source_name,mt.date,mt.tags,mt.status,COALESCE(mt.duplicate_of,0),mt.created_at,mt.updated_at,
COALESCE(c.title,''),COALESCE(c.username,''),
COALESCE(md.type,''),COALESCE(md.mime_type,''),COALESCE(md.sha256,''),COALESCE(md.original_path,''),COALESCE(md.thumbnail_path,''),COALESCE(md.preview_path,''),COALESCE(md.width,0),COALESCE(md.height,0),COALESCE(md.duration,0),COALESCE(md.size,0),COALESCE(md.source_url,''),
COALESCE(m.views,0),COALESCE(m.forwards,0),
COALESCE((SELECT GROUP_CONCAT(m3.message_id,',') FROM material_messages mm JOIN messages m3 ON m3.id=mm.message_rowid WHERE mm.material_id=mt.id),''),
COALESCE((SELECT MIN(s.serial) FROM media s WHERE s.message_rowid IN (SELECT message_rowid FROM material_messages WHERE material_id=mt.id)),0),
COALESCE((SELECT MIN(s2.serial) FROM media s2 WHERE s2.message_rowid=mt.anchor_rowid),0)`

const materialViewFrom = ` FROM materials mt
LEFT JOIN chats c ON c.id=mt.chat_id
LEFT JOIN media md ON md.id=COALESCE((SELECT MIN(pm.id) FROM media pm WHERE pm.message_rowid=mt.anchor_rowid AND pm.sha256 IS NOT NULL AND pm.sha256<>''),(SELECT MIN(pm.id) FROM media pm WHERE pm.message_rowid=mt.anchor_rowid))
LEFT JOIN messages m ON m.id=mt.anchor_rowid`

const materialViewSelect = "SELECT " + materialViewColumns + materialViewFrom

func scanMaterialView(sc rowScanner) (MaterialView, error) {
	var v MaterialView
	err := sc.Scan(&v.ID, &v.ChatID, &v.AnchorRowID, &v.AnchorMessageID, &v.GroupedID, &v.Kind, &v.Title, &v.Body, &v.Caption,
		&v.ContentHash, &v.TextHash, &v.MediaCount, &v.MediaKind, &v.CharCount, &v.SourceURL, &v.SourceName,
		&v.Date, &v.Tags, &v.Status, &v.DuplicateOf, &v.CreatedAt, &v.UpdatedAt,
		&v.ChatTitle, &v.ChatUsername,
		&v.MediaType, &v.MediaMIME, &v.MediaSHA, &v.MediaPath, &v.ThumbnailPath, &v.PreviewPath,
		&v.Width, &v.Height, &v.Duration, &v.MediaSize, &v.MediaURL, &v.Views, &v.Forwards, &v.MessageIDs,
		&v.Serial, &v.anchorSerial)
	if v.Serial == 0 {
		v.Serial = v.anchorSerial
	}
	return v, err
}

// ListMaterials returns one page of materials, newest first.
func (s *Store) ListMaterials(ctx context.Context, f MaterialFilter) ([]MaterialView, error) {
	where, args := f.where()
	if f.PageSize <= 0 || f.PageSize > 5000 {
		f.PageSize = 200
	}
	if f.Page < 1 {
		f.Page = 1
	}
	q := materialViewSelect + where + " ORDER BY mt.date DESC, mt.id DESC LIMIT ? OFFSET ?"
	args = append(args, f.PageSize, (f.Page-1)*f.PageSize)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MaterialView
	for rows.Next() {
		v, err := scanMaterialView(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// CountMaterials returns how many materials match the filter.
func (s *Store) CountMaterials(ctx context.Context, f MaterialFilter) (int, error) {
	where, args := f.where()
	var n int
	err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM materials mt"+where, args...).Scan(&n)
	return n, err
}

// MaterialStats summarizes the material layer for the CLI and the web dashboard.
type MaterialStats struct {
	Total      int            `json:"total"`
	Active     int            `json:"active"`
	Duplicates int            `json:"duplicates"`
	WithMedia  int            `json:"with_media"`
	ByKind     map[string]int `json:"by_kind"`
	ByChat     map[int64]int  `json:"by_chat"`
}

func (s *Store) MaterialStats(ctx context.Context) (MaterialStats, error) {
	st := MaterialStats{ByKind: map[string]int{}, ByChat: map[int64]int{}}
	rows, err := s.db.QueryContext(ctx, "SELECT kind,status,CASE WHEN media_count>0 THEN 1 ELSE 0 END,chat_id,COUNT(*) FROM materials GROUP BY kind,status,chat_id")
	if err != nil {
		return st, err
	}
	defer rows.Close()
	for rows.Next() {
		var kind, status string
		var hasMedia int
		var chatID int64
		var n int
		if err := rows.Scan(&kind, &status, &hasMedia, &chatID, &n); err != nil {
			return st, err
		}
		st.Total += n
		if status == "duplicate" {
			st.Duplicates += n
		} else {
			st.Active += n
		}
		if hasMedia == 1 {
			st.WithMedia += n
		}
		st.ByKind[kind] += n
		st.ByChat[chatID] += n
	}
	return st, rows.Err()
}

// MarkDuplicateMaterials flags every material whose content_hash already
// appeared earlier in the archive. The earliest copy (lowest date, then lowest
// id) stays active; later copies point at it through duplicate_of.
//
// This is the cross-post dedupe: the same picture re-posted in ten channels
// yields one active material and nine duplicates, and nothing is deleted, so a
// later review can still see where a piece of content spread.
func (s *Store) MarkDuplicateMaterials(ctx context.Context) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	// Reset first: a rebuild must be able to un-flag a material whose twin was
	// removed since the previous pass. The duplicate_of clause also catches the
	// half-written rows left by older builds, where status had been pushed back
	// to 'active' while duplicate_of kept its pointer.
	if _, err = tx.ExecContext(ctx, "UPDATE materials SET status='active',duplicate_of=NULL WHERE status='duplicate' OR duplicate_of IS NOT NULL"); err != nil {
		return 0, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,content_hash,text_hash,media_count FROM materials
WHERE content_hash<>'' ORDER BY date ASC, id ASC`)
	if err != nil {
		return 0, err
	}
	type key struct {
		hash string
		kind int // 0 = content hash, 1 = text-only hash
	}
	first := map[key]int64{}
	type pending struct {
		id    int64
		first int64
	}
	var marks []pending
	for rows.Next() {
		var id int64
		var contentHash, textHash string
		var mediaCount int
		if err := rows.Scan(&id, &contentHash, &textHash, &mediaCount); err != nil {
			rows.Close()
			return 0, err
		}
		// A text-only post is fully identified by its text; a post with media is
		// only a duplicate when the media set matches too.
		if mediaCount == 0 && textHash != "" {
			k := key{textHash, 1}
			if owner, ok := first[k]; ok {
				marks = append(marks, pending{id, owner})
				continue
			}
			first[k] = id
		}
		k := key{contentHash, 0}
		if owner, ok := first[k]; ok {
			marks = append(marks, pending{id, owner})
			continue
		}
		first[k] = id
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	for _, m := range marks {
		// No "AND duplicate_of IS NULL" guard here: the reset above already
		// cleared every pointer, and keeping the guard meant a row that had been
		// written as active-with-pointer could never be flagged again — which is
		// exactly how a library with 93 repeated content hashes reported zero
		// duplicates forever.
		if _, err = tx.ExecContext(ctx, "UPDATE materials SET status='duplicate',duplicate_of=?,updated_at=? WHERE id=?", m.first, time.Now().Unix(), m.id); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(marks), nil
}

// MaterialByRowID loads one material view; used by publish/preview paths.
func (s *Store) MaterialByRowID(ctx context.Context, rowID int64) (MaterialView, error) {
	return scanMaterialView(s.db.QueryRowContext(ctx, materialViewSelect+" WHERE mt.anchor_rowid=?", rowID))
}

// MaterialMessages returns the raw message ids composing one material.
func (s *Store) MaterialMessages(ctx context.Context, materialID int64) ([]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT m3.message_id FROM material_messages mm JOIN messages m3 ON m3.id=mm.message_rowid
WHERE mm.material_id=? ORDER BY mm.position`, materialID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// MaterialMediaPaths lists the downloaded files of a material in album order, so
// a publisher can re-upload an album instead of only its anchor.
func (s *Store) MaterialMediaPaths(ctx context.Context, materialID int64) ([]ReusableMedia, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT COALESCE(md.sha256,''),COALESCE(md.original_path,''),COALESCE(md.thumbnail_path,''),COALESCE(md.preview_path,''),COALESCE(md.mime_type,''),COALESCE(md.size,0),COALESCE(md.type,'')
FROM material_messages mm JOIN media md ON md.message_rowid=mm.message_rowid
WHERE mm.material_id=? AND md.status='completed' AND md.original_path IS NOT NULL AND md.original_path<>''
ORDER BY mm.position`, materialID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ReusableMedia
	for rows.Next() {
		var m ReusableMedia
		var kind string
		if err := rows.Scan(&m.SHA256, &m.OriginalPath, &m.ThumbnailPath, &m.PreviewPath, &m.MIME, &m.Size, &kind); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// MaterialKinds lists the distinct material kinds present in the archive, for a
// CLI/UI filter dropdown that never offers an empty option.
func (s *Store) MaterialKinds(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT kind,COUNT(*) FROM materials GROUP BY kind ORDER BY 2 DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var k string
		var n int
		if err := rows.Scan(&k, &n); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}
