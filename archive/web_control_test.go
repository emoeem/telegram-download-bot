package archive

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iyear/tdl/archive/store"
)

func newControlTestServer(t *testing.T, ctl *Controller, auth WebAuthConfig) (*store.Store, *httptest.Server) {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(NewManagedWebServer(s, ctl, auth).Handler())
	t.Cleanup(func() { ts.Close(); s.Close() })
	return s, ts
}

func TestWebControlUnmanaged(t *testing.T) {
	_, ts := newControlTestServer(t, nil, WebAuthConfig{})

	resp, err := http.Get(ts.URL + "/api/auth/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var status authStatusAPIResponse
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	if status.Managed {
		t.Fatalf("expected unmanaged server, got %+v", status)
	}
	if status.Hint == "" {
		t.Fatal("expected a hint for read-only mode")
	}

	// Control-plane endpoints must be rejected with 503 when no controller.
	for _, tc := range []struct {
		method, path string
		body         string
	}{
		{http.MethodPost, "/api/targets", `{"target":"@foo"}`},
		{http.MethodDelete, "/api/targets?chat_id=1", ""},
		{http.MethodPost, "/api/monitor/sync", `{"target":"@foo"}`},
		{http.MethodPost, "/api/login/qr/start", ""},
		{http.MethodPost, "/api/login/password", `{"password":"x"}`},
		{http.MethodGet, "/api/jobs", ""},
	} {
		req, _ := http.NewRequest(tc.method, ts.URL+tc.path, strings.NewReader(tc.body))
		if tc.body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", tc.method, tc.path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("%s %s: status=%d body=%s", tc.method, tc.path, resp.StatusCode, body)
		}
		if !strings.Contains(string(body), "management disabled") {
			t.Fatalf("%s %s: body should explain management is disabled, got %q", tc.method, tc.path, body)
		}
	}
}

func TestWebControlRequiresAuth(t *testing.T) {
	_, ts := newControlTestServer(t, &Controller{}, WebAuthConfig{Token: "secret"})

	// Control endpoints are behind the same auth middleware as the viewer.
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/login/qr/start", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated login start: status=%d", resp.StatusCode)
	}

	req, _ = http.NewRequest(http.MethodGet, ts.URL+"/api/auth/status", nil)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var status authStatusAPIResponse
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	if !status.Managed {
		t.Fatalf("expected managed server, got %+v", status)
	}
}

func TestWebControlControllerValidation(t *testing.T) {
	// A zero-value controller has no credentials: management endpoints must
	// fail fast with a clear error and never attempt a network connection.
	_, ts := newControlTestServer(t, &Controller{}, WebAuthConfig{})

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/login/qr/start", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("qr start without creds: status=%d", resp.StatusCode)
	}
	if !strings.Contains(string(body), "app-id") {
		t.Fatalf("qr start error should mention credentials, got %q", body)
	}

	req, _ = http.NewRequest(http.MethodPost, ts.URL+"/api/targets", bytes.NewBufferString(`{"target":"@foo"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("add monitor without creds: status=%d", resp.StatusCode)
	}
	if !strings.Contains(string(body), "app-id") {
		t.Fatalf("add monitor error should mention credentials, got %q", body)
	}

	// add target validation: missing target field is a 400, not a 409.
	req, _ = http.NewRequest(http.MethodPost, ts.URL+"/api/targets", bytes.NewBufferString(`{}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	// zero-value controller fails credential check first; both 400 and 409 are
	// acceptable here as long as it is not a 5xx crash.
	if resp.StatusCode >= 500 {
		t.Fatalf("empty target should not crash: status=%d", resp.StatusCode)
	}
}

func TestControllerStateHelpers(t *testing.T) {
	c := &Controller{}

	if err := c.checkCreds(); err == nil {
		t.Fatal("expected credential check to fail")
	}
	if st := c.QRState(); st != QRIdle {
		t.Fatalf("qr state = %q, want idle", st)
	}
	if err := c.SubmitPassword("x"); err == nil {
		t.Fatal("expected SubmitPassword without challenge to fail")
	}
	if jobs := c.Jobs(); len(jobs) != 0 {
		t.Fatalf("expected no jobs, got %v", jobs)
	}

	// QR login state transitions with a pending challenge.
	c.qrCancel = func() {}
	c.qrPwd = make(chan string, 1)
	c.passwordMode = true
	if st := c.QRState(); st != QRPasswordNeeded {
		t.Fatalf("qr state = %q, want password_needed", st)
	}
	if err := c.SubmitPassword("x"); err != nil {
		t.Fatalf("SubmitPassword: %v", err)
	}
	if err := c.SubmitPassword("y"); err == nil {
		t.Fatal("second SubmitPassword should be rejected while the first is pending")
	}
	select {
	case p := <-c.qrPwd:
		if p != "x" {
			t.Fatalf("password = %q", p)
		}
	default:
		t.Fatal("password not delivered")
	}
}

func TestParseSinceValue(t *testing.T) {
	if v, err := parseSinceValue(""); err != nil || v != nil {
		t.Fatalf("empty: %v %v", v, err)
	}
	v, err := parseSinceValue("2026-01-02")
	if err != nil || v == nil || v.Year() != 2026 {
		t.Fatalf("date: %v %v", v, err)
	}
	if v, err = parseSinceValue("2026-01-02T03:04:05Z"); err != nil || v == nil {
		t.Fatalf("rfc3339: %v %v", v, err)
	}
	if _, err = parseSinceValue("not-a-date"); err == nil {
		t.Fatal("expected error for invalid since")
	}
}
