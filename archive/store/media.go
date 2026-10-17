package store

import (
	"context"
	"database/sql"
	"strconv"
	"time"
)

func (s *Store) EnsureMedia(ctx context.Context, m Message, meta MediaMeta) error {
	id, err := s.RowID(ctx, m.ChatID, m.MessageID)
	if err != nil {
		return err
	}
	now := time.Now().Unix()
	_, err = s.db.ExecContext(ctx, `INSERT INTO media(message_rowid,type,file_id_unique,file_name,mime_type,size,width,height,duration,status,ref_count,created_at,updated_at)
 VALUES(?,?,?,?,?,?,?,?,?,'pending',1,?,?)
 ON CONFLICT(message_rowid) DO UPDATE SET
  type=excluded.type,
  file_id_unique=COALESCE(excluded.file_id_unique,media.file_id_unique),
  file_name=COALESCE(NULLIF(excluded.file_name,''),media.file_name),
  mime_type=COALESCE(NULLIF(excluded.mime_type,''),media.mime_type),
  size=COALESCE(excluded.size,media.size),
  width=COALESCE(excluded.width,media.width),
  height=COALESCE(excluded.height,media.height),
  duration=COALESCE(excluded.duration,media.duration),
  updated_at=excluded.updated_at`,
		id, meta.Kind, nullStr(meta.FileUniqueID), meta.Name, meta.MIME,
		nullableInt64(meta.Size), nullInt(meta.Width), nullInt(meta.Height), nullInt(meta.Duration), now, now)
	return err
}

// nullStr stores an empty string as NULL so COALESCE-based merge in the
// upsert above keeps whatever a previous, richer update already recorded.
func nullStr(v string) any {
	if v == "" {
		return nil
	}
	return v
}

func nullInt(v int) any {
	if v == 0 {
		return nil
	}
	return v
}

// mediaPauseKey is the archive_settings key holding the timestamp until which
// the media queue stays idle after hitting a server rate limit.
const mediaPauseKey = "media_paused_until"

// SetMediaPausedUntil parks the media queue until t (see MediaPausedUntil).
func (s *Store) SetMediaPausedUntil(ctx context.Context, t time.Time) error {
	return s.SetSetting(ctx, mediaPauseKey, strconv.FormatInt(t.Unix(), 10))
}

// MediaPausedUntil reports the timestamp until which the media queue is paused;
// the zero time means "not paused".
func (s *Store) MediaPausedUntil(ctx context.Context) time.Time {
	v, err := s.Setting(ctx, mediaPauseKey)
	if err != nil || v == "" {
		return time.Time{}
	}
	ts, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.Unix(ts, 0)
}

// FilesMime returns the stored mime type of a content-addressed object.
func (s *Store) FilesMime(ctx context.Context, sha string) string {
	var mime string
	_ = s.db.QueryRowContext(ctx, `SELECT mime_type FROM files WHERE sha256=?`, sha).Scan(&mime)
	return mime
}

