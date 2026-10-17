package archive

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gotd/td/telegram/uploader"
	"github.com/gotd/td/tg"
	"github.com/iyear/tdl/archive/store"
	"github.com/iyear/tdl/pkg/consts"
)

type ChannelExporter struct {
	Runtime *Runtime
	Store   *store.Store
}

func (e *ChannelExporter) CreateChannel(ctx context.Context, title, about string) (int64, error) {
	res, err := e.Runtime.API.ChannelsCreateChannel(ctx, &tg.ChannelsCreateChannelRequest{Broadcast: true, Title: title, About: about})
	if err != nil {
		return 0, err
	}
	switch x := res.(type) {
	case *tg.Updates:
		for _, c := range x.Chats {
			if ch, ok := c.(*tg.Channel); ok {
				e.Runtime.Peers.Channel(ch)
				return ch.ID, nil
			}
		}
	case *tg.UpdatesCombined:
		for _, c := range x.Chats {
			if ch, ok := c.(*tg.Channel); ok {
				e.Runtime.Peers.Channel(ch)
				return ch.ID, nil
			}
		}
	}
	return 0, fmt.Errorf("channel id not returned")
}

func (e *ChannelExporter) SendMedia(ctx context.Context, peer tg.InputPeerClass, path, mime, caption string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return 0, err
	}
	in, err := uploader.NewUploader(e.Runtime.API).WithThreads(4).Upload(ctx, uploader.NewUpload(filepath.Base(path), f, st.Size()))
	if err != nil {
		return 0, err
	}
	var media tg.InputMediaClass
	if strings.HasPrefix(strings.ToLower(mime), "image/") {
		media = &tg.InputMediaUploadedPhoto{File: in}
	} else {
		media = &tg.InputMediaUploadedDocument{File: in, MimeType: mime}
	}
	res, err := e.Runtime.API.MessagesSendMedia(ctx, &tg.MessagesSendMediaRequest{Peer: peer, Media: media, Message: caption, RandomID: time.Now().UnixNano()})
	if err != nil {
		return 0, err
	}
	return updateMessageID(res)
}

func updateMessageID(v tg.UpdatesClass) (int64, error) {
	switch x := v.(type) {
	case *tg.Updates:
		for _, u := range x.Updates {
			if m, ok := u.(*tg.UpdateMessageID); ok {
				return int64(m.ID), nil
			}
		}
	case *tg.UpdatesCombined:
		for _, u := range x.Updates {
			if m, ok := u.(*tg.UpdateMessageID); ok {
				return int64(m.ID), nil
			}
		}
	}
	return 0, fmt.Errorf("telegram response did not contain destination message id")
}

// CopyMessage uses Telegram's native forwardMessages with DropAuthor=true.
// This is the MTProto API's "copy" semantics in the gotd version used by this project.
// mapForwardedMessageIDs maps Telegram's UpdateMessageID acknowledgements by
// random_id. Never rely on response ordering: Telegram may interleave updateMessageID
// and updateNewChannelMessage constructors in the Updates result.
func mapForwardedMessageIDs(v tg.UpdatesClass, sourceIDs []int, randomIDs []int64) (map[int]int64, error) {
	if len(sourceIDs) != len(randomIDs) {
		return nil, fmt.Errorf("forward mapping input mismatch: ids=%d random_ids=%d", len(sourceIDs), len(randomIDs))
	}
	randomToSource := make(map[int64]int, len(randomIDs))
	for i, randomID := range randomIDs {
		randomToSource[randomID] = sourceIDs[i]
	}
	out := make(map[int]int64, len(sourceIDs))
	var updates []tg.UpdateClass
	switch x := v.(type) {
	case *tg.Updates:
		updates = x.Updates
	case *tg.UpdatesCombined:
		updates = x.Updates
	default:
		return nil, fmt.Errorf("unexpected forwardMessages result %T", v)
	}
	for _, u := range updates {
		if ack, ok := u.(*tg.UpdateMessageID); ok {
			if sourceID, exists := randomToSource[ack.RandomID]; exists {
				out[sourceID] = int64(ack.ID)
			}
		}
	}
	if len(out) != len(sourceIDs) {
		return out, fmt.Errorf("forwardMessages returned %d/%d UpdateMessageID mappings", len(out), len(sourceIDs))
	}
	return out, nil
}

