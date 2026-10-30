package archive

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/iyear/tdl/archive/store"
	"github.com/iyear/tdl/pkg/consts"
)

// Telegram-format export.
//
// The point of this format is interoperability, not prettiness: a file produced
// here has to be openable by the tools that already read what Telegram Desktop
// writes. Field names, their order, the chat type vocabulary and the "text is a
// string until it has formatting" rule therefore follow Telegram Desktop's own
// exporter (export_output_json.cpp), not this project's conventions.
//
// The one deliberate divergence is time zones: this writer emits UTC while the
// client emits local time. An archive can be exported on a server in any zone,
// and a stable offset beats an implicit one.

// Skip reasons copied from Telegram Desktop's exporter. They are the contract
// that tells a reader "this message has media, the file just isn't here" — very
// different from "this message has no media", and collapsing the two would make
// an export look complete when it is not.
const (
	mediaSkippedNotExported = "(File not included. Change data exporting settings to download.)"
	mediaSkippedMissing     = "(File unavailable, please try again later)"
)

// TGExportOptions configures one export run.
type TGExportOptions struct {
	// OutDir receives result.json (and the media directories beside it).
	OutDir string
	// WithMedia copies the archived files next to the JSON.
	WithMedia bool
	// ChatIDs restricts the export; empty means every archived chat.
	ChatIDs []int64
	// Limit caps messages per chat; 0 means all.
	Limit int
	// Split writes one directory per chat instead of a single result.json.
	// This is the layout whose result.json has the chat object at the top
	// level, which is what some importers expect.
	Split bool
}

// TGExportStats reports what actually landed on disk.
type TGExportStats struct {
	Chats       int
	Messages    int64
	MediaCopied int
	MediaMissed int
	MediaBytes  int64
}

// tgMessage mirrors one element of a chat's "messages" array.
//
// Field order is load-bearing: encoding/json emits struct fields in declaration
// order, and the goal is a file that diffs cleanly against a real client export.
// Pointer-free values with omitempty match the client, which omits a key rather
// than writing a zero.
type tgMessage struct {
	ID             int    `json:"id"`
	Type           string `json:"type"`
	Date           string `json:"date"`
	DateUnixtime   string `json:"date_unixtime"`
	Edited         string `json:"edited,omitempty"`
	EditedUnixtime string `json:"edited_unixtime,omitempty"`

	From          string `json:"from,omitempty"`
	FromID        string `json:"from_id,omitempty"`
	ReplyTo       int    `json:"reply_to_message_id,omitempty"`
	ForwardedFrom string `json:"forwarded_from,omitempty"`
	ForwardedID   string `json:"forwarded_from_id,omitempty"`

	// Text is a string for plain text and an array once formatting exists. This
	// writer only emits plain text, so it is always a string; a reader that
	// handles both shapes (all of them do) needs no special case.
	Text any `json:"text"`

	Photo        string `json:"photo,omitempty"`
	PhotoSize    int64  `json:"photo_file_size,omitempty"`
	File         string `json:"file,omitempty"`
	FileName     string `json:"file_name,omitempty"`
	FileSize     int64  `json:"file_size,omitempty"`
	MediaType    string `json:"media_type,omitempty"`
	MimeType     string `json:"mime_type,omitempty"`
	DurationSecs int    `json:"duration_seconds,omitempty"`
	Width        int    `json:"width,omitempty"`
	Height       int    `json:"height,omitempty"`

	Views int `json:"views,omitempty"`
}

// mediaLayout says where one media kind goes and what media_type it carries.
// A zero mediaType means "this kind has no media_type key", matching the
// client: photos and plain documents are described by photo/file alone.
func mediaLayout(kind string) (dir, mediaType string) {
	switch strings.ToLower(kind) {
	case "photo":
		return "photos", ""
	case "video":
		return "video_files", "video_file"
	case "video_note":
		return "round_video_messages", "video_message"
	case "voice":
		return "voice_messages", "voice_message"
	case "audio":
		return "music", "audio_file"
	case "sticker":
		return "stickers", "sticker"
	case "animation":
		return "animations", "animation"
	default:
		return "files", ""
	}
}

// bareChatID strips the -100 prefix from a channel peer id. Telegram's export
// writes the bare id, so a consumer matching chats by id would otherwise miss
// every channel by exactly 1e12.
func bareChatID(id int64) int64 {
	if id <= -1_000_000_000_000 {
		return -id - 1_000_000_000_000
	}
	return id
}

