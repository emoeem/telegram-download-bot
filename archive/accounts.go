package archive

// Multi-account support for the web control plane.
//
// Why every account gets its own SQLite file instead of one shared database
// with an account column: chat_id is scoped to an account. The same -1001234
// is a channel on account A and an unrelated channel on account B. Mixing them
// into one messages table makes every chat_id lookup ambiguous, and that class
// of bug does not raise an error — it silently returns the other account's rows.
//
// Why every account gets its own storage namespace: tdl keys the Telegram
// session under a namespace, so `namespace` IS the account. Two namespaces can
// never share a session, and deleting the session is all "sign out" needs to be.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-faster/errors"
	cstorage "github.com/iyear/tdl/core/storage"
	"github.com/iyear/tdl/core/storage/keygen"

	"github.com/iyear/tdl/archive/store"
)

const (
	// accountsNamespace holds the registry itself, never a Telegram session.
	accountsNamespace = "archive-accounts"
	registryKey       = "accounts"
	activeKey         = "active"

	// legacyNamespace is the namespace every pre-multi-account deployment used.
	// On first start its session is adopted as the "default" account so an
	// upgrade never signs an existing installation out.
	legacyNamespace = "archive"
	legacyAccountID = "default"

	// sessionKey is where tdl stores the Telegram session inside a namespace.
	// Deleting it plus the app marker is exactly what app/tgbot does on
	// /logout, so signing out here behaves identically to the bot command.
	sessionKey = "session"
)

var (
	// ErrAccountNotFound is returned when an operation names an id the
	// registry does not know.
	ErrAccountNotFound = errors.New("account not found")
	// ErrAccountNameEmpty guards the only human-visible identifier.
	ErrAccountNameEmpty = errors.New("account name is empty")
)

// KVOpener is the subset of pkg/kv.Storage the manager needs. Keeping it narrow
// lets tests drive an in-memory implementation instead of a real bolt file.
type KVOpener interface {
	Namespaces() ([]string, error)
	Open(ns string) (cstorage.Storage, error)
}

// KVWrapper optionally seals a namespace at rest (archive.SecureNamespace).
// nil means plaintext, which is what an installation without a passphrase uses.
type KVWrapper func(cstorage.Storage) (cstorage.Storage, error)

// Account is the serialisable half: what the UI lists and what survives a
// restart. Runtime handles live in AccountSession.
type Account struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Namespace string    `json:"namespace"`
	AddedAt   time.Time `json:"added_at"`
	LastUsed  time.Time `json:"last_used,omitempty"`
	// Legacy marks the account adopted from the pre-multi-account namespace.
	// Its data directory is the old shared one, so deleting it must not wipe
	// a directory that other things may still reference.
	Legacy bool `json:"legacy,omitempty"`
}

// AccountSession is everything needed to serve one account: its storage, its
// archive database, and — only while it is the active account — a controller
// holding a live Telegram connection.
type AccountSession struct {
	Account *Account
	KV      cstorage.Storage
	Store   *store.Store
	Ctl     *Controller
}

// AccountManager owns the registry, the active account and the live session.
type AccountManager struct {
	ctx     context.Context
	reg     cstorage.Storage
	root    KVOpener
	dataDir string
	appID   int
	appHash string
	wrap    KVWrapper

	mu       sync.RWMutex
	accounts []*Account
	byID     map[string]*Account
	active   string

	// session is the currently activated account. Exactly one exists at a time:
	// a second live controller would open a second Telegram connection and run a
	// second archiver against the same UI, which is not what "switch account"
	// means.
	session *AccountSession
}

// NewAccountManager prepares the registry. Load must be called before use.
func NewAccountManager(ctx context.Context, root KVOpener, dataDir string, appID int, appHash string, wrap KVWrapper) (*AccountManager, error) {
	if root == nil {
		return nil, errors.New("account manager: nil kv root")
	}
	reg, err := root.Open(accountsNamespace)
	if err != nil {
		return nil, errors.Wrap(err, "open accounts namespace")
	}
	if wrap != nil {
		if reg, err = wrap(reg); err != nil {
			return nil, errors.Wrap(err, "seal accounts namespace")
		}
	}
	return &AccountManager{
		ctx: ctx, reg: reg, root: root, dataDir: dataDir,
		appID: appID, appHash: appHash, wrap: wrap,
		byID: map[string]*Account{},
	}, nil
}

