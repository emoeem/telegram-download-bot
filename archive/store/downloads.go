package store

import (
	"context"
	"time"
)

type DownloadSummary struct {
	Queued    int `json:"queued"`
	Running   int `json:"running"`
	Completed int `json:"completed"`
	Failed    int `json:"failed"`
	Paused    int `json:"paused"`
}

func (s *Store) DownloadSummary(ctx context.Context, running bool, paused bool) (DownloadSummary, error) {
	var q DownloadSummary
	err := s.db.QueryRowContext(ctx, `SELECT
COALESCE(SUM(CASE WHEN status IN ('pending','failed') AND (status!='failed' OR next_retry_at IS NOT NULL) AND (next_retry_at IS NULL OR next_retry_at<=?) THEN 1 ELSE 0 END),0),
COALESCE(SUM(CASE WHEN status='completed' THEN 1 ELSE 0 END),0),
COALESCE(SUM(CASE WHEN status='failed' THEN 1 ELSE 0 END),0)
FROM media`, time.Now().Unix()).Scan(&q.Queued, &q.Completed, &q.Failed)
	if err != nil {
		return q, err
	}
	if running {
		q.Running = 1
	}
	if paused {
		q.Paused = 1
	}
	return q, nil
}

func (s *Store) RetryFailedMedia(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE media SET status='pending',error=NULL,error_kind=NULL,error_detail=NULL,last_error_at=NULL,next_retry_at=NULL,updated_at=? WHERE status='failed' AND next_retry_at IS NOT NULL`, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
