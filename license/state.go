package license

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// StateFileName is the activation record inside the licence directory. It lives
// next to the archive data so it is covered by the same backup and the same
// app-private permissions.
const StateFileName = "activation.json"

// clockSkewTolerance is how far the wall clock may move backwards before the
// licence refuses to run.
//
// Some drift is normal (NTP correction, a laptop waking in another time zone,
// a user fixing a genuinely wrong clock). What must not pass is the specific
// move that turns an expiring licence into a permanent one: setting the clock
// back before expiry. Six hours separates those two cases comfortably.
const clockSkewTolerance = 6 * time.Hour

// State is the persisted activation record.
//
// Everything here is a cache of decisions that can be re-derived from Key plus
// the current device, except Device: when the key carries device code 0 the
// binding is decided at activation time and only this file remembers it.
type State struct {
	// Key is the full licence text as entered. Re-verifying it on every start
	// means a tampered activation file fails the same way a forged key does.
	Key string `json:"key"`
	// Device is the device code this activation was bound to. It is only
	// consulted for keys whose payload carries device 0.
	Device uint64 `json:"device"`
	// Serial mirrors the payload's serial for support and display. It is never
	// trusted: the payload is re-parsed on every load.
	Serial uint64 `json:"serial"`
	// ActivatedAt is when this machine first accepted the key.
	ActivatedAt int64 `json:"activated_at"`
	// LastSeen is the latest wall-clock instant observed while the licence was
	// valid. It is the forward ratchet the clock-rollback check compares to.
	LastSeen int64 `json:"last_seen"`
	// Server is the online licence service this activation registered with.
	// Empty means the activation was purely offline.
	//
	// It is recorded here so a later start knows where to re-check without the
	// user having to retype the address — and so revocation reaches the client
	// without any further configuration.
	Server string `json:"server,omitempty"`
	// LastCheck is the unix time of the last successful online verification.
	// A revoked key stops working at the next check, which is why the gate
	// treats a stale LastCheck as "needs to phone home" rather than as valid.
	LastCheck int64 `json:"last_check,omitempty"`
}

// Status is the read-only view handed to the UI and the API.
type Status struct {
	// Configured is false when this build was compiled without any verifier.
	Configured bool `json:"configured"`
	// Activated is true when a key has been accepted on this machine.
	Activated bool `json:"activated"`
	// Valid is true when Activated and every check currently passes.
	Valid bool `json:"valid"`
	// Kind is "short" or "strong".
	Kind string `json:"kind,omitempty"`
	// Serial is the hex-free display serial of the active key.
	Serial string `json:"serial,omitempty"`
	// Forever is true for a key with no expiry.
	Forever bool `json:"forever,omitempty"`
	// ExpiresAt is the RFC3339 expiry, empty for a forever key.
	ExpiresAt string `json:"expires_at,omitempty"`
	// DaysLeft is whole days remaining, -1 for a forever key.
	DaysLeft int `json:"days_left,omitempty"`
	// BoundDevice is the device code the active key is bound to.
	BoundDevice string `json:"bound_device,omitempty"`
	// ThisDevice is this machine's code, shown on the activation screen so the
	// customer can read it to the issuer.
	ThisDevice string `json:"this_device"`
	// Reason explains why Valid is false, in customer-facing language.
	Reason string `json:"reason,omitempty"`
}

// Manager owns the activation state for one machine.
//
// It is safe for concurrent use: the web server calls Gate on every request.
type Manager struct {
	dir      string
	verifier *Verifier
	device   uint64

	mu    sync.Mutex
	state *State
	// loaded records whether an activation file was read, so a missing file is
	// not re-stat'ed on every request.
	loaded bool
}

// NewManager prepares the manager. A nil verifier yields a manager that fails
// closed: Gate returns ErrNotConfigured and Status reports Configured=false.
func NewManager(dir string, v *Verifier, device uint64) *Manager {
	return &Manager{dir: dir, verifier: v, device: device}
}

// Dir is where the activation file lives.
func (m *Manager) Dir() string { return m.dir }