// peerRef renders a peer as Telegram's export does: a prefixed bare id.
func peerRef(id, chatID int64) string {
	if id == 0 {
		return ""
	}
	if id < 0 {
		if s := -id; s >= 1_000_000_000_000 {
			return fmt.Sprintf("channel%d", s-1_000_000_000_000)
		}
		return fmt.Sprintf("chat%d", -id)
	}
	// A positive sender equal to the chat is the channel posting as itself.
	if id == chatID || bareChatID(id) == bareChatID(chatID) {
		return fmt.Sprintf("channel%d", bareChatID(id))
	}
	return fmt.Sprintf("user%d", id)
}

// mediaExt picks an extension for a file the client would have named itself.
func mediaExt(v store.MessageView) string {
	if ext := strings.ToLower(filepath.Ext(v.FileName)); ext != "" && len(ext) <= 8 {
		return ext
	}
	switch strings.ToLower(v.MediaMime) {
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "video/mp4":
		return ".mp4"
	case "audio/mpeg":
		return ".mp3"
	case "audio/ogg":
		return ".ogg"
	case "application/pdf":
		return ".pdf"
	}
	return ""
}

// objectPath resolves the archived bytes for one media row. The objects tree is
// content-addressed, so this is the canonical location; MediaPath is tried
// first because a migrated archive may keep real copies elsewhere.
func mediaObjectPath(v store.MessageView) string {
	if v.MediaPath != "" {
		if _, err := os.Stat(v.MediaPath); err == nil {
			return v.MediaPath
		}
	}
	if len(v.MediaSHA256) < 4 {
		return ""
	}
	return ObjectPath(filepath.Join(consts.DataDir, "archive"), v.MediaSHA256)
}

// copyOrLink prefers a hard link: an export is a second name for bytes the
// archive already owns, and linking makes a 2 GB export instant instead of a
// 2 GB copy. It falls back to a copy when the target is on another filesystem.
func copyOrLink(src, dst string) (int64, error) {
	if err := os.Link(src, dst); err == nil {
		if st, err := os.Stat(dst); err == nil {
			return st.Size(), nil
		}
		return 0, nil
	}
	in, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return 0, err
	}
	n, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return n, copyErr
	}
	return n, closeErr
}

// tgExporter carries the per-run state that would otherwise be threaded through
// every function.
type tgExporter struct {
	st    *store.Store
	opts  TGExportOptions
	stats TGExportStats
	// seq numbers media files globally, so two chats never collide on a name.
	seq int
}

// mediaWriter emits the path cell for one message and, when enabled, places the
// file. It returns the path string the JSON should carry.
func (e *tgExporter) mediaWriter(v store.MessageView, root string, chatID int64) (path string) {
	if v.MediaSHA256 == "" && v.MediaPath == "" {
		return ""
	}
	dir, _ := mediaLayout(v.MediaType)
	ext := mediaExt(v)
	if !e.opts.WithMedia {
		return mediaSkippedNotExported
	}
	src := mediaObjectPath(v)
	if src == "" {
		e.stats.MediaMissed++
		return mediaSkippedMissing
	}
	e.seq++
	name := fmt.Sprintf("%s_%d@%s%s", dirPrefix(v.MediaType), e.seq,
		time.Unix(v.Date, 0).UTC().Format("02-01-2006_15-04-05"), ext)
	rel := filepath.ToSlash(filepath.Join(dir, name))
	dst := filepath.Join(root, dir, name)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		e.stats.MediaMissed++
		return mediaSkippedMissing
	}
	n, err := copyOrLink(src, dst)
	if err != nil {
		e.stats.MediaMissed++
		return mediaSkippedMissing
	}
	e.stats.MediaCopied++
	e.stats.MediaBytes += n
	return rel
}

// dirPrefix is the filename stem the client uses per media kind.
func dirPrefix(kind string) string {
	switch strings.ToLower(kind) {
	case "photo":
		return "photo"
	case "video":
		return "video"
	case "video_note":
		return "round_video"
	case "voice":
		return "audio"
	case "audio":
		return "audio"
	case "sticker":
		return "sticker"
	case "animation":
		return "animation"
	default:
		return "file"
	}
}

