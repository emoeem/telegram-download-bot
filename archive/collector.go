package archive

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/gotd/td/telegram/query"
	"github.com/gotd/td/tg"
	"github.com/iyear/tdl/archive/store"
)

type Collector struct {
	Store    *store.Store
	Exporter *Exporter
	// Notifier, when set, receives every realtime-archived message that passed
	// Accept; notification is best-effort and never fails the archive write.
	Notifier *Notifier
	// AutoClick, when set, presses inline buttons / opens t.me links on
	// auto-click enabled targets and captures the bot's response.
	AutoClick *AutoClicker
}

func NewCollector(s *store.Store) *Collector { return &Collector{Store: s} }
func (c *Collector) Accept(t store.Target, m store.Message) bool {
	if !t.Enabled {
		return false
	}
	text := strings.ToLower(m.Text + " " + m.Caption)
	if len(t.Keywords) > 0 {
		ok := false
		for _, k := range t.Keywords {
			if strings.Contains(text, strings.ToLower(k)) {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	for _, k := range t.ExcludeKeywords {
		if strings.Contains(text, strings.ToLower(k)) {
			return false
		}
	}
	if !t.ArchiveText && !m.HasMedia {
		return false
	}
	return true
}

// gapBackfillCap bounds one gap-closing backfill so a very stale archive cannot
// trigger an unbounded history walk in a single sweep.
const gapBackfillCap = 2000

// rescanBatch is how many newest messages the routine sweep re-reads per target
// when the archive is already caught up.
const rescanBatch = 200

type BackfillOptions struct {
	Limit    int
	Since    *time.Time
	Resume   bool
	Progress func(done int)
}

func (c *Collector) Backfill(ctx context.Context, rt *Runtime, t store.Target, o BackfillOptions) (int, error) {
	// Resolve by the stored chat id: private chats have no username, and the
	// peers cache (backed by the archive KV store) knows the access hash.
	p, err := ResolveChatID(ctx, rt, t.ChatID)
	if err != nil && t.Username != "" {
		if p2, e2 := Resolve(ctx, rt, t.Username); e2 == nil {
			p, err = p2, nil
		}
	}
	if err != nil {
		return 0, err
	}
	offset := 0
	if o.Resume {
		// Prefer the persisted backlog watermark: unlike MIN(message_id) it
		// survives the oldest rows being pruned and records that a deep walk is
		// in progress, so an interrupted resume continues where it stopped.
		cur, _ := c.Store.SyncCursor(ctx, 0, t.ChatID)
		offset = cur.BacklogOldestMsgID
		if offset == 0 {
			offset, _ = c.Store.FirstMessageID(ctx, t.ChatID)
		}
		if offset > 0 {
			offset--
		}
	}
	state := store.BacklogDone
	if o.Resume {
		state = store.BacklogRunning
	}
	record := func(err error) {
		st := state
		if err != nil {
			st = store.BacklogPending
		}
		c.recordSyncState(ctx, t.ChatID, st)
	}
	if o.Since == nil {
		o.Since = t.Since
	}
	it := query.Messages(rt.API).GetHistory(p.InputPeer()).BatchSize(100).OffsetID(offset).Iter()
	n := 0
	var pendingGroup []store.Message
	var pendingGroupID int64
	flushGroup := func() error {
		if len(pendingGroup) == 0 || c.Exporter == nil {
			pendingGroup = nil
			pendingGroupID = 0
			return nil
		}
		err := c.Exporter.channel.ExportMessages(ctx, pendingGroup)
		pendingGroup = nil
		pendingGroupID = 0
		return err
	}
	for it.Next(ctx) {
		if service, ok := it.Value().Msg.(*tg.MessageService); ok {
			if err := c.Store.UpsertEntities(ctx, tg.Entities{Users: it.Value().Entities.Users(), Chats: it.Value().Entities.Chats(), Channels: it.Value().Entities.Channels()}); err != nil {
				return n, err
			}
			e := tg.Entities{Users: it.Value().Entities.Users(), Chats: it.Value().Entities.Chats(), Channels: it.Value().Entities.Channels()}
			if err := c.SaveService(ctx, e, service); err != nil {
				return n, err
			}
			n++
			if o.Progress != nil {
				o.Progress(n)
			}
			if o.Limit > 0 && n >= o.Limit {
				break
			}
			continue
		}
		msg, ok := it.Value().Msg.(*tg.Message)
		if !ok {
			continue
		}
		if o.Since != nil && time.Unix(int64(msg.Date), 0).Before(*o.Since) {
			if err := flushGroup(); err != nil {
				return n, err
			}
			return n, it.Err()
		}
		m := store.FromTG(msg)
		e := tg.Entities{Users: it.Value().Entities.Users(), Chats: it.Value().Entities.Chats(), Channels: it.Value().Entities.Channels()}
		if err := c.Store.UpsertEntities(ctx, e); err != nil {
			return n, err
		}
		if m.ChatID == 0 {
			m.ChatID = p.ID()
		}
		if !c.Accept(t, m) {
			continue
		}
		if err := c.Store.UpsertMessage(ctx, m); err != nil {
			return n, err
		}
		if err := c.Store.RefreshMetadata(ctx, m); err != nil {
			return n, err
		}
		if meta, ok := store.MediaInfo(msg); ok && t.ArchiveMedia && (t.DownloadMedia || t.ArchiveMode == "both") {
			// A failed media-row write must not be silently swallowed: otherwise
			// the message is archived but its download can never be discovered by
			// the media queue, creating a permanent data-integrity hole.
			if err := c.Store.EnsureMedia(ctx, m, meta); err != nil {
				return n, err
			}
		}
		if err := c.ensureLinkMedia(ctx, m, msg, t); err != nil {
			return n, err
		}
		if c.Exporter != nil {
			if m.GroupedID != 0 {
				if pendingGroupID != 0 && pendingGroupID != m.GroupedID {
					if err := flushGroup(); err != nil {
						return n, err
					}
				}
				pendingGroupID = m.GroupedID
				pendingGroup = append(pendingGroup, m)
			} else {
				if err := flushGroup(); err != nil {
					return n, err
				}
				if err := c.Exporter.ExportMessage(ctx, m); err != nil {
					return n, err
				}
			}
		}
		n++
		if o.Progress != nil {
			o.Progress(n)
		}
		if o.Limit > 0 && n >= o.Limit {
			break
		}
	}
	if err := flushGroup(); err != nil {
		record(err)
		return n, err
	}
	if err := it.Err(); err != nil {
		record(err)
		return n, err
	}
	record(nil)
	return n, nil
}

// recordSyncState persists how far this chat has been archived: the newest and
// oldest archived ids plus the chat's last_msg_id watermark used by the gap
// check. It is best-effort — archiving must not fail because a watermark write
// failed.
func (c *Collector) recordSyncState(ctx context.Context, chatID int64, backlogState string) {
	oldest, newest, err := c.Store.MessageIDRange(ctx, chatID)
	if err != nil {
		return
	}
	_ = c.Store.SetChatLastMsgID(ctx, chatID, newest)
	_ = c.Store.SaveSyncCursor(ctx, store.SyncCursor{
		AccountID:          0,
		ChatID:             chatID,
		BacklogState:       backlogState,
		BacklogOldestMsgID: oldest,
		LastMsgID:          newest,
		LastSeenAt:         time.Now().Unix(),
	})
}

// GapReport describes how far the local archive trails the server.
type GapReport struct {
	ChatID       int64
	LocalNewest  int
	RemoteNewest int
}

// Missing reports the number of message ids the archive appears to be behind.
func (g GapReport) Missing() int {
	if g.RemoteNewest <= g.LocalNewest {
		return 0
	}
	return g.RemoteNewest - g.LocalNewest
}

// ServerNewestMessageID asks the server for the newest message id of a chat.
// One message is fetched, so this is cheap enough to run on every sweep.
func (c *Collector) ServerNewestMessageID(ctx context.Context, rt *Runtime, t store.Target) (int, error) {
	p, err := ResolveChatID(ctx, rt, t.ChatID)
	if err != nil && t.Username != "" {
		if p2, e2 := Resolve(ctx, rt, t.Username); e2 == nil {
			p, err = p2, nil
		}
	}
	if err != nil {
		return 0, err
	}
	it := query.Messages(rt.API).GetHistory(p.InputPeer()).BatchSize(1).Iter()
	if !it.Next(ctx) {
		return 0, it.Err()
	}
	return it.Value().Msg.GetID(), it.Err()
}

// CheckGap compares the newest archived id with the server's newest id. This is
// the "did we miss anything while offline" check from the design (§8): it closes
// the tail gap that realtime updates cannot replay.
func (c *Collector) CheckGap(ctx context.Context, rt *Runtime, t store.Target) (GapReport, error) {
	local := 0
	if _, newest, err := c.Store.MessageIDRange(ctx, t.ChatID); err == nil {
		local = newest
	}
	remote, err := c.ServerNewestMessageID(ctx, rt, t)
	if err != nil {
		return GapReport{ChatID: t.ChatID, LocalNewest: local}, err
	}
	return GapReport{ChatID: t.ChatID, LocalNewest: local, RemoteNewest: remote}, nil
}

// CloseGap backfills just enough newest history to reach the server's newest id.
func (c *Collector) CloseGap(ctx context.Context, rt *Runtime, t store.Target, g GapReport) (int, error) {
	if g.Missing() <= 0 {
		return 0, nil
	}
	limit := g.Missing() + 20
	if limit > gapBackfillCap {
		limit = gapBackfillCap
	}
	return c.Backfill(ctx, rt, t, BackfillOptions{Limit: limit})
}
func (c *Collector) SaveUpdateGroup(ctx context.Context, e tg.Entities, messages []*tg.Message) error {
	if len(messages) == 0 {
		return nil
	}
	if err := c.Store.UpsertEntities(ctx, e); err != nil {
		return err
	}
	accepted := make([]store.Message, 0, len(messages))
	// groupItem pairs each parsed message with its target so the auto-click
	// pass does not parse (and re-marshal the raw JSON) a second time.
	type groupItem struct {
		m        *tg.Message
		x        store.Message
		t        store.Target
		accepted bool
	}
	items := make([]groupItem, 0, len(messages))
	for _, m := range messages {
		x := store.FromTG(m)
		if x.ChatID == 0 {
			continue
		}
		item := groupItem{m: m, x: x}
		t, ok, terr := c.target(ctx, x.ChatID)
		if terr != nil {
			return terr
		}
		if ok && c.Accept(t, x) {
			item.t, item.accepted = t, true
			if err := c.Store.UpsertMessage(ctx, x); err != nil {
				return err
			}
			if err := c.Store.RefreshMetadata(ctx, x); err != nil {
				return err
			}
			if meta, ok := store.MediaInfo(m); ok && t.ArchiveMedia && (t.DownloadMedia || t.ArchiveMode == "both") {
				if err := c.Store.EnsureMedia(ctx, x, meta); err != nil {
					return err
				}
			}
			if err := c.ensureLinkMedia(ctx, x, m, t); err != nil {
				return err
			}
			accepted = append(accepted, x)
		}
		items = append(items, item)
	}
	if c.Notifier != nil {
		for _, x := range accepted {
			c.Notifier.DispatchAsync(x)
		}
	}
	if c.AutoClick != nil {
		for _, it := range items {
			if it.m.Out {
				continue
			}
			// Response capture listens on every message in the window (even
			// ones Accept would exclude); clicking stays limited to accepted
			// messages so keyword filters are honored.
			if c.AutoClick.Observe(ctx, it.x) {
				continue
			}
			if !it.accepted {
				continue
			}
			c.AutoClick.OnNewMessage(ctx, it.t, it.x, it.m)
		}
	}
	if c.Exporter == nil || len(accepted) == 0 {
		return nil
	}
	if len(accepted) == 1 {
		logExportFailure(accepted[0].ChatID, accepted[0].MessageID, c.Exporter.ExportMessage(ctx, accepted[0]))
		return nil
	}
	logExportFailure(accepted[0].ChatID, accepted[0].MessageID, c.Exporter.channel.ExportMessages(ctx, accepted))
	return nil
}

func (c *Collector) SaveUpdate(ctx context.Context, e tg.Entities, m *tg.Message) error {
	if err := c.Store.UpsertEntities(ctx, e); err != nil {
		return err
	}
	x := store.FromTG(m)
	if x.ChatID == 0 {
		return nil
	}
	// A message inside an open response-capture window is the outcome of a
	// click we made; forward it and do not click on it again.
	var captured bool
	if c.AutoClick != nil && !m.Out {
		captured = c.AutoClick.Observe(ctx, x)
	}
	t, ok, err := c.target(ctx, x.ChatID)
	if err != nil {
		return err
	}
	if !ok || !c.Accept(t, x) {
		return nil
	}
	if err := c.Store.UpsertMessage(ctx, x); err != nil {
		return err
	}
	if err := c.Store.RefreshMetadata(ctx, x); err != nil {
		return err
	}
	if meta, ok := store.MediaInfo(m); ok && t.ArchiveMedia && (t.DownloadMedia || t.ArchiveMode == "both") {
		if err := c.Store.EnsureMedia(ctx, x, meta); err != nil {
			return err
		}
	}
	if err := c.ensureLinkMedia(ctx, x, m, t); err != nil {
		return err
	}
	if c.Notifier != nil {
		c.Notifier.DispatchAsync(x)
	}
	if c.AutoClick != nil && !m.Out && !captured {
		c.AutoClick.OnNewMessage(ctx, t, x, m)
	}
	if c.Exporter != nil {
		logExportFailure(x.ChatID, x.MessageID, c.Exporter.Export(ctx, m, x.ChatID))
	}
	return nil
}

// SaveReactions applies a realtime updateMessageReactions delivery. The update
// carries the full aggregate counter set, so this is a plain rewrite.
func (c *Collector) SaveReactions(ctx context.Context, u *tg.UpdateMessageReactions) error {
	chatID := store.PeerID(u.Peer)
	if chatID == 0 {
		return nil
	}
	return c.Store.SetMessageReactions(ctx, chatID, u.MsgID, store.ReactionsFromTG(&u.Reactions))
}
func (c *Collector) SaveService(ctx context.Context, e tg.Entities, m *tg.MessageService) error {
	if err := c.Store.UpsertEntities(ctx, e); err != nil {
		return err
	}
	if m == nil {
		return nil
	}
	chatID := store.MessageEventChatID(m)
	if chatID == 0 {
		return nil
	}
	t, ok, err := c.target(ctx, chatID)
	if err != nil || !ok || !t.Enabled {
		return err
	}
	return c.Store.SaveMessageService(ctx, m)
}

// target resolves the monitor rule for a chat. A missing row means "not
// monitored" and is not an error; anything else is a real read failure that must
// not be mistaken for a filter miss, or a broken database silently stops
// realtime archiving.
func (c *Collector) target(ctx context.Context, chatID int64) (store.Target, bool, error) {
	t, err := c.Store.Target(ctx, chatID)
	if errors.Is(err, sql.ErrNoRows) {
		return store.Target{}, false, nil
	}
	if err != nil {
		return store.Target{}, false, err
	}
	return t, true, nil
}

// logExportFailure reports an export error without failing the update: the
// message is already archived and gotd treats a handler error as a reason to
// reset the connection.
func logExportFailure(chatID int64, messageID int, err error) {
	if err == nil {
		return
	}
	slog.Default().Warn("archive: export failed",
		slog.Int64("chat_id", chatID), slog.Int("message_id", messageID),
		slog.String("error", err.Error()))
}

func (c *Collector) SaveEdit(ctx context.Context, e tg.Entities, m *tg.Message) error {
	if err := c.Store.UpsertEntities(ctx, e); err != nil {
		return err
	}
	x := store.FromTG(m)
	if x.ChatID == 0 {
		return nil
	}
	t, ok, err := c.target(ctx, x.ChatID)
	if err != nil {
		return err
	}
	if !ok || !c.Accept(t, x) {
		return nil
	}
	if x.EditDate != nil {
		if _, err := c.Store.RowID(ctx, x.ChatID, x.MessageID); err == nil {
			if err := c.Store.RecordEdit(ctx, x); err != nil {
				return err
			}
		}
	}
	if err := c.Store.UpsertMessage(ctx, x); err != nil {
		return err
	}
	if err := c.Store.RefreshMetadata(ctx, x); err != nil {
		return err
	}
	if c.Exporter != nil {
		if err := c.Exporter.channel.MirrorEdit(ctx, x.ChatID, x.MessageID); err != nil {
			// The edit is archived. An export failure must not travel back into
			// the update handler: gotd would drop the connection over a mirror
			// that can be retried independently.
			slog.Default().Warn("archive: mirror edit failed",
				slog.Int64("chat_id", x.ChatID), slog.Int("message_id", x.MessageID),
				slog.String("error", err.Error()))
		}
	}
	return nil
}
func (c *Collector) SaveDelete(ctx context.Context, chatID int64, ids []int) error {
	if chatID == 0 {
		return nil
	}
	if err := c.Store.SoftDelete(ctx, chatID, ids); err != nil {
		return err
	}
	if c.Exporter != nil {
		return c.Exporter.channel.MirrorDelete(ctx, chatID, ids)
	}
	return nil
}
func (c *Collector) hasTelegramDestination(ctx context.Context) bool {
	ds, err := c.Store.Destinations(ctx)
	if err != nil {
		return false
	}
	for _, d := range ds {
		if d.Enabled && d.Kind == "telegram" && d.ChatID.Valid {
			return true
		}
	}
	return false
}

// ensureLinkMedia queues the external videos a message's own text points at.
//
// This covers the half MediaInfo structurally cannot see. Telegram only builds
// a MessageMediaWebPage when it decided to render a preview; a channel with
// previews turned off — or one whose preview fetch failed on the server — posts
// "看这个 https://youtu.be/x" as pure text, with the media field empty. Without
// this pass the sentence is archived and the video it advertises is dropped.
//
// Each URL lands as its own media row, so one post can carry several.
func (c *Collector) ensureLinkMedia(ctx context.Context, m store.Message, msg *tg.Message, t store.Target) error {
	if !t.ArchiveMedia || (!t.DownloadMedia && t.ArchiveMode != "both") {
		return nil
	}
	for _, meta := range store.LinkMedias(msg) {
		if err := c.Store.EnsureMedia(ctx, m, meta); err != nil {
			return err
		}
	}
	return nil
}
