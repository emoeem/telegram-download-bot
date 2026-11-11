package archive

import (
	"net/http"
	"net/url"
	"strings"
)

// csrfGuard blocks cross-site state changes on the JSON API.
//
// Two independent checks, because each covers a different gap:
//   - A JSON content type is required whenever there is a body. HTML forms can
//     only send urlencoded, multipart or text/plain, and Go's JSON decoder would
//     happily accept a text/plain body, so without this a cross-site form can
//     post valid JSON.
//   - The Origin (or Sec-Fetch-Site, when Origin is absent) must match this
//     host. Basic-auth credentials are attached by the browser automatically,
//     so an origin check is the only server-side line of defence for that mode.
//
// Non-browser clients (curl, scripts) send neither header and are allowed: they
// are not the CSRF threat model.
func (w *WebServer) csrfGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(rw, r)
			return
		}
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(rw, r)
			return
		}
		if !sameOrigin(r) {
			http.Error(rw, "cross-origin request rejected", http.StatusForbidden)
			return
		}
		if r.ContentLength != 0 {
			ct := strings.ToLower(r.Header.Get("Content-Type"))
			if !strings.HasPrefix(ct, "application/json") {
				http.Error(rw, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
				return
			}
		}
		next.ServeHTTP(rw, r)
	})
}

// sameOrigin reports whether a request came from this same host. A request with
// no Origin and no Sec-Fetch-Site header is a non-browser client and is allowed.
//
// "null" is treated as absent: a document with an opaque origin (Android WebView
// posting back to 127.0.0.1, sandboxed frames, some redirect chains) sends that
// literal string, and parsing it would otherwise yield an empty host and reject
// the very clients this guard exists to protect.
func sameOrigin(r *http.Request) bool {
	if origin := r.Header.Get("Origin"); origin != "" && origin != "null" {
		u, err := url.Parse(origin)
		if err != nil || u.Host == "" {
			return false
		}
		return strings.EqualFold(u.Host, r.Host)
	}
	switch r.Header.Get("Sec-Fetch-Site") {
	case "", "same-origin", "none":
		return true
	default:
		return false
	}
}
