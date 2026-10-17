package archive

import (
	"context"
	"fmt"
	"os"

	"github.com/gotd/td/telegram/peers"
	"github.com/gotd/td/tg"
	"github.com/iyear/tdl/archive/store"
)

// statsRefreshWindow is how many newest archived messages one stats refresh
// covers per chat. One getMessagesViews call answers all of them, so the cost
// is one request per chat per sweep regardless of window size.
const statsRefreshWindow = 200

// RefreshChatStats re-reads view/forward counters for a chat's newest archived
// messages via messages.getMessagesViews. Only channel-like peers (broadcast
// channels and supergroups) expose those counters — other kinds are a no-op.
// The server answers with one row per requested id, in request order.
func RefreshChatStats(ctx context.Context, rt *Runtime, s *store.Store, chatID int64) (int, error) {
	p, err := ResolveChatID(ctx, rt, chatID)
	if err != nil {
		return 0, err
	}
	ch, ok := p.(peers.Channel)
	if !ok || (!ch.IsBroadcast() && !ch.IsSupergroup()) {
		return 0, nil
	}
	ids, err := s.RecentMessageIDs(ctx, chatID, statsRefreshWindow)
	if err != nil || len(ids) == 0 {
		return 0, err
	}
	res, err := rt.API.MessagesGetMessagesViews(ctx, &tg.MessagesGetMessagesViewsRequest{
		Peer:      ch.InputPeer(),
		ID:        ids,
		Increment: false,
	})
	if err != nil {
		return 0, err
	}
	rows := make([]store.MessageViewStatsRow, 0, len(res.Views))
	for i, v := range res.Views {
		if i >= len(ids) {
			break
		}
		rows = append(rows, store.MessageViewStatsRow{MessageID: ids[i], Views: v.Views, Forwards: v.Forwards})
	}
	return s.ApplyMessageViews(ctx, chatID, rows)
}

// RefreshStatsTargets refreshes view counters for every enabled target. It is
// wired into the same periodic sweep as the gap rescan in both `archive run`
// and the managed serve session, so view counters never go stale even when
// the realtime stream missed the individual updates.
func RefreshStatsTargets(ctx context.Context, s *store.Store, rt *Runtime) {
	ts, err := s.Targets(ctx)
	if err != nil {
		return
	}
	for _, t := range ts {
		if !t.Enabled {
			continue
		}
		if _, err := RefreshChatStats(ctx, rt, s, t.ChatID); err != nil && ctx.Err() == nil {
			fmt.Fprintf(os.Stderr, "archive stats %d: %v\n", t.ChatID, err)
		}
	}
}
