package archive

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/gotd/td/telegram/downloader"
	"github.com/gotd/td/telegram/peers"
	"github.com/gotd/td/telegram/query/messages"
	"github.com/gotd/td/tg"
	"github.com/iyear/tdl/archive/store"
)

// errTMePostMissing is a linked post the account cannot read: deleted, or in a
// chat it never joined. Neither state changes by retrying.
var errTMePostMissing = errors.New("linked post not found or not accessible")

// processTMe archives the attachment of a post that another chat quoted with a
// t.me message link.
//
// This is the one link kind the Telegram API can answer by itself. A "link" job
// exists because the bytes sit on some other website behind an extractor; here
// the bytes sit behind the same API the archive already speaks, so the job
// resolves the peer, reads that single post and downloads its file through the
// ordinary CAS path.
func (w *MediaWorker) processTMe(ctx context.Context, j store.MediaJob) error {
	ctx, cancel := context.WithTimeout(ctx, linkJobTimeout)
	defer cancel()
	started := time.Now()
	log := slog.Default().With(
		slog.Int64("media_id", j.ID),
		slog.Int64("chat_id", j.ChatID),
		slog.Int("message_id", j.MessageID),
		slog.String("url", j.URL),
	)
	t, err := w.Store.Target(ctx, j.ChatID)
	if err != nil {
		return w.fail(ctx, j, err, true)
	}
	ref, ok := store.ParseTMeMessage(j.URL)
	if !ok {
		wctx, cancelState := stateCtx(ctx)
		defer cancelState()
		return w.Store.MarkMediaSkippedWithError(wctx, j.ID,
			"not a t.me message link: "+j.URL, "unsupported", "", j.Attempts)
	}
	// The same quoted post appears in every channel that re-posts it. The link
	// is stable across all of them, so one download serves them all and the
	// copies land as ref-counted rows on one object.
	if reusable, ok, e := w.Store.FindReusableMedia(ctx, ref.TMeURL()); e == nil && ok {
		obj := filepath.Join(w.Root, "objects", reusable.SHA256[:2], reusable.SHA256[2:4], reusable.SHA256)
		if _, statErr := os.Stat(obj); statErr == nil {
			return w.Store.MarkMediaReused(ctx, j.ID, reusable)
		}
	}
	peer, err := w.resolveTMePeer(ctx, ref)
	if err != nil {
		// Either the chat does not exist or this account cannot see it. Both are
		// permanent: waiting does not grant access.
		return w.fail(ctx, j, fmt.Errorf("resolve %s: %w", ref.TMeURL(), err), true)
	}
	post, err := w.fetchTMePost(ctx, peer, ref.MessageID)
	if err != nil {
		if errors.Is(err, errTMePostMissing) {
			return w.fail(ctx, j, err, true)
		}
		return w.fail(ctx, j, err, false)
	}
	file, ok := messages.Elem{Msg: post}.File()
	if !ok {
		wctx, cancelState := stateCtx(ctx)
		defer cancelState()
		// The post is readable but carries no attachment of its own: plain text,
		// a poll, or a preview of somebody else's page. Terminal on purpose — a
		// retry cannot change what the post contains, and following the preview
		// would silently turn one quoted line into an unbounded crawl.
		return w.Store.MarkMediaSkippedWithError(wctx, j.ID,
			"linked post has no downloadable attachment ("+mediaKindName(post)+"): "+ref.TMeURL(),
			"unsupported", ref.TMeURL(), j.Attempts)
	}
	maxSize := t.MediaMaxSize
	staging := filepath.Join(w.Root, "staging")
	if err := os.MkdirAll(staging, 0o755); err != nil {
		return w.fail(ctx, j, err, false)
	}
	tmp := filepath.Join(staging, fmt.Sprintf("%d.tme", j.ID))
	defer os.Remove(tmp)
	fmt.Fprintf(os.Stderr, "archive media: downloading t.me post job=%d chat=%d msg=%d url=%s\n", j.ID, j.ChatID, j.MessageID, j.URL)
	if _, err := downloader.NewDownloader().WithPartSize(1024*1024).
		Download(w.Runtime.API, file.Location).WithThreads(2).ToPath(ctx, tmp); err != nil {
		return w.fail(ctx, j, err, false)
	}
	if err := w.ingestObject(ctx, j, tmp, file.MIMEType, maxSize, thumbLocation(post)); err != nil {
		return w.fail(ctx, j, err, false)
	}
	log.Info("media worker t.me post fetched",
		slog.String("name", file.Name),
		slog.Int64("duration_ms", time.Since(started).Milliseconds()),
	)
	return nil
}

// resolveTMePeer turns a parsed message link into a peer the API can address.
func (w *MediaWorker) resolveTMePeer(ctx context.Context, ref store.TMeRef) (peers.Peer, error) {
	if ref.IsPrivate() {
		return w.Runtime.Peers.ResolveChannelID(ctx, ref.ChannelID)
	}
	return w.Runtime.Peers.Resolve(ctx, ref.Username)
}

// fetchTMePost reads one post by id out of another chat.
//
// channels.getMessages and messages.getMessages are separate methods in MTProto
// and are not interchangeable: a channel id sent to the generic one comes back
// empty rather than raising, which would look like a deleted post.
func (w *MediaWorker) fetchTMePost(ctx context.Context, peer peers.Peer, id int) (*tg.Message, error) {
	ids := []tg.InputMessageClass{&tg.InputMessageID{ID: id}}
	var (
		res tg.MessagesMessagesClass
		err error
	)
	if ch, ok := peer.(peers.Channel); ok {
		res, err = w.Runtime.API.ChannelsGetMessages(ctx, &tg.ChannelsGetMessagesRequest{
			Channel: ch.InputChannel(),
			ID:      ids,
		})
	} else {
		res, err = w.Runtime.API.MessagesGetMessages(ctx, ids)
	}
	if err != nil {
		return nil, err
	}
	for _, m := range tmeMessageList(res) {
		if mm, ok := m.(*tg.Message); ok && mm.ID == id {
			return mm, nil
		}
	}
	return nil, errTMePostMissing
}

// tmeMessageList flattens the four shapes messages.getMessages can answer with.
// MessagesNotModified carries no list at all and simply yields nothing.
func tmeMessageList(res tg.MessagesMessagesClass) []tg.MessageClass {
	switch v := res.(type) {
	case *tg.MessagesMessages:
		return v.Messages
	case *tg.MessagesMessagesSlice:
		return v.Messages
	case *tg.MessagesChannelMessages:
		return v.Messages
	}
	return nil
}

// mediaKindName labels a post's payload for the skip reason, so an operator can
// tell "quoted a poll" from "quoted an empty post" without opening it.
func mediaKindName(m *tg.Message) string {
	if m == nil {
		return "unreadable"
	}
	md, ok := m.GetMedia()
	if !ok || md == nil {
		return "text only"
	}
	return md.TypeName()
}