func (s *Store) PendingMedia(ctx context.Context, limit int) ([]MediaJob, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	started := time.Now()
	rows, err := s.db.QueryContext(ctx, `SELECT media.id,m.chat_id,m.message_id,media.type,COALESCE(media.status,''),media.attempts,COALESCE(media.size,0),COALESCE(media.file_id_unique,'') FROM media JOIN messages m ON m.id=media.message_rowid WHERE media.status IN ('pending','failed') AND (media.status!='failed' OR media.next_retry_at IS NOT NULL) AND (media.next_retry_at IS NULL OR media.next_retry_at<=?) ORDER BY media.id LIMIT ?`, time.Now().Unix(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MediaJob
	for rows.Next() {
		var j MediaJob
		if err := rows.Scan(&j.ID, &j.ChatID, &j.MessageID, &j.Type, &j.Status, &j.Attempts, &j.Size, &j.FileUniqueID); err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	err = rows.Err()
	logSlowQuery("PendingMedia", time.Since(started), int64(len(out)))
	return out, err
}

type MediaJob struct {
	ID, ChatID   int64
	MessageID    int
	Type, Status string
	Attempts     int
	Size         int64
	FileUniqueID string
}

// MarkMediaRetryAfter schedules a retry at an explicit time. It is used for
// server rate limits, where the backoff is dictated by Telegram rather than by
// our own attempt counter.
func (s *Store) MarkMediaRetryAfter(ctx context.Context, id int64, errText, kind, detail string, attempts int, next *time.Time) error {
	now := time.Now().Unix()
	var retry any
	if next != nil {
		retry = next.Unix()
	}
	_, err := s.db.ExecContext(ctx, "UPDATE media SET status='failed',error=?,error_kind=?,error_detail=?,last_error_at=?,attempts=?,next_retry_at=?,updated_at=? WHERE id=?", errText, kind, detail, now, attempts, retry, now, id)
	return err
}

func (s *Store) MarkMediaFailed(ctx context.Context, id int64, errText, kind, detail string, attempts int, next time.Time) error {
	return s.MarkMediaRetryAfter(ctx, id, errText, kind, detail, attempts, &next)
}

func (s *Store) MarkMediaTerminal(ctx context.Context, id int64, errText, kind, detail string, attempts int) error {
	return s.MarkMediaRetryAfter(ctx, id, errText, kind, detail, attempts, nil)
}
func (s *Store) MarkMediaCompleted(ctx context.Context, id int64, sha string, size int64, mime string, originalPath, thumbnailPath, previewPath string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().Unix()
	if _, err = tx.ExecContext(ctx, "UPDATE media SET status='completed',sha256=?,original_path=?,thumbnail_path=?,preview_path=?,size=?,mime_type=?,downloaded_at=?,updated_at=?,error=NULL,error_kind=NULL,error_detail=NULL,last_error_at=NULL,next_retry_at=NULL WHERE id=?", sha, originalPath, thumbnailPath, previewPath, size, mime, now, now, id); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO files(sha256,size,mime_type,state,ref_count,created_at) VALUES(?,?,?,'ready',1,?) ON CONFLICT(sha256) DO UPDATE SET ref_count=files.ref_count+1", sha, size, mime, now); err != nil {
		return err
	}
	return tx.Commit()
}

type ReusableMedia struct {
	SHA256, OriginalPath, ThumbnailPath, PreviewPath, MIME string
	Size                                                   int64
}

func (s *Store) FindReusableMedia(ctx context.Context, fileID string) (ReusableMedia, bool, error) {
	if fileID == "" {
		return ReusableMedia{}, false, nil
	}
	var m ReusableMedia
	var sha, original, thumb, preview, mime string
	err := s.db.QueryRowContext(ctx, `SELECT m.sha256,COALESCE(m.original_path,''),COALESCE(m.thumbnail_path,''),COALESCE(m.preview_path,''),COALESCE(m.mime_type,''),COALESCE(m.size,0)
FROM media m JOIN files f ON f.sha256=m.sha256
WHERE m.file_id_unique=? AND m.status='completed' AND m.sha256 IS NOT NULL AND f.state='ready' LIMIT 1`, fileID).Scan(&sha, &original, &thumb, &preview, &mime, &m.Size)
	if err == sql.ErrNoRows {
		return ReusableMedia{}, false, nil
	}
	if err != nil {
		return ReusableMedia{}, false, err
	}
	m.SHA256, m.OriginalPath, m.ThumbnailPath, m.PreviewPath, m.MIME = sha, original, thumb, preview, mime
	return m, true, nil
}

func (s *Store) MarkMediaReused(ctx context.Context, id int64, r ReusableMedia) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().Unix()
	if _, err = tx.ExecContext(ctx, "UPDATE media SET status='completed',sha256=?,original_path=?,thumbnail_path=?,preview_path=?,size=?,mime_type=?,downloaded_at=?,updated_at=?,error=NULL,error_kind=NULL,error_detail=NULL,last_error_at=NULL,next_retry_at=NULL WHERE id=?", r.SHA256, r.OriginalPath, r.ThumbnailPath, r.PreviewPath, r.Size, r.MIME, now, now, id); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE files SET ref_count=ref_count+1 WHERE sha256=?", r.SHA256); err != nil {
		return err
	}
	return tx.Commit()
}
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
func (s *Store) MarkMediaSkipped(ctx context.Context, id int64, reason string) error {
	_, err := s.db.ExecContext(ctx, "UPDATE media SET status='skipped',error=?,updated_at=? WHERE id=?", reason, time.Now().Unix(), id)
	return err
}

func (s *Store) MarkMediaSkippedWithError(ctx context.Context, id int64, reason, kind, detail string, attempts int) error {
	now := time.Now().Unix()
	_, err := s.db.ExecContext(ctx, "UPDATE media SET status='skipped',error=?,error_kind=?,error_detail=?,last_error_at=?,attempts=?,next_retry_at=NULL,updated_at=? WHERE id=?", reason, kind, detail, now, attempts, now, id)
	return err
}
func (s *Store) RetryMedia(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, "UPDATE media SET status='pending',error=NULL,next_retry_at=NULL,updated_at=? WHERE id=?", time.Now().Unix(), id)
	return err
}

func (s *Store) MediaVariant(ctx context.Context, sha, variant string) (string, error) {
	var p string
	col := "original_path"
	if variant == "thumb" {
		col = "thumbnail_path"
	} else if variant == "preview" {
		col = "preview_path"
	}
	err := s.db.QueryRowContext(ctx, "SELECT COALESCE("+col+",'') FROM media WHERE sha256=? LIMIT 1", sha).Scan(&p)
	return p, err
}

// OrphanFile is a content-addressed object that no completed media row points at
// any more: the archive can safely reclaim it.
type OrphanFile struct {
	SHA256 string
	Size   int64
	MIME   string
}

// OrphanFiles lists files-table rows with no completed media referencing them.
func (s *Store) OrphanFiles(ctx context.Context) ([]OrphanFile, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT f.sha256,COALESCE(f.size,0),COALESCE(f.mime_type,'') FROM files f
WHERE NOT EXISTS (SELECT 1 FROM media m WHERE m.sha256=f.sha256 AND m.status='completed') ORDER BY f.sha256`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []OrphanFile{}
	for rows.Next() {
		var f OrphanFile
		if err := rows.Scan(&f.SHA256, &f.Size, &f.MIME); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// DeleteFileRow drops a physical-object registration row after its object has
// been reclaimed.
func (s *Store) DeleteFileRow(ctx context.Context, sha string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM files WHERE sha256=?", sha)
	return err
}

// CompletedMedia lists the sha256 of every completed object so a repair pass can
// verify the files are actually on disk.
func (s *Store) CompletedMedia(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT DISTINCT sha256 FROM media WHERE status='completed' AND sha256 IS NOT NULL")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var sha string
		if err := rows.Scan(&sha); err != nil {
			return nil, err
		}
		out = append(out, sha)
	}
	return out, rows.Err()
}

// ResetMediaBySHA queues the media rows of a lost object for re-download.
func (s *Store) ResetMediaBySHA(ctx context.Context, sha string) (int64, error) {
	res, err := s.db.ExecContext(ctx, "UPDATE media SET status='pending',next_retry_at=NULL,attempts=0,error='object missing, re-queued',updated_at=? WHERE sha256=? AND status='completed'", time.Now().Unix(), sha)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
