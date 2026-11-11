package license

import (
	"bytes"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// Verifier validates keys using whatever material this build was compiled with.
//
// A build that carries neither key reports ErrNotConfigured rather than
// accepting everything: a licensing bug must fail closed, because the failure
// mode of the other choice is "the product ships free".
type Verifier struct {
	hmacKey []byte
	pub     ed25519.PublicKey
}

// NewVerifier builds a verifier from the two injected strings. Either may be
// empty, in which case that key format is unavailable and produces
// ErrNotConfigured for keys of that shape.
func NewVerifier(hmacKeyB64, pubB64 string) (*Verifier, error) {
	v := &Verifier{}
	if s := strings.TrimSpace(hmacKeyB64); s != "" {
		k, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			return nil, fmt.Errorf("HMAC 校验密钥解码失败: %w", err)
		}
		if len(k) != 32 {
			return nil, fmt.Errorf("HMAC 校验密钥长度应为 32 字节，实际 %d", len(k))
		}
		v.hmacKey = k
	}
	if s := strings.TrimSpace(pubB64); s != "" {
		p, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			return nil, fmt.Errorf("Ed25519 公钥解码失败: %w", err)
		}
		if len(p) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("Ed25519 公钥长度应为 %d 字节，实际 %d", ed25519.PublicKeySize, len(p))
		}
		v.pub = ed25519.PublicKey(p)
	}
	return v, nil
}

// Configured reports whether this build can verify anything at all.
func (v *Verifier) Configured() bool {
	if v == nil {
		return false
	}
	return len(v.hmacKey) > 0 || len(v.pub) > 0
}

// Supports reports whether this build can verify the given format.
func (v *Verifier) Supports(k Kind) bool {
	if v == nil {
		return false
	}
	switch k {
	case KindShort:
		return len(v.hmacKey) > 0
	case KindStrong:
		return len(v.pub) > 0
	}
	return false
}

// DetectKind reports which format a piece of text claims to be.
//
// It looks at the prefix only, so a caller can report "这个构建不支持强密钥"
// instead of "格式不正确" when a customer pastes a strong key into a build that
// was compiled without the public key.
func DetectKind(text string) Kind {
	if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(text)), StrongPrefix) {
		return KindStrong
	}
	return KindShort
}

// Parse verifies text and returns the statement it carries.
//
// Checks run cheapest-first, and the shape is fully validated before any
// cryptographic work: a mistyped key should say "格式不正确", not "校验失败",
// because those two messages send the customer down entirely different paths.
func (v *Verifier) Parse(text string) (Payload, Kind, error) {
	kind := DetectKind(text)
	if !v.Supports(kind) {
		return Payload{}, kind, ErrNotConfigured
	}

	body, err := splitBody(text, kind)
	if err != nil {
		return Payload{}, kind, err
	}
	raw, err := decodeBytes(body, rawLenFor(kind))
	if err != nil {
		return Payload{}, kind, wrapMalformed(err, kind)
	}

	// The short format carries a masked payload; the strong format does not
	// (127 characters is not something anyone eyeballs for structure).
	masked := raw[:PayloadLen]
	var (
		payloadBytes [PayloadLen]byte
		sig          = raw[PayloadLen:]
	)
	switch kind {
	case KindShort:
		if !hmac.Equal(shortMACFromKey(v.hmacKey, masked), sig) {
			return Payload{}, kind, ErrSignature
		}
		payloadBytes = xorPayload(toArray(masked), maskFor(v.hmacKey))
	case KindStrong:
		if !ed25519.Verify(v.pub, signedMessage(toArray(masked)), sig) {
			return Payload{}, kind, ErrSignature
		}
		payloadBytes = toArray(masked)
	}

	p, err := unpack(payloadBytes[:])
	if err != nil {
		return Payload{}, kind, err
	}
	return p, kind, nil
}