// Configured reports whether this build can verify keys at all.
func (m *Manager) Configured() bool { return m.verifier.Configured() }

// DeviceCode is this machine's code.
func (m *Manager) DeviceCode() uint64 { return m.device }

// ThisDevice renders this machine's code for display.
func (m *Manager) ThisDevice() string { return FormatDeviceCode(m.device) }

// statePath is the activation file location.
func (m *Manager) statePath() string { return filepath.Join(m.dir, StateFileName) }

// load reads the activation file once. A missing file is not an error: that is
// a fresh install waiting to be activated.
func (m *Manager) load() {
	if m.loaded {
		return
	}
	m.loaded = true
	b, err := os.ReadFile(m.statePath())
	if err != nil {
		return
	}
	var st State
	if err := json.Unmarshal(b, &st); err != nil {
		// A corrupt file is treated as "not activated" rather than a hard
		// failure: the customer can always re-enter the key, and refusing to
		// start would be worse than asking again.
		return
	}
	m.state = &st
}

// save persists the current state atomically.
//
// The write goes to a temporary file in the same directory and is renamed over
// the target, so a crash mid-write leaves the previous activation intact
// instead of a truncated JSON file that reads as "not activated".
func (m *Manager) save() error {
	if m.state == nil {
		return nil
	}
	if err := os.MkdirAll(m.dir, 0o700); err != nil {
		return fmt.Errorf("创建授权目录: %w", err)
	}
	b, err := json.MarshalIndent(m.state, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化授权状态: %w", err)
	}
	tmp := m.statePath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("写入授权状态: %w", err)
	}
	if err := os.Rename(tmp, m.statePath()); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("替换授权状态: %w", err)
	}
	return nil
}

// Clear removes the activation so a different key can be entered. The archive
// data is untouched.
func (m *Manager) Clear() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.loaded = true
	m.state = nil
	if err := os.Remove(m.statePath()); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("清除授权状态: %w", err)
	}
	return nil
}

// CurrentKey returns the activation key currently on disk, or "" when this
// machine is not activated. Used to re-verify against the licence server.
func (m *Manager) CurrentKey() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.load()
	if m.state == nil {
		return ""
	}
	return m.state.Key
}

// SetServer records the online licence service for the current activation.
//
// Called right after a successful online activation so later starts can
// re-check without asking the user for the address again.
func (m *Manager) SetServer(url string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.load()
	if m.state == nil {
		return ErrNotActivated()
	}
	m.state.Server = url
	return m.save()
}

// Server returns the recorded licence service address, or "" for an offline
// activation.
func (m *Manager) Server() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.load()
	if m.state == nil {
		return ""
	}
	return m.state.Server
}

// NeedsOnlineCheck reports whether the activation should phone home now.
//
// An offline activation never needs it. An online one does once maxAge has
// elapsed since the last successful check — that interval is the revocation
// latency the customer experiences, so it is a product decision, not a
// technical one.
func (m *Manager) NeedsOnlineCheck(now time.Time, maxAge time.Duration) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.load()
	if m.state == nil || m.state.Server == "" {
		return false
	}
	if m.state.LastCheck == 0 {
		return true
	}
	return now.Sub(time.Unix(m.state.LastCheck, 0)) >= maxAge
}

// MarkChecked records a successful online verification.
func (m *Manager) MarkChecked(now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.load()
	if m.state == nil {
		return ErrNotActivated()
	}
	m.state.LastCheck = now.Unix()
	return m.save()
}