// toTGMessage renders one archived message.
func (e *tgExporter) toTGMessage(chatID int64, chatTitle string, v store.MessageView, root string) tgMessage {
	m := tgMessage{
		ID:           v.MessageID,
		Type:         "message",
		Date:         time.Unix(v.Date, 0).UTC().Format("2006-01-02T15:04:05"),
		DateUnixtime: fmt.Sprintf("%d", v.Date),
		Text:         v.Text,
		Views:        v.Views,
	}
	if v.SenderName != "" || v.SenderID != 0 {
		m.FromID = peerRef(v.SenderID, chatID)
		// A message must never carry from_id without from. Readers key their
		// user table on the pair: an id with no name is a dangling reference,
		// and telegram-chat-parser (the reference implementation) throws on it.
		// The archive can legitimately have no senders row — a channel posting
		// as itself has no users entry — so the name falls back the way the
		// client displays it: the chat's own name, then the peer id.
		switch {
		case v.SenderName != "":
			m.From = v.SenderName
		case v.SenderUsername != "":
			m.From = v.SenderUsername
		case bareChatID(v.SenderID) == bareChatID(chatID):
			m.From = chatTitle
		default:
			m.From = m.FromID
		}
		if m.From == "" {
			m.From = m.FromID
		}
	}
	if v.ReplyToMessageID > 0 {
		m.ReplyTo = v.ReplyToMessageID
	}
	if v.ForwardFromName != "" {
		m.ForwardedFrom = v.ForwardFromName
	}
	if v.ForwardFromID != 0 {
		m.ForwardedID = peerRef(v.ForwardFromID, chatID)
	}
	if v.EditDate > 0 {
		m.Edited = time.Unix(v.EditDate, 0).UTC().Format("2006-01-02T15:04:05")
		m.EditedUnixtime = fmt.Sprintf("%d", v.EditDate)
	}

	_, mediaType := mediaLayout(v.MediaType)
	if v.MediaSHA256 != "" || v.MediaPath != "" {
		path := e.mediaWriter(v, root, chatID)
		if strings.ToLower(v.MediaType) == "photo" {
			m.Photo = path
			m.PhotoSize = v.MediaSize
		} else {
			m.File = path
			m.FileSize = v.MediaSize
		}
		m.MediaType = mediaType
		// The client describes a photo with photo/size/width/height and no
		// mime_type; mime_type belongs to document-style media only.
		if mediaType != "" {
			m.MimeType = v.MediaMime
		}
		m.DurationSecs = v.Duration
		if v.Width > 0 && v.Height > 0 {
			m.Width = v.Width
			m.Height = v.Height
		}
	}
	if v.FileName != "" && !strings.HasPrefix(v.FileName, mediaSkippedNotExported) {
		m.FileName = v.FileName
	}
	return m
}

