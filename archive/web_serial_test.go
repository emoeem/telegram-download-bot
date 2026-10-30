package archive

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/iyear/tdl/archive/store"
)

// newSerialWeb builds a viewer whose data root is a temp dir, so a purge test
// can never reach the real archive.
func newSerialWeb(t *testing.T) (*store.Store, *httptest.Server, string) {
	t.Helper()
	root := t.TempDir()
	s, err := store.Open(filepath.Join(t.TempDir(), "archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	w := NewWebServer(s)
	w.Root = root
	ts := httptest.NewServer(w.Handler())
	t.Cleanup(func() { ts.Close(); s.Close() })
	return s, ts, root
}

func getJSON(t *testing.T, url string, dst any) int {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if dst != nil && resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(body, dst); err != nil {
			t.Fatalf("decode %s: %v (%s)", url, err, body)
		}
	}
	return resp.StatusCode
}

func TestSerialsAPIListsLedger(t *testing.T) {
	s, ts, root := newSerialWeb(t)
	_, serial, obj := seedCompleted(t, s, root, 1, "photo", "image/jpeg", []byte("abc"))
	if _, err := LinkSerialView(root, serial, "photo", "image/jpeg", obj); err != nil {
		t.Fatal(err)
	}
	var out struct {
		Summary struct {
			Total, Max, Pending, Completed int64
		}
		Items []struct {
			Serial int64
			Kind   string
			Status string
			Path   string
		}
		View string
	}
	if code := getJSON(t, ts.URL+"/api/serials?limit=10", &out); code != 200 {
		t.Fatalf("status=%d", code)
	}
	if out.Summary.Total != 1 || out.Summary.Max != serial {
		t.Fatalf("summary=%+v", out.Summary)
	}
	if len(out.Items) != 1 || out.Items[0].Serial != serial || out.Items[0].Kind != "photo" {
		t.Fatalf("items=%+v", out.Items)
	}
	if out.View == "" {
		t.Fatal("view root not reported")
	}
}

func TestSerialAPIDetailRendersPost(t *testing.T) {
	s, ts, root := newSerialWeb(t)
	_, serial, _ := seedCompleted(t, s, root, 2, "photo", "image/jpeg", []byte("abc"))
	var out struct {
		Item struct {
			Serial  int64
			Text    string
			HasFile bool
		}
		Post string
	}
	if code := getJSON(t, ts.URL+"/api/serial?serial="+itoa(serial), &out); code != 200 {
		t.Fatalf("status=%d", code)
	}
	if out.Item.Serial != serial || out.Item.Text != "hello" {
		t.Fatalf("item=%+v", out.Item)
	}
	if out.Post == "" || out.Post[0] != '#' {
		t.Fatalf("post=%q", out.Post)
	}
	if code := getJSON(t, ts.URL+"/api/serial?serial=999999", nil); code != http.StatusNotFound {
		t.Fatalf("missing serial status=%d", code)
	}
	if code := getJSON(t, ts.URL+"/api/serial", nil); code != http.StatusBadRequest {
		t.Fatalf("no serial status=%d", code)
	}
}

func TestPurgeAPIIsDryRunUntilConfirmed(t *testing.T) {
	s, ts, root := newSerialWeb(t)
	_, serial, obj := seedCompleted(t, s, root, 3, "photo", "image/jpeg", []byte("payload"))
	view := SerialViewPath(root, serial, "photo", "image/jpeg")
	if _, err := LinkSerialView(root, serial, "photo", "image/jpeg", obj); err != nil {
		t.Fatal(err)
	}
	post := func(body string) (int, map[string]any) {
		resp, err := http.Post(ts.URL+"/api/purge", "application/json", bytes.NewBufferString(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		return resp.StatusCode, m
	}

	// Unconfirmed: reports what would go, deletes nothing.
	code, m := post(`{"serial":` + itoa(serial) + `}`)
	if code != 200 {
		t.Fatalf("dry run status=%d body=%v", code, m)
	}
	if executed, _ := m["executed"].(bool); executed {
		t.Fatal("dry run executed the purge")
	}
	if _, err := os.Stat(obj); err != nil {
		t.Fatalf("dry run removed the object: %v", err)
	}

	// Confirmed: rows, object and view all go.
	code, m = post(`{"serial":` + itoa(serial) + `,"confirm":true}`)
	if code != 200 {
		t.Fatalf("confirm status=%d body=%v", code, m)
	}
	if executed, _ := m["executed"].(bool); !executed {
		t.Fatal("confirmed purge reported not executed")
	}
	if _, err := os.Stat(obj); !os.IsNotExist(err) {
		t.Fatalf("object survived: %v", err)
	}
	if _, err := os.Stat(view); !os.IsNotExist(err) {
		t.Fatalf("view survived: %v", err)
	}
	if sum, _ := s.SerialSummary(t.Context()); sum.Total != 0 {
		t.Fatalf("ledger not empty: %+v", sum)
	}
}

func TestPurgeAPIRejectsEmptyAndNonPost(t *testing.T) {
	_, ts, _ := newSerialWeb(t)
	resp, err := http.Post(ts.URL+"/api/purge", "application/json", bytes.NewBufferString(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty selector status=%d", resp.StatusCode)
	}
	r, err := http.Get(ts.URL + "/api/purge")
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET status=%d", r.StatusCode)
	}
}

func itoa(n int64) string {
	return jsonNumber(n)
}

func jsonNumber(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// A cross-site form can post text/plain without a CORS preflight; the purge
// endpoint must refuse anything that is not JSON.
func TestPurgeAPIRequiresJSONContentType(t *testing.T) {
	_, ts, _ := newSerialWeb(t)
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/purge", bytes.NewBufferString(`{"serial":1,"confirm":true}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "text/plain")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("status=%d want 415", resp.StatusCode)
	}
}

// The ledger is exportable in both formats; the row shape is what importers key on.
func TestExportSerialsSource(t *testing.T) {
	s, _, root := newSerialWeb(t)
	_, serial, _ := seedCompleted(t, s, root, 1, "photo", "image/jpeg", []byte("abc"))
	src, err := BuildExportSource(t.Context(), s, "serials", "", "", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(src.header) == 0 || src.header[0] != "serial" {
		t.Fatalf("header=%v", src.header)
	}
	rows, err := src.fetch(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows=%d", len(rows))
	}
	if len(rows[0]) != len(src.header) {
		t.Fatalf("row width %d != header width %d", len(rows[0]), len(src.header))
	}
	if rows[0][0] != jsonNumber(serial) {
		t.Fatalf("serial column=%q want %d", rows[0][0], serial)
	}
	if rows[0][2] != "in" {
		t.Fatalf("direction column=%q", rows[0][2])
	}
}
