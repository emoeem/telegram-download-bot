package archive

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iyear/tdl/archive/store"
)

func newAuthTestServer(t *testing.T, auth WebAuthConfig) (*store.Store, *httptest.Server) {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(NewWebServer(s, auth).Handler())
	t.Cleanup(func() { ts.Close(); s.Close() })
	return s, ts
}

func TestWebAuthTokenRequired(t *testing.T) {
	_, ts := newAuthTestServer(t, WebAuthConfig{Token: "secret-token"})
	resp, err := http.Get(ts.URL + "/api/messages/page?page=1&page_size=5")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if got := resp.Header.Get("WWW-Authenticate"); got != "Bearer" {
		t.Fatalf("WWW-Authenticate=%q", got)
	}
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/messages/page?page=1&page_size=5", nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("authorized status=%d", resp.StatusCode)
	}
}

func TestWebAuthBasicRequired(t *testing.T) {
	_, ts := newAuthTestServer(t, WebAuthConfig{BasicUser: "admin", BasicPassword: "pass"})
	resp, err := http.Get(ts.URL + "/messages")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if got := resp.Header.Get("WWW-Authenticate"); got != "Basic realm="+string([]byte{34})+"tdl-archive"+string([]byte{34}) {
		t.Fatalf("WWW-Authenticate=%q", got)
	}
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/messages", nil)
	req.SetBasicAuth("admin", "pass")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("authorized status=%d", resp.StatusCode)
	}
}

func TestWebAuthHealthzExempt(t *testing.T) {
	_, ts := newAuthTestServer(t, WebAuthConfig{Token: "secret-token"})
	for _, path := range []string{"/healthz", "/readyz"} {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", path, resp.StatusCode, body)
		}
		if !strings.Contains(string(body), "status") {
			t.Fatalf("%s body=%s", path, body)
		}
	}
}
