package store

import (
	"context"
	"database/sql"
	"strings"
	"time"
)

type Job struct {
	ID            int64      `json:"id"`
	Kind          string     `json:"kind"`
	TargetID      *int64     `json:"target_id,omitempty"`
	ChatID        *int64     `json:"chat_id,omitempty"`
	Status        string     `json:"status"`
	ProgressDone  int        `json:"progress_done"`
	ProgressTotal *int       `json:"progress_total,omitempty"`
	Error         string     `json:"error,omitempty"`
	StartedAt     *time.Time `json:"started_at,omitempty"`
	FinishedAt    *time.Time `json:"finished_at,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

type JobFilter struct {
	Status []string
	Kind   []string
	Limit  int
	Since  *time.Time
}

func (s *Store) CreateJob(ctx context.Context, j Job) (int64, error) {
	now := time.Now()
	var started any
	if j.StartedAt != nil {
		started = j.StartedAt.Unix()
	}
	var total any
	if j.ProgressTotal != nil {
		total = *j.ProgressTotal
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO jobs(kind,target_id,chat_id,status,progress_done,progress_total,error,started_at,finished_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		j.Kind, j.TargetID, j.ChatID, j.Status, j.ProgressDone, total, nullIfEmpty(j.Error), started, nil, now.Unix(), now.Unix())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func nullIfEmpty(v string) any {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	return v
}

func (s *Store) UpdateJobProgress(ctx context.Context, id int64, done int, total *int) error {
	var tv any
	if total != nil {
		tv = *total
	}
	_, err := s.db.ExecContext(ctx, "UPDATE jobs SET progress_done=?,progress_total=?,updated_at=? WHERE id=?", done, tv, time.Now().Unix(), id)
	return err
}

func (s *Store) FinishJob(ctx context.Context, id int64, status, errMsg string) error {
	now := time.Now().Unix()
	var finished any = now
	_, err := s.db.ExecContext(ctx, "UPDATE jobs SET status=?,error=?,finished_at=?,updated_at=? WHERE id=?", status, nullIfEmpty(errMsg), finished, now, id)
	return err
}

func (s *Store) GetJob(ctx context.Context, id int64) (Job, error) {
	return scanJob(s.db.QueryRowContext(ctx, `SELECT id,kind,target_id,chat_id,status,progress_done,progress_total,COALESCE(error,''),started_at,finished_at,created_at,updated_at FROM jobs WHERE id=?`, id))
}

func (s *Store) ListJobs(ctx context.Context, f JobFilter) ([]Job, error) {
	started := time.Now()
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	where := []string{"1=1"}
	args := []any{}
	addIn := func(col string, vals []string) {
		if len(vals) == 0 {
			return
		}
		ph := make([]string, len(vals))
		for i, v := range vals {
			ph[i] = "?"
			args = append(args, v)
		}
		where = append(where, col+" IN ("+strings.Join(ph, ",")+")")
	}
	addIn("status", f.Status)
	addIn("kind", f.Kind)
	if f.Since != nil {
		where = append(where, "created_at>=?")
		args = append(args, f.Since.Unix())
	}
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, "SELECT id,kind,target_id,chat_id,status,progress_done,progress_total,COALESCE(error,''),started_at,finished_at,created_at,updated_at FROM jobs WHERE "+strings.Join(where, " AND ")+" ORDER BY created_at DESC,id DESC LIMIT ?", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	err = rows.Err()
	logSlowQuery("ListJobs", time.Since(started), int64(len(out)))
	return out, err
}

type jobScanner interface{ Scan(...any) error }

func scanJob(row jobScanner) (Job, error) {
	var j Job
	var target, chat, started, finished, created, updated sql.NullInt64
	var total sql.NullInt64
	err := row.Scan(&j.ID, &j.Kind, &target, &chat, &j.Status, &j.ProgressDone, &total, &j.Error, &started, &finished, &created, &updated)
	if target.Valid {
		j.TargetID = &target.Int64
	}
	if chat.Valid {
		j.ChatID = &chat.Int64
	}
	if total.Valid {
		x := int(total.Int64)
		j.ProgressTotal = &x
	}
	if started.Valid {
		x := time.Unix(started.Int64, 0)
		j.StartedAt = &x
	}
	if finished.Valid {
		x := time.Unix(finished.Int64, 0)
		j.FinishedAt = &x
	}
	j.CreatedAt = time.Unix(created.Int64, 0)
	j.UpdatedAt = time.Unix(updated.Int64, 0)
	return j, err
}

func (s *Store) MarkInterruptedJobs(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, "UPDATE jobs SET status='interrupted',error='server restarted',finished_at=?,updated_at=? WHERE status IN ('queued','running')", time.Now().Unix(), time.Now().Unix())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
