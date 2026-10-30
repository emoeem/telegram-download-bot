package archive

import (
	"encoding/json"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
)

func newCredentialServer(t *testing.T) (baseURL string, credPath string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".credentials")
	cfg, err := BootstrapCredentialFile(path, WebAuthConfig{
		LoginUser: "admin", LoginPassword: "hunter2hunter2",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, ts := newAuthTestServer(t, cfg)
	return ts.URL, path
}

func postAccount(t *testing.T, client *http.Client, base, body string) (int, map[string]any) {
	t.Helper()
	resp, err := client.Post(base+"/api/auth/account", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestAccountChangePasswordRotatesSessions(t *testing.T) {
	base, _ := newCredentialServer(t)
	operator, _ := loginClient(t, base, "admin", "hunter2hunter2")
	// A second device holding a cookie issued under the old password.
	other, _ := loginClient(t, base, "admin", "hunter2hunter2")

	if code := getStatus(t, other, base+"/api/stats"); code != 200 {
		t.Fatalf("other device status=%d before the change", code)
	}

	code, out := postAccount(t, operator, base,
		`{"current_password":"hunter2hunter2","new_password":"brandnewpass1","confirm_password":"brandnewpass1"}`)
	if code != 200 {
		t.Fatalf("change status=%d body=%v", code, out)
	}
	if out["ok"] != true {
		t.Fatalf("response=%v", out)
	}

	// The tab that made the change must still work: it was handed a fresh
	// cookie signed with the new key.
	if code := getStatus(t, operator, base+"/api/stats"); code != 200 {
		t.Fatalf("the changing session was logged out (status=%d); reissueSession is broken", code)
	}
	// Every other holder of an old cookie must be locked out.
	if code := getStatus(t, other, base+"/api/stats"); code != 401 {
		t.Fatalf("other device status=%d after the change; the signing key did not rotate", code)
	}

	// And the new password must be the one that works from now on.
	loginClient(t, base, "admin", "brandnewpass1")
	resp, err := noRedirect().PostForm(base+loginPath, url.Values{
		"username": {"admin"}, "password": {"hunter2hunter2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusSeeOther {
		t.Fatal("the old password still logs in")
	}
}

func getStatus(t *testing.T, client *http.Client, url string) int {
	t.Helper()
	resp, err := client.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

func TestAccountChangeRejectsWrongCurrentPassword(t *testing.T) {
	base, _ := newCredentialServer(t)
	client, _ := loginClient(t, base, "admin", "hunter2hunter2")

	code, out := postAccount(t, client, base,
		`{"current_password":"notthepassword","new_password":"brandnewpass1","confirm_password":"brandnewpass1"}`)
	if code != 401 {
		t.Fatalf("status=%d body=%v want 401", code, out)
	}
	// The password must be untouched.
	loginClient(t, base, "admin", "hunter2hunter2")
}

func TestAccountChangeValidatesNewPassword(t *testing.T) {
	base, _ := newCredentialServer(t)
	client, _ := loginClient(t, base, "admin", "hunter2hunter2")

	cases := map[string]string{
		"too-short":    `{"current_password":"hunter2hunter2","new_password":"short","confirm_password":"short"}`,
		"colon":        `{"current_password":"hunter2hunter2","new_password":"with:colon1","confirm_password":"with:colon1"}`,
		"mismatch":     `{"current_password":"hunter2hunter2","new_password":"brandnewpass1","confirm_password":"brandnewpass2"}`,
		"same-as-now":  `{"current_password":"hunter2hunter2","new_password":"hunter2hunter2","confirm_password":"hunter2hunter2"}`,
		"empty-change": `{"current_password":"hunter2hunter2"}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			code, out := postAccount(t, client, base, body)
			if code != 400 {
				t.Fatalf("status=%d body=%v want 400", code, out)
			}
		})
	}
	// All of those must have left the original password working.
	loginClient(t, base, "admin", "hunter2hunter2")
}

func TestAccountChangeUsernameOnly(t *testing.T) {
	base, _ := newCredentialServer(t)
	client, _ := loginClient(t, base, "admin", "hunter2hunter2")

	code, out := postAccount(t, client, base,
		`{"current_password":"hunter2hunter2","new_username":"operator"}`)
	if code != 200 {
		t.Fatalf("status=%d body=%v", code, out)
	}
	if out["username"] != "operator" {
		t.Fatalf("response username=%v", out["username"])
	}

	loginClient(t, base, "operator", "hunter2hunter2")
	resp, err := noRedirect().PostForm(base+loginPath, url.Values{
		"username": {"admin"}, "password": {"hunter2hunter2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusSeeOther {
		t.Fatal("the old username still logs in")
	}
}

func TestAccountChangeRequiresSession(t *testing.T) {
	base, _ := newCredentialServer(t)
	// An anonymous change attempt must not even reach the current-password
	// check, so it cannot be used to probe the credential.
	resp, err := http.Post(base+"/api/auth/account", "application/json",
		strings.NewReader(`{"current_password":"guess","new_password":"brandnewpass1"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status=%d want 401", resp.StatusCode)
	}
}

func TestAccountChangeThrottlesPasswordGuessing(t *testing.T) {
	base, _ := newCredentialServer(t)
	client, _ := loginClient(t, base, "admin", "hunter2hunter2")

	var last int
	for i := 0; i < 5; i++ {
		last, _ = postAccount(t, client, base,
			`{"current_password":"wrong-guess","new_password":"brandnewpass1","confirm_password":"brandnewpass1"}`)
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("status after 5 wrong guesses=%d want 429; the change endpoint is a free oracle", last)
	}
}

func TestAccountChangeRejectsUnknownFields(t *testing.T) {
	base, _ := newCredentialServer(t)
	client, _ := loginClient(t, base, "admin", "hunter2hunter2")
	code, _ := postAccount(t, client, base,
		`{"current_password":"hunter2hunter2","new_password":"brandnewpass1","is_admin":true}`)
	if code != 400 {
		t.Fatalf("status=%d want 400: an unknown field means the caller and the server disagree", code)
	}
}

func TestAccountChangeSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".credentials")
	cfg, err := BootstrapCredentialFile(path, WebAuthConfig{LoginUser: "admin", LoginPassword: "hunter2hunter2"})
	if err != nil {
		t.Fatal(err)
	}
	_, ts := newAuthTestServer(t, cfg)
	client, _ := loginClient(t, ts.URL, "admin", "hunter2hunter2")
	if code, out := postAccount(t, client, ts.URL,
		`{"current_password":"hunter2hunter2","new_password":"brandnewpass1","confirm_password":"brandnewpass1"}`); code != 200 {
		t.Fatalf("status=%d body=%v", code, out)
	}

	// Simulate a restart with the same stale flag install.sh still passes.
	reloaded, err := BootstrapCredentialFile(path, WebAuthConfig{LoginUser: "admin", LoginPassword: "hunter2hunter2"})
	if err != nil {
		t.Fatal(err)
	}
	_, ts2 := newAuthTestServer(t, reloaded)
	loginClient(t, ts2.URL, "admin", "brandnewpass1")
}
