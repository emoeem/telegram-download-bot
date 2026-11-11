// Package license implements the offline commercial licence that gates this
// build: a device-bound, optionally expiring key the vendor signs on their own
// machine and the customer enters once.
//
// # Why there are two key formats
//
// The issuer has no server. Legitimacy must therefore be provable from the key
// alone, which points at public-key signatures. But Ed25519 signatures are 64
// bytes, and a signed 15-byte payload renders as 127 base32 characters — nobody
// reads that out over the phone or types it from a screenshot, and a licence
// key nobody can enter protects nothing.
//
// Truncating an Ed25519 signature is not an option: verification is an equation
// over the whole signature, so a 10-byte prefix cannot be verified at all. The
// two requirements ("customer can type it" and "customer cannot forge it") are
// genuinely in tension, so this package ships both ends of the trade-off rather
// than pretending one of them does not exist:
//
//	short key (40 chars)  payload + 10-byte HMAC-SHA256 truncation.
//	                      Typeable. Sound against anyone who does not extract
//	                      the HMAC secret out of the binary — which is the
//	                      bystander, not the reverse engineer.
//	strong key (117 chars) payload + full Ed25519 signature.
//	                      Unforgeable without the private key even if the whole
//	                      binary is disassembled. Paste-only.
//
// The issuer chooses per customer with `--strong`. Both are verified by the same
// Parse call, which dispatches on the prefix.
package license

import (
	"errors"
	"fmt"
	"time"
)

// Versions. The SHORT key carries its version inside the payload, because every
// byte of the 40-character budget is spoken for. The STRONG key pays for an
// explicit textual prefix instead, which is cheap when the key is 117
// characters.
const (
	Version = 1

	// StrongPrefix marks an Ed25519-signed key. It is deliberately not a
	// Crockford character sequence that could occur by chance in a short key:
	// short keys never contain '-', and the body after the prefix is 127
	// characters, so the two shapes cannot be confused.
	StrongPrefix = "TDL2-"

	// PayloadLen is the signed portion: version, serial, device, expiry.
	PayloadLen = 15
	// ShortMACLen is how many HMAC bytes a short key carries.
	ShortMACLen = 10
	// ShortRawLen is a short key before encoding (15 + 10 = 25 bytes).
	ShortRawLen = PayloadLen + ShortMACLen
	// ShortLen is a short key's encoded length with separators stripped.
	ShortLen = ShortRawLen * 8 / 5 // 40
	// GroupSize is how many characters sit between dashes in the display form.
	GroupSize = 8

	// Ed25519SigLen is a full signature.
	Ed25519SigLen = 64
	// StrongRawLen is a strong key before encoding (15 + 64 = 79 bytes).
	StrongRawLen = PayloadLen + Ed25519SigLen
	// StrongLen is a strong key's encoded body length.
	StrongLen = (StrongRawLen*8 + 4) / 5 // 127, ceil for the trailing 4 bits
)

// Errors are distinct because the UI says different things for each. A typo and
// a forged key are not the same conversation.
var (
	// ErrMalformed means the text cannot be a key at all.
	ErrMalformed = errors.New("密钥格式不正确")
	// ErrSignature means the shape is right but the MAC/signature is not.
	ErrSignature = errors.New("密钥校验失败")
	// ErrExpired means the key was valid and has run out.
	ErrExpired = errors.New("密钥已过期")
	// ErrDevice means the key is bound to a different machine.
	ErrDevice = errors.New("密钥不适用于本机")
	// ErrClock means the system clock moved backwards past a recorded instant,
	// which is how an expired key would otherwise be kept alive forever.
	ErrClock = errors.New("系统时间异常，请校准后重试")
	// ErrNotConfigured means this build carries no verifier at all.
	ErrNotConfigured = errors.New("本构建未配置授权校验")
)

// Kind distinguishes the two wire formats.
type Kind int

const (
	KindShort  Kind = iota + 1 // HMAC, typeable
	KindStrong                 // Ed25519, paste-only
)

func (k Kind) String() string {
	switch k {
	case KindShort:
		return "short"
	case KindStrong:
		return "strong"
	}
	return "unknown"
}

// Payload is the signed statement: which key this is, which machine it may run
// on, and until when.
type Payload struct {
	// Serial identifies this issuance. Support conversations quote it and the
	// local state records it, so it must differ per issuance.
	Serial uint64
	// Device is the 40-bit device code this key is bound to, or 0 for a key
	// that binds itself to whichever machine activates it first.
	Device uint64
	// ExpiryDay is days since the Unix epoch, or 0 for a key that never
	// expires. Day granularity removes the "expired at 23:59:59" argument and
	// keeps the payload small enough to fit the character budget.
	ExpiryDay uint32
}

// Forever reports whether the key never expires.
func (p Payload) Forever() bool { return p.ExpiryDay == 0 }

// ExpiresAt is the instant the key stops being valid: the end of the named UTC
// day, so a "1 天" key covers the whole day it names.
func (p Payload) ExpiresAt() time.Time {
	if p.Forever() {
		return time.Time{}
	}
	return time.Unix(int64(p.ExpiryDay)*86400, 0).UTC()
}

// Expired reports whether the key has run out at now.
func (p Payload) Expired(now time.Time) bool {
	if p.Forever() {
		return false
	}
	return !now.UTC().Before(p.ExpiresAt())
}

// DaysLeft is whole days remaining, or -1 when the key never expires.
func (p Payload) DaysLeft(now time.Time) int {
	if p.Forever() {
		return -1
	}
	d := int(p.ExpiresAt().Sub(now.UTC()).Hours() / 24)
	if d < 0 {
		return 0
	}
	return d
}

// pack serialises the payload into its fixed 15-byte form.
//
//	byte  0      version
//	bytes 1..5   serial   (40 bits, big endian)
//	bytes 6..10  device   (40 bits, big endian)
//	bytes 11..14 expiryDay(32 bits, big endian)
func (p Payload) pack() [PayloadLen]byte {
	var b [PayloadLen]byte
	b[0] = Version
	put40(b[1:], p.Serial)
	put40(b[6:], p.Device)
	b[11] = byte(p.ExpiryDay >> 24)
	b[12] = byte(p.ExpiryDay >> 16)
	b[13] = byte(p.ExpiryDay >> 8)
	b[14] = byte(p.ExpiryDay)
	return b
}

// unpack parses the fixed 15-byte payload.
func unpack(b []byte) (Payload, error) {
	if len(b) != PayloadLen {
		return Payload{}, ErrMalformed
	}
	if b[0] != Version {
		return Payload{}, fmt.Errorf("%w：版本 %d 不受支持", ErrMalformed, b[0])
	}
	return Payload{
		Serial:    get40(b[1:6]),
		Device:    get40(b[6:11]),
		ExpiryDay: uint32(b[11])<<24 | uint32(b[12])<<16 | uint32(b[13])<<8 | uint32(b[14]),
	}, nil
}

func put40(dst []byte, v uint64) {
	dst[0] = byte(v >> 32)
	dst[1] = byte(v >> 24)
	dst[2] = byte(v >> 16)
	dst[3] = byte(v >> 8)
	dst[4] = byte(v)
}

func get40(src []byte) uint64 {
	return uint64(src[0])<<32 | uint64(src[1])<<24 | uint64(src[2])<<16 |
		uint64(src[3])<<8 | uint64(src[4])
}