// Activate verifies key and, if it is usable on this machine, records it.
//
// The device rule is the heart of the "one key, one machine" requirement:
//   - a key that carries a device code must match this machine exactly;
//   - a key that carries 0 binds to this machine now, and every later start
//     compares against the code saved in the activation file.
func (m *Manager) Activate(key string, now time.Time) (Status, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.load()

	payload, _, err := m.verifier.Parse(key)
	if err != nil {
		return m.statusLocked(now, err), err
	}
	if payload.Expired(now) {
		return m.statusLocked(now, ErrExpired), ErrExpired
	}
	if payload.Device != 0 && payload.Device != m.device {
		return m.statusLocked(now, ErrDevice), ErrDevice
	}

	prev := m.state
	// The licence server address survives a key change: the same deployment
	// serves every key, and losing it here would silently turn a revocable
	// activation into an offline one that can never be revoked.
	var prevServer string
	if prev != nil {
		prevServer = prev.Server
	}
	m.state = &State{
		Key:         key,
		Device:      m.device,
		Serial:      payload.Serial,
		ActivatedAt: now.Unix(),
		LastSeen:    now.Unix(),
		Server:      prevServer,
	}
	if err := m.save(); err != nil {
		// Restore the previous activation: a failed write must not leave the
		// in-memory state claiming an activation that is not on disk.
		m.state = prev
		return m.statusLocked(now, err), err
	}
	return m.statusLocked(now, nil), nil
}

// Gate is the check the request path runs. It returns nil only when a valid
// activation exists for this machine right now.
//
// It also advances the LastSeen ratchet, which is what makes a later clock
// rollback detectable.
func (m *Manager) Gate(now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.load()
	return m.gateLocked(now)
}

func (m *Manager) gateLocked(now time.Time) error {
	if !m.verifier.Configured() {
		return ErrNotConfigured
	}
	if m.state == nil {
		return errNotActivated
	}
	payload, _, err := m.verifier.Parse(m.state.Key)
	if err != nil {
		return err
	}
	if payload.Expired(now) {
		return ErrExpired
	}
	// A bound key is authoritative; an unbound one falls back to the device
	// recorded at activation.
	want := payload.Device
	if want == 0 {
		want = m.state.Device
	}
	if want != m.device {
		return ErrDevice
	}
	// Clock rollback: the wall clock may not go backwards past the last instant
	// the licence was known good.
	if m.state.LastSeen > 0 {
		if now.Unix() < m.state.LastSeen-int64(clockSkewTolerance/time.Second) {
			return ErrClock
		}
	}
	if now.Unix() > m.state.LastSeen {
		m.state.LastSeen = now.Unix()
		// Persisting on every request would turn a read into a write; the
		// ratchet only needs to survive a restart, so best-effort is right.
		_ = m.save()
	}
	return nil
}

// errNotActivated is the "nothing entered yet" case, kept separate from a
// rejected key so the UI can show the activation form rather than an error.
var errNotActivated = fmt.Errorf("尚未激活")

// ErrNotActivated exposes the sentinel for callers that need to branch on it.
func ErrNotActivated() error { return errNotActivated }

// IsNotActivated reports whether err means "no key has been entered".
func IsNotActivated(err error) bool { return err == errNotActivated }

// Status reports the current activation state without changing it.
func (m *Manager) Status(now time.Time) Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.load()
	err := m.gateLocked(now)
	return m.statusLocked(now, err)
}

// statusLocked builds the view. reason is the error that made the licence
// invalid, or nil when it is valid.
func (m *Manager) statusLocked(now time.Time, reason error) Status {
	st := Status{
		Configured: m.verifier.Configured(),
		ThisDevice: FormatDeviceCode(m.device),
	}
	if reason != nil {
		st.Reason = reason.Error()
	}
	if m.state == nil {
		return st
	}
	st.Activated = true
	payload, kind, err := m.verifier.Parse(m.state.Key)
	if err != nil {
		if st.Reason == "" {
			st.Reason = err.Error()
		}
		return st
	}
	st.Kind = kind.String()
	st.Serial = encode36(payload.Serial, 8)
	st.Forever = payload.Forever()
	if payload.Forever() {
		st.DaysLeft = -1
	} else {
		st.ExpiresAt = payload.ExpiresAt().Format(time.RFC3339)
		st.DaysLeft = payload.DaysLeft(now)
	}
	bound := payload.Device
	if bound == 0 {
		bound = m.state.Device
	}
	st.BoundDevice = FormatDeviceCode(bound)
	st.Valid = reason == nil
	return st
}
