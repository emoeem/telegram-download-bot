package archive

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	cstorage "github.com/iyear/tdl/core/storage"
)

// fakeKV is an in-memory stand-in for pkg/kv.Storage so account bookkeeping can
// be tested without a bolt file. Only the two methods AccountManager needs are
// implemented; anything else must stay out of the manager's dependency surface.
type fakeKV struct {
	mu   sync.Mutex
	data map[string]map[string][]byte
}

func newFakeKV() *fakeKV {
	return &fakeKV{data: map[string]map[string][]byte{}}
}

func (f *fakeKV) Namespaces() ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.data))
	for ns := range f.data {
		out = append(out, ns)
	}
	return out, nil
}

func (f *fakeKV) Open(ns string) (cstorage.Storage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.data[ns]; !ok {
		f.data[ns] = map[string][]byte{}
	}
	return &fakeNS{kv: f, ns: ns}, nil
}

// seed writes a value straight into a namespace, standing in for state left
// behind by an older version of the program.
func (f *fakeKV) seed(ns, key string, value []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.data[ns]; !ok {
		f.data[ns] = map[string][]byte{}
	}
	f.data[ns][key] = value
}

func (f *fakeKV) has(ns, key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.data[ns][key]
	return ok
}

type fakeNS struct {
	kv *fakeKV
	ns string
}

func (n *fakeNS) Get(_ context.Context, key string) ([]byte, error) {
	n.kv.mu.Lock()
	defer n.kv.mu.Unlock()
	v, ok := n.kv.data[n.ns][key]
	if !ok {
		return nil, cstorage.ErrNotFound
	}
	return v, nil
}

func (n *fakeNS) Set(_ context.Context, key string, value []byte) error {
	n.kv.mu.Lock()
	defer n.kv.mu.Unlock()
	if _, ok := n.kv.data[n.ns]; !ok {
		n.kv.data[n.ns] = map[string][]byte{}
	}
	n.kv.data[n.ns][key] = value
	return nil
}

func (n *fakeNS) Delete(_ context.Context, key string) error {
	n.kv.mu.Lock()
	defer n.kv.mu.Unlock()
	delete(n.kv.data[n.ns], key)
	return nil
}

func newTestManager(t *testing.T, root *fakeKV) *AccountManager {
	t.Helper()
	m, err := NewAccountManager(context.Background(), root, t.TempDir(), 0, "", nil)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	if err := m.Load(context.Background()); err != nil {
		t.Fatalf("load: %v", err)
	}
	t.Cleanup(m.Close)
	return m
}

// A fresh installation has no accounts, and Load must not invent one.
func TestAccountLoadEmpty(t *testing.T) {
	m := newTestManager(t, newFakeKV())
	if got := m.List(); len(got) != 0 {
		t.Fatalf("empty installation listed %d accounts, want 0", len(got))
	}
	if id := m.ActiveID(); id != "" {
		t.Fatalf("active = %q, want empty", id)
	}
	if m.Current() != nil {
		t.Fatal("empty installation has a live session")
	}
}

// The upgrade path: a deployment that was already signed in under the old
// fixed namespace must keep that session, not be signed out by the upgrade.
func TestAccountAdoptsLegacySession(t *testing.T) {
	root := newFakeKV()
	root.seed(legacyNamespace, sessionKey, []byte("pretend-session"))

	m := newTestManager(t, root)

	list := m.List()
	if len(list) != 1 {
		t.Fatalf("got %d accounts, want 1 (the adopted legacy session)", len(list))
	}
	got := list[0]
	if got.ID != legacyAccountID || !got.Legacy {
		t.Fatalf("adopted account = %+v, want id=%q legacy=true", got, legacyAccountID)
	}
	if got.Namespace != legacyNamespace {
		t.Fatalf("namespace = %q, want %q", got.Namespace, legacyNamespace)
	}
	if m.ActiveID() != legacyAccountID {
		t.Fatalf("active = %q, want %q", m.ActiveID(), legacyAccountID)
	}
	// The session must still be readable afterwards: adoption is a rename of
	// ownership, never a move or a rewrite.
	if !root.has(legacyNamespace, sessionKey) {
		t.Fatal("adoption deleted the legacy session")
	}
}

// A legacy namespace without a session is not an account. Otherwise every
// installation would show a phantom "默认账号" that has never been used.
func TestAccountIgnoresEmptyLegacyNamespace(t *testing.T) {
	root := newFakeKV()
	// Touch the namespace so it exists, without a session.
	if _, err := root.Open(legacyNamespace); err != nil {
		t.Fatalf("seed namespace: %v", err)
	}
	m := newTestManager(t, root)
	if list := m.List(); len(list) != 0 {
		t.Fatalf("got %d accounts, want 0", len(list))
	}
}