type registryDoc struct {
	Accounts []*Account `json:"accounts"`
	Active   string     `json:"active"`
}

// Load reads the registry and, on a fresh installation, adopts the legacy
// namespace. Migration is deliberately one-way and non-destructive: the old
// namespace keeps its name, so if anything goes wrong the session is still
// there under `archive`.
func (m *AccountManager) Load(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	raw, err := m.reg.Get(ctx, registryKey)
	switch {
	case err == nil:
		var doc registryDoc
		if err := json.Unmarshal(raw, &doc); err != nil {
			return errors.Wrap(err, "parse account registry")
		}
		m.accounts = doc.Accounts
		m.active = doc.Active
	case errors.Is(err, cstorage.ErrNotFound):
		// Fresh registry: fall through to migration below.
	default:
		return errors.Wrap(err, "read account registry")
	}

	m.reindexLocked()

	if len(m.accounts) == 0 {
		adopted, err := m.adoptLegacyLocked(ctx)
		if err != nil {
			return err
		}
		if adopted != nil {
			m.accounts = append(m.accounts, adopted)
			m.active = adopted.ID
			m.reindexLocked()
			if err := m.saveLocked(ctx); err != nil {
				return err
			}
		}
	}

	if m.active == "" && len(m.accounts) > 0 {
		m.active = m.accounts[0].ID
		_ = m.saveLocked(ctx)
	}
	// Registering the accounts is not enough: the server serves whatever
	// Current() points at, so the active account has to be opened here or every
	// request would fall through to a nil store and report "no account".
	if m.active != "" {
		if a, ok := m.byID[m.active]; ok {
			if err := m.activateLocked(ctx, a); err != nil {
				return err
			}
		}
	}
	return nil
}

// adoptLegacyLocked builds a "default" account from the pre-multi-account
// namespace when that namespace actually holds a session. An installation that
// never logged in gets an empty registry instead of a phantom account.
func (m *AccountManager) adoptLegacyLocked(ctx context.Context) (*Account, error) {
	nsList, err := m.root.Namespaces()
	if err != nil {
		return nil, errors.Wrap(err, "list namespaces")
	}
	found := false
	for _, ns := range nsList {
		if ns == legacyNamespace {
			found = true
			break
		}
	}
	if !found {
		return nil, nil
	}
	kv, err := m.openKVLocked(legacyNamespace)
	if err != nil {
		return nil, err
	}
	if _, err := kv.Get(ctx, sessionKey); err != nil {
		if errors.Is(err, cstorage.ErrNotFound) {
			return nil, nil
		}
		return nil, errors.Wrap(err, "probe legacy session")
	}
	return &Account{
		ID:        legacyAccountID,
		Name:      "默认账号",
		Namespace: legacyNamespace,
		AddedAt:   time.Now(),
		Legacy:    true,
	}, nil
}

func (m *AccountManager) reindexLocked() {
	m.byID = make(map[string]*Account, len(m.accounts))
	for _, a := range m.accounts {
		m.byID[a.ID] = a
	}
}

func (m *AccountManager) saveLocked(ctx context.Context) error {
	b, err := json.Marshal(registryDoc{Accounts: m.accounts, Active: m.active})
	if err != nil {
		return errors.Wrap(err, "encode account registry")
	}
	if err := m.reg.Set(ctx, registryKey, b); err != nil {
		return errors.Wrap(err, "write account registry")
	}
	if err := m.reg.Set(ctx, activeKey, []byte(m.active)); err != nil {
		return errors.Wrap(err, "write active account")
	}
	return nil
}

// List returns a copy ordered by creation time, so the UI does not depend on
// map iteration order or on the order accounts happen to be added.
func (m *AccountManager) List() []Account {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Account, 0, len(m.accounts))
	for _, a := range m.accounts {
		out = append(out, *a)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].AddedAt.Equal(out[j].AddedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].AddedAt.Before(out[j].AddedAt)
	})
	return out
}

// ActiveID reports which account is live, or "" when none is configured.
func (m *AccountManager) ActiveID() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.active
}

