package archive

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"

	cstorage "github.com/iyear/tdl/core/storage"
)

type memKV struct {
	mu sync.Mutex
	m  map[string][]byte
}

func newMemKV() *memKV { return &memKV{m: map[string][]byte{}} }

func (m *memKV) Get(_ context.Context, key string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.m[key]
	if !ok {
		return nil, cstorage.ErrNotFound
	}
	return bytes.Clone(v), nil
}

func (m *memKV) Set(_ context.Context, key string, value []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.m[key] = bytes.Clone(value)
	return nil
}

func (m *memKV) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.m, key)
	return nil
}

func TestSecureStoreRoundtripAndSeal(t *testing.T) {
	ctx := context.Background()
	inner := newMemKV()

	st, err := SecureNamespace(ctx, inner, "correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Set(ctx, "session", []byte("secret-session-bytes")); err != nil {
		t.Fatal(err)
	}
	got, err := st.Get(ctx, "session")
	if err != nil || string(got) != "secret-session-bytes" {
		t.Fatalf("roundtrip: %q %v", got, err)
	}

	raw, _ := inner.Get(ctx, "session")
	if !isSealed(raw) {
		t.Fatal("raw value should be sealed")
	}
	if bytes.Contains(raw, []byte("secret")) {
		t.Fatal("raw value leaks plaintext")
	}

	enc, err := IsEncrypted(ctx, inner)
	if err != nil || !enc {
		t.Fatalf("IsEncrypted=%v %v", enc, err)
	}
}

func TestSecureStoreWrongPassphrase(t *testing.T) {
	ctx := context.Background()
	inner := newMemKV()

	st, err := SecureNamespace(ctx, inner, "right")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Set(ctx, "session", []byte("topsecret")); err != nil {
		t.Fatal(err)
	}

	wrong, err := SecureNamespace(ctx, inner, "wrong")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wrong.Get(ctx, "session"); !errors.Is(err, ErrPassphraseRequired) {
		t.Fatalf("want ErrPassphraseRequired, got %v", err)
	}
}

func TestSecureStoreLegacyMigration(t *testing.T) {
	ctx := context.Background()
	inner := newMemKV()

	// Simulate a legacy plaintext session + app identity.
	if err := inner.Set(ctx, "session", []byte("legacy-session")); err != nil {
		t.Fatal(err)
	}
	if err := inner.Set(ctx, "app", []byte("desktop")); err != nil {
		t.Fatal(err)
	}

	st, err := SecureNamespace(ctx, inner, "pass")
	if err != nil {
		t.Fatal(err)
	}
	// The sensitive keys must have been re-sealed on open...
	got, err := st.Get(ctx, "session")
	if err != nil || string(got) != "legacy-session" {
		t.Fatalf("legacy session unreadable: %q %v", got, err)
	}
	rawSession, _ := inner.Get(ctx, "session")
	if !isSealed(rawSession) {
		t.Fatal("legacy session key was not migrated to sealed form")
	}
	rawApp, _ := inner.Get(ctx, "app")
	if !isSealed(rawApp) {
		t.Fatal("legacy app key was not migrated to sealed form")
	}

	// ...and the salt must survive so a reopen with the right passphrase works.
	reopen, err := SecureNamespace(ctx, inner, "pass")
	if err != nil {
		t.Fatal(err)
	}
	if got, err = reopen.Get(ctx, "session"); err != nil || string(got) != "legacy-session" {
		t.Fatalf("reopen: %q %v", got, err)
	}
}

func TestSecureStoreLegacyPlaintextStillReadable(t *testing.T) {
	ctx := context.Background()
	inner := newMemKV()

	st, err := SecureNamespace(ctx, inner, "pass")
	if err != nil {
		t.Fatal(err)
	}
	// A peers/state value written before encryption stays readable.
	if err := inner.Set(ctx, "peer:123", []byte("plaintext-peer")); err != nil {
		t.Fatal(err)
	}
	got, err := st.Get(ctx, "peer:123")
	if err != nil || string(got) != "plaintext-peer" {
		t.Fatalf("legacy non-sensitive value: %q %v", got, err)
	}
	// Rewriting it seals it.
	if err := st.Set(ctx, "peer:123", []byte("plaintext-peer")); err != nil {
		t.Fatal(err)
	}
	raw, _ := inner.Get(ctx, "peer:123")
	if !isSealed(raw) {
		t.Fatal("rewritten value should be sealed")
	}
}

func TestSecureStoreDelete(t *testing.T) {
	ctx := context.Background()
	inner := newMemKV()
	st, err := SecureNamespace(ctx, inner, "pass")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Set(ctx, "k", []byte("v")); err != nil {
		t.Fatal(err)
	}
	if err := st.Delete(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Get(ctx, "k"); !errors.Is(err, cstorage.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestSecureStoreEmptyPassphrase(t *testing.T) {
	if _, err := SecureNamespace(context.Background(), newMemKV(), "  "); err == nil {
		t.Fatal("empty passphrase must be rejected")
	}
}
