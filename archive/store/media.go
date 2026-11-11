package store

import (
	"context"
	"database/sql"
	"strconv"
	"strings"
	"time"
)

// EnsureMedia queues one downloadable payload of a message.
//
// The conflict target is (message_rowid, source_url), not message_rowid: a post
// carries at most one attached file but may name several external videos, each
// of which needs its own row. Attached files keep source_url "", so they still
// collapse to a single row per message.
func (s *Store) EnsureMedia(ctx context.Context, m Message, meta MediaMeta) error {
	id, err := s.RowID(ctx, m.ChatID, m.MessageID)
	if err != nil {
		return err
	}
	now := time.Now().Unix()
	// The row and its receive number are written together: a cancel or a busy
	// database between the two statements would leave an unnumbered row that
	// only a later Open() backfill notices.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO media(message_rowid,type,file_id_unique,file_name,mime_type,size,width,height,duration,source_url,status,ref_count,created_at,updated_at)
 VALUES(?,?,?,?,?,?,?,?,?,?,'pending',1,?,?)
 ON CONFLICT(message_rowid,source_url) DO UPDATE SET
  type=excluded.type,
  file_id_unique=COALESCE(excluded.file_id_unique,media.file_id_unique),
  file_name=COALESCE(NULLIF(excluded.file_name,''),media.file_name),
  mime_type=COALESCE(NULLIF(excluded.mime_type,''),media.mime_type),
  size=COALESCE(excluded.size,media.size),
  width=COALESCE(excluded.width,media.width),
  height=COALESCE(excluded.height,media.height),
  duration=COALESCE(excluded.duration,media.duration),
  source_url=COALESCE(NULLIF(excluded.source_url,''),media.source_url),
  updated_at=excluded.updated_at`,
		id, meta.Kind, nullStr(meta.FileUniqueID), meta.Name, meta.MIME,
		nullableInt64(meta.Size), nullInt(meta.Width), nullInt(meta.Height), nullInt(meta.Duration), meta.URL, now, now)
	if err != nil {
		return err
	}
	// Number the item now, not when the bytes arrive: the receive ledger must
	// list something that is still queued, skipped or failing, otherwise "what
	// did my account get today" would silently depend on download success.
	// The UPDATE is a no-op for a row that already has a number, which keeps
	// re-ingest (edits, rescans) from renumbering anything.
	if _, err = tx.ExecContext(ctx, `UPDATE media SET serial=`+nextSerialExpr+`
 WHERE message_rowid=? AND source_url=? AND serial IS NULL`, id, meta.URL); err != nil {
		return err
	}
	return tx.Commit()
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

// mediaClaimLease is how long a claimed job may stay 'running' without a
// heartbeat before another worker is allowed to take it over. It only matters
// after a crash (a live worker refreshes it while downloading); the value is
// deliberately generous so a slow multi-gigabyte file cannot be stolen.
const mediaClaimLease = 15 * time.Minute

// ClaimPendingMedia selects up to limit due jobs and atomically marks them
// 'running' before returning them.
//
// Without a claim, two worker passes — or two processes sharing a data
// directory — can pick the same row and download it twice into the same
// staging/<id>.tmp, corrupting each other's writes. Leases make that
// impossible: a row leaves the pending set the moment it is claimed, and a
// claim whose owner disappeared is returned to the queue after the lease.
func (s *Store) ClaimPendingMedia(ctx context.Context, limit int) ([]MediaJob, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	started := time.Now()
	now := started.Unix()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	// A write first: it takes the reserved lock immediately, so the SELECT
	// below and the UPDATEs that follow cannot interleave with another
	// claimer's transaction.
	if _, err := tx.ExecContext(ctx,
		`UPDATE media SET status='pending',updated_at=? WHERE status='running' AND updated_at<=?`,
		now, started.Add(-mediaClaimLease).Unix()); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT media.id,m.chat_id,m.message_id,media.type,COALESCE(media.status,''),media.attempts,COALESCE(media.size,0),COALESCE(media.file_id_unique,''),COALESCE(media.source_url,'') FROM media JOIN messages m ON m.id=media.message_rowid WHERE media.status IN ('pending','failed') AND (media.status!='failed' OR media.next_retry_at IS NOT NULL) AND (media.next_retry_at IS NULL OR media.next_retry_at<=?) ORDER BY media.id LIMIT ?`, now, limit)
	if err != nil {
		return nil, err
	}
	var out []MediaJob
	for rows.Next() {
		var j MediaJob
		if err := rows.Scan(&j.ID, &j.ChatID, &j.MessageID, &j.Type, &j.Status, &j.Attempts, &j.Size, &j.FileUniqueID, &j.URL); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, j)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	for _, j := range out {
		if _, err := tx.ExecContext(ctx,
			`UPDATE media SET status='running',updated_at=? WHERE id=? AND status<>'running'`,
			now, j.ID); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	logSlowQuery("ClaimPendingMedia", time.Since(started), int64(len(out)))
	return out, nil
}

