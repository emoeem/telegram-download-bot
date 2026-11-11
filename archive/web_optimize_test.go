package archive

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iyear/tdl/archive/store"
	"github.com/iyear/tdl/license"
)

// SerialSummary 是 UI 轮询页的全表聚合，必须走 5s 缓存
// （与 metricsSnapshot 同款语义）：TTL 内的第二次调用不得看到库里的新行。
func TestSerialSummaryCacheTTL(t *testing.T) {
	s := openTestStore(t)
	w := NewWebServer(s)
	ctx := context.Background()

	seed := func(msgID int) {
		m := store.Message{ChatID: -100, MessageID: msgID,
			Date: time.Unix(1700000000, 0), HasMedia: true}
		if err := s.UpsertMessage(ctx, m); err != nil {
			t.Fatal(err)
		}
		if err := s.EnsureMedia(ctx, m, store.MediaMeta{Kind: "photo", Name: "p.jpg", MIME: "image/jpeg", Size: 1024}); err != nil {
			t.Fatal(err)
		}
	}
	seed(1)

	first, err := w.serialSummary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	seed(2)
	second, err := w.serialSummary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if first.Total != second.Total {
		t.Fatalf("TTL 内缓存未命中: first=%+v second=%+v", first, second)
	}
}

// 登录限流器的过期项必须在 fail() 里顺带回收，否则攻击者轮换源地址
// 会让 fails map 随服务生命周期无限增长。
func TestLoginLimiterReapsExpiredEntries(t *testing.T) {
	var l loginLimiter
	now := time.Now()
	l.fail("attacker-1", now.Add(-2*time.Hour)) // 1 小时空闲线早已过
	l.fail("attacker-2", now.Add(-3*time.Hour))

	l.fail("victim", now) // 触发回收

	if n := len(l.fails); n != 1 {
		l.mu.Lock()
		defer l.mu.Unlock()
		t.Fatalf("过期项应被回收, 剩 %d 项", n)
	}
	if _, ok := l.fails["victim"]; !ok {
		t.Fatal("当前失败项不得被回收")
	}
}

// licence 到期核对必须单飞：到期边界上的一批并发请求只放一个出网 verify，
// 其余直接走陈旧但有效的本地激活（否则 = N 个并发上游调用 + N 个请求阻塞）。
func TestLicenceRefreshIsSingleFlight(t *testing.T) {
	var verifies atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/activate":
			_, _ = rw.Write([]byte(`{"ok":true}`))
		case "/v1/verify":
			verifies.Add(1)
			time.Sleep(150 * time.Millisecond) // 拉长窗口让并发叠加
			_, _ = rw.Write([]byte(`{"ok":true}`))
		default:
			http.NotFound(rw, r)
		}
	}))
	defer upstream.Close()

	clk := &testClock{}
	clk.Set(time.Now().UTC())
	// 复用 newLicenceTestServerInner 的组装，但把 LicenceServer 指向计数上游
	master, mgr, ts, _ := func() ([]byte, *license.Manager, *httptest.Server, *store.Store) {
		t.Helper()
		master, err := license.NewMasterKey()
		if err != nil {
			t.Fatalf("NewMasterKey: %v", err)
		}
		ver, err := license.NewVerifier(license.EncodeHMACKey(master), license.EncodePublicKey(master))
		if err != nil {
			t.Fatalf("NewVerifier: %v", err)
		}
		mgr := license.NewManager(t.TempDir(), ver, license.DeviceCode("test-device"))
		s, err := store.Open(t.TempDir() + "/archive.db")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		w := NewWebServer(s)
		w.Licence = mgr
		w.LicenceServer = upstream.URL
		w.Now = clk.Now
		return master, mgr, httptest.NewServer(w.Handler()), s
	}()
	defer ts.Close()

	// 在线激活：form POST → /v1/activate → rememberOnline(MarkChecked)
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
	resp.Body.Close()

	// 时钟推过核对间隔 → NeedsOnlineCheck 为真
	clk.Set(time.Now().Add(13 * time.Hour).UTC())

	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/stats", nil)
			resp, err := c.Do(req)
			if err == nil {
				resp.Body.Close()
			}
		}()
	}
	wg.Wait()

	if got := verifies.Load(); got != 1 {
		t.Fatalf("到期核对应只发出一次, got %d", got)
	}
}

// 激活表单在 csrfGuard 之外，必须自带同源校验。
func TestActivateFormRejectsCrossOriginPost(t *testing.T) {
	master, mgr, ts, _ := newLicenceTestServer(t)
	key, _, err := license.IssueShort(master, mgr.DeviceCode(), 30)
	if err != nil {
		t.Fatalf("IssueShort: %v", err)
	}
	form := url.Values{"key": {key}, "next": {"/"}}
	req, _ := http.NewRequest(http.MethodPost, ts.URL+activatePath, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://evil.example")
	resp, err := noRedirectClient(t).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("跨站激活表单状态 = %d，应为 403", resp.StatusCode)
	}
}

// Android WebView 提交回环文档时会发 Origin: null，反向代理会把 Host 改写成
// 上游地址 —— 两种真实客户端都过不了头校验。表单里的进程级 token 必须能
// 独立证明"这个请求来自本进程渲染的页面"。
func TestActivateFormAcceptsIssuedToken(t *testing.T) {
	cases := []struct {
		name    string
		headers map[string]string
		csrf    bool // true = 使用页面签发的 token
		want    int
	}{
		{
			name:    "webview opaque origin",
			headers: map[string]string{"Origin": "null", "Sec-Fetch-Site": "cross-site"},
			csrf:    true,
			want:    http.StatusSeeOther,
		},
		{
			name:    "reverse proxy rewrote host",
			headers: map[string]string{"Origin": "https://ui.example", "Sec-Fetch-Site": "same-origin"},
			csrf:    true,
			want:    http.StatusSeeOther,
		},
		{
			name:    "forged token",
			headers: map[string]string{"Origin": "https://evil.example"},
			csrf:    false,
			want:    http.StatusForbidden,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			master, mgr, ts, _ := newLicenceTestServer(t)
			c := noRedirectClient(t)

			token := fetchActivateToken(t, c, ts.URL+activatePath)
			if token == "" {
				t.Fatal("激活页没有签发 csrf token")
			}
			if !tc.csrf {
				token = "deadbeef"
			}

			key, _, err := license.IssueShort(master, mgr.DeviceCode(), 30)
			if err != nil {
				t.Fatalf("IssueShort: %v", err)
			}
			form := url.Values{"key": {key}, "next": {"/"}, "csrf": {token}}
			req, _ := http.NewRequest(http.MethodPost, ts.URL+activatePath, strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			resp, err := c.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Fatalf("状态 = %d，应为 %d", resp.StatusCode, tc.want)
			}
		})
	}
}

// fetchActivateToken 从渲染出的表单里取回隐藏字段。
func fetchActivateToken(t *testing.T, c *http.Client, url string) string {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	const marker = `name="csrf" value="`
	page := string(body)
	i := strings.Index(page, marker)
	if i < 0 {
		return ""
	}
	rest := page[i+len(marker):]
	j := strings.IndexByte(rest, '"')
	if j < 0 {
		return ""
	}
	return rest[:j]
}
