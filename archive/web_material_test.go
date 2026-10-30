package archive

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/iyear/tdl/archive/store"
)

// The material layer existed only behind `tdl archive material` and the export
// endpoint, so the viewer showed an unsorted pile of raw messages while the
// tidied layer sat unreachable. These tests pin the front door: the default view
// is the deduped one, and the rebuild endpoint refuses anything that is not an
// explicit JSON POST.

func seedMaterials(t *testing.T, s *store.Store) {
	t.Helper()
	ctx := context.Background()
	mk := func(msgID int, date int64) store.Material {
		return store.Material{
			ChatID:          -1001,
			AnchorRowID:     int64(msgID),
			AnchorMessageID: msgID,
			Kind:            "photo",
			Title:           "同一张图",
			ContentHash:     "same-hash",
			MediaCount:      1,
			MediaKind:       "photo",
			Date:            date,
			Status:          "active",
		}
	}
	if _, err := s.ReplaceMaterial(ctx, mk(1, 1700000000), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReplaceMaterial(ctx, mk(2, 1700000001), nil); err != nil {
		t.Fatal(err)
	}
	// The flag is what makes one of them invisible in the default view, so the
	// fixture has to run the real dedupe pass rather than assume its outcome.
	if _, err := s.MarkDuplicateMaterials(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestMaterialsAPIDefaultsToDedupedView(t *testing.T) {
	s, ts, _ := newAdminWeb(t, false)
	seedMaterials(t, s)

	resp, err := http.Get(ts.URL + "/api/materials")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body materialsAPIResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	// Same content twice: the default view shows the original only.
	if len(body.Items) != 1 {
		t.Fatalf("deduped view returned %d items, want 1", len(body.Items))
	}
	if body.Total != 1 {
		t.Fatalf("total = %d, want 1", body.Total)
	}
	if body.Stats.Duplicates != 1 {
		t.Fatalf("stats.duplicates = %d, want 1", body.Stats.Duplicates)
	}
	if body.Stats.Total != 2 {
		t.Fatalf("stats.total = %d, want 2", body.Stats.Total)
	}

	// status=all brings the repeat back into view.
	resp2, err := http.Get(ts.URL + "/api/materials?status=all")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	var all materialsAPIResponse
	if err := json.NewDecoder(resp2.Body).Decode(&all); err != nil {
		t.Fatal(err)
	}
	if len(all.Items) != 2 {
		t.Fatalf("status=all returned %d items, want 2", len(all.Items))
	}
	if all.Items[0].Status != "duplicate" && all.Items[1].Status != "duplicate" {
		t.Fatalf("neither row is flagged duplicate: %q / %q", all.Items[0].Status, all.Items[1].Status)
	}
}

// A GET on the rebuild endpoint must not start a rebuild, and a form-encoded
// POST must not reach it either: a cross-site form can send text/plain without a
// preflight.
func TestMaterialRefreshRejectsWrongMethodAndContentType(t *testing.T) {
	s, ts, _ := newAdminWeb(t, false)
	seedMaterials(t, s)

	resp, err := http.Get(ts.URL + "/api/material/refresh")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d, want 405", resp.StatusCode)
	}

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/material/refresh", strings.NewReader(`{"rebuild":true}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "text/plain")
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("text/plain status = %d, want 415", resp2.StatusCode)
	}
}

func TestMaterialRefreshRunsBuild(t *testing.T) {
	s, ts, _ := newAdminWeb(t, false)
	// The endpoint materializes raw messages, so the fixture has to provide
	// messages; seeding the material table directly would leave the build with
	// nothing to fold.
	ctx := context.Background()
	for i, msgID := range []int{11, 12} {
		if err := s.UpsertMessage(ctx, store.Message{
			ChatID:    -1001,
			MessageID: msgID,
			Date:      time.Unix(int64(1700000000+i), 0),
			Text:      "材料层测试消息",
		}); err != nil {
			t.Fatal(err)
		}
	}

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/material/refresh", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var rep BuildReport
	if err := json.NewDecoder(resp.Body).Decode(&rep); err != nil {
		t.Fatal(err)
	}
	if rep.Materials == 0 {
		t.Fatalf("build folded nothing: %+v", rep)
	}
}