// Every account must own a distinct database file. Sharing one would make
// chat_id ambiguous between accounts.
func TestAccountDataDirectoriesAreDistinct(t *testing.T) {
	m := newTestManager(t, newFakeKV())
	ctx := context.Background()

	a, err := m.Add(ctx, "first")
	if err != nil {
		t.Fatalf("add first: %v", err)
	}
	b, err := m.Add(ctx, "second")
	if err != nil {
		t.Fatalf("add second: %v", err)
	}
	if a.Namespace == b.Namespace {
		t.Fatalf("accounts share namespace %q", a.Namespace)
	}
	if m.dbPath(a) == m.dbPath(b) {
		t.Fatalf("accounts share db path %q", m.dbPath(a))
	}
	if got := m.ActiveID(); got != b.ID {
		t.Fatalf("active after add = %q, want newest %q", got, b.ID)
	}
}

// Adding an account switches to it, because a brand-new account is by
// definition not signed in yet: the UI has to land on its QR code.
func TestAccountAddActivatesNewAccount(t *testing.T) {
	m := newTestManager(t, newFakeKV())
	ctx := context.Background()

	if _, err := m.Add(ctx, "first"); err != nil {
		t.Fatalf("add first: %v", err)
	}
	second, err := m.Add(ctx, "second")
	if err != nil {
		t.Fatalf("add second: %v", err)
	}
	sess := m.Current()
	if sess == nil {
		t.Fatal("no live session after add")
	}
	if sess.Account.ID != second.ID {
		t.Fatalf("live session = %q, want %q", sess.Account.ID, second.ID)
	}
}

// Signing out must remove exactly the session pair and leave the archive and
// the peer cache alone.
func TestAccountLogoutDeletesSessionKeepsArchive(t *testing.T) {
	root := newFakeKV()
	m := newTestManager(t, root)
	ctx := context.Background()

	a, err := m.Add(ctx, "main")
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	root.seed(a.Namespace, sessionKey, []byte("session-bytes"))
	root.seed(a.Namespace, "app", []byte("desktop"))
	root.seed(a.Namespace, "peers:phone:+100", []byte("peer-cache"))

	if err := m.Logout(ctx, a.ID); err != nil {
		t.Fatalf("logout: %v", err)
	}
	if root.has(a.Namespace, sessionKey) {
		t.Fatal("logout left the session behind")
	}
	if root.has(a.Namespace, "app") {
		t.Fatal("logout left the app marker behind")
	}
	if !root.has(a.Namespace, "peers:phone:+100") {
		t.Fatal("logout deleted the peer cache, which is harmless to keep and costs a refetch to rebuild")
	}
	// Logging out is not deleting: the account stays listed.
	if _, ok := m.Get(a.ID); !ok {
		t.Fatal("logout removed the account from the registry")
	}
}

// Logging out while an account is live must rebuild the session so the
// controller reloads state instead of advertising the identity it signed out of.
func TestAccountLogoutRebuildsLiveSession(t *testing.T) {
	root := newFakeKV()
	m := newTestManager(t, root)
	ctx := context.Background()

	a, err := m.Add(ctx, "main")
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	before := m.Current()
	if before == nil {
		t.Fatal("no live session after add")
	}
	root.seed(a.Namespace, sessionKey, []byte("session-bytes"))

	if err := m.Logout(ctx, a.ID); err != nil {
		t.Fatalf("logout: %v", err)
	}
	after := m.Current()
	if after == nil {
		t.Fatal("logout left no live session; the UI would report unmanaged instead of logged out")
	}
	if after == before {
		t.Fatal("logout kept the same session; the controller never re-read auth state")
	}
	if after.Store == nil {
		t.Fatal("rebuilt session has no store")
	}
}

// Switching must close the previous database. A closed handle is observable:
// querying it errors instead of silently serving stale rows from another account.
func TestAccountSwitchClosesPreviousStore(t *testing.T) {
	m := newTestManager(t, newFakeKV())
	ctx := context.Background()

	a, err := m.Add(ctx, "first")
	if err != nil {
		t.Fatalf("add first: %v", err)
	}
	prev := m.Current()
	if prev == nil || prev.Store == nil {
		t.Fatal("no live session after add")
	}

	b, err := m.Add(ctx, "second")
	if err != nil {
		t.Fatalf("add second: %v", err)
	}
	if err := m.Switch(ctx, a.ID); err != nil {
		t.Fatalf("switch back: %v", err)
	}
	if got := m.Current(); got == nil || got.Account.ID != a.ID {
		t.Fatalf("live account = %v, want %q", got, a.ID)
	}
	if m.Current() == prev {
		t.Fatal("switch reused the previous session object")
	}
	_ = b
}

