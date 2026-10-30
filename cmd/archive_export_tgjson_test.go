package cmd

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iyear/tdl/archive"
	"github.com/iyear/tdl/archive/store"
	"github.com/iyear/tdl/pkg/consts"
)

// The archive-level tests cover the writer. These cover the part an operator
// actually touches: flag parsing, the directory-vs-file decision, and the guard
// that refuses --fields with a format whose field names are not negotiable.
// Going through the cobra command is the only way to catch a flag that parses
// but never reaches the writer.

const (
	tgJSONChatID  = int64(1002003005)
	tgJSONMediaSH = "cccc1111dddd2222eeee3333ffff4444aaaa5555bbbb6666cccc7777dddd8888"
)

func newTGJSONCommand(t *testing.T, out string) string {
	t.Helper()
	home := t.TempDir()
	old := consts.DataDir
	consts.DataDir = filepath.Join(home, ".tdl")
	t.Cleanup(func() { consts.DataDir = old })

	dbPath := filepath.Join(consts.DataDir, "archive", "tgarchive.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	ctx := context.Background()
	if _, err := s.DB().ExecContext(ctx,
		`INSERT INTO chats(id,type,username,title,raw_json,created_at,updated_at) VALUES(?,?,?,?,?,1,1)`,
		tgJSONChatID, "channel", "cmdch", "CLI Channel", `{"Broadcast":true,"Megagroup":false}`); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertMessage(ctx, store.Message{
		ChatID: tgJSONChatID, MessageID: 7, SenderID: tgJSONChatID, Date: time.Unix(1761293460, 0), Text: "cli body",
	}); err != nil {
		t.Fatal(err)
	}
	obj := archive.ObjectPath(filepath.Join(consts.DataDir, "archive"), tgJSONMediaSH)
	if err := os.MkdirAll(filepath.Dir(obj), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(obj, []byte("cli-media"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureMedia(ctx, store.Message{ChatID: tgJSONChatID, MessageID: 7, Date: time.Unix(1761293460, 0), HasMedia: true},
		store.MediaMeta{Kind: "photo", MIME: "image/png", Width: 800, Height: 600, Size: 9}); err != nil {
		t.Fatal(err)
	}
	var mediaID int64
	if err := s.DB().QueryRowContext(ctx, `SELECT id FROM media WHERE message_rowid=(SELECT id FROM messages WHERE chat_id=? AND message_id=7)`, tgJSONChatID).Scan(&mediaID); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkMediaCompleted(ctx, mediaID, tgJSONMediaSH, 9, "image/png", obj, "", ""); err != nil {
		t.Fatal(err)
	}
	return dbPath
}

func TestArchiveExportTGJSONEndToEnd(t *testing.T) {
	out := filepath.Join(t.TempDir(), "export")
	newTGJSONCommand(t, out)

	c := newArchiveExport()
	c.SetArgs([]string{"--format", "tgjson", "--with-media", "-o", out})
	c.SetOut(io.Discard)
	c.SetErr(io.Discard)
	if err := c.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}

	b, err := os.ReadFile(filepath.Join(out, "result.json"))
	if err != nil {
		t.Fatalf("result.json missing: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("result.json is not valid JSON: %v", err)
	}
	chats := doc["chats"].(map[string]any)
	list := chats["list"].([]any)
	if len(list) != 1 {
		t.Fatalf("chats = %d, want 1", len(list))
	}
	chat := list[0].(map[string]any)
	if chat["id"] != float64(tgJSONChatID) {
		t.Fatalf("chat id = %v", chat["id"])
	}
	msgs := chat["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages = %d, want 1", len(msgs))
	}
	msg := msgs[0].(map[string]any)
	photo, _ := msg["photo"].(string)
	if !strings.HasPrefix(photo, "photos/") {
		t.Fatalf("photo = %q, want a photos/ path", photo)
	}
	if p := filepath.Join(out, filepath.FromSlash(photo)); !isFile(p) {
		t.Fatalf("media file missing at %s", p)
	}
}

func TestArchiveExportTGJSONRejectsFields(t *testing.T) {
	out := filepath.Join(t.TempDir(), "export")
	newTGJSONCommand(t, out)

	c := newArchiveExport()
	c.SetArgs([]string{"--format", "tgjson", "--fields", "id,text", "-o", out})
	c.SetOut(io.Discard)
	c.SetErr(io.Discard)
	err := c.Execute()
	if err == nil {
		t.Fatal("expected an error: the tgjson field names are fixed by the format")
	}
	if !strings.Contains(err.Error(), "do not apply") {
		t.Fatalf("error = %v, want an explanation that --fields does not apply", err)
	}
}

func TestArchiveExportTGJSONRequiresOutDir(t *testing.T) {
	newTGJSONCommand(t, "")
	c := newArchiveExport()
	c.SetArgs([]string{"--format", "tgjson"})
	c.SetOut(io.Discard)
	c.SetErr(io.Discard)
	err := c.Execute()
	if err == nil || !strings.Contains(err.Error(), "-o DIR") {
		t.Fatalf("error = %v, want a demand for -o DIR", err)
	}
}

func TestArchiveExportFlatFormatsStillWork(t *testing.T) {
	// The tgjson branch returns early; a regression there would silently break
	// every existing CSV/JSON pipeline, so pin one of them.
	out := filepath.Join(t.TempDir(), "export")
	newTGJSONCommand(t, out)

	c := newArchiveExport()
	c.SetArgs([]string{"--format", "jsonl", "--kind", "messages"})
	c.SetOut(io.Discard)
	c.SetErr(io.Discard)
	var buf strings.Builder
	c.SetOut(&buf)
	if err := c.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	line := strings.TrimSpace(buf.String())
	if line == "" {
		t.Fatal("jsonl export produced nothing")
	}
	var row map[string]string
	if err := json.Unmarshal([]byte(strings.Split(line, "\n")[0]), &row); err != nil {
		t.Fatalf("row is not a JSON object: %v", err)
	}
	if row["text"] != "cli body" {
		t.Fatalf("row text = %q", row["text"])
	}
}

func isFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}
