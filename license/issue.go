package license

import (
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"time"
)

// domainSeparator prefixes every signed message.
//
// Without it, the same master secret signs two different message shapes (the
// short key's MAC and the strong key's Ed25519 signature over the same payload
// bytes), and a value produced for one context could be replayed in the other.
// Domain separation is cheap and removes the question entirely.
const domainSeparator = "tdl-license-v1\x00"

// MasterKeySize is the size of the one secret the issuer has to guard. Both the
// HMAC key and the Ed25519 seed are derived from it, so losing or leaking it is
// one event to reason about rather than two.
const MasterKeySize = 32

// KDF labels. Changing either value invalidates every key already issued, so
// they are constants and never derived from input.
const (
	labelHMAC = "tdl-license/hmac/v1"
	labelEd   = "tdl-license/ed25519/v1"
)

// derive expands the master key into a sub-key for one purpose.
//
// HMAC-SHA256 as a KDF is the standard "expand one secret into several" move
// when the sub-keys only ever feed hashes and signatures; a full HKDF would add
// API surface without changing the security argument here.
func derive(master []byte, label string) []byte {
	h := hmac.New(sha256.New, master)
	h.Write([]byte(label))
	return h.Sum(nil)
}

// HMACKey returns the short-key verification key derived from master.
func HMACKey(master []byte) []byte { return derive(master, labelHMAC) }

// Ed25519Seed returns the strong-key signing seed derived from master.
func Ed25519Seed(master []byte) []byte { return derive(master, labelEd) }

// Ed25519Private returns the signing key derived from master.
func Ed25519Private(master []byte) ed25519.PrivateKey {
	return ed25519.NewKeyFromSeed(Ed25519Seed(master))
}

// Ed25519Public returns the verification key derived from master. This is the
// value compiled into the shipped binary; the master never is.
func Ed25519Public(master []byte) ed25519.PublicKey {
	return Ed25519Private(master).Public().(ed25519.PublicKey)
}

// NewMasterKey returns a fresh master secret for a new issuer.
func NewMasterKey() ([]byte, error) {
	b := make([]byte, MasterKeySize)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("生成主密钥: %w", err)
	}
	return b, nil
}

// signedMessage is what both formats sign: the separator followed by the packed
// payload.
func signedMessage(payload [PayloadLen]byte) []byte {
	msg := make([]byte, 0, len(domainSeparator)+PayloadLen)
	msg = append(msg, domainSeparator...)
	msg = append(msg, payload[:]...)
	return msg
}

// maskDomain separates the payload mask from every other derived value.
const maskDomain = "tdl-license/mask/v1"

// maskFor derives the 15-byte payload mask from the short-key verifier.
//
// A raw payload leaks its own structure: a key for an unbound device renders as
// "...-S4000000-00000000-...", and anyone comparing two keys can read off where
// the device field starts and whether it is zero. Masking costs nothing, keeps
// the character budget identical, and makes a key look like what it is to a
// customer — opaque.
//
// This is obfuscation, not a security boundary: the mask is derivable from the
// verifier that ships in the binary. The security boundary is the MAC below it.
func maskFor(verifierKey []byte) [PayloadLen]byte {
	h := hmac.New(sha256.New, verifierKey)
	h.Write([]byte(maskDomain))
	sum := h.Sum(nil)
	var m [PayloadLen]byte
	copy(m[:], sum[:PayloadLen])
	return m
}

func xorPayload(in [PayloadLen]byte, mask [PayloadLen]byte) [PayloadLen]byte {
	var out [PayloadLen]byte
	for i := 0; i < PayloadLen; i++ {
		out[i] = in[i] ^ mask[i]
	}
	return out
}

// shortMAC is the 10-byte truncation carried by a short key, computed over the
// masked payload so the MAC covers exactly the bytes that travel on the wire.
func shortMAC(master []byte, masked [PayloadLen]byte) []byte {
	key := HMACKey(master)
	h := hmac.New(sha256.New, key)
	h.Write(signedMessage(masked))
	return h.Sum(nil)[:ShortMACLen]
}

