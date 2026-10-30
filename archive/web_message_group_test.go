package archive

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/iyear/tdl/archive/store"
)

// One album is one post, and the caption rides on exactly one member. The
// detail view therefore has to fetch the whole group, not the single row the
// operator happened to click.
func TestMessageGroupEndpointReturnsWholeAlbum(t *testing.T) {
	s, ts, _ := newAdminWeb(t, true)
	ctx := context.Background()

	const chat = int64(-1001)
	const gid = int64(909090)
	for i := 0; i < 4; i++ {
		m := store.Message{ChatID: chat, MessageID: 700 + i, Date: time.Unix(1700000000+int64(i), 0), GroupedID: gid, HasMedia: true}
		if i == 0 {
			m.Caption = "相册文字只在这一条"
		}
		if err := s.UpsertMessage(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.UpsertMessage(ctx, store.Message{ChatID: chat, MessageID: 800, Date: time.Unix(1700000100, 0), Text: "别的消息"}); err != nil {
		t.Fatal(err)
	}
	// A member marked deleted must not reappear through this endpoint.
	if err := s.UpsertMessage(ctx, store.Message{ChatID: chat, MessageID: 900, Date: time.Unix(1700000200, 0), GroupedID: gid}); err != nil {
		t.Fatal(err)
	}
	if err := s.SoftDelete(ctx, chat, []int{900}); err != nil {
		t.Fatalf("soft delete: %v", err)
	}

	resp, err := http.Get(ts.URL + "/api/message/group?chat=-1001&gid=909090")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var out struct {
		Items []store.MessageView `json:"items"`
		Total int                 `json:"total"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Items) != 4 {
		t.Fatalf("items = %d, want 4 (deleted member must stay hidden)", len(out.Items))
	}
	// Oldest first, the order Telegram sent them.
	for i := 1; i < len(out.Items); i++ {
		if out.Items[i-1].RowID > out.Items[i].RowID {
			t.Fatalf("order not ascending at %d", i)
		}
	}
	withText := 0
	for _, v := range out.Items {
		if v.Text == "相册文字只在这一条" {
			withText++
		}
	}
	if withText != 1 {
		t.Fatalf("caption found on %d members, want exactly 1", withText)
	}

	// Missing gid is a client error, not an empty album.
	bad, err := http.Get(ts.URL + "/api/message/group")
	if err != nil {
		t.Fatal(err)
	}
	bad.Body.Close()
	if bad.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing gid status = %d, want 400", bad.StatusCode)
	}
}
