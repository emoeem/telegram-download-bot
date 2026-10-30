package archive

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iyear/tdl/archive/store"
	"github.com/iyear/tdl/pkg/consts"
)

// These tests pin the *Telegram Desktop* export contract, not this project's
// preferences. The field names, the chat type vocabulary and the "media key is
// photo for photos and file for everything else" split all come from Telegram's
// own writer (export_output_json.cpp). If a change here makes a test fail, the
// question to ask is which of the two sides moved.

const (
	testPhotoSHA = "aaaa1111bbbb2222cccc3333dddd4444eeee5555ffff6666aaaa7777bbbb8888"
	testVideoSHA = "9999aaaa0000bbbb1111cccc2222dddd3333eeee4444ffff5555aaaa6666bbbb7777"
	testChatID   = int64(1002003004)
	testChat2ID  = int64(1005006007)
	testUserID   = int64(777888999)
)

type exportFixture struct {
	store *store.Store
	data  string
	out   string
}

func newExportFixture(t *testing.T) *exportFixture {
	t.Helper()
	root := t.TempDir()
	data := filepath.Join(root, "data")
	out := filepath.Join(root, "out")
	if err := os.MkdirAll(data, 0o755); err != nil {
		t.Fatal(err)
	}
	old := consts.DataDir
	consts.DataDir = data
	t.Cleanup(func() { consts.DataDir = old })

	s, err := store.Open(filepath.Join(root, "archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return &exportFixture{store: s, data: data, out: out}
}

func (f *exportFixture) addChat(t *testing.T, id int64, title, username, raw string) {
	t.Helper()
	_, err := f.store.DB().ExecContext(context.Background(),
		`INSERT INTO chats(id,type,username,title,raw_json,created_at,updated_at) VALUES(?,?,?,?,?,1,1)`,
		id, "channel", username, title, raw)
	if err != nil {
		t.Fatalf("insert chat %d: %v", id, err)
	}
}

func (f *exportFixture) addMessage(t *testing.T, chatID int64, msgID int, senderID int64, date int64, text string) {
	t.Helper()
	err := f.store.UpsertMessage(context.Background(), store.Message{
		ChatID: chatID, MessageID: msgID, SenderID: senderID,
		Date: time.Unix(date, 0), Text: text,
	})
	if err != nil {
		t.Fatalf("insert message %d/%d: %v", chatID, msgID, err)
	}
}

// addMedia queues a completed media row and writes its object into the CAS, so
// an export with --with-media has real bytes to link.
func (f *exportFixture) addMedia(t *testing.T, chatID int64, msgID int, kind, mime, sha string, w, h, dur int, size int64) {
	t.Helper()
	ctx := context.Background()
	msg := store.Message{ChatID: chatID, MessageID: msgID, Date: time.Unix(1761290000, 0), HasMedia: true}
	if err := f.store.EnsureMedia(ctx, msg, store.MediaMeta{
		Kind: kind, MIME: mime, Width: w, Height: h, Duration: dur, Size: size,
	}); err != nil {
		t.Fatalf("ensure media %d/%d: %v", chatID, msgID, err)
	}
	var id int64
	if err := f.store.DB().QueryRowContext(ctx,
		`SELECT id FROM media WHERE message_rowid=(SELECT id FROM messages WHERE chat_id=? AND message_id=?)`,
		chatID, msgID).Scan(&id); err != nil {
		t.Fatalf("locate media row %d/%d: %v", chatID, msgID, err)
	}
	obj := ObjectPath(filepath.Join(f.data, "archive"), sha)
	if err := os.MkdirAll(filepath.Dir(obj), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(obj, []byte("bytes-of-"+sha[:8]), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := f.store.MarkMediaCompleted(ctx, id, sha, size, mime, obj, "", ""); err != nil {
		t.Fatalf("complete media %d: %v", id, err)
	}
}

func readResult(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("result.json is not valid JSON: %v\nfirst 400 bytes: %s", err, string(b[:min(400, len(b))]))
	}
	return doc
}

func chatList(t *testing.T, doc map[string]any) []any {
	t.Helper()
	chats, ok := doc["chats"].(map[string]any)
	if !ok {
		t.Fatalf("missing chats object, got %T", doc["chats"])
	}
	list, ok := chats["list"].([]any)
	if !ok {
		t.Fatalf("missing chats.list array, got %T", chats["list"])
	}
	return list
}

func exportMessagesOf(t *testing.T, chat any) []any {
	t.Helper()
	c, ok := chat.(map[string]any)
	if !ok {
		t.Fatalf("chat is %T", chat)
	}
	msgs, ok := c["messages"].([]any)
	if !ok {
		t.Fatalf("missing messages array, got %T", c["messages"])
	}
	return msgs
}

func msgAt(t *testing.T, msgs []any, i int) map[string]any {
	t.Helper()
	if i >= len(msgs) {
		t.Fatalf("want message %d, have %d", i, len(msgs))
	}
	m, ok := msgs[i].(map[string]any)
	if !ok {
		t.Fatalf("message %d is %T", i, msgs[i])
	}
	return m
}

// seedChats builds a two-chat archive with text, photo and video messages.
func seedChats(t *testing.T, f *exportFixture) {
	t.Helper()
	f.addChat(t, testChatID, "Archive Channel", "archivech", `{"Broadcast":true,"Megagroup":false}`)
	f.addChat(t, testChat2ID, "Private Group", "", `{"Broadcast":false,"Megagroup":true}`)

	f.addMessage(t, testChatID, 1, testChatID, 1761293460, "hello archive")
	f.addMessage(t, testChatID, 2, testChatID, 1761293520, "")
	f.addMessage(t, testChatID, 3, testChatID, 1761293580, "")
	f.addMessage(t, testChat2ID, 10, testUserID, 1761293640, "second chat")
	f.addMedia(t, testChatID, 2, "photo", "image/jpeg", testPhotoSHA, 1280, 960, 0, 245000)
	f.addMedia(t, testChatID, 3, "video", "video/mp4", testVideoSHA, 1920, 1080, 42, 2100000)
}

func TestTelegramExportStructure(t *testing.T) {
	f := newExportFixture(t)
	seedChats(t, f)

	stats, err := WriteTelegramExport(context.Background(), f.store, TGExportOptions{OutDir: f.out})
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if stats.Chats != 2 || stats.Messages != 4 {
		t.Fatalf("stats = %d chats / %d messages, want 2 / 4", stats.Chats, stats.Messages)
	}

	doc := readResult(t, filepath.Join(f.out, "result.json"))
	if doc["about"] == nil {
		t.Fatal("top-level about is missing: a reader uses it to recognise the format")
	}
	list := chatList(t, doc)
	if len(list) != 2 {
		t.Fatalf("chats = %d, want 2", len(list))
	}

	first, _ := list[0].(map[string]any)
	if got := first["name"]; got != "Archive Channel" {
		t.Fatalf("chat name = %v", got)
	}
	if got := first["type"]; got != "public_channel" {
		t.Fatalf("chat type = %v, want public_channel (Broadcast + username)", got)
	}
	// Telegram writes the bare id: a consumer matching by id would miss every
	// channel by exactly 1e12 if the -100 prefix leaked through.
	if got := first["id"]; got != float64(testChatID) {
		t.Fatalf("chat id = %v, want %d", got, testChatID)
	}

	msgs := exportMessagesOf(t, list[0])
	if len(msgs) != 3 {
		t.Fatalf("messages = %d, want 3", len(msgs))
	}
	m0 := msgAt(t, msgs, 0)
	if got := m0["id"]; got != float64(1) {
		t.Fatalf("message id = %v", got)
	}
	if got := m0["type"]; got != "message" {
		t.Fatalf("message type = %v, want message", got)
	}
	if got := m0["date"]; got != "2025-10-24T08:11:00" {
		t.Fatalf("date = %v, want UTC ISO without offset", got)
	}
	if got := m0["date_unixtime"]; got != "1761293460" {
		t.Fatalf("date_unixtime = %v", got)
	}
	// The sender row may be absent from the archive (a channel posting as
	// itself has no users entry), so the name falls back to the chat title —
	// the same label the client shows.
	if got := m0["from"]; got != "Archive Channel" {
		t.Fatalf("from = %v, want the chat title fallback", got)
	}
	if got := m0["from_id"]; got != "channel1002003004" {
		t.Fatalf("from_id = %v, want channel1002003004", got)
	}
	// Every from_id must have a from. telegram-chat-parser keys its user table
	// on the pair and throws on an id with no name, so a dangling from_id makes
	// the whole file unreadable — not just one message.
	for i, raw := range msgs {
		m, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("message %d is %T", i, raw)
		}
		_, hasID := m["from_id"]
		_, hasFrom := m["from"]
		if hasID && !hasFrom {
			t.Fatalf("message %d carries from_id=%v without from: readers key users on the pair", i, m["from_id"])
		}
	}

	// Plain text is a JSON string, not an array: that is the client's rule, and
	// readers branch on it.
	if _, isString := m0["text"].(string); !isString {
		t.Fatalf("text is %T, want string for plain text", m0["text"])
	}
}

func TestTelegramExportMediaKeysByKind(t *testing.T) {
	f := newExportFixture(t)
	seedChats(t, f)
	if _, err := WriteTelegramExport(context.Background(), f.store, TGExportOptions{OutDir: f.out}); err != nil {
		t.Fatalf("export: %v", err)
	}
	doc := readResult(t, filepath.Join(f.out, "result.json"))
	msgs := exportMessagesOf(t, chatList(t, doc)[0])

	photo := msgAt(t, msgs, 1)
	if photo["photo"] == nil {
		t.Fatal("photo message has no photo key")
	}
	if _, has := photo["file"]; has {
		t.Fatal("photo message must not carry a file key")
	}
	if _, has := photo["media_type"]; has {
		t.Fatal("photo message must not carry media_type: the client describes photos with photo alone")
	}
	if _, has := photo["mime_type"]; has {
		t.Fatal("photo message must not carry mime_type: the client only writes it for document-style media")
	}
	if got := photo["width"]; got != float64(1280) {
		t.Fatalf("photo width = %v", got)
	}
	if got := photo["height"]; got != float64(960) {
		t.Fatalf("photo height = %v", got)
	}

	video := msgAt(t, msgs, 2)
	if video["file"] == nil {
		t.Fatal("video message has no file key")
	}
	if _, has := video["photo"]; has {
		t.Fatal("video message must not carry a photo key")
	}
	if got := video["media_type"]; got != "video_file" {
		t.Fatalf("media_type = %v, want video_file", got)
	}
	if got := video["mime_type"]; got != "video/mp4" {
		t.Fatalf("mime_type = %v", got)
	}
	if got := video["duration_seconds"]; got != float64(42) {
		t.Fatalf("duration_seconds = %v, want 42", got)
	}
}

func TestTelegramExportWithMediaLandsInKindDirs(t *testing.T) {
	f := newExportFixture(t)
	seedChats(t, f)
	stats, err := WriteTelegramExport(context.Background(), f.store, TGExportOptions{OutDir: f.out, WithMedia: true})
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if stats.MediaCopied != 2 || stats.MediaMissed != 0 {
		t.Fatalf("media copied=%d missed=%d, want 2/0", stats.MediaCopied, stats.MediaMissed)
	}

	doc := readResult(t, filepath.Join(f.out, "result.json"))
	msgs := exportMessagesOf(t, chatList(t, doc)[0])

	photoPath, _ := msgAt(t, msgs, 1)["photo"].(string)
	if !strings.HasPrefix(photoPath, "photos/photo_") {
		t.Fatalf("photo path = %q, want photos/photo_N@date.ext", photoPath)
	}
	if strings.Contains(photoPath, "\\") {
		t.Fatalf("photo path uses backslashes: %q", photoPath)
	}
	if !strings.HasSuffix(photoPath, ".jpg") {
		t.Fatalf("photo path = %q, want a .jpg suffix from image/jpeg", photoPath)
	}
	onDisk := filepath.Join(f.out, filepath.FromSlash(photoPath))
	b, err := os.ReadFile(onDisk)
	if err != nil {
		t.Fatalf("photo file not written at %s: %v", onDisk, err)
	}
	if string(b) != "bytes-of-"+testPhotoSHA[:8] {
		t.Fatalf("photo bytes = %q, want the archived object", b)
	}

	videoPath, _ := msgAt(t, msgs, 2)["file"].(string)
	if !strings.HasPrefix(videoPath, "video_files/video_") {
		t.Fatalf("video path = %q, want video_files/video_N@date.ext", videoPath)
	}
	if _, err := os.Stat(filepath.Join(f.out, filepath.FromSlash(videoPath))); err != nil {
		t.Fatalf("video file missing: %v", err)
	}
}

func TestTelegramExportSkipReasonsAreExplicit(t *testing.T) {
	// Without --with-media the key must still be present: "this post has media,
	// the file is not here" is not the same claim as "this post has no media",
	// and an export that conflates them looks complete while being partial.
	f := newExportFixture(t)
	seedChats(t, f)
	if _, err := WriteTelegramExport(context.Background(), f.store, TGExportOptions{OutDir: f.out}); err != nil {
		t.Fatalf("export: %v", err)
	}
	doc := readResult(t, filepath.Join(f.out, "result.json"))
	msgs := exportMessagesOf(t, chatList(t, doc)[0])
	if got := msgAt(t, msgs, 1)["photo"]; got != mediaSkippedNotExported {
		t.Fatalf("photo without --with-media = %v, want the client's 'not included' notice", got)
	}

	// Now enable media but remove the object: that is a different failure and
	// must read differently.
	f2 := newExportFixture(t)
	seedChats(t, f2)
	obj := ObjectPath(filepath.Join(f2.data, "archive"), testPhotoSHA)
	if err := os.Remove(obj); err != nil {
		t.Fatal(err)
	}
	stats, err := WriteTelegramExport(context.Background(), f2.store, TGExportOptions{OutDir: f2.out, WithMedia: true})
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if stats.MediaMissed != 1 {
		t.Fatalf("media missed = %d, want 1", stats.MediaMissed)
	}
	doc2 := readResult(t, filepath.Join(f2.out, "result.json"))
	msgs2 := exportMessagesOf(t, chatList(t, doc2)[0])
	if got := msgAt(t, msgs2, 1)["photo"]; got != mediaSkippedMissing {
		t.Fatalf("photo with a vanished object = %v, want the 'unavailable' notice", got)
	}
}

func TestTelegramExportSplitWritesSingleChatLayout(t *testing.T) {
	f := newExportFixture(t)
	seedChats(t, f)
	if _, err := WriteTelegramExport(context.Background(), f.store, TGExportOptions{OutDir: f.out, Split: true, WithMedia: true}); err != nil {
		t.Fatalf("export: %v", err)
	}
	entries, err := os.ReadDir(f.out)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("split produced %d entries, want 2 chat directories", len(entries))
	}
	var found bool
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		// The id suffix keeps two same-titled channels apart.
		if !strings.HasSuffix(e.Name(), "_1002003004") {
			continue
		}
		found = true
		doc := readResult(t, filepath.Join(f.out, e.Name(), "result.json"))
		if _, isWrapped := doc["chats"]; isWrapped {
			t.Fatal("split result.json must have the chat object at the top level, not a chats wrapper")
		}
		if got := doc["type"]; got != "public_channel" {
			t.Fatalf("split chat type = %v", got)
		}
		msgs, ok := doc["messages"].([]any)
		if !ok || len(msgs) != 3 {
			t.Fatalf("split messages = %v, want 3", doc["messages"])
		}
		// Media paths are relative to that chat's own directory.
		photo, _ := msgAt(t, msgs, 1)["photo"].(string)
		if _, err := os.Stat(filepath.Join(f.out, e.Name(), filepath.FromSlash(photo))); err != nil {
			t.Fatalf("split media not beside result.json: %v", err)
		}
	}
	if !found {
		t.Fatal("no per-chat directory named after the channel id")
	}
}