func (e *ChannelExporter) CopyMessage(ctx context.Context, source, destination tg.InputPeerClass, messageID int) (int64, error) {
	res, err := e.Runtime.API.MessagesForwardMessages(ctx, &tg.MessagesForwardMessagesRequest{
		FromPeer: source, ToPeer: destination, ID: []int{messageID}, RandomID: []int64{time.Now().UnixNano()}, DropAuthor: true,
	})
	if err != nil {
		return 0, err
	}
	return updateMessageID(res)
}

// SendMessage re-posts text content as a fresh message authored by the archive
// account. Unlike forwardMessages, a newly authored message is not subject to
// the source chat's no-forwards (protected content) restriction.
func (e *ChannelExporter) SendMessage(ctx context.Context, peer tg.InputPeerClass, text string) (int64, error) {
	res, err := e.Runtime.API.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{Peer: peer, Message: text, RandomID: time.Now().UnixNano()})
	if err != nil {
		return 0, err
	}
	return updateMessageID(res)
}

// fallbackText picks the body worth re-sending when the native copy fails:
// prefer the message text, fall back to the media caption.
func fallbackText(view store.MessageView) string {
	if t := strings.TrimSpace(view.Text); t != "" {
		return t
	}
	return strings.TrimSpace(view.Caption)
}

func (e *ChannelExporter) annotateCopy(ctx context.Context, peer tg.InputPeerClass, messageID int, view store.MessageView) error {
	author := strings.TrimSpace(view.SenderName)
	if view.SenderUsername != "" {
		author += " @" + view.SenderUsername
	}
	if author == "" {
		author = "Unknown sender"
	}
	footer := fmt.Sprintf("\n\n— %s · source %d/%d · %s", author, view.ChatID, view.MessageID, time.Unix(view.Date, 0).Format(time.RFC3339))
	text := view.Text
	if text == "" {
		text = view.Caption
	}
	if text == "" {
		text = footer
	} else {
		text += footer
	}
	_, err := e.Runtime.API.MessagesEditMessage(ctx, &tg.MessagesEditMessageRequest{Peer: peer, ID: messageID, Message: text})
	return err
}

func missingMediaGroupMessages(group []store.Message, existing map[int]int64) ([]store.Message, map[int]int64) {
	out := make([]store.Message, 0, len(group))
	mapped := make(map[int]int64, len(existing))
	for _, msg := range group {
		if destID, ok := existing[msg.MessageID]; ok {
			mapped[msg.MessageID] = destID
			continue
		}
		out = append(out, msg)
	}
	return out, mapped
}

type MediaGroup struct {
	Messages    []store.Message
	Destination store.Destination
}

