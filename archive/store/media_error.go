package store

import (
	"context"
	"database/sql"
)

type MediaError struct {
	Kind        string
	Detail      string
	NextRetryAt *int64
}

func (s *Store) MediaErrorsByRowIDs(ctx context.Context, ids []int64) (map[int64]MediaError, error) {
	out := make(map[int64]MediaError)
	if len(ids) == 0 {
		return out, nil
	}
	args := make([]any, 0, len(ids))
	ph := make([]byte, 0, len(ids)*2)
	for i, id := range ids {
		if i > 0 {
			ph = append(ph, ',')
		}
		ph = append(ph, '?')
		args = append(args, id)
	}
	rows, err := s.db.QueryContext(ctx, "SELECT message_rowid,COALESCE(error_kind,''),COALESCE(error_detail,''),CASE WHEN status='failed' THEN next_retry_at ELSE NULL END FROM media WHERE message_rowid IN ("+string(ph)+")", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var e MediaError
		var next sql.NullInt64
		if err := rows.Scan(&id, &e.Kind, &e.Detail, &next); err != nil {
			return nil, err
		}
		if next.Valid {
			v := next.Int64
			e.NextRetryAt = &v
		}
		out[id] = e
	}
	return out, rows.Err()
}
