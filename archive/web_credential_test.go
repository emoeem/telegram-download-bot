package archive

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCredentialHashRoundTrip(t *testing.T) {
	c, err := NewCredential("admin", "hunter2hunter2", 1000) // low iterations: this tests wiring, not cost
	if err != nil {
		t.Fatal(err)
	}
	if c.Hash == "" || c.Salt == "" {
		t.Fatal("NewCredential produced an empty salt or hash")
	}
	if strings.Contains(c.Hash, "hunter2") {
		t.Fatal("hash contains the plaintext")
	}
	if err := c.Verify("hunter2hunter2"); err != nil {
		t.Fatalf("correct password rejected: %v", err)
	}
	if err := c.Verify("hunter2hunter3"); !errors.Is(err, ErrCredentialMismatch) {
		t.Fatalf("wrong password err=%v want ErrCredentialMismatch", err)
	}
	if err := c.Verify(""); !errors.Is(err, ErrCredentialMismatch) {
		t.Fatalf("empty password err=%v want ErrCredentialMismatch", err)
	}
}

func TestCredentialSaltIsRandom(t *testing.T) {
	a, err := NewCredential("admin", "hunter2hunter2", 1000)
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewCredential("admin", "hunter2hunter2", 1000)
	if err != nil {
		t.Fatal(err)
	}
	if a.Salt == b.Salt || a.Hash == b.Hash {
		t.Fatal("two hashes of the same password share a salt; rainbow tables apply")
	}
	if a.Verifier() == b.Verifier() {
		t.Fatal("verifiers collide across salts")
	}
}

func TestCredentialEncodeDecode(t *testing.T) {
	c, err := NewCredential("admin", "hunter2hunter2", 1000)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeCredential(c.Encode())
	if err != nil {
		t.Fatal(err)
	}
	if got.Username != "" {
		// Username is not part of the verifier on purpose: it is stored next to
		// it, so a rename cannot silently change the password check.
		t.Fatalf("decoded verifier carried a username: %q", got.Username)
	}
	if err := got.Verify("hunter2hunter2"); err != nil {
		t.Fatalf("decoded credential rejected the password: %v", err)
	}
	if got.Verifier() != c.Verifier() {
		t.Fatal("encode/decode is not idempotent; rotating a login would invalidate sessions for no reason")
	}
}

func TestDecodeCredentialRejectsGarbage(t *testing.T) {
	for _, in := range []string{"", "pbkdf2", "pbkdf2$x$y$z", "plain:", "argon2$1$a$b"} {
		if _, err := DecodeCredential(in); err == nil {
			t.Fatalf("DecodeCredential(%q) accepted garbage", in)
		}
	}
}

func TestCredentialFileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", ".credentials")

	c, err := NewCredential("admin", "hunter2hunter2", 1000)
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveCredential(path, c); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("mode=%o want 600: any local user could read the verifier", perm)
	}
	loaded, err := LoadCredential(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Username != "admin" {
		t.Fatalf("username=%q", loaded.Username)
	}
	if loaded.Verifier() != c.Verifier() {
		t.Fatal("reloaded verifier differs; every restart would log everyone out")
	}
	if err := loaded.Verify("hunter2hunter2"); err != nil {
		t.Fatal(err)
	}
}

func TestLoadCredentialUpgradesLegacyPlaintext(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".credentials")
	// Exactly what install.sh wrote before hashing existed.
	if err := os.WriteFile(path, []byte("user=admin\npassword=hunter2hunter2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	legacy, err := LoadCredential(path)
	if err != nil {
		t.Fatal(err)
	}
	if legacy.plaintext == "" {
		t.Fatal("legacy parser did not pick up the plaintext password")
	}
	if err := legacy.Verify("hunter2hunter2"); err != nil {
		t.Fatal(err)
	}

	upgraded, err := legacy.Upgrade(path)
	if err != nil {
		t.Fatal(err)
	}
	if upgraded.Hash == "" {
		t.Fatal("upgrade did not produce a hash")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "hunter2hunter2") {
		t.Fatalf("plaintext survived the upgrade:\n%s", raw)
	}

	// And the upgraded file must still authenticate the same password.
	reloaded, err := LoadCredential(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := reloaded.Verify("hunter2hunter2"); err != nil {
		t.Fatalf("password stopped working after upgrade: %v", err)
	}
}

func TestLoadCredentialRejectsCorruptFile(t *testing.T) {
	cases := map[string]string{
		"empty":       "",
		"json-no-usr": `{"version":1,"hash":"abc"}`,
		"json-broken": `{"version":1,`,
		"legacy-nopw": "user=admin\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), ".credentials")
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadCredential(path); err == nil {
				t.Fatal("corrupt credential file was accepted; a silent reset would reopen the default password")
			}
		})
	}
}

func TestSaveCredentialRefusesMissingFields(t *testing.T) {
	dir := t.TempDir()
	if err := SaveCredential(filepath.Join(dir, "x"), Credential{Username: "", Hash: "a"}); err == nil {
		t.Fatal("saved a credential with no username")
	}
	if err := SaveCredential(filepath.Join(dir, "x"), Credential{Username: "admin"}); err == nil {
		t.Fatal("saved a credential with no hash")
	}
}