// IssueShort returns a typeable key. See the package comment for what its
// strength does and does not cover.
func IssueShort(master []byte, device uint64, days int) (string, Payload, error) {
	p, err := newPayload(device, days)
	if err != nil {
		return "", Payload{}, err
	}
	pb := p.pack()
	masked := xorPayload(pb, maskFor(HMACKey(master)))
	raw := make([]byte, 0, ShortRawLen)
	raw = append(raw, masked[:]...)
	raw = append(raw, shortMAC(master, masked)...)
	return group(encodeBytes(raw), GroupSize, 0), p, nil
}

// IssueStrong returns a paste-only key whose signature cannot be forged even by
// someone who disassembles the binary, because the private half is not in it.
func IssueStrong(master []byte, device uint64, days int) (string, Payload, error) {
	p, err := newPayload(device, days)
	if err != nil {
		return "", Payload{}, err
	}
	pb := p.pack()
	sig := ed25519.Sign(Ed25519Private(master), signedMessage(pb))
	raw := make([]byte, 0, StrongRawLen)
	raw = append(raw, pb[:]...)
	raw = append(raw, sig...)
	return StrongPrefix + group(encodeBytes(raw), GroupSize, len(StrongPrefix)), p, nil
}

// newPayload validates the issuance parameters and draws a serial.
func newPayload(device uint64, days int) (Payload, error) {
	if days < 0 {
		return Payload{}, errors.New("有效天数不能为负")
	}
	if device > maxDevice {
		return Payload{}, fmt.Errorf("设备码超出 40 位范围")
	}
	serial, err := randomSerial()
	if err != nil {
		return Payload{}, err
	}
	var expiry uint32
	if days > 0 {
		// End of the last covered UTC day, so "30 天" is 30 whole days from
		// today rather than 30 days minus the current time of day.
		expiry = uint32(time.Now().UTC().AddDate(0, 0, days).Unix() / 86400)
		if expiry == 0 {
			// Never let a real expiry collide with the "forever" sentinel.
			expiry = 1
		}
	}
	return Payload{Serial: serial, Device: device, ExpiryDay: expiry}, nil
}

// maxDevice is the largest 40-bit device code.
const maxDevice = (1 << 40) - 1

// randomSerial draws a 40-bit serial with the top bit cleared, leaving 39 bits
// of entropy (about 5.5e11 values). The top bit is reserved so a future format
// can use it as a flag without invalidating keys already in the field.
func randomSerial() (uint64, error) {
	var b [5]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0, fmt.Errorf("生成序列号: %w", err)
	}
	return get40(b[:]) & ((1 << 39) - 1), nil
}

// EncodeMaster renders a master key for storage. The issuer keeps this string;
// it is the only thing they must not lose.
func EncodeMaster(master []byte) string {
	return "TDLSK1-" + base64.RawURLEncoding.EncodeToString(master)
}

// ParseMaster reads a master key back.
func ParseMaster(s string) ([]byte, error) {
	const p = "TDLSK1-"
	if len(s) < len(p) || s[:len(p)] != p {
		return nil, errors.New("主密钥格式不正确（应以 TDLSK1- 开头）")
	}
	b, err := base64.RawURLEncoding.DecodeString(s[len(p):])
	if err != nil {
		return nil, fmt.Errorf("主密钥解码失败: %w", err)
	}
	if len(b) != MasterKeySize {
		return nil, fmt.Errorf("主密钥长度应为 %d 字节，实际 %d", MasterKeySize, len(b))
	}
	return b, nil
}

// EncodePublicKey renders the verification key for -ldflags injection.
func EncodePublicKey(master []byte) string {
	return base64.StdEncoding.EncodeToString(Ed25519Public(master))
}

// EncodeHMACKey renders the short-key verifier for -ldflags injection.
func EncodeHMACKey(master []byte) string {
	return base64.StdEncoding.EncodeToString(HMACKey(master))
}

// group inserts dashes every n characters, skipping a leading prefix that must
// be reproduced verbatim.
func group(body string, n int, prefixLen int) string {
	out := make([]byte, 0, len(body)+len(body)/n+1)
	for i := 0; i < len(body); i++ {
		if i > 0 && i%n == 0 {
			out = append(out, '-')
		}
		out = append(out, body[i])
	}
	return string(out)
}
