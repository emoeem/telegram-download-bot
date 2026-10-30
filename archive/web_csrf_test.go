package archive

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A cross-site form can post text/plain without a CORS preflight, and Go's JSON
// decoder accepts it. The guard must reject that shape on every mutating API
// route, not only on the three handlers that used to check it themselves.
func TestCSRFGuardRejectsCrossSiteAndNonJSON(t *testing.T) {
	w, _ := exportTestServer(t)
	h := w.Handler()

	cases := []struct {
		name    string
		method  string
		body    string
		headers map[string]string
		want    int
	}{
		{
			name: "cross-site origin", method: http.MethodPost, body: `{"chat_id":99}`,
			headers: map[string]string{"Content-Type": "application/json", "Origin": "https://evil.example"},
			want:    http.StatusForbidden,
		},
		{
			name: "form content type", method: http.MethodPost, body: `{"chat_id":99}`,
			headers: map[string]string{"Content-Type": "text/plain"},
			want:    http.StatusUnsupportedMediaType,
		},
		{
			name: "same-origin json", method: http.MethodPost, body: `{"target":"@foo"}`,
			headers: map[string]string{"Content-Type": "application/json", "Origin": "http://example.com", "Sec-Fetch-Site": "same-origin"},
			want:    http.StatusServiceUnavailable, // reaches the controller gate
		},
		{
			name: "bodyless delete", method: http.MethodDelete,
			headers: map[string]string{"Origin": "http://example.com"},
			want:    http.StatusServiceUnavailable,
		},
		{
			name: "fetch metadata cross-site", method: http.MethodPost, body: `{}`,
			headers: map[string]string{"Content-Type": "application/json", "Sec-Fetch-Site": "cross-site"},
			want:    http.StatusForbidden,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, "/api/targets", strings.NewReader(tc.body))
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status=%d body=%s, want %d", rec.Code, rec.Body.String(), tc.want)
			}
		})
	}
}
