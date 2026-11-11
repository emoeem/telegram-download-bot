package archive

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iyear/tdl/archive/store"
	"github.com/iyear/tdl/license"
)

// testClock is a settable clock for the licence gate.
type testClock struct{ v atomic.Int64 }

func (c *testClock) Now() time.Time  { return time.Unix(0, c.v.Load()).UTC() }
func (c *testClock) Set(t time.Time) { c.v.Store(t.UnixNano()) }

// newLicenceTestServer builds a server whose licence gate is active, plus the
// master key so the test can issue keys the server will accept.
func newLicenceTestServer(t *testing.T) (master []byte, mgr *license.Manager, ts *httptest.Server, s *store.Store) {
	t.Helper()
	return newLicenceTestServerInner(t, nil)
}

// newLicenceTestServerWithClock returns a server whose gate clock the test can
// move, so expiry can be crossed without sleeping.
func newLicenceTestServerWithClock(t *testing.T) (master []byte, mgr *license.Manager, ts *httptest.Server, clk *testClock) {
	t.Helper()
	clk = &testClock{}
	clk.Set(time.Now().UTC())
	master, mgr, ts, _ = newLicenceTestServerInner(t, clk)
	return master, mgr, ts, clk
}

func newLicenceTestServerInner(t *testing.T, clk *testClock) (master []byte, mgr *license.Manager, ts *httptest.Server, s *store.Store) {
	t.Helper()
	master, err := license.NewMasterKey()
	if err != nil {
		t.Fatalf("NewMasterKey: %v", err)
	}
	ver, err := license.NewVerifier(license.EncodeHMACKey(master), license.EncodePublicKey(master))
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	device := license.DeviceCode("test-device")
	mgr = license.NewManager(t.TempDir(), ver, device)

	s, err = store.Open(filepath.Join(t.TempDir(), "archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	w := NewWebServer(s)
	w.Licence = mgr
	if clk != nil {
		w.Now = clk.Now
	}
	ts = httptest.NewServer(w.Handler())
	t.Cleanup(func() { ts.Close(); s.Close() })
	return master, mgr, ts, s
}

// noRedirectClient surfaces the 303 instead of following it, which is what these
// tests assert on.
func noRedirectClient(t *testing.T) *http.Client {
	t.Helper()
	return &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func TestLicenceGateRedirectsPageToActivate(t *testing.T) {
	_, _, ts, _ := newLicenceTestServer(t)
	c := noRedirectClient(t)
	resp, err := c.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("未激活访问首页状态 = %d，应为 303", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if !strings.HasPrefix(loc, activatePath) {
		t.Fatalf("Location = %q，应指向 %s", loc, activatePath)
	}
	// The original destination must survive so activation lands there.
	if !strings.Contains(loc, "next=") {
		t.Fatalf("Location = %q，应携带 next 参数", loc)
	}
}

func TestLicenceGateRejectsAPIWith402(t *testing.T) {
	_, _, ts, _ := newLicenceTestServer(t)
	resp, err := http.Get(ts.URL + "/api/stats")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("未激活访问 API 状态 = %d，应为 402", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"ok":false`) {
		t.Fatalf("API 应返回 JSON 错误，实际 %s", body)
	}
}

func TestActivatePageReachableWhileLocked(t *testing.T) {
	_, mgr, ts, _ := newLicenceTestServer(t)
	resp, err := http.Get(ts.URL + activatePath)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("激活页状态 = %d，应为 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), mgr.ThisDevice()) {
		t.Fatalf("激活页应显示本机设备码 %s", mgr.ThisDevice())
	}
	if !strings.Contains(string(body), `name="key"`) {
		t.Fatal("激活页应包含密钥输入框")
	}
}

func TestActivateWithValidKeyUnlocks(t *testing.T) {
	master, mgr, ts, _ := newLicenceTestServer(t)
	key, _, err := license.IssueShort(master, mgr.DeviceCode(), 30)
	if err != nil {
		t.Fatalf("IssueShort: %v", err)
	}
	c := noRedirectClient(t)
	form := url.Values{"key": {key}, "next": {"/"}}
	resp, err := c.PostForm(ts.URL+activatePath, form)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("激活状态 = %d，应为 303（body=%s）", resp.StatusCode, body)
	}

	// Now the archive answers.
	resp2, err := c.Get(ts.URL + "/api/stats")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("激活后 API 状态 = %d，应为 200", resp2.StatusCode)
	}
}

func TestActivateWithWrongDeviceFails(t *testing.T) {
	master, _, ts, _ := newLicenceTestServer(t)
	// A key for a different machine.
	key, _, err := license.IssueShort(master, license.DeviceCode("other-machine"), 30)
	if err != nil {
		t.Fatalf("IssueShort: %v", err)
	}
	resp, err := http.PostForm(ts.URL+activatePath, url.Values{"key": {key}})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态 = %d，应为 200（渲染表单并显示错误）", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "另一台设备") {
		t.Fatalf("应提示设备不匹配，实际 body 片段：%s", snippet(string(body), "err"))
	}
}

func TestActivateWithGarbageFails(t *testing.T) {
	_, _, ts, _ := newLicenceTestServer(t)
	resp, err := http.PostForm(ts.URL+activatePath, url.Values{"key": {"HELLO-WORLD"}})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "格式不正确") {
		t.Fatalf("应提示格式不正确，实际 body 片段：%s", snippet(string(body), "err"))
	}
}

func TestLicenceStatusAPI(t *testing.T) {
	master, mgr, ts, _ := newLicenceTestServer(t)
	resp, err := http.Get(ts.URL + licenceStatusPath)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), `"activated":false`) {
		t.Fatalf("未激活状态应为 activated:false，实际 %s", body)
	}

	key, _, _ := license.IssueShort(master, mgr.DeviceCode(), 10)
	if _, err := mgr.Activate(key, time.Now()); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	resp, err = http.Get(ts.URL + licenceStatusPath)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ = io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"valid":true`) {
		t.Fatalf("激活后状态应为 valid:true，实际 %s", body)
	}
	if !strings.Contains(string(body), `"this_device":"`+mgr.ThisDevice()+`"`) {
		t.Fatalf("状态应包含本机设备码，实际 %s", body)
	}
}

func TestStaticAssetsPassThroughGate(t *testing.T) {
	_, _, ts, _ := newLicenceTestServer(t)
	for _, p := range []string{"/static/theme.css", "/manifest.webmanifest", "/sw.js"} {
		resp, err := http.Get(ts.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s 状态 = %d，未激活时静态资源应放行", p, resp.StatusCode)
		}
	}
}

func TestHealthProbesBypassGate(t *testing.T) {
	_, _, ts, _ := newLicenceTestServer(t)
	for _, p := range []string{"/healthz"} {
		resp, err := http.Get(ts.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s 状态 = %d，健康检查应放行", p, resp.StatusCode)
		}
	}
}

// TestNoGateWithoutVerifier is the compatibility guarantee: a server that was
// never given a manager behaves exactly as it did before this feature existed.
func TestNoGateWithoutVerifier(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ts := httptest.NewServer(NewWebServer(s).Handler())
	defer ts.Close()

	c := noRedirectClient(t)
	resp, err := c.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusSeeOther {
		t.Fatalf("没有 licence manager 时不应跳转激活页")
	}
}

// TestUnconfiguredManagerDoesNotGate covers a binary built without verifier
// material: the manager exists but cannot verify anything, so the gate must
// stand aside rather than lock the customer out of a build that has no way to
// let them in.
func TestUnconfiguredManagerDoesNotGate(t *testing.T) {
	ver, err := license.NewVerifier("", "")
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(filepath.Join(t.TempDir(), "archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	w := NewWebServer(s)
	w.Licence = license.NewManager(t.TempDir(), ver, license.DeviceCode("x"))
	ts := httptest.NewServer(w.Handler())
	defer ts.Close()

	c := noRedirectClient(t)
	resp, err := c.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusSeeOther {
		t.Fatal("未注入校验材料的构建不应拦截请求")
	}
}

// TestActivateAPIReturnsJSON covers the JSON path the app and scripts use.
func TestActivateAPIReturnsJSON(t *testing.T) {
	master, mgr, ts, _ := newLicenceTestServer(t)
	key, _, _ := license.IssueShort(master, mgr.DeviceCode(), 5)
	resp, err := http.Post(ts.URL+licenceActivatePath, "application/json",
		strings.NewReader(`{"key":"`+key+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态 = %d，body=%s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), `"ok":true`) {
		t.Fatalf("应返回 ok:true，实际 %s", body)
	}
}

// TestExpiredLicenceLocksAgain proves the gate re-checks on every request
// rather than trusting "it was activated once". The clock is injected so the
// expiry boundary is crossed deterministically instead of after a real day.
func TestExpiredLicenceLocksAgain(t *testing.T) {
	master, mgr, ts, clk := newLicenceTestServerWithClock(t)
	key, payload, err := license.IssueShort(master, mgr.DeviceCode(), 1)
	if err != nil {
		t.Fatal(err)
	}
	base := clk.Now()
	if _, err := mgr.Activate(key, base); err != nil {
		t.Fatalf("Activate: %v", err)
	}

	c := noRedirectClient(t)
	resp, err := c.Get(ts.URL + "/api/stats")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("有效期内 API 状态 = %d，应为 200", resp.StatusCode)
	}

	// Jump the server clock past the expiry instant.
	clk.Set(payload.ExpiresAt().Add(24 * time.Hour))
	resp, err = c.Get(ts.URL + "/api/stats")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("过期后 API 状态 = %d，应为 402", resp.StatusCode)
	}

	// And the page route goes back to the activation form.
	resp, err = c.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("过期后首页状态 = %d，应为 303", resp.StatusCode)
	}
}

// snippet extracts a short window around a marker for readable failures.
func snippet(s, marker string) string {
	i := strings.Index(s, marker)
	if i < 0 {
		if len(s) > 200 {
			return s[:200]
		}
		return s
	}
	end := i + 200
	if end > len(s) {
		end = len(s)
	}
	return s[i:end]
}