func TestValidateNewPassword(t *testing.T) {
	if err := ValidateNewPassword("short"); err == nil {
		t.Fatal("accepted a 5-character password")
	}
	if err := ValidateNewPassword("with:colon1"); err == nil {
		t.Fatal("accepted a colon; install.sh builds user:pass from it")
	}
	if err := ValidateNewPassword(" leading-space"); err == nil {
		t.Fatal("accepted leading whitespace")
	}
	if err := ValidateNewPassword("goodpassword1"); err != nil {
		t.Fatalf("rejected a valid password: %v", err)
	}
}

func TestValidateNewUsername(t *testing.T) {
	for _, bad := range []string{"", "a:b", "a\nb", strings.Repeat("x", 65)} {
		if err := ValidateNewUsername(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
	if err := ValidateNewUsername("admin2"); err != nil {
		t.Fatal(err)
	}
}

func TestBootstrapCredentialFileSeedsFromFlags(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".credentials")
	cfg, err := BootstrapCredentialFile(path, WebAuthConfig{LoginUser: "admin", LoginPassword: "hunter2hunter2"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PasswordHash == "" {
		t.Fatal("bootstrap did not record a hash")
	}
	if cfg.LoginPassword != "" {
		t.Fatal("bootstrap kept the plaintext password in the config")
	}
	if !cfg.LoginEnabled() {
		t.Fatal("bootstrap produced a config that does not enable login")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "hunter2hunter2") {
		t.Fatal("bootstrap wrote the plaintext password to disk")
	}
}

func TestBootstrapCredentialFilePrefersExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".credentials")
	first, err := BootstrapCredentialFile(path, WebAuthConfig{LoginUser: "admin", LoginPassword: "hunter2hunter2"})
	if err != nil {
		t.Fatal(err)
	}
	// A stale --auth-login from a re-run install.sh must not undo a password
	// the operator changed in the panel.
	second, err := BootstrapCredentialFile(path, WebAuthConfig{LoginUser: "admin", LoginPassword: "staleinstallpw"})
	if err != nil {
		t.Fatal(err)
	}
	if first.PasswordHash != second.PasswordHash {
		t.Fatal("an existing credential file was overwritten by the startup flag")
	}
	cred, err := DecodeCredential(second.PasswordHash)
	if err != nil {
		t.Fatal(err)
	}
	if err := cred.Verify("hunter2hunter2"); err != nil {
		t.Fatalf("file password lost: %v", err)
	}
	if err := cred.Verify("staleinstallpw"); err == nil {
		t.Fatal("stale flag password was accepted")
	}
}

func TestBootstrapCredentialFileUpgradesLegacyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".credentials")
	if err := os.WriteFile(path, []byte("user=admin\npassword=hunter2hunter2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := BootstrapCredentialFile(path, WebAuthConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.LoginEnabled() {
		t.Fatal("legacy file was not turned into a working login")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "hunter2hunter2") {
		t.Fatal("legacy plaintext survived the upgrade on disk")
	}
}

func TestBootstrapCredentialFileFailsClosedOnCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".credentials")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := BootstrapCredentialFile(path, WebAuthConfig{LoginUser: "admin", LoginPassword: "hunter2hunter2"}); err == nil {
		t.Fatal("corrupt file was ignored and the startup flag used instead")
	}
}

func TestAccountAPIReportsScheme(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".credentials")
	cfg, err := BootstrapCredentialFile(path, WebAuthConfig{LoginUser: "admin", LoginPassword: "hunter2hunter2"})
	if err != nil {
		t.Fatal(err)
	}
	_, ts := newAuthTestServer(t, cfg)
	client, jar := loginClient(t, ts.URL, "admin", "hunter2hunter2")
	_ = jar

	resp, err := client.Get(ts.URL + "/api/auth/account")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var got accountAPIResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Username != "admin" {
		t.Fatalf("username=%q", got.Username)
	}
	if !got.CanChange {
		t.Fatal("can_change=false although a credential file was configured")
	}
	if got.Scheme != "pbkdf2" {
		t.Fatalf("scheme=%q want pbkdf2", got.Scheme)
	}
	if !got.PasswordOK {
		t.Fatal("password_set=false on a configured login")
	}
	if strings.Contains(mustJSON(t, got), "hunter2") {
		t.Fatal("the status payload leaked the password")
	}
}

func TestAccountAPIRefusesWithoutCredentialFile(t *testing.T) {
	_, ts := newAuthTestServer(t, WebAuthConfig{LoginUser: "admin", LoginPassword: "hunter2hunter2"})
	client, _ := loginClient(t, ts.URL, "admin", "hunter2hunter2")

	resp, err := client.Post(ts.URL+"/api/auth/account", "application/json",
		strings.NewReader(`{"current_password":"hunter2hunter2","new_password":"brandnewpass1"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 409 {
		t.Fatalf("status=%d want 409: there is nowhere to persist the change", resp.StatusCode)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// loginClient returns a cookie-carrying client that has already signed in.
// Redirects are not followed so a failed login stays observable.
func loginClient(t *testing.T, base, user, pass string) (*http.Client, *cookiejar.Jar) {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{
		Jar:           jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.PostForm(base+loginPath, url.Values{
		"username": {user},
		"password": {pass},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login failed: status=%d", resp.StatusCode)
	}
	return client, jar
}
