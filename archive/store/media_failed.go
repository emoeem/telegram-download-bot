package store

import (
	"context"
	"database/sql"
)

type FailedMediaItem struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	ErrorKind   string `json:"error_kind"`
	ErrorDetail string `json:"error_detail"`
	NextRetryAt *int64 `json:"next_retry_at,omitempty"`
}

func (s *Store) ListFailedMedia(ctx context.Context, limit int) ([]FailedMediaItem, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,COALESCE(file_name,''),COALESCE(error_kind,''),COALESCE(error_detail,error,''),next_retry_at FROM media WHERE status='failed' ORDER BY updated_at DESC,id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FailedMediaItem
	for rows.Next() {
		var x FailedMediaItem
		var next sql.NullInt64
		if err := rows.Scan(&x.ID, &x.Name, &x.ErrorKind, &x.ErrorDetail, &next); err != nil {
			return nil, err
		}
		if next.Valid {
			v := next.Int64
			x.NextRetryAt = &v
		}
		out = append(out, x)
	}
	return out, rows.Err()
}
