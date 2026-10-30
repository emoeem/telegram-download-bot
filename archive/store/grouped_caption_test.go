package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// Telegram puts the words of a media post in the caption and leaves the text
// empty — FromTG does exactly that. A plain COALESCE(text, caption) therefore
// never reaches the caption, because text is an empty string and not NULL.
// The post then renders as a picture with no text at all.
func TestMediaCaptionSurvivesEmptyText(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	const want = "🔰编号:AB123(点击编号自动复制) 🦋一键查询验证视频🦋"
	m := Message{
		ChatID:    10,
		MessageID: 100,
		Date:      time.Unix(1700000000, 0),
		Text:      "",
		Caption:   want,
		GroupedID: 77,
		HasMedia:  true,
	}
	if err := s.UpsertMessage(ctx, m); err != nil {
		t.Fatal(err)
	}

	v, err := s.MessageView(ctx, 10, 100)
	if err != nil {
		t.Fatal(err)
	}
	if v.Text != want {
		t.Fatalf("MessageView.Text = %q, want the caption %q", v.Text, want)
	}

	page, err := s.ListMessages(ctx, MessageFilter{Page: 1, PageSize: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(page.Items))
	}
	if page.Items[0].Text != want {
		t.Fatalf("ListMessages Text = %q, want %q", page.Items[0].Text, want)
	}

	// A media row whose caption is empty must still render as empty, not as a
	// stray blank: the NULLIF pair has to keep working when both columns are
	// empty strings.
	if err := s.UpsertMessage(ctx, Message{ChatID: 10, MessageID: 101, Date: time.Unix(1700000001, 0), GroupedID: 77}); err != nil {
		t.Fatal(err)
	}
	v2, err := s.MessageView(ctx, 10, 101)
	if err != nil {
		t.Fatal(err)
	}
	if v2.Text != "" {
		t.Fatalf("empty media Text = %q, want empty", v2.Text)
	}
}

// An album is one logical post that happens to span several rows. The page
// window must not slice it: the half that carries the caption would land on
// another page and the visible half would look like a bare picture.
func TestAlbumSurvivesPageBoundary(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	base := int64(1700000000)
	// Three newer standalone posts, then the album, then three older ones.
	for i := 0; i < 3; i++ {
		if err := s.UpsertMessage(ctx, Message{ChatID: 5, MessageID: 300 + i, Date: time.Unix(base+100-int64(i), 0), Text: "newer"}); err != nil {
			t.Fatal(err)
		}
	}
	const album = int64(424242)
	const members = 5
	for i := 0; i < members; i++ {
		msg := Message{ChatID: 5, MessageID: 400 + i, Date: time.Unix(base-int64(i), 0), GroupedID: album, HasMedia: true}
		if i == 0 {
			msg.Caption = "组内唯一的文字"
		}
		if err := s.UpsertMessage(ctx, msg); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 3; i++ {
		if err := s.UpsertMessage(ctx, Message{ChatID: 5, MessageID: 500 + i, Date: time.Unix(base-100-int64(i), 0), Text: "older"}); err != nil {
			t.Fatal(err)
		}
	}

	// pageSize 4 cuts through the album: 3 newer posts + album member #0.
	page, err := s.ListMessages(ctx, MessageFilter{Page: 1, PageSize: 4})
	if err != nil {
		t.Fatal(err)
	}
	got := 0
	for _, v := range page.Items {
		if v.GroupedID == album {
			got++
		}
	}
	if got != members {
		t.Fatalf("album members on page = %d, want all %d (page was size %d)", got, members, len(page.Items))
	}

	// The expanded rows must keep the page order: newest first.
	for i := 1; i < len(page.Items); i++ {
		if page.Items[i-1].Date < page.Items[i].Date {
			t.Fatalf("order broken at %d: %d < %d", i, page.Items[i-1].Date, page.Items[i].Date)
		}
	}

	// And the caption still rides along on the member that carries it.
	found := false
	for _, v := range page.Items {
		if v.GroupedID == album && v.Text == "组内唯一的文字" {
			found = true
		}
	}
	if !found {
		t.Fatal("album caption missing from the expanded page")
	}
}