// Current returns the live session, or nil when the installation has no
// account yet. Callers must treat the result as read-only.
func (m *AccountManager) Current() *AccountSession {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.session
}

// Get returns a copy of one account.
func (m *AccountManager) Get(id string) (Account, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	a, ok := m.byID[id]
	if !ok {
		return Account{}, false
	}
	return *a, true
}

func newAccountID() (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", errors.Wrap(err, "generate account id")
	}
	return hex.EncodeToString(b[:]), nil
}

// dbPath is per-account so a delete can remove exactly one archive.
func (m *AccountManager) dbPath(a *Account) string {
	if a.Legacy {
		return filepath.Join(m.dataDir, "tgarchive.db")
	}
	return filepath.Join(m.dataDir, "accounts", a.ID, "tgarchive.db")
}

func (m *AccountManager) openKVLocked(namespace string) (cstorage.Storage, error) {
	kv, err := m.root.Open(namespace)
	if err != nil {
		return nil, errors.Wrap(err, "open namespace")
	}
	if m.wrap != nil {
		if kv, err = m.wrap(kv); err != nil {
			return nil, errors.Wrap(err, "seal namespace")
		}
	}
	return kv, nil
}

// Add creates a fresh account and makes it active: a new account is never
// logged in yet, so the UI must land on its QR code rather than stay on the
// previous account's data.
func (m *AccountManager) Add(ctx context.Context, name string) (*Account, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	id, err := newAccountID()
	if err != nil {
		return nil, err
	}
	for m.byID[id] != nil {
		if id, err = newAccountID(); err != nil {
			return nil, err
		}
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = "账号 " + id[:4]
	}
	a := &Account{
		ID:        id,
		Name:      name,
		Namespace: "archive-" + id,
		AddedAt:   time.Now(),
	}

	prevAccounts, prevActive := m.accounts, m.active
	m.accounts = append(m.accounts, a)
	m.active = a.ID
	m.reindexLocked()
	if err := m.saveLocked(ctx); err != nil {
		m.accounts, m.active = prevAccounts, prevActive
		m.reindexLocked()
		return nil, err
	}
	if err := m.activateLocked(ctx, a); err != nil {
		// Roll the registry back so a half-created account cannot be listed.
		m.accounts, m.active = prevAccounts, prevActive
		m.reindexLocked()
		_ = m.saveLocked(ctx)
		return nil, err
	}
	return a, nil
}

// Switch makes another account live. Closing the old controller first is not
// optional: leaving it running would keep a second Telegram connection and a
// second archiver pointed at an account the UI no longer shows.
func (m *AccountManager) Switch(ctx context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	a, ok := m.byID[id]
	if !ok {
		return errors.Wrapf(ErrAccountNotFound, "%q", id)
	}
	if m.active == id && m.session != nil {
		return nil
	}
	prevActive := m.active
	m.active = id
	if err := m.saveLocked(ctx); err != nil {
		m.active = prevActive
		return err
	}
	return m.activateLocked(ctx, a)
}

// activateLocked tears down the current session and opens the requested one.
// The store of an inactive account is closed rather than cached: holding N
// SQLite handles for accounts nobody is looking at costs file descriptors and
// buys nothing, and a switch is a deliberate human action, not a hot path.
func (m *AccountManager) activateLocked(ctx context.Context, a *Account) error {
	if m.session != nil {
		m.closeSessionLocked()
	}
	kv, err := m.openKVLocked(a.Namespace)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(m.dbPath(a)), 0o755); err != nil {
		return errors.Wrap(err, "create account data dir")
	}
	st, err := store.Open(m.dbPath(a))
	if err != nil {
		return errors.Wrap(err, "open archive store")
	}
	sess := &AccountSession{Account: a, KV: kv, Store: st}
	if m.appID > 0 && strings.TrimSpace(m.appHash) != "" {
		sess.Ctl = NewController(m.ctx, kv, st, m.appID, m.appHash)
	}
	m.session = sess
	a.LastUsed = time.Now()
	return nil
}

func (m *AccountManager) closeSessionLocked() {
	if m.session == nil {
		return
	}
	if m.session.Ctl != nil {
		m.session.Ctl.Close()
	}
	if m.session.Store != nil {
		_ = m.session.Store.Close()
	}
	m.session = nil
}

