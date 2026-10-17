package archive

import (
	"context"
	"fmt"
	"os"

	"github.com/iyear/tdl/archive/store"
)

// RescanTargets runs the periodic gap sweep over every enabled target. It is the
// single implementation shared by `archive run` and the managed serve session so
// both close "missed while offline" gaps the same way:
//
//  1. compare the newest archived id with the server's newest id;
//  2. if the archive trails, backfill exactly the missing tail (bounded);
//  3. otherwise do the routine newest-N rescan, which also rewrites the
//     last_msg_id / backlog watermarks.
//
// All three write paths are idempotent upserts, so running this concurrently
// with realtime updates is safe.
func RescanTargets(ctx context.Context, s *store.Store, rt *Runtime, col *Collector) {
	ts, err := s.Targets(ctx)
	if err != nil {
		return
	}
	for _, t := range ts {
		if !t.Enabled {
			continue
		}
		if g, err := col.CheckGap(ctx, rt, t); err == nil && g.Missing() > 0 {
			fmt.Fprintf(os.Stderr, "archive gap %d: %d message(s) behind, closing\n", t.ChatID, g.Missing())
			if _, err := col.CloseGap(ctx, rt, t, g); err != nil {
				fmt.Fprintf(os.Stderr, "archive gap %d: %v\n", t.ChatID, err)
			}
			continue
		}
		if _, err := col.Backfill(ctx, rt, t, BackfillOptions{Limit: rescanBatch}); err != nil {
			fmt.Fprintf(os.Stderr, "archive rescan %d: %v\n", t.ChatID, err)
		}
	}
}