// TouchMediaClaim refreshes the lease of a running job. It is a no-op for a
// row that already finished, so callers do not need to track transitions.
func (s *Store) TouchMediaClaim(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE media SET updated_at=? WHERE id=? AND status='running'`,
		time.Now().Unix(), id)
	return err
}

// ReleaseMediaClaims returns jobs still marked running by this worker back to
// the pending queue. Called on a graceful exit so a shutdown does not leave
// the queue asleep until the lease expires; terminal rows are untouched.
func (s *Store) ReleaseMediaClaims(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	placeholders := make([]string, len(ids))
	args := make([]any, 0, len(ids)+1)
	args = append(args, time.Now().Unix())
	for i, id := range ids {
		placeholders[i] = "?"
		args = append(args, id)
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE media SET status='pending',updated_at=? WHERE status='running' AND id IN (`+
			strings.Join(placeholders, ",")+`)`, args...)
	return err
}

func (s *Store) PendingMedia(ctx context.Context, limit int) ([]MediaJob, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	started := time.Now()
	rows, err := s.db.QueryContext(ctx, `SELECT media.id,m.chat_id,m.message_id,media.type,COALESCE(media.status,''),media.attempts,COALESCE(media.size,0),COALESCE(media.file_id_unique,''),COALESCE(media.source_url,'') FROM media JOIN messages m ON m.id=media.message_rowid WHERE media.status IN ('pending','failed') AND (media.status!='failed' OR media.next_retry_at IS NOT NULL) AND (media.next_retry_at IS NULL OR media.next_retry_at<=?) ORDER BY media.id LIMIT ?`, time.Now().Unix(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MediaJob
	for rows.Next() {
		var j MediaJob
		if err := rows.Scan(&j.ID, &j.ChatID, &j.MessageID, &j.Type, &j.Status, &j.Attempts, &j.Size, &j.FileUniqueID, &j.URL); err != nil {
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
	// URL is set for Type=="link": the external page the video lives on. Such a
	// job has no Telegram file to locate, so it skips the history walk entirely.
	URL string
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
	// A completed file must carry a number: assign inside the same tx so no
	// committed row can be both downloaded and unnumbered.
	if err = assignSerialTx(ctx, tx, id); err != nil {
		return err
	}
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

// MarkMediaTargetMissing parks a media row whose chat has no monitor_targets
// row yet.
//
// It stays 'pending' on purpose: the wait is a backoff, not a verdict. Attempts
// are not touched (the download itself was never tried), and error_kind is set
// to target_missing so WakeTargetMedia can find the row when the target is
// finally added.
func (s *Store) MarkMediaTargetMissing(ctx context.Context, id int64, next time.Time) error {
	now := time.Now().Unix()
	_, err := s.db.ExecContext(ctx, `UPDATE media
 SET status='pending', error='chat is not a monitored target yet; retries automatically once it is added',
     error_kind='target_missing', error_detail=NULL, last_error_at=?, next_retry_at=?, updated_at=?
 WHERE id=?`, now, next.Unix(), now, id)
	return err
}

// targetMissingCond matches rows parked by MarkMediaTargetMissing, including the
// history written before that state existed: those were stored as skipped with
// error_kind='unknown' and the sql.ErrNoRows text, because the old code treated
// a missing target as a permanent failure.
const targetMissingCond = `(error_kind='target_missing' OR (status='skipped' AND error LIKE '%no rows in result set%'))`

// WakeTargetMedia re-queues every media row parked because its chat was not a
// monitored target. It is called whenever a target is added or updated, which
// makes <monitor add> and <monitor set --enable> self-healing: adding a chat
// that was archived before it was monitored picks the old downloads back up
// without a manual retry.
func (s *Store) WakeTargetMedia(ctx context.Context, chatID int64) (int64, error) {
	res, err := s.db.ExecContext(ctx, wakeTargetMediaSQL, time.Now().Unix(), chatID)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

const wakeTargetMediaSQL = `UPDATE media
 SET status='pending', error=NULL, error_kind=NULL, error_detail=NULL, next_retry_at=NULL, updated_at=?
 WHERE ` + targetMissingCond + `
   AND message_rowid IN (SELECT id FROM messages WHERE chat_id=?)`

// wakeTargetMediaTx is the same update inside a caller's transaction, so the
// target row and the un-parked downloads commit together.
func wakeTargetMediaTx(ctx context.Context, tx *sql.Tx, chatID int64) error {
	_, err := tx.ExecContext(ctx, wakeTargetMediaSQL, time.Now().Unix(), chatID)
	return err
}

// WakeAllMonitoredMedia is WakeTargetMedia across every target that currently
// exists, so a single call repairs the whole archive after an upgrade. Rows
// belonging to chats that are still unmonitored stay parked at the long
// backoff instead of being retried pointlessly.
func (s *Store) WakeAllMonitoredMedia(ctx context.Context) (int64, error) {
	now := time.Now().Unix()
	res, err := s.db.ExecContext(ctx, `UPDATE media
 SET status='pending', error=NULL, error_kind=NULL, error_detail=NULL, next_retry_at=NULL, updated_at=?
 WHERE `+targetMissingCond+`
   AND message_rowid IN (SELECT id FROM messages WHERE chat_id IN (SELECT chat_id FROM monitor_targets))`, now)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
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
