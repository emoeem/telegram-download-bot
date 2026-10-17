package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestCreateAndFinishJob(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now()
	total := 10
	id, err := s.CreateJob(context.Background(), Job{Kind: "backfill", Status: "running", ProgressTotal: &total, StartedAt: &now})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateJobProgress(context.Background(), id, 4, &total); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishJob(context.Background(), id, "done", ""); err != nil {
		t.Fatal(err)
	}
	j, err := s.GetJob(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if j.Status != "done" || j.ProgressDone != 4 || j.FinishedAt == nil {
		t.Fatalf("job=%+v", j)
	}
}

func TestListJobsByStatus(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	for _, st := range []string{"running", "done", "failed"} {
		if _, err := s.CreateJob(ctx, Job{Kind: "sync", Status: st}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.ListJobs(ctx, JobFilter{Status: []string{"failed", "running"}, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("jobs=%d", len(got))
	}
}

func TestJobInterruptedOnRestart(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	id, err := s.CreateJob(ctx, Job{Kind: "backfill", Status: "running"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateJob(ctx, Job{Kind: "sync", Status: "queued"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkInterruptedJobs(ctx); err != nil {
		t.Fatal(err)
	}
	j, err := s.GetJob(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if j.Status != "interrupted" || j.Error != "server restarted" {
		t.Fatalf("job=%+v", j)
	}
}