func TestTelegramExportChatTypeMapping(t *testing.T) {
	cases := []struct {
		name     string
		raw      string
		username string
		want     string
	}{
		{"broadcast with username", `{"Broadcast":true,"Megagroup":false}`, "news", "public_channel"},
		{"broadcast without username", `{"Broadcast":true,"Megagroup":false}`, "", "private_channel"},
		{"supergroup with username", `{"Broadcast":false,"Megagroup":true}`, "chat", "public_supergroup"},
		{"supergroup without username", `{"Broadcast":false,"Megagroup":true}`, "", "private_supergroup"},
		{"neither flag set", `{"Broadcast":false,"Megagroup":false}`, "", "private_channel"},
		{"no raw_json at all", "", "hub", "public_channel"},
		{"no raw_json, no username", "", "", "private_channel"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := store.ExportChatType([]byte(c.raw), c.username); got != c.want {
				t.Fatalf("ExportChatType = %q, want %q", got, c.want)
			}
		})
	}
}

func TestTelegramExportChatFilterAndLimit(t *testing.T) {
	f := newExportFixture(t)
	seedChats(t, f)

	stats, err := WriteTelegramExport(context.Background(), f.store, TGExportOptions{
		OutDir: f.out, ChatIDs: []int64{testChat2ID},
	})
	if err != nil {
		t.Fatalf("filtered export: %v", err)
	}
	if stats.Chats != 1 || stats.Messages != 1 {
		t.Fatalf("filtered stats = %d chats / %d messages, want 1/1", stats.Chats, stats.Messages)
	}

	out2 := filepath.Join(filepath.Dir(f.out), "out2")
	stats2, err := WriteTelegramExport(context.Background(), f.store, TGExportOptions{
		OutDir: out2, Limit: 2,
	})
	if err != nil {
		t.Fatalf("limited export: %v", err)
	}
	// The cap applies per chat: chat 1 has 3 messages (2 kept), chat 2 has 1
	// (nothing to cut), so 3 survive in total.
	if stats2.Messages != 3 {
		t.Fatalf("limited messages = %d, want 3 (2 from chat 1, 1 from chat 2)", stats2.Messages)
	}
	doc := readResult(t, filepath.Join(out2, "result.json"))
	want := []int{2, 1}
	for i, c := range chatList(t, doc) {
		if n := len(exportMessagesOf(t, c)); n != want[i] {
			t.Fatalf("chat %d has %d messages, want %d", i, n, want[i])
		}
	}
}