// Logout signs the Telegram account out while keeping its archive. Deleting
// the session key plus the app marker is the same pair app/tgbot removes on
// /logout; the peer and update-state caches are keyed by user/peer id, so they
// can stay and only save a re-fetch after the next sign-in.
func (m *AccountManager) Logout(ctx context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	a, ok := m.byID[id]
	if !ok {
		return errors.Wrapf(ErrAccountNotFound, "%q", id)
	}
	kv, err := m.openKVLocked(a.Namespace)
	if err != nil {
		return err
	}
	if err := kv.Delete(ctx, sessionKey); err != nil {
		return errors.Wrap(err, "delete session")
	}
	if err := kv.Delete(ctx, keygen.New("app")); err != nil {
		return errors.Wrap(err, "delete app marker")
	}
	// Rebuild the live session so the controller re-reads state and reports
	// logged_out instead of continuing to advertise the old identity.
	if m.active == id {
		m.closeSessionLocked()
		return m.activateLocked(ctx, a)
	}
	return nil
}

// Rename only touches the label; the namespace and the data directory keep
// their generated names so nothing has to move on disk.
func (m *AccountManager) Rename(ctx context.Context, id, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	a, ok := m.byID[id]
	if !ok {
		return errors.Wrapf(ErrAccountNotFound, "%q", id)
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return ErrAccountNameEmpty
	}
	prev := a.Name
	a.Name = name
	if err := m.saveLocked(ctx); err != nil {
		a.Name = prev
		return err
	}
	return nil
}

// Remove drops an account from the registry and signs it out. purgeData also
// deletes its archive directory; without it the messages stay on disk and the
// row disappears from the UI, which is the safer default for a mis-click.
//
// The sign-out is inlined rather than delegated to Logout on purpose: Logout
// rebuilds the live session when the account is active, and rebuilding the
// session of the account being deleted would re-open its database right before
// we drop it.
func (m *AccountManager) Remove(ctx context.Context, id string, purgeData bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	a, ok := m.byID[id]
	if !ok {
		return errors.Wrapf(ErrAccountNotFound, "%q", id)
	}
	if m.active == id {
		m.closeSessionLocked()
	}
	// Sign out before forgetting the namespace: once the row is gone we would
	// have no way to name the namespace to clean up.
	kv, err := m.openKVLocked(a.Namespace)
	if err != nil {
		return err
	}
	if err := kv.Delete(ctx, sessionKey); err != nil {
		return errors.Wrap(err, "delete session")
	}
	if err := kv.Delete(ctx, keygen.New("app")); err != nil {
		return errors.Wrap(err, "delete app marker")
	}

	kept := m.accounts[:0]
	next := ""
	for _, acc := range m.accounts {
		if acc.ID == id {
			continue
		}
		kept = append(kept, acc)
		if next == "" {
			next = acc.ID
		}
	}
	m.accounts = kept
	if m.active == id {
		m.active = next
	}
	m.reindexLocked()
	if err := m.saveLocked(ctx); err != nil {
		return err
	}

	if purgeData && !a.Legacy {
		dir := filepath.Join(m.dataDir, "accounts", a.ID)
		if err := os.RemoveAll(dir); err != nil {
			return errors.Wrap(err, "remove account data")
		}
	}
	if m.active != "" && m.session == nil {
		if acc, ok := m.byID[m.active]; ok {
			return m.activateLocked(ctx, acc)
		}
	}
	return nil
}

// Close releases the live session. The registry itself needs no flush: every
// mutation writes through immediately.
func (m *AccountManager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closeSessionLocked()
}

// HasSession reports whether the account holds a stored Telegram session.
// It is a presence check, not a validity check: a session the user revoked on
// another device stays on disk until the next connection attempt notices.
func (m *AccountManager) HasSession(ctx context.Context, id string) bool {
	m.mu.RLock()
	a, ok := m.byID[id]
	m.mu.RUnlock()
	if !ok {
		return false
	}
	kv, err := m.openKVFor(a.Namespace)
	if err != nil {
		return false
	}
	_, err = kv.Get(ctx, sessionKey)
	return err == nil
}

func (m *AccountManager) openKVFor(namespace string) (cstorage.Storage, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.openKVLocked(namespace)
}