// writeChat streams one dialog: header, then messages one at a time. Paging by
// message_id keeps memory flat regardless of how deep the chat is.
func (e *tgExporter) writeChat(ctx context.Context, w *bufio.Writer, c store.ExportChat, root string) (int64, error) {
	var head strings.Builder
	head.WriteString("{")
	if c.Title != "" {
		head.WriteString(`"name":` + jsonString(c.Title) + ",")
	}
	head.WriteString(`"type":` + jsonString(c.Type) + `,`)
	head.WriteString(fmt.Sprintf(`"id":%d,`, bareChatID(c.ID)))
	head.WriteString(`"messages":[`)
	if _, err := w.WriteString(head.String()); err != nil {
		return 0, err
	}

	var written int64
	after := 0
	first := true
	for {
		if e.opts.Limit > 0 && written >= int64(e.opts.Limit) {
			break
		}
		msgs, err := e.st.ChatMessagesAsc(ctx, c.ID, after, 500)
		if err != nil {
			return written, err
		}
		if len(msgs) == 0 {
			break
		}
		for _, v := range msgs {
			if e.opts.Limit > 0 && written >= int64(e.opts.Limit) {
				break
			}
			obj := e.toTGMessage(c.ID, c.Title, v, root)
			b, err := json.Marshal(obj)
			if err != nil {
				return written, err
			}
			if !first {
				if err := w.WriteByte(','); err != nil {
					return written, err
				}
			}
			first = false
			if _, err := w.Write(b); err != nil {
				return written, err
			}
			written++
		}
		after = msgs[len(msgs)-1].MessageID
		if len(msgs) < 500 {
			break
		}
	}
	if _, err := w.WriteString("]}"); err != nil {
		return written, err
	}
	return written, nil
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// chatDirName builds a filesystem-safe directory name for one chat in split
// mode. The id suffix is not decoration: two channels can share a title, and
// silently merging their exports would lose one of them.
func chatDirName(c store.ExportChat) string {
	name := strings.TrimSpace(c.Title)
	if name == "" {
		name = c.Type
	}
	repl := strings.NewReplacer("/", "_", "\\", "_", ":", "_", "*", "_",
		"?", "_", "\"", "_", "<", "_", ">", "_", "|", "_", "\n", " ", "\r", " ")
	name = strings.TrimSpace(repl.Replace(name))
	if name == "" {
		name = "chat"
	}
	if r := []rune(name); len(r) > 60 {
		name = strings.TrimSpace(string(r[:60]))
	}
	return fmt.Sprintf("%s_%d", name, bareChatID(c.ID))
}

// selectChats applies the ChatIDs filter, preserving the store's order.
func selectChats(all []store.ExportChat, want []int64) []store.ExportChat {
	if len(want) == 0 {
		return all
	}
	keep := make(map[int64]bool, len(want))
	for _, id := range want {
		keep[id] = true
		keep[bareChatID(id)] = true
	}
	out := make([]store.ExportChat, 0, len(want))
	for _, c := range all {
		if keep[c.ID] || keep[bareChatID(c.ID)] {
			out = append(out, c)
		}
	}
	return out
}

// WriteTelegramExport writes the archive in Telegram Desktop's export format.
//
// The JSON is written to a temporary file and renamed only after it is complete
// and synced. A half-written result.json is worse than no file at all: every
// reader treats it as a finished export and reports the missing tail as lost
// messages.
func WriteTelegramExport(ctx context.Context, st *store.Store, opts TGExportOptions) (TGExportStats, error) {
	var stats TGExportStats
	if strings.TrimSpace(opts.OutDir) == "" {
		return stats, errors.New("export directory is required")
	}
	all, err := st.ListExportChats(ctx)
	if err != nil {
		return stats, err
	}
	chats := selectChats(all, opts.ChatIDs)
	if len(chats) == 0 {
		return stats, errors.New("no archived chats to export")
	}
	if err := os.MkdirAll(opts.OutDir, 0o755); err != nil {
		return stats, err
	}

	e := &tgExporter{st: st, opts: opts}

	if opts.Split {
		for _, c := range chats {
			dir := filepath.Join(opts.OutDir, chatDirName(c))
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return stats, err
			}
			n, err := e.writeChatFile(ctx, c, dir)
			if err != nil {
				return stats, err
			}
			stats.Chats++
			stats.Messages += n
		}
		stats.MediaCopied = e.stats.MediaCopied
		stats.MediaMissed = e.stats.MediaMissed
		stats.MediaBytes = e.stats.MediaBytes
		return stats, nil
	}

	final := filepath.Join(opts.OutDir, "result.json")
	tmp := final + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return stats, err
	}
	w := bufio.NewWriterSize(f, 1<<20)
	cleanup := func() { _ = os.Remove(tmp) }

	if _, err := w.WriteString(`{"about":"Telegram Desktop Data Export","chats":{"about":"Archived chats","list":[`); err != nil {
		f.Close()
		cleanup()
		return stats, err
	}
	for i, c := range chats {
		if i > 0 {
			if err := w.WriteByte(','); err != nil {
				f.Close()
				cleanup()
				return stats, err
			}
		}
		n, err := e.writeChat(ctx, w, c, opts.OutDir)
		if err != nil {
			f.Close()
			cleanup()
			return stats, err
		}
		stats.Chats++
		stats.Messages += n
	}
	if _, err := w.WriteString("]}}"); err != nil {
		f.Close()
		cleanup()
		return stats, err
	}
	if err := w.Flush(); err != nil {
		f.Close()
		cleanup()
		return stats, err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		cleanup()
		return stats, err
	}
	if err := f.Close(); err != nil {
		cleanup()
		return stats, err
	}
	if err := os.Rename(tmp, final); err != nil {
		cleanup()
		return stats, err
	}
	stats.MediaCopied = e.stats.MediaCopied
	stats.MediaMissed = e.stats.MediaMissed
	stats.MediaBytes = e.stats.MediaBytes
	return stats, nil
}

// writeChatFile writes one chat as a standalone result.json, which is the layout
// Telegram produces when exporting a single chat. The chat object is the whole
// document here, not a member of chats.list.
func (e *tgExporter) writeChatFile(ctx context.Context, c store.ExportChat, dir string) (int64, error) {
	final := filepath.Join(dir, "result.json")
	tmp := final + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return 0, err
	}
	w := bufio.NewWriterSize(f, 1<<20)
	n, err := e.writeChat(ctx, w, c, dir)
	if err == nil {
		err = w.Flush()
	}
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return n, err
	}
	return n, os.Rename(tmp, final)
}