// MirrorMediaGroup forwards all currently-missing messages in one
// messages.forwardMessages request. We intentionally choose the "missing only"
// idempotency strategy: re-sending an already archived member would create a
// duplicate album in the destination. A partially archived album therefore keeps
// its existing mappings and forwards only the missing members.
func (e *ChannelExporter) MirrorMediaGroup(ctx context.Context, group MediaGroup) (map[int]int64, error) {
	out := make(map[int]int64, len(group.Messages))
	if len(group.Messages) == 0 {
		return out, nil
	}
	if group.Destination.Kind != "telegram" || !group.Destination.ChatID.Valid {
		return out, fmt.Errorf("media group destination is not a Telegram chat")
	}
	groupedID := group.Messages[0].GroupedID
	if groupedID == 0 {
		return out, fmt.Errorf("media group has no grouped_id")
	}
	for _, msg := range group.Messages {
		if msg.ChatID != group.Messages[0].ChatID || msg.GroupedID != groupedID {
			return out, fmt.Errorf("invalid media group: chat/grouped_id mismatch")
		}
	}

	source, err := Resolve(ctx, e.Runtime, fmt.Sprint(group.Messages[0].ChatID))
	if err != nil {
		return out, err
	}
	target, err := Resolve(ctx, e.Runtime, fmt.Sprint(group.Destination.ChatID.Int64))
	if err != nil {
		return out, err
	}

	rows := make(map[int]int64, len(group.Messages))
	existing := make(map[int]int64, len(group.Messages))
	for _, msg := range group.Messages {
		row, rowErr := e.Store.RowID(ctx, msg.ChatID, msg.MessageID)
		if rowErr != nil {
			return out, rowErr
		}
		rows[msg.MessageID] = row
		if destMsg, exportErr := e.Store.Exported(ctx, row, group.Destination.ID); exportErr == nil {
			existing[msg.MessageID] = destMsg
		}
	}
	missing, alreadyMapped := missingMediaGroupMessages(group.Messages, existing)
	for msgID, destMsg := range alreadyMapped {
		out[msgID] = destMsg
	}
	if len(missing) == 0 {
		log.Printf("archive media group grouped_id=%d messages=%d success=%d failed=0 destination=%d (idempotent)", groupedID, len(group.Messages), len(out), group.Destination.ID)
		return out, nil
	}

	ids := make([]int, 0, len(missing))
	randomIDs := make([]int64, 0, len(missing))
	for _, msg := range missing {
		ids = append(ids, msg.MessageID)
		randomIDs = append(randomIDs, time.Now().UnixNano()+int64(msg.MessageID))
	}
	res, forwardErr := e.Runtime.API.MessagesForwardMessages(ctx, &tg.MessagesForwardMessagesRequest{
		FromPeer: source.InputPeer(), ToPeer: target.InputPeer(), ID: ids, RandomID: randomIDs, DropAuthor: true,
	})
	mapped := map[int]int64{}
	if forwardErr == nil {
		mapped, forwardErr = mapForwardedMessageIDs(res, ids, randomIDs)
	}

	mappings := make([]store.ExportMapping, 0, len(missing))
	fallbackReasons := make(map[int]string)
	failed := 0
	for _, msg := range missing {
		destMsg, ok := mapped[msg.MessageID]
		if !ok || forwardErr != nil {
			view, viewErr := e.Store.MessageView(ctx, msg.ChatID, msg.MessageID)
			if viewErr == nil && view.MediaSHA256 != "" {
				p := filepath.Join(consts.DataDir, "archive", "objects", view.MediaSHA256[:2], view.MediaSHA256[2:4], view.MediaSHA256)
				if _, statErr := os.Stat(p); statErr == nil {
					destMsg, viewErr = e.SendMedia(ctx, target.InputPeer(), p, view.MediaMime, view.Caption)
					if viewErr == nil {
						out[msg.MessageID] = destMsg
						fallbackReasons[msg.MessageID] = fmt.Sprintf("grouped_id=%d native forward failed: %v", groupedID, forwardErr)
						mappings = append(mappings, store.ExportMapping{RowID: rows[msg.MessageID], DestinationID: group.Destination.ID, DestinationMessageID: destMsg, GroupedID: groupedID})
						continue
					}
				}
			}
			if forwardErr == nil {
				forwardErr = fmt.Errorf("missing UpdateMessageID mapping for source message %d", msg.MessageID)
			}
			if viewErr != nil && !strings.Contains(viewErr.Error(), "grouped_id") {
				forwardErr = fmt.Errorf("group fallback failed for source message %d: %w (native: %v)", msg.MessageID, viewErr, forwardErr)
			}
			failed++
			mappings = append(mappings, store.ExportMapping{RowID: rows[msg.MessageID], DestinationID: group.Destination.ID, DestinationMessageID: 0, GroupedID: groupedID, Err: forwardErr})
			continue
		}
		out[msg.MessageID] = destMsg
		if view, viewErr := e.Store.MessageView(ctx, msg.ChatID, msg.MessageID); viewErr == nil {
			if annotationErr := e.annotateCopy(ctx, target.InputPeer(), int(destMsg), view); annotationErr != nil {
				log.Printf("archive media group grouped_id=%d message=%d annotation failed: %v", groupedID, msg.MessageID, annotationErr)
			}
		}
		mappings = append(mappings, store.ExportMapping{RowID: rows[msg.MessageID], DestinationID: group.Destination.ID, DestinationMessageID: destMsg, GroupedID: groupedID})
	}
	if err := e.Store.MarkExportsBatch(ctx, mappings); err != nil {
		return out, err
	}
	for msgID, reason := range fallbackReasons {
		if err := e.Store.SetExportFallback(ctx, rows[msgID], group.Destination.ID, reason); err != nil {
			return out, err
		}
	}
	log.Printf("archive media group grouped_id=%d messages=%d success=%d failed=%d destination=%d", groupedID, len(group.Messages), len(out), failed, group.Destination.ID)
	if failed > 0 {
		return out, fmt.Errorf("media group grouped_id=%d partially failed: %d/%d", groupedID, failed, len(group.Messages))
	}
	return out, nil
}

