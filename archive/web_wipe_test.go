package archive

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iyear/tdl/archive/store"
)

// Both wipe endpoints destroy data, so both must refuse to act on a request
// that has not said, in as many words, that it means it. A boolean flag is not
// enough for the archive-wide purge: one stray click sets a boolean.
func TestWipeEndpointsRequireExplicitConfirmation(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	w := NewWebServer(s)
	w.Ctl = &Controller{}
	h := w.Handler()

	post := func(path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", "http://example.com")
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	del := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodDelete, path, nil)
		req.Header.Set("Origin", "http://example.com")
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	if got := post("/api/purge", `{"all":true,"confirm":true}`); got.Code != http.StatusBadRequest {
		t.Fatalf("purge without confirm_text: status=%d body=%s", got.Code, got.Body.String())
	}
	if got := post("/api/purge", `{"all":true,"confirm":true,"confirm_text":"delete"}`); got.Code != http.StatusBadRequest {
		t.Fatalf("purge with the wrong word: status=%d body=%s", got.Code, got.Body.String())
	}

	// A dry run must stay readable without the word. The UI's first call is
	// exactly this one — it needs the counts in order to tell the operator how
	// much they are about to lose before asking them to type it.
	got := post("/api/purge", `{"all":true,"confirm":false}`)
	if got.Code != http.StatusOK {
		t.Fatalf("purge dry run: status=%d body=%s", got.Code, got.Body.String())
	}
	if !strings.Contains(got.Body.String(), `"everything":true`) {
		t.Fatalf("dry run body = %s, want everything:true", got.Body.String())
	}

	if got := del("/api/targets?all=1"); got.Code != http.StatusBadRequest {
		t.Fatalf("remove-all without confirm: status=%d body=%s", got.Code, got.Body.String())
	}
	// A request that forgot chat_id must still be treated as "which one?",
	// never as "all of them".
	if got := del("/api/targets"); got.Code != http.StatusBadRequest {
		t.Fatalf("delete without chat_id: status=%d body=%s", got.Code, got.Body.String())
	}
}
