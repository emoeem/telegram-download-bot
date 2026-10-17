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
		// first encrypted open: seal pre-existing plaintext session material
		for _, k := range []string{keygen.New("app"), keygen.New("session")} {
			if v, e := inner.Get(ctx, k); e == nil {
				if e = s.Set(ctx, k, v); e != nil {
					return nil, e
				}
			}
		}
	}
	return s, nil
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

// Reseal re-encrypts the sensitive session/app keys under the current key.
// Used after a fresh login: login helpers write the new session in plaintext
// through their own raw KV handle, and this seals it again post-hoc.
func (s *EncryptedStorage) Reseal(ctx context.Context) error {
	for _, k := range []string{keygen.New("app"), keygen.New("session")} {
		if v, e := s.inner.Get(ctx, k); e == nil {
			if e = s.Set(ctx, k, v); e != nil {
				return e
			}
		}
	}
	return nil
}

var _ cstorage.Storage = (*EncryptedStorage)(nil)
