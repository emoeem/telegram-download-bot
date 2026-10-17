package archive

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// rawRequest 绕过 net/http 客户端的路径归一化，把原始请求行直接发给服务器，
// 用来验证“含 ../ 的路径”在服务端的行为（ServeMux 会清洗 → 301，handler 正则兜底 → 404）。
func rawRequest(t *testing.T, ts *httptest.Server, target string) (int, string) {
	t.Helper()
	conn, err := net.Dial("tcp", ts.Listener.Addr().String())
	if err != nil {
		t.Fatalf("dial error = %v", err)
	}
	defer conn.Close()
	if _, err := fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n", target); err != nil {
		t.Fatalf("write request error = %v", err)
	}
	raw, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("read response error = %v", err)
	}
	text := string(raw)
	status := 0
	if _, err := fmt.Sscanf(text, "HTTP/1.1 %d", &status); err != nil {
		t.Fatalf("bad status line: %q", text)
	}
	return status, text
}

func getPath(t *testing.T, ts *httptest.Server, path string) (int, string) {
	t.Helper()
	resp, err := http.Get(ts.URL + path)
	if err != nil {
		t.Fatalf("GET %s error = %v", path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// /media/ 只接受 64 位十六进制 sha256，任何路径穿越/非法输入都必须 404。
func TestMediaEndpointRejectsInvalidPaths(t *testing.T) {
	_, ts := newAuthTestServer(t, WebAuthConfig{})
	cases := []string{
		"/media/",
		"/media/short",
		"/media/%2e%2e%2f%2e%2e%2fetc%2fpasswd",
		"/media/thumb/not-a-sha",
		"/media/preview/zzzz",
		"/media/" + strings.Repeat("A", 64), // 大写不是 sha256 十六进制小写形式
		"/media/" + strings.Repeat("a", 63),
	}
	for _, path := range cases {
		status, _ := getPath(t, ts, path)
		if status != http.StatusNotFound {
			t.Errorf("GET %s status = %d, want 404", path, status)
		}
	}

	// 未编码的 ../ 由 ServeMux 清洗（301）而不是落到 handler；无论如何不得读到文件内容。
	status, body := rawRequest(t, ts, "/media/../etc/passwd")
	if status == http.StatusOK && strings.Contains(body, "root:") {
		t.Fatalf("path traversal leaked file content: %d %q", status, body)
	}
	if status != http.StatusNotFound && (status < 300 || status >= 400) {
		t.Fatalf("traversal status = %d, want 3xx (ServeMux clean) or 404", status)
	}
}

// 合法 sha 但对象不存在 → 404（不得泄露目录信息）。
func TestMediaEndpointMissingObject(t *testing.T) {
	_, ts := newAuthTestServer(t, WebAuthConfig{})
	status, body := getPath(t, ts, "/media/"+strings.Repeat("a", 64))
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body=%s)", status, body)
	}
}

// /avatar/ 必须是 <kind>/<id> 两段且 id 为整数。
func TestAvatarEndpointRejectsInvalidPaths(t *testing.T) {
	_, ts := newAuthTestServer(t, WebAuthConfig{})
	cases := []string{
		"/avatar/",
		"/avatar/user",
		"/avatar/user/abc",
		"/avatar/user/1/extra",
		"/avatar/%2e%2e%2f%2e%2e%2fetc%2fpasswd/1",
	}
	for _, path := range cases {
		status, _ := getPath(t, ts, path)
		if status != http.StatusNotFound {
			t.Errorf("GET %s status = %d, want 404", path, status)
		}
	}

	status, body := rawRequest(t, ts, "/avatar/../../etc/passwd/1")
	if status == http.StatusOK && strings.Contains(body, "root:") {
		t.Fatalf("avatar traversal leaked file content: %d %q", status, body)
	}
}

// 无认证配置时本地只读 API 必须可用。
func TestReadOnlyAPIsServeJSON(t *testing.T) {
	_, ts := newAuthTestServer(t, WebAuthConfig{})
	for _, path := range []string{"/api/stats", "/api/targets", "/api/messages/page?page=1&page_size=5", "/api/search?q=test"} {
		status, body := getPath(t, ts, path)
		if status != http.StatusOK {
			t.Errorf("GET %s status = %d (body=%s)", path, status, body)
			continue
		}
		trimmed := strings.TrimSpace(body)
		if !strings.HasPrefix(trimmed, "{") && !strings.HasPrefix(trimmed, "[") {
			t.Errorf("GET %s body is not JSON: %s", path, body)
		}
	}
}

// 空结果的列表接口必须返回 [] 而不是 null（前端/脚本可直接遍历）。
func TestEmptyListEndpointsReturnArrays(t *testing.T) {
	_, ts := newAuthTestServer(t, WebAuthConfig{})
	for _, path := range []string{"/api/targets", "/api/search?q=nothingmatches", "/api/destinations"} {
		status, body := getPath(t, ts, path)
		if status != http.StatusOK {
			t.Errorf("GET %s status = %d (body=%s)", path, status, body)
			continue
		}
		if strings.TrimSpace(body) == "null" {
			t.Errorf("GET %s returned null, want []", path)
		}
	}
}
