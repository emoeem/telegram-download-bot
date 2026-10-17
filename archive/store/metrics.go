package store

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

type Metrics struct {
	Jobs   JobMetrics   `json:"jobs"`
	Media  MediaMetrics `json:"media"`
	Errors ErrorMetrics `json:"errors"`
	DB     DBMetrics    `json:"db"`
}

type JobMetrics struct {
	Running     int64 `json:"running"`
	Queued      int64 `json:"queued"`
	Completed   int64 `json:"completed"`
	Failed      int64 `json:"failed"`
	Interrupted int64 `json:"interrupted"`
}

type MediaMetrics struct {
	Pending              int64 `json:"pending"`
	Completed            int64 `json:"completed"`
	Failed               int64 `json:"failed"`
	Skipped              int64 `json:"skipped"`
	TotalBytesDownloaded int64 `json:"total_bytes_downloaded"`
}

type ErrorMetrics struct {
	FloodWait  int64 `json:"flood_wait"`
	Timeout    int64 `json:"timeout"`
	NotFound   int64 `json:"not_found"`
	Permission int64 `json:"permission"`
	DiskFull   int64 `json:"disk_full"`
	IO         int64 `json:"io"`
	Unknown    int64 `json:"unknown"`
}

type DBMetrics struct {
	MessagesTotal int64 `json:"messages_total"`
	MediaTotal    int64 `json:"media_total"`
	LinksTotal    int64 `json:"links_total"`
	EventsTotal   int64 `json:"events_total"`
	DBSizeBytes   int64 `json:"db_size_bytes"`
	WALSizeBytes  int64 `json:"wal_size_bytes"`
}

func (s *Store) MetricsSnapshot(ctx context.Context) (Metrics, error) {
	var m Metrics
	started := time.Now()
	err := s.db.QueryRowContext(ctx, `
SELECT
  (SELECT COUNT(*) FROM jobs WHERE status='running'),
  (SELECT COUNT(*) FROM jobs WHERE status='queued'),
  (SELECT COUNT(*) FROM jobs WHERE status IN ('done','completed')),
  (SELECT COUNT(*) FROM jobs WHERE status='failed'),
  (SELECT COUNT(*) FROM jobs WHERE status='interrupted'),
  (SELECT COUNT(*) FROM media WHERE status='pending'),
  (SELECT COUNT(*) FROM media WHERE status='completed'),
  (SELECT COUNT(*) FROM media WHERE status='failed'),
  (SELECT COUNT(*) FROM media WHERE status='skipped'),
  (SELECT COALESCE(SUM(size),0) FROM media WHERE status='completed'),
  (SELECT COUNT(*) FROM media),
  (SELECT COUNT(*) FROM messages),
  (SELECT COUNT(*) FROM message_links),
  (SELECT COUNT(*) FROM message_events),
  (SELECT COUNT(*) FROM media WHERE error_kind='flood_wait'),
  (SELECT COUNT(*) FROM media WHERE error_kind='timeout'),
  (SELECT COUNT(*) FROM media WHERE error_kind='not_found'),
  (SELECT COUNT(*) FROM media WHERE error_kind='permission'),
  (SELECT COUNT(*) FROM media WHERE error_kind='disk_full'),
  (SELECT COUNT(*) FROM media WHERE error_kind='io'),
  (SELECT COUNT(*) FROM media WHERE error_kind='unknown')
`).Scan(
		&m.Jobs.Running, &m.Jobs.Queued, &m.Jobs.Completed, &m.Jobs.Failed, &m.Jobs.Interrupted,
		&m.Media.Pending, &m.Media.Completed, &m.Media.Failed, &m.Media.Skipped,
		&m.Media.TotalBytesDownloaded, &m.DB.MediaTotal, &m.DB.MessagesTotal, &m.DB.LinksTotal, &m.DB.EventsTotal,
		&m.Errors.FloodWait, &m.Errors.Timeout, &m.Errors.NotFound, &m.Errors.Permission,
		&m.Errors.DiskFull, &m.Errors.IO, &m.Errors.Unknown,
	)
	if err != nil {
		return Metrics{}, err
	}
	m.DB.DBSizeBytes = fileSize(s.path)
	m.DB.WALSizeBytes = fileSize(s.path + "-wal")
	logSlowQuery("metrics snapshot", time.Since(started), 1)
	return m, nil
}

func fileSize(path string) int64 {
	fi, err := os.Stat(filepath.Clean(path))
	if err != nil {
		return 0
	}
	return fi.Size()
}

func logSlowQuery(query string, duration time.Duration, rows int64) {
	if duration > 100*time.Millisecond {
		slog.Warn("sqlite slow query",
			slog.String("query", query),
			slog.Int64("duration_ms", duration.Milliseconds()),
			slog.Int64("rows", rows),
		)
	}
}
