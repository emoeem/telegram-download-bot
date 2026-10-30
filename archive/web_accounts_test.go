package archive

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newAccountServer(t *testing.T) (*WebServer, *AccountManager) {
	t.Helper()
	root := newFakeKV()
	mgr, err := NewAccountManager(context.Background(), root, t.TempDir(), 0, "", nil)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	if err := mgr.Load(context.Background()); err != nil {
		t.Fatalf("load: %v", err)
	}
	t.Cleanup(mgr.Close)
	// No auth config: authMiddleware passes everything through, which keeps the
	// test focused on the account endpoints rather than on session cookies.
	return &WebServer{Accounts: mgr}, mgr
}

func doReq(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var rd *strings.Reader
	if body == "" {
		rd = strings.NewReader("")
	} else {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rd)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

type accountsEnvelope struct {
	Status   string        `json:"status"`
	ID       string        `json:"id"`
	Active   string        `json:"active"`
	Accounts []accountJSON `json:"accounts"`
	Managed  bool          `json:"managed"`
}

func decodeEnvelope(t *testing.T, rec *httptest.ResponseRecorder) accountsEnvelope {
	t.Helper()
	var out accountsEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return out
}

// A server without an AccountManager must say so instead of returning an empty
// list, which would read as "you have no accounts" rather than "unsupported".
func TestAccountsAPIUnmanaged(t *testing.T) {
	w := &WebServer{}
	rec := doReq(t, w.Handler(), http.MethodGet, "/api/accounts", "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestAccountsAPIListEmpty(t *testing.T) {
	w, _ := newAccountServer(t)
	rec := doReq(t, w.Handler(), http.MethodGet, "/api/accounts", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	got := decodeEnvelope(t, rec)
	if !got.Managed {
		t.Fatal("managed = false, want true")
	}
	if len(got.Accounts) != 0 {
		t.Fatalf("got %d accounts, want 0", len(got.Accounts))
	}
}

func TestAccountsAPICreate(t *testing.T) {
	w, _ := newAccountServer(t)
	rec := doReq(t, w.Handler(), http.MethodPost, "/api/accounts", `{"name":"工作号"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	got := decodeEnvelope(t, rec)
	if got.Status != "created" || got.ID == "" {
		t.Fatalf("envelope = %+v", got)
	}
	if len(got.Accounts) != 1 || got.Accounts[0].Name != "工作号" {
		t.Fatalf("accounts = %+v", got.Accounts)
	}
	if !got.Accounts[0].Active {
		t.Fatal("newly created account is not active")
	}
	if got.Accounts[0].HasSession {
		t.Fatal("a brand-new account reports an existing session")
	}
}

// Creating an account with no body must still work: the name is optional and
// the UI lets people add first, name later.
func TestAccountsAPICreateWithoutBody(t *testing.T) {
	w, _ := newAccountServer(t)
	rec := doReq(t, w.Handler(), http.MethodPost, "/api/accounts", "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	got := decodeEnvelope(t, rec)
	if len(got.Accounts) != 1 || strings.TrimSpace(got.Accounts[0].Name) == "" {
		t.Fatalf("accounts = %+v, want one with a generated name", got.Accounts)
	}
}

func TestAccountsAPISwitch(t *testing.T) {
	w, mgr := newAccountServer(t)
	ctx := context.Background()
	first, err := mgr.Add(ctx, "first")
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if _, err := mgr.Add(ctx, "second"); err != nil {
		t.Fatalf("add: %v", err)
	}

	rec := doReq(t, w.Handler(), http.MethodPost, "/api/accounts/"+first.ID+"/switch", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	got := decodeEnvelope(t, rec)
	if got.Status != "switched" || got.Active != first.ID {
		t.Fatalf("envelope = %+v, want active %q", got, first.ID)
	}
}

func TestAccountsAPISwitchUnknown(t *testing.T) {
	w, _ := newAccountServer(t)
	rec := doReq(t, w.Handler(), http.MethodPost, "/api/accounts/nope/switch", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestAccountsAPILogout(t *testing.T) {
	w, mgr := newAccountServer(t)
	ctx := context.Background()
	a, err := mgr.Add(ctx, "main")
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	rec := doReq(t, w.Handler(), http.MethodPost, "/api/accounts/"+a.ID+"/logout", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	got := decodeEnvelope(t, rec)
	if got.Status != "signed_out" {
		t.Fatalf("status field = %q, want signed_out", got.Status)
	}
	// Signing out must not remove the account from the list.
	if len(got.Accounts) != 1 {
		t.Fatalf("accounts after logout = %+v, want the account kept", got.Accounts)
	}
}

func TestAccountsAPIRename(t *testing.T) {
	w, mgr := newAccountServer(t)
	ctx := context.Background()
	a, err := mgr.Add(ctx, "old")
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	rec := doReq(t, w.Handler(), http.MethodPost, "/api/accounts/"+a.ID+"/rename", `{"name":"new"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	got := decodeEnvelope(t, rec)
	if got.Accounts[0].Name != "new" {
		t.Fatalf("name = %q, want new", got.Accounts[0].Name)
	}
}

func TestAccountsAPIRenameEmpty(t *testing.T) {
	w, mgr := newAccountServer(t)
	ctx := context.Background()
	a, err := mgr.Add(ctx, "keep")
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	rec := doReq(t, w.Handler(), http.MethodPost, "/api/accounts/"+a.ID+"/rename", `{"name":"  "}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if got, _ := mgr.Get(a.ID); got.Name != "keep" {
		t.Fatalf("name = %q, want keep", got.Name)
	}
}

func TestAccountsAPIRemove(t *testing.T) {
	w, mgr := newAccountServer(t)
	ctx := context.Background()
	a, err := mgr.Add(ctx, "gone")
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	rec := doReq(t, w.Handler(), http.MethodDelete, "/api/accounts/"+a.ID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	got := decodeEnvelope(t, rec)
	if got.Status != "removed" || len(got.Accounts) != 0 {
		t.Fatalf("envelope = %+v, want removed and empty", got)
	}
}

// A stale id in the URL is a client bug, not a server fault.
func TestAccountsAPIRemoveUnknown(t *testing.T) {
	w, _ := newAccountServer(t)
	rec := doReq(t, w.Handler(), http.MethodDelete, "/api/accounts/missing", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// The active account's live controller state must be what the list reports,
// because the UI uses it to decide between "扫码登录" and "已登录".
func TestAccountsAPIReportsActiveState(t *testing.T) {
	w, mgr := newAccountServer(t)
	ctx := context.Background()
	a, err := mgr.Add(ctx, "stateful")
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	// No Telegram credentials were passed, so no controller exists; the account
	// must still be listed and still be the only active one.
	rec := doReq(t, w.Handler(), http.MethodGet, "/api/accounts/"+a.ID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var single accountJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &single); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if single.ID != a.ID || !single.Active {
		t.Fatalf("account = %+v", single)
	}
}

// authStatus must not claim read-only mode when the real problem is "every
// account was removed": the fix for those two states is different.
func TestAuthStatusNoAccountIsNotUnmanaged(t *testing.T) {
	w, _ := newAccountServer(t)
	rec := doReq(t, w.Handler(), http.MethodGet, "/api/auth/status", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got authStatusAPIResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.State != "no_account" {
		t.Fatalf("state = %q, want no_account", got.State)
	}
	if !got.Managed {
		t.Fatal("managed = false; account support is configured")
	}
}

// Removing the last account must leave the server answering, not panicking:
// the account endpoints have to stay reachable so a new account can be added,
// while data endpoints report the missing account instead of dereferencing a
// nil store.
func TestAccountGuardAfterRemovingAllAccounts(t *testing.T) {
	w, mgr := newAccountServer(t)
	ctx := context.Background()
	a, err := mgr.Add(ctx, "only")
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := mgr.Remove(ctx, a.ID, false); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if mgr.Current() != nil {
		t.Fatal("an account is still live after removing the last one")
	}

	rec := doReq(t, w.Handler(), http.MethodGet, "/api/accounts", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("accounts endpoint = %d, want 200 so a new account can be added", rec.Code)
	}

	rec = doReq(t, w.Handler(), http.MethodGet, "/api/stats", "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("data endpoint = %d, want 503 with no active account", rec.Code)
	}

	rec = doReq(t, w.Handler(), http.MethodGet, "/api/auth/status", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("auth status = %d, want 200", rec.Code)
	}

	// Probes and media routes must answer instead of dereferencing the nil store.
	for _, path := range []string{"/readyz", "/media/" + strings.Repeat("a", 64), "/avatar/user/1"} {
		rec = doReq(t, w.Handler(), http.MethodGet, path, "")
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s = %d, want 503 with no active account", path, rec.Code)
		}
	}
	rec = doReq(t, w.Handler(), http.MethodGet, "/healthz", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz = %d, want 200 even without an account", rec.Code)
	}
}
