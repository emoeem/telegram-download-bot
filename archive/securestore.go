package archive

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/subtle"
	"strings"

	"github.com/go-faster/errors"
	"golang.org/x/crypto/scrypt"

	cstorage "github.com/iyear/tdl/core/storage"
	"github.com/iyear/tdl/core/storage/keygen"
)

// Session-at-rest encryption: values inside the archive namespace are sealed
// with AES-GCM under a key derived from a passphrase (scrypt). Key names stay
// plaintext (they are opaque hashes and reveal nothing); values gain a magic
// prefix so legacy plaintext values remain readable and are re-sealed on their
// next write. The salt lives in a meta key inside the namespace itself, so a
// session directory is self-describing.

const (
	encMetaKey  = "__tga_enc_meta_v1"
	encMagic    = "TGAE1\x01"
	encSaltLen  = 16
	encNonceLen = 12
	encKeyLen   = 32

	scryptN = 32768
	scryptR = 8
	scryptP = 1
)

// ErrPassphraseRequired reports an encrypted namespace opened without (or with
// a wrong) passphrase.
var ErrPassphraseRequired = errors.New("archive session storage is encrypted: set TGA_SESSION_PASSPHRASE or --session-passphrase")

type EncryptedStorage struct {
	inner cstorage.Storage
	aead  cipher.AEAD
}

// IsEncrypted reports whether the namespace carries the encryption meta key.
func IsEncrypted(ctx context.Context, kv cstorage.Storage) (bool, error) {
	_, err := kv.Get(ctx, encMetaKey)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, cstorage.ErrNotFound) {
		return false, nil
	}
	return false, err
}

// SecureNamespace returns a storage view of inner whose values are sealed with
// a key derived from passphrase. Opening an empty namespace creates the salt;
// opening a legacy plaintext namespace re-seals the sensitive session/app keys
// in place; other plaintext values stay readable until rewritten.
func SecureNamespace(ctx context.Context, inner cstorage.Storage, passphrase string) (*EncryptedStorage, error) {
	passphrase = strings.TrimSpace(passphrase)
	if passphrase == "" {
		return nil, errors.New("empty passphrase")
	}
	salt, err := inner.Get(ctx, encMetaKey)
	fresh := false
	switch {
	case err == nil:
		// existing salt
	case errors.Is(err, cstorage.ErrNotFound):
		fresh = true
		salt = make([]byte, encSaltLen)
		if _, err = rand.Read(salt); err != nil {
			return nil, err
		}
		if err = inner.Set(ctx, encMetaKey, salt); err != nil {
			return nil, err
		}
	default:
		return nil, err
	}
	dk, err := scrypt.Key([]byte(passphrase), salt, scryptN, scryptR, scryptP, encKeyLen)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(dk)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	s := &EncryptedStorage{inner: inner, aead: aead}
	if fresh {
		// First encrypted open. Seal every pre-existing plaintext value, not
		// just app/session: a namespace may also hold other secrets (for
		// example the app hash written by an older build or an account
		// namespace key) and leaving those readable would defeat the point of
		// turning encryption on.
		if err := s.sealLegacyPlaintext(ctx); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// namespaceKeyLister is an optional capability of the underlying storage.
// storage.Storage only mandates Get/Set/Delete, so a backend that cannot
// enumerate its keys is handled by the fallback below.
type namespaceKeyLister interface {
	Keys(context.Context) ([]string, error)
}

// fallbackSecretKeys are the keys guaranteed to hold session material, used
// only when the backend cannot enumerate the namespace.
var fallbackSecretKeys = []string{keygen.New("app"), keygen.New("session")}

// sealLegacyPlaintext seals every value in the namespace that is not already
// sealed. Values are fetched through the inner storage so what is checked is
// the bytes actually at rest. encMetaKey is skipped because the salt has to be
// readable without a key.
func (s *EncryptedStorage) sealLegacyPlaintext(ctx context.Context) error {
	lister, ok := s.inner.(namespaceKeyLister)
	if !ok {
		for _, k := range fallbackSecretKeys {
			v, e := s.inner.Get(ctx, k)
			if e == nil {
				if e = s.Set(ctx, k, v); e != nil {
					return e
				}
			}
		}
		return nil
	}
	keys, err := lister.Keys(ctx)
	if err != nil {
		return err
	}
	for _, k := range keys {
		if k == encMetaKey {
			continue
		}
		v, err := s.inner.Get(ctx, k)
		if err != nil {
			if errors.Is(err, cstorage.ErrNotFound) {
				continue
			}
			return err
		}
		if isSealed(v) {
			continue
		}
		if err := s.Set(ctx, k, v); err != nil {
			return err
		}
	}
	return nil
}

func (s *EncryptedStorage) seal(plain []byte) ([]byte, error) {
	nonce := make([]byte, encNonceLen)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	out := make([]byte, 0, len(encMagic)+len(nonce)+len(plain)+s.aead.Overhead())
	out = append(out, encMagic...)
	out = append(out, nonce...)
	return s.aead.Seal(out, nonce, plain, nil), nil
}

func (s *EncryptedStorage) open(data []byte) ([]byte, error) {
	if !isSealed(data) {
		return data, nil // legacy plaintext value
	}
	prefix := len(encMagic) + encNonceLen
	if len(data) < prefix+s.aead.Overhead() {
		return nil, ErrPassphraseRequired
	}
	plain, err := s.aead.Open(nil, data[len(encMagic):prefix], data[prefix:], nil)
	if err != nil {
		return nil, ErrPassphraseRequired // wrong passphrase or corruption
	}
	return plain, nil
}

func isSealed(v []byte) bool {
	return len(v) > len(encMagic) && subtle.ConstantTimeCompare(v[:len(encMagic)], []byte(encMagic)) == 1
}

func (s *EncryptedStorage) Get(ctx context.Context, key string) ([]byte, error) {
	v, err := s.inner.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	return s.open(v)
}

func (s *EncryptedStorage) Set(ctx context.Context, key string, value []byte) error {
	if isSealed(value) { // defensive: never double-seal
		return s.inner.Set(ctx, key, value)
	}
	sealed, err := s.seal(value)
	if err != nil {
		return err
	}
	return s.inner.Set(ctx, key, sealed)
}

func (s *EncryptedStorage) Delete(ctx context.Context, key string) error {
	return s.inner.Delete(ctx, key)
}

// Reseal re-encrypts every plaintext value under the current key.
// Used after a fresh login: login helpers write the new session in plaintext
// through their own raw KV handle, and this seals it again post-hoc. Sealing
// the whole namespace instead of only app/session also cleans up material an
// older build wrote before full migration existed.
func (s *EncryptedStorage) Reseal(ctx context.Context) error {
	return s.sealLegacyPlaintext(ctx)
}

var _ cstorage.Storage = (*EncryptedStorage)(nil)
