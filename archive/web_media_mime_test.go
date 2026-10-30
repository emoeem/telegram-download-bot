package archive

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iyear/tdl/archive/store"
	"github.com/iyear/tdl/pkg/consts"
)

// mediaTestServer points consts.DataDir at a temp directory and seeds one
// content-addressed object plus its files row.
func mediaTestServer(t *testing.T, sha, mime string, payload []byte) *WebServer {
	t.Helper()
	old := consts.DataDir
	consts.DataDir = t.TempDir()
	t.Cleanup(func() { consts.DataDir = old })

	obj := filepath.Join(consts.DataDir, "archive", "objects", sha[:2], sha[2:4], sha)
	if err := os.MkdirAll(filepath.Dir(obj), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(obj, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(filepath.Join(t.TempDir(), "archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if _, err := s.DB().Exec("INSERT INTO files(sha256,size,mime_type,state,ref_count,created_at) VALUES(?,?,?,'ready',1,1)", sha, len(payload), mime); err != nil {
		t.Fatal(err)
	}
	return NewWebServer(s)
}

const svgSHA = "1111111111111111111111111111111111111111111111111111111111111111"
const pngSHA = "2222222222222222222222222222222222222222222222222222222222222222"

// An SVG is a script document. Served inline from the archive origin it would
// run with the operator's session cookie, so it must arrive as a download.
func TestMediaServesSVGAsAttachment(t *testing.T) {
	w := mediaTestServer(t, svgSHA, "image/svg+xml", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`))
	rec := doReq(t, w.Handler(), http.MethodGet, "/media/"+svgSHA, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Fatalf("svg content-type=%q, want application/octet-stream", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment") {
		t.Fatalf("svg content-disposition=%q, want attachment", cd)
	}
}

func TestMediaServesImageInlineWithSecurityHeaders(t *testing.T) {
	w := mediaTestServer(t, pngSHA, "image/png", []byte("\x89PNG\r\n\x1a\n"))
	rec := doReq(t, w.Handler(), http.MethodGet, "/media/"+pngSHA, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/png" {
		t.Fatalf("content-type=%q", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); cd != "" {
		t.Fatalf("image must render inline, got disposition %q", cd)
	}
	for header, want := range map[string]string{
		"X-Content-Type-Options":  "nosniff",
		"Content-Security-Policy": "sandbox",
		"Cache-Control":           "private, max-age=3600",
	} {
		if got := rec.Header().Get(header); got != want {
			t.Fatalf("%s=%q, want %q", header, got, want)
		}
	}
}

func TestIndexHasFrameAndContentSecurityPolicy(t *testing.T) {
	_, ts := newAuthTestServer(t, WebAuthConfig{})
	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("X-Frame-Options"); got != "DENY" {
		t.Fatalf("X-Frame-Options=%q", got)
	}
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Fatalf("CSP=%q", csp)
	}
}
