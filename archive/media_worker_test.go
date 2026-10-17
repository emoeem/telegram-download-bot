package archive

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/gotd/td/tgerr"
	"github.com/iyear/tdl/archive/store"
)

func mediaJobStore(t *testing.T) (*store.Store, int64) {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	if err := s.UpsertMessage(ctx, store.Message{ChatID: 1, MessageID: 1, Date: time.Unix(1, 0), Text: "img", HasMedia: true}); err != nil {
		t.Fatal(err)
	}
	row, err := s.RowID(ctx, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.DB().Exec("INSERT INTO media(message_rowid,type,status,created_at,updated_at) VALUES(?,'photo','pending',1,1)", row)
	if err != nil {
		t.Fatal(err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return s, id
}

func mediaStatus(t *testing.T, s *store.Store, id int64) (string, int) {
	t.Helper()
	var status string
	var attempts int
	if err := s.DB().QueryRow("SELECT status,attempts FROM media WHERE id=?", id).Scan(&status, &attempts); err != nil {
		t.Fatal(err)
	}
	return status, attempts
}

// A permanent failure must park the job instead of leaving it pending forever.
func TestMediaWorkerFailParksPermanentFailure(t *testing.T) {
	s, id := mediaJobStore(t)
	w := &MediaWorker{Store: s}
	ctx := context.Background()

	if err := w.fail(ctx, store.MediaJob{ID: id, Attempts: 0}, errors.New("chat gone"), true); err != nil {
		t.Fatalf("permanent failure should be swallowed: %v", err)
	}
	status, _ := mediaStatus(t, s, id)
	if status != "skipped" {
		t.Fatalf("status=%q want skipped", status)
	}
	// Skipped jobs leave the pending queue for good.
	jobs, err := s.PendingMedia(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 0 {
		t.Fatalf("parked job still queued: %+v", jobs)
	}
}

func TestMediaWorkerFailRetriesThenGivesUp(t *testing.T) {
	s, id := mediaJobStore(t)
	w := &MediaWorker{Store: s}
	ctx := context.Background()

	err := w.fail(ctx, store.MediaJob{ID: id, Attempts: 0}, errors.New("flood wait"), false)
	if err == nil {
		t.Fatal("a retryable failure must be reported to the caller")
	}
	status, attempts := mediaStatus(t, s, id)
	if status != "failed" || attempts != 1 {
		t.Fatalf("status=%q attempts=%d", status, attempts)
	}

	// The last allowed attempt parks the job rather than retrying forever.
	if err := w.fail(ctx, store.MediaJob{ID: id, Attempts: mediaMaxAttempts - 1}, errors.New("still failing"), false); err != nil {
		t.Fatalf("final attempt should be swallowed: %v", err)
	}
	status, _ = mediaStatus(t, s, id)
	if status != "skipped" {
		t.Fatalf("status=%q want skipped after %d attempts", status, mediaMaxAttempts)
	}
}

// A rate limit must also park the whole queue, not just the job: retrying the
// other jobs straight into the same limit makes the flood worse.
func TestMediaWorkerPausesQueueOnTimeout(t *testing.T) {
	s, id := mediaJobStore(t)
	w := &MediaWorker{Store: s}
	ctx := context.Background()

	_ = w.fail(ctx, store.MediaJob{ID: id, Attempts: 0}, context.DeadlineExceeded, false)
	until := s.MediaPausedUntil(ctx)
	if !until.After(time.Now().Add(floodPause - time.Minute)) {
		t.Fatalf("queue not paused long enough: %s", until)
	}
}

// A timeout tells us nothing about the file, so it must never park the job —
// even after many attempts.
func TestMediaWorkerFailOnTimeoutNeverParks(t *testing.T) {
	s, id := mediaJobStore(t)
	w := &MediaWorker{Store: s}
	ctx := context.Background()

	if err := w.fail(ctx, store.MediaJob{ID: id, Attempts: 20}, context.DeadlineExceeded, false); err == nil {
		t.Fatal("a timeout should be reported to the caller")
	}
	status, attempts := mediaStatus(t, s, id)
	if status != "failed" {
		t.Fatalf("timeout parked the job: status=%q", status)
	}
	if attempts != 21 {
		t.Fatalf("attempts=%d want 21 (backoff must keep growing)", attempts)
	}
}

// The failure that matters most is the one caused by our own timeout: the job
// context is already canceled, so the state write must not use it.
func TestMediaWorkerFailWritesWithExpiredContext(t *testing.T) {
	s, id := mediaJobStore(t)
	w := &MediaWorker{Store: s}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // exactly the state a job is in when it times out

	if err := w.fail(ctx, store.MediaJob{ID: id, Attempts: 0}, errors.New("context deadline exceeded"), false); err == nil {
		t.Fatal("a retryable failure must be reported to the caller")
	}
	status, attempts := mediaStatus(t, s, id)
	if status != "failed" || attempts != 1 {
		t.Fatalf("expired-context write was dropped: status=%q attempts=%d", status, attempts)
	}
}

// A rate limit says nothing about the file being unavailable, so it must never
// be the reason a job is parked for good.
func TestMediaWorkerFailOnFloodWaitNeverGivesUp(t *testing.T) {
	s, id := mediaJobStore(t)
	w := &MediaWorker{Store: s}
	ctx := context.Background()

	waitErr := tgerr.New(420, "FLOOD_WAIT_60")
	if _, ok := tgerr.AsFloodWait(waitErr); !ok {
		t.Fatalf("test setup: %v is not recognised as a flood wait", waitErr)
	}
	// Even on the very last allowed attempt the job stays retryable.
	if err := w.fail(ctx, store.MediaJob{ID: id, Attempts: mediaMaxAttempts - 1}, waitErr, false); err == nil {
		t.Fatal("flood wait should be reported to the caller")
	}
	status, attempts := mediaStatus(t, s, id)
	if status != "failed" {
		t.Fatalf("flood wait parked the job: status=%q", status)
	}
	if attempts != mediaMaxAttempts {
		t.Fatalf("flood wait should only advance the backoff, attempts=%d", attempts)
	}
	var next sql.NullInt64
	if err := s.DB().QueryRow("SELECT next_retry_at FROM media WHERE id=?", id).Scan(&next); err != nil {
		t.Fatal(err)
	}
	if !next.Valid || next.Int64 <= time.Now().Unix() {
		t.Fatalf("no future retry scheduled: %+v", next)
	}
}

// DrainMediaQueue must honor the global pause gate and stop as soon as the
// queue is empty — the same contract the managed serve session and `archive
// run` both rely on.
func TestDrainMediaQueue(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	passes := 0
	n, err := DrainMediaQueue(ctx, s, 5, func(ctx context.Context) error {
		passes++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 || passes != 1 {
		t.Fatalf("empty queue ran %d passes", n)
	}

	// A pending job makes the loop keep going until batches is exhausted.
	if err := s.UpsertMessage(ctx, store.Message{ChatID: 1, MessageID: 1, Date: time.Unix(1, 0), Text: "m"}); err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureMedia(ctx, store.Message{ChatID: 1, MessageID: 1, Date: time.Unix(1, 0)}, store.MediaMeta{Kind: "photo", FileUniqueID: "tg-photo-1"}); err != nil {
		t.Fatal(err)
	}
	passes = 0
	n, err = DrainMediaQueue(ctx, s, 3, func(ctx context.Context) error {
		passes++
		// Consume the queue on the first pass like a real worker would.
		js, _ := s.PendingMedia(ctx, 10)
		for _, j := range js {
			_ = s.MarkMediaSkipped(ctx, j.ID, "test")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 || passes != 1 {
		t.Fatalf("queue drained in %d passes, want 1", n)
	}

	// The pause gate set by a flood wait must short-circuit the drain.
	if err := s.UpsertMessage(ctx, store.Message{ChatID: 1, MessageID: 2, Date: time.Unix(2, 0), Text: "m2"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetMediaPausedUntil(ctx, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureMedia(ctx, store.Message{ChatID: 1, MessageID: 2, Date: time.Unix(2, 0)}, store.MediaMeta{Kind: "photo", FileUniqueID: "tg-photo-2"}); err != nil {
		t.Fatal(err)
	}
	passes = 0
	n, err = DrainMediaQueue(ctx, s, 3, func(ctx context.Context) error {
		passes++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 || passes != 0 {
		t.Fatalf("paused queue ran %d passes", n)
	}
}