func (e *ChannelExporter) exportOne(ctx context.Context, msg store.Message, dest store.Destination) error {
	row, err := e.Store.RowID(ctx, msg.ChatID, msg.MessageID)
	if err != nil {
		return err
	}
	if _, err = e.Store.Exported(ctx, row, dest.ID); err == nil {
		return nil
	}
	source, err := Resolve(ctx, e.Runtime, fmt.Sprint(msg.ChatID))
	if err != nil {
		return err
	}
	target, err := Resolve(ctx, e.Runtime, fmt.Sprint(dest.ChatID.Int64))
	if err != nil {
		return err
	}
	view, err := e.Store.MessageView(ctx, msg.ChatID, msg.MessageID)
	if err != nil {
		return err
	}

	mid, err := e.CopyMessage(ctx, source.InputPeer(), target.InputPeer(), msg.MessageID)
	fallbackReason := ""
	if err != nil {
		// Protected (no-forwards) sources reject the native copy server-side.
		// Re-sending from local data produces a fresh message authored by the
		// archive account and is not subject to the source restriction.
		fallbackReason = err.Error()
		if view.MediaSHA256 != "" {
			p := filepath.Join(consts.DataDir, "archive", "objects", view.MediaSHA256[:2], view.MediaSHA256[2:4], view.MediaSHA256)
			if _, e2 := os.Stat(p); e2 == nil {
				mid, err = e.SendMedia(ctx, target.InputPeer(), p, view.MediaMime, view.Caption)
			}
		}
		if err != nil {
			if text := fallbackText(view); text != "" {
				// pure text, or a media object that is missing locally
				mid, err = e.SendMessage(ctx, target.InputPeer(), text)
			} else {
				err = fmt.Errorf("copy fallback unavailable: native copy failed: %w", err)
			}
		}
	}
	if err == nil && mid > 0 {
		if aerr := e.annotateCopy(ctx, target.InputPeer(), int(mid), view); aerr != nil {
			err = fmt.Errorf("copied but author annotation failed: %w", aerr)
		}
	}
	if me := e.Store.MarkExport(ctx, row, dest.ID, mid, err); me != nil {
		return me
	}
	if fallbackReason != "" && err == nil {
		_ = e.Store.SetExportFallback(ctx, row, dest.ID, fallbackReason)
	}
	return err
}

func (e *ChannelExporter) ExportMessages(ctx context.Context, msgs []store.Message) error {
	if e == nil || e.Store == nil || len(msgs) == 0 {
		return nil
	}
	t, err := e.Store.Target(ctx, msgs[0].ChatID)
	if err != nil {
		return nil
	}
	if t.ArchiveMode == "local" {
		return nil
	}
	dests, err := e.Store.TargetDestinations(ctx, msgs[0].ChatID)
	if err != nil {
		return err
	}
	groups := make(map[int64][]store.Message)
	groupOrder := make([]int64, 0)
	for _, msg := range msgs {
		if msg.GroupedID != 0 {
			if _, exists := groups[msg.GroupedID]; !exists {
				groupOrder = append(groupOrder, msg.GroupedID)
			}
			groups[msg.GroupedID] = append(groups[msg.GroupedID], msg)
			continue
		}
		for _, dest := range dests {
			if dest.Kind == "telegram" && dest.ChatID.Valid {
				if err := e.exportOne(ctx, msg, dest); err != nil {
					return err
				}
			}
		}
	}
	for _, groupedID := range groupOrder {
		for _, dest := range dests {
			if dest.Kind != "telegram" || !dest.ChatID.Valid {
				continue
			}
			if _, err := e.MirrorMediaGroup(ctx, MediaGroup{Messages: groups[groupedID], Destination: dest}); err != nil {
				return err
			}
		}
	}
	return nil
}