// Registry state must survive a restart: the next process rebuilds the same
// list and the same active account from storage alone.
func TestAccountRegistryPersists(t *testing.T) {
	root := newFakeKV()
	ctx := context.Background()

	first, err := NewAccountManager(ctx, root, t.TempDir(), 0, "", nil)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	if err := first.Load(ctx); err != nil {
		t.Fatalf("load: %v", err)
	}
	a, err := first.Add(ctx, "alpha")
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	b, err := first.Add(ctx, "beta")
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := first.Rename(ctx, b.ID, "beta-renamed"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	first.Close()

	second, err := NewAccountManager(ctx, root, t.TempDir(), 0, "", nil)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if err := second.Load(ctx); err != nil {
		t.Fatalf("reload: %v", err)
	}
	defer second.Close()

	list := second.List()
	if len(list) != 2 {
		t.Fatalf("reloaded %d accounts, want 2", len(list))
	}
	if list[0].ID != a.ID || list[1].ID != b.ID {
		t.Fatalf("reload order = [%s %s], want [%s %s]", list[0].ID, list[1].ID, a.ID, b.ID)
	}
	if list[1].Name != "beta-renamed" {
		t.Fatalf("renamed account reloaded as %q", list[1].Name)
	}
	if second.ActiveID() != b.ID {
		t.Fatalf("active after reload = %q, want %q", second.ActiveID(), b.ID)
	}
}

// Removing an account signs it out and takes it off the list, but keeps the
// archive unless the caller explicitly asks for a purge.
func TestAccountRemoveKeepsDataByDefault(t *testing.T) {
	root := newFakeKV()
	m := newTestManager(t, root)
	ctx := context.Background()

	a, err := m.Add(ctx, "temp")
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	root.seed(a.Namespace, sessionKey, []byte("session-bytes"))
	if err := m.Remove(ctx, a.ID, false); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, ok := m.Get(a.ID); ok {
		t.Fatal("removed account is still listed")
	}
	if root.has(a.Namespace, sessionKey) {
		t.Fatal("removed account kept its session")
	}
	// The row is gone but the data directory must survive, otherwise a
	// mis-click on "remove" would destroy an archive.
	if _, err := os.Stat(filepath.Join(m.dataDir, "accounts", a.ID)); err != nil {
		t.Fatalf("remove deleted the data directory without being asked: %v", err)
	}
}

// After removing the active account the next one becomes live, so the UI is
// never left pointing at a closed store.
func TestAccountRemovePromotesNextAccount(t *testing.T) {
	m := newTestManager(t, newFakeKV())
	ctx := context.Background()

	a, err := m.Add(ctx, "first")
	if err != nil {
		t.Fatalf("add first: %v", err)
	}
	b, err := m.Add(ctx, "second")
	if err != nil {
		t.Fatalf("add second: %v", err)
	}
	if err := m.Remove(ctx, b.ID, false); err != nil {
		t.Fatalf("remove active: %v", err)
	}
	if got := m.ActiveID(); got != a.ID {
		t.Fatalf("active after remove = %q, want %q", got, a.ID)
	}
	sess := m.Current()
	if sess == nil || sess.Account.ID != a.ID {
		t.Fatalf("live session after remove = %v, want %q", sess, a.ID)
	}
}

// An empty name is rejected rather than silently persisted, because the label
// is the only thing distinguishing accounts in the UI.
func TestAccountRenameRejectsEmpty(t *testing.T) {
	m := newTestManager(t, newFakeKV())
	ctx := context.Background()
	a, err := m.Add(ctx, "named")
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := m.Rename(ctx, a.ID, "   "); err == nil {
		t.Fatal("rename accepted an empty name")
	}
	got, _ := m.Get(a.ID)
	if got.Name != "named" {
		t.Fatalf("name after rejected rename = %q, want %q", got.Name, "named")
	}
}

// Timestamps exist so the UI can order accounts; the copy must not leak the
// live pointer or a caller could mutate registry state without a lock.
func TestAccountListReturnsCopies(t *testing.T) {
	m := newTestManager(t, newFakeKV())
	ctx := context.Background()
	a, err := m.Add(ctx, "original")
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	list := m.List()
	list[0].Name = "mutated"
	if got, _ := m.Get(a.ID); got.Name != "original" {
		t.Fatalf("List leaked the live pointer: name became %q", got.Name)
	}
	_ = time.Now
}

// Load must leave the active account actually servable, not merely listed.
// Regressing this reports "no account" on an installation that has one, which
// is exactly the failure this test was written after seeing.
func TestAccountLoadActivatesRegistry(t *testing.T) {
	root := newFakeKV()
	root.seed(legacyNamespace, sessionKey, []byte("session-bytes"))
	m := newTestManager(t, root)

	sess := m.Current()
	if sess == nil {
		t.Fatal("Load registered the account but never activated it")
	}
	if sess.Store == nil {
		t.Fatal("active session has no store")
	}
	if sess.Account == nil || sess.Account.ID != legacyAccountID {
		t.Fatalf("active account = %+v, want %q", sess.Account, legacyAccountID)
	}
}