// shortMACFromKey recomputes the truncation from a raw HMAC key, mirroring
// shortMAC so the issuer and the verifier cannot drift apart.
func shortMACFromKey(key []byte, masked []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(signedMessage(toArray(masked)))
	return h.Sum(nil)[:ShortMACLen]
}

func toArray(b []byte) [PayloadLen]byte {
	var a [PayloadLen]byte
	copy(a[:], b)
	return a
}

// rawLenFor is the decoded byte length of a key body.
func rawLenFor(k Kind) int {
	if k == KindStrong {
		return StrongRawLen
	}
	return ShortRawLen
}

// splitBody strips the display formatting (dashes, spaces, line breaks, case)
// and returns the bare encoded body.
func splitBody(text string, kind Kind) (string, error) {
	s := strings.TrimSpace(text)
	if kind == KindStrong {
		// The prefix is matched case-insensitively so a chat client that
		// lower-cased the message does not turn a valid key into a typo.
		if len(s) < len(StrongPrefix) || !strings.EqualFold(s[:len(StrongPrefix)], StrongPrefix) {
			return "", ErrMalformed
		}
		s = s[len(StrongPrefix):]
	}
	body := stripSeparators(s)
	if kind == KindShort && len(body) != ShortLen {
		return "", fmt.Errorf("%w：应为 %d 个字符，实际 %d 个", ErrMalformed, ShortLen, len(body))
	}
	if kind == KindStrong && len(body) != StrongLen {
		return "", fmt.Errorf("%w：应为 %d 个字符，实际 %d 个", ErrMalformed, StrongLen, len(body))
	}
	return body, nil
}

// stripSeparators removes everything a human, a chat client, or a copy-paste
// may have inserted, and upper-cases the rest.
func stripSeparators(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '-', ' ', '\t', '\n', '\r', '_', '.', '·':
			continue
		}
		b.WriteByte(upper(c))
	}
	return b.String()
}

// wrapMalformed rewrites a length error for the strong format so the customer is
// told which length to look for.
func wrapMalformed(err error, k Kind) error {
	if k == KindStrong && errors.Is(err, ErrMalformed) {
		return err
	}
	return err
}

// encodeBytes renders raw bytes as Crockford base32, padding the final partial
// group on the right with zero bits.
//
// The padding is only meaningful to decodeBytes, which rejects a final character
// whose low bits are not zero — otherwise two different strings would decode to
// the same key, and a key could be "valid" in more than one spelling.
func encodeBytes(raw []byte) string {
	out := make([]byte, 0, (len(raw)*8+4)/5)
	var acc uint32
	var bits uint
	for _, b := range raw {
		acc = acc<<8 | uint32(b)
		bits += 8
		for bits >= 5 {
			bits -= 5
			out = append(out, alphabet[(acc>>bits)&0x1f])
		}
	}
	if bits > 0 {
		out = append(out, alphabet[(acc<<(5-bits))&0x1f])
	}
	return string(out)
}

// decodeBytes is the inverse of encodeBytes, enforcing canonical padding.
func decodeBytes(body string, wantLen int) ([]byte, error) {
	var acc uint32
	var bits uint
	out := make([]byte, 0, wantLen)
	for i := 0; i < len(body); i++ {
		d := decodeMap[body[i]]
		if d < 0 {
			return nil, fmt.Errorf("%w：含有无效字符 %q", ErrMalformed, string(body[i]))
		}
		acc = acc<<5 | uint32(d)
		bits += 5
		if bits >= 8 {
			bits -= 8
			out = append(out, byte(acc>>bits))
		}
	}
	// Any leftover bits are padding and must be zero.
	if bits > 0 && acc&((1<<bits)-1) != 0 {
		return nil, ErrMalformed
	}
	if len(out) != wantLen {
		return nil, ErrMalformed
	}
	return out, nil
}

// VerifyBytes is a test hook: it reports whether raw is exactly what Encode
// produced, which catches display/parse drift without going through text.
func VerifyBytes(a, b []byte) bool { return bytes.Equal(a, b) }