func (e *ChannelExporter) ExportMessage(ctx context.Context, msg store.Message) error {
	return e.ExportMessages(ctx, []store.Message{msg})
}

func (e *ChannelExporter) ExportRecent(ctx context.Context, chatID int64, limit int) error {
	t, err := e.Store.Target(ctx, chatID)
	if err != nil {
		return err
	}
	if t.ArchiveMode == "local" {
		return nil
	}
	msgs, err := e.Store.RecentMessages(ctx, chatID, limit)
	if err != nil {
		return err
	}
	toExport := make([]store.Message, 0, len(msgs))
	for _, m := range msgs {
		toExport = append(toExport, store.Message{ChatID: m.ChatID, MessageID: m.MessageID, SenderID: m.SenderID, Date: time.Unix(m.Date, 0), Text: m.Text, GroupedID: m.GroupedID, HasMedia: m.MediaSHA256 != ""})
	}
	return e.ExportMessages(ctx, toExport)
}

type Exporter struct{ channel *ChannelExporter }

func NewExporter(rt *Runtime, s *store.Store) *Exporter {
	return &Exporter{channel: &ChannelExporter{Runtime: rt, Store: s}}
}
func (e *Exporter) Export(ctx context.Context, m *tg.Message, chatID int64) error {
	if e == nil || e.channel == nil {
		return nil
	}
	return e.channel.ExportMessage(ctx, store.FromTG(m))
}
func (e *Exporter) ExportPending(ctx context.Context, chatID int64, limit int) error {
	if e == nil || e.channel == nil {
		return nil
	}
	return e.channel.ExportRecent(ctx, int64(chatID), limit)
}
func (e *Exporter) ExportMessage(ctx context.Context, m store.Message) error {
	if e == nil || e.channel == nil {
		return nil
	}
	return e.channel.ExportMessage(ctx, m)
}
func (e *Exporter) ExportMedia(ctx context.Context, chatID int64, msgID int) error {
	if e == nil || e.channel == nil {
		return nil
	}
	return e.channel.ExportMedia(ctx, chatID, msgID)
}

func (e *ChannelExporter) ExportMedia(ctx context.Context, chatID int64, msgID int) error {
	v, err := e.Store.MessageView(ctx, chatID, msgID)
	if err != nil {
		return err
	}
	return e.ExportMessage(ctx, store.Message{ChatID: chatID, MessageID: msgID, SenderID: v.SenderID, Date: time.Unix(v.Date, 0), Text: v.Text, Caption: v.Caption, HasMedia: true})
}

func (e *ChannelExporter) MirrorEdit(ctx context.Context, chatID int64, msgID int) error {
	row, err := e.Store.RowID(ctx, chatID, msgID)
	if err != nil {
		return nil
	}
	recs, err := e.Store.ExportRecords(ctx, row)
	if err != nil {
		return err
	}
	v, err := e.Store.MessageView(ctx, chatID, msgID)
	if err != nil {
		return err
	}
	for _, r := range recs {
		if !r.Destination.ChatID.Valid {
			continue
		}
		peer, err := Resolve(ctx, e.Runtime, fmt.Sprint(r.Destination.ChatID.Int64))
		if err != nil {
			return err
		}
		if err := e.annotateCopy(ctx, peer.InputPeer(), int(r.DestinationMessageID), v); err != nil {
			return err
		}
	}
	return nil
}
func (e *ChannelExporter) MirrorDelete(ctx context.Context, chatID int64, ids []int) error {
	for _, id := range ids {
		row, err := e.Store.RowID(ctx, chatID, id)
		if err != nil {
			continue
		}
		recs, err := e.Store.ExportRecords(ctx, row)
		if err != nil {
			return err
		}
		for _, r := range recs {
			if !r.Destination.ChatID.Valid {
				continue
			}
			_, err = e.Runtime.API.MessagesDeleteMessages(ctx, &tg.MessagesDeleteMessagesRequest{Revoke: true, ID: []int{int(r.DestinationMessageID)}})
			if err != nil {
				return err
			}
		}
	}
	return nil
}
