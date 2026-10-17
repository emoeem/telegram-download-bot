package archive

import (
	"context"
	"testing"
	"time"
)

// A managed serve session must schedule its own maintenance: realtime updates
// only enqueue media rows, so without this loop nothing ever downloads them.
func TestManagedMaintenanceCadence(t *testing.T) {
	if MediaTick <= 0 || MediaTick > 5*time.Minute {
		t.Fatalf("MediaTick=%s: the media queue would stall in managed mode", MediaTick)
	}
	if sweepTick < MediaTick || sweepTick > 30*time.Minute {
		t.Fatalf("sweepTick=%s: out of range relative to MediaTick=%s", sweepTick, MediaTick)
	}
	if passTimeout <= 0 || mediaBatches < 1 {
		t.Fatalf("passTimeout=%s mediaBatches=%d", passTimeout, mediaBatches)
	}
}

// maintenance must never block on an unauthorized controller: an idle
// serve session has no runtime and no business holding the worker slot.
func TestMaintenanceWithoutRuntimeReturns(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := &Controller{jobs: map[int64]*Job{}}
	c.ctx, c.cancel = ctx, cancel
	c.setState(CtrlState{State: StateLoggedOut})

	busy := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		c.mediaPass(busy)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("mediaPass blocked without an authorized runtime")
	}
	if len(busy) != 0 {
		t.Fatalf("the busy token was not released: len=%d", len(busy))
	}
}

// Overlapping passes are skipped, not queued.
func TestMaintenanceSkipsOverlappingPass(t *testing.T) {
	c := &Controller{jobs: map[int64]*Job{}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.ctx, c.cancel = ctx, cancel

	busy := make(chan struct{}, 1)
	busy <- struct{}{} // pretend a pass is already running
	done := make(chan struct{})
	go func() {
		c.mediaPass(busy)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("second media pass did not return immediately")
	}
	if len(busy) != 1 {
		t.Fatalf("skipped pass consumed the busy token: len=%d", len(busy))
	}
}

func TestTaskLoopStopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	c := &Controller{jobs: map[int64]*Job{}}
	c.ctx, c.cancel = ctx, cancel

	done := make(chan struct{})
	go func() {
		c.taskLoop()
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("taskLoop did not stop when the controller context was cancelled")
	}
}

// The watchdog must not wedge the process it runs in.
func TestSuperviseLoopStopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	c := &Controller{jobs: map[int64]*Job{}}
	c.ctx, c.cancel = ctx, cancel

	done := make(chan struct{})
	go func() {
		c.superviseLoop()
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("superviseLoop did not stop when the controller context was cancelled")
	}
}

// A healthy session must not be disturbed by the watchdog.
func TestSuperviseLoopIgnoresHealthyStates(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := &Controller{jobs: map[int64]*Job{}}
	c.ctx, c.cancel = ctx, cancel
	c.setState(CtrlState{State: StateAuthorized, Authorized: true})

	if st := c.State(); st.State != StateAuthorized {
		t.Fatalf("state = %+v", st)
	}
}