func TestTelegramExportEmptyArchiveFails(t *testing.T) {
	f := newExportFixture(t)
	if _, err := WriteTelegramExport(context.Background(), f.store, TGExportOptions{OutDir: f.out}); err == nil {
		t.Fatal("expected an error when there is nothing to export")
	}
	if _, err := os.Stat(filepath.Join(f.out, "result.json")); !os.IsNotExist(err) {
		t.Fatal("a failed export must not leave a result.json behind")
	}
}

func TestTelegramExportRequiresOutDir(t *testing.T) {
	f := newExportFixture(t)
	seedChats(t, f)
	if _, err := WriteTelegramExport(context.Background(), f.store, TGExportOptions{}); err == nil {
		t.Fatal("expected an error when OutDir is empty")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// A channel peer id can be stored either bare (1002003004) or with the -100
// marker (-1001002003004), depending on which code path wrote it. Telegram's
// export always writes the bare form, so the writer has to normalise. Without
// this case the prefix stripping was untested: every other test used a positive
// id and never reached the branch.
func TestTelegramExportStripsChannelPrefix(t *testing.T) {
	f := newExportFixture(t)
	const prefixed = int64(-1001002003004)
	const bare = int64(1002003004)
	f.addChat(t, prefixed, "Prefixed Channel", "pfx", `{"Broadcast":true,"Megagroup":false}`)
	f.addMessage(t, prefixed, 1, prefixed, 1761293460, "hello")

	if _, err := WriteTelegramExport(context.Background(), f.store, TGExportOptions{OutDir: f.out}); err != nil {
		t.Fatalf("export: %v", err)
	}
	doc := readResult(t, filepath.Join(f.out, "result.json"))
	list := chatList(t, doc)
	if len(list) != 1 {
		t.Fatalf("chats = %d, want 1", len(list))
	}
	chat, _ := list[0].(map[string]any)
	if got := chat["id"]; got != float64(bare) {
		t.Fatalf("chat id = %v, want the bare id %d", got, bare)
	}
	msgs := exportMessagesOf(t, list[0])
	if got := msgAt(t, msgs, 0)["from_id"]; got != "channel1002003004" {
		t.Fatalf("from_id = %v, want channel1002003004", got)
	}

	// Filtering by either form must select the same chat: an operator pastes
	// whichever id they happen to have.
	out2 := filepath.Join(filepath.Dir(f.out), "out2")
	stats, err := WriteTelegramExport(context.Background(), f.store, TGExportOptions{
		OutDir: out2, ChatIDs: []int64{bare},
	})
	if err != nil {
		t.Fatalf("filter by bare id: %v", err)
	}
	if stats.Chats != 1 {
		t.Fatalf("filter by bare id selected %d chats, want 1", stats.Chats)
	}
}
