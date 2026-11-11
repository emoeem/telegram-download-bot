package license

import (
	"crypto/ed25519"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// newIssuer builds a master key and the matching verifier, which is what a real
// deployment gets by compiling the derived keys in.
func newIssuer(t *testing.T) (master []byte, v *Verifier) {
	t.Helper()
	m, err := NewMasterKey()
	if err != nil {
		t.Fatalf("NewMasterKey: %v", err)
	}
	ver, err := NewVerifier(EncodeHMACKey(m), EncodePublicKey(m))
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	return m, ver
}

// fixedTime is inside the valid window of any key issued for >= 1 day.
var fixedTime = time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

func TestShortKeyRoundTrip(t *testing.T) {
	master, ver := newIssuer(t)
	dev := DeviceCode("android", "abc")
	key, issued, err := IssueShort(master, dev, 30)
	if err != nil {
		t.Fatalf("IssueShort: %v", err)
	}
	if n := len(stripSeparators(key)); n != ShortLen {
		t.Fatalf("短密钥应为 %d 字符，实际 %d（%q）", ShortLen, n, key)
	}
	got, kind, err := ver.Parse(key)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if kind != KindShort {
		t.Fatalf("kind = %v, want short", kind)
	}
	if got.Serial != issued.Serial || got.Device != dev || got.ExpiryDay != issued.ExpiryDay {
		t.Fatalf("payload 往返不一致: got %+v want %+v", got, issued)
	}
}

func TestStrongKeyRoundTrip(t *testing.T) {
	master, ver := newIssuer(t)
	dev := DeviceCode("android", "xyz")
	key, issued, err := IssueStrong(master, dev, 0)
	if err != nil {
		t.Fatalf("IssueStrong: %v", err)
	}
	if !strings.HasPrefix(key, StrongPrefix) {
		t.Fatalf("强密钥应带前缀 %q，实际 %q", StrongPrefix, key[:12])
	}
	got, kind, err := ver.Parse(key)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if kind != KindStrong {
		t.Fatalf("kind = %v, want strong", kind)
	}
	if got.Serial != issued.Serial || !got.Forever() {
		t.Fatalf("payload 往返不一致: got %+v want %+v", got, issued)
	}
}

// TestTamperedKeyRejected flips one payload bit and requires a signature error
// rather than a parse error: the shape is still legal, only the signature says
// the payload was not issued by us.
func TestTamperedKeyRejected(t *testing.T) {
	master, ver := newIssuer(t)
	key, _, err := IssueShort(master, 12345, 30)
	if err != nil {
		t.Fatalf("IssueShort: %v", err)
	}
	body := stripSeparators(key)
	// Find a character whose increment keeps it inside the alphabet, and change
	// it. The serial lives in bytes 1..5, i.e. roughly characters 2..9.
	idx := 3
	flipped := []byte(body)
	for i := 0; i < len(alphabet); i++ {
		c := alphabet[i]
		if c != flipped[idx] {
			flipped[idx] = c
			break
		}
	}
	bad := string(flipped)
	if _, _, err := ver.Parse(bad); !errors.Is(err, ErrSignature) {
		t.Fatalf("篡改后应返回 ErrSignature，实际 %v", err)
	}
}

// TestDifferentMasterRejected proves a key from one issuer does not work in a
// build compiled against another issuer's material — the property that makes
// "only I can issue" true rather than aspirational.
func TestDifferentMasterRejected(t *testing.T) {
	theirs, _ := newIssuer(t)
	_, mine := newIssuer(t)
	key, _, err := IssueShort(theirs, 1, 30)
	if err != nil {
		t.Fatalf("IssueShort: %v", err)
	}
	if _, _, err := mine.Parse(key); !errors.Is(err, ErrSignature) {
		t.Fatalf("他人签发的密钥应被拒绝，实际 %v", err)
	}
}

func TestMalformedInputs(t *testing.T) {
	_, ver := newIssuer(t)
	cases := []struct {
		name string
		in   string
	}{
		{"empty", ""},
		{"too short", "ABCD"},
		{"illegal char", strings.Repeat("U", ShortLen)},
		{"too long", strings.Repeat("A", ShortLen+1)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, _, err := ver.Parse(c.in); err == nil {
				t.Fatalf("%q 应被拒绝", c.in)
			}
		})
	}
}

// TestFormattingForgiven requires that the dashes, spaces and lower case a chat
// client may add do not change the key.
func TestFormattingForgiven(t *testing.T) {
	master, ver := newIssuer(t)
	key, want, err := IssueShort(master, 777, 7)
	if err != nil {
		t.Fatalf("IssueShort: %v", err)
	}
	variants := []string{
		key,
		strings.ToLower(key),
		strings.ReplaceAll(key, "-", ""),
		strings.ReplaceAll(key, "-", " "),
		"  " + key + "\n",
		strings.ToUpper(strings.ReplaceAll(key, "-", "_")),
	}
	for _, v := range variants {
		got, _, err := ver.Parse(v)
		if err != nil {
			t.Fatalf("变体 %q 应通过：%v", v, err)
		}
		if got.Serial != want.Serial {
			t.Fatalf("变体 %q 解析出不同序列号", v)
		}
	}
}

// TestO1OmittedLetters covers the Crockford folding: a customer who reads 0 as
// O or 1 as I must still activate.
func TestO1OmittedLetters(t *testing.T) {
	master, ver := newIssuer(t)
	// Craft a key whose body contains 0 and 1 so the substitution is exercised.
	key, _, err := IssueShort(master, 0x01010, 5)
	if err != nil {
		t.Fatalf("IssueShort: %v", err)
	}
	body := stripSeparators(key)
	messed := strings.NewReplacer("0", "O", "1", "I").Replace(body)
	if _, _, err := ver.Parse(messed); err != nil {
		t.Fatalf("O/I 替换后应仍可解析：%v", err)
	}
}

func TestExpiry(t *testing.T) {
	master, ver := newIssuer(t)
	key, p, err := IssueShort(master, 0, 1)
	if err != nil {
		t.Fatalf("IssueShort: %v", err)
	}
	if p.Forever() {
		t.Fatal("1 天密钥不应是永久")
	}
	// Two days after issuance the key must be expired.
	future := time.Now().UTC().AddDate(0, 0, 2)
	if !p.Expired(future) {
		t.Fatalf("到期后应判为过期，expires=%s now=%s", p.ExpiresAt(), future)
	}
	if _, _, err := ver.Parse(key); err != nil {
		t.Fatalf("Parse 本身不该关心时间：%v", err)
	}
}

func TestForeverKey(t *testing.T) {
	master, _ := newIssuer(t)
	_, p, err := IssueShort(master, 0, 0)
	if err != nil {
		t.Fatalf("IssueShort: %v", err)
	}
	if !p.Forever() {
		t.Fatal("days=0 应签发永久密钥")
	}
	if p.Expired(time.Now().AddDate(100, 0, 0)) {
		t.Fatal("永久密钥不应过期")
	}
	if p.DaysLeft(time.Now()) != -1 {
		t.Fatal("永久密钥 DaysLeft 应为 -1")
	}
}

func TestNegativeDaysRejected(t *testing.T) {
	master, _ := newIssuer(t)
	if _, _, err := IssueShort(master, 0, -1); err == nil {
		t.Fatal("负天数应被拒绝")
	}
}

// TestDeviceBinding covers the requirement that one key cannot activate a
// second machine.
func TestDeviceBinding(t *testing.T) {
	master, ver := newIssuer(t)
	devA := DeviceCode("phone-A")
	devB := DeviceCode("phone-B")
	key, _, err := IssueShort(master, devA, 30)
	if err != nil {
		t.Fatalf("IssueShort: %v", err)
	}

	dirA := t.TempDir()
	mA := NewManager(dirA, ver, devA)
	if _, err := mA.Activate(key, fixedTime); err != nil {
		t.Fatalf("A 机激活应成功：%v", err)
	}
	if err := mA.Gate(fixedTime); err != nil {
		t.Fatalf("A 机通过应成功：%v", err)
	}

	dirB := t.TempDir()
	mB := NewManager(dirB, ver, devB)
	_, err = mB.Activate(key, fixedTime)
	if !errors.Is(err, ErrDevice) {
		t.Fatalf("B 机应被设备绑定拒绝，实际 %v", err)
	}
}

// TestFirstActivationBinds covers device==0: the key binds to whichever machine
// activates it first, and a second machine with a copied activation file is
// still rejected because the payload's zero means "check the recorded device".
func TestFirstActivationBinds(t *testing.T) {
	master, ver := newIssuer(t)
	devA := DeviceCode("phone-A")
	devB := DeviceCode("phone-B")
	key, _, err := IssueShort(master, 0, 30)
	if err != nil {
		t.Fatalf("IssueShort: %v", err)
	}

	mA := NewManager(t.TempDir(), ver, devA)
	if _, err := mA.Activate(key, fixedTime); err != nil {
		t.Fatalf("A 机激活应成功：%v", err)
	}

	// Simulate copying the activation file to B.
	bDir := t.TempDir()
	if mB := NewManager(bDir, ver, devA); true {
		if _, err := mB.Activate(key, fixedTime); err != nil {
			t.Fatalf("准备阶段：%v", err)
		}
	}
	if err := copyState(t, filepathOf(mA), filepathOf(NewManager(bDir, ver, devB))); err != nil {
		t.Fatalf("复制激活文件失败：%v", err)
	}
	mB2 := NewManager(bDir, ver, devB)
	if err := mB2.Gate(fixedTime); !errors.Is(err, ErrDevice) {
		t.Fatalf("复制激活文件到 B 机应被拒绝，实际 %v", err)
	}
}

func filepathOf(m *Manager) string { return m.statePath() }

func copyState(t *testing.T, from, to string) error {
	t.Helper()
	b, err := os.ReadFile(from)
	if err != nil {
		return err
	}
	return os.WriteFile(to, b, 0o600)
}

// TestClockRollback proves an expired key cannot be kept alive by moving the
// system clock backwards.
func TestClockRollback(t *testing.T) {
	master, ver := newIssuer(t)
	dev := DeviceCode("phone")
	key, _, err := IssueShort(master, dev, 30)
	if err != nil {
		t.Fatalf("IssueShort: %v", err)
	}
	m := NewManager(t.TempDir(), ver, dev)
	if _, err := m.Activate(key, fixedTime); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	// Advance well past the tolerance so the ratchet is set.
	later := fixedTime.Add(48 * time.Hour)
	if err := m.Gate(later); err != nil {
		t.Fatalf("Gate(later): %v", err)
	}
	// Now wind the clock back before the recorded instant.
	if err := m.Gate(fixedTime); !errors.Is(err, ErrClock) {
		t.Fatalf("时钟回拨应被拒绝，实际 %v", err)
	}
	// Small drift must still be tolerated.
	if err := m.Gate(later.Add(-time.Hour)); err != nil {
		t.Fatalf("一小时内漂移应被容忍：%v", err)
	}
}

func TestExpiredActivationRejected(t *testing.T) {
	master, ver := newIssuer(t)
	dev := DeviceCode("phone")
	key, p, err := IssueShort(master, dev, 1)
	if err != nil {
		t.Fatalf("IssueShort: %v", err)
	}
	m := NewManager(t.TempDir(), ver, dev)
	if _, err := m.Activate(key, fixedTime); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	after := p.ExpiresAt().Add(24 * time.Hour)
	if err := m.Gate(after); !errors.Is(err, ErrExpired) {
		t.Fatalf("到期后应被拒绝，实际 %v", err)
	}
}

func TestNotActivatedGate(t *testing.T) {
	_, ver := newIssuer(t)
	m := NewManager(t.TempDir(), ver, DeviceCode("x"))
	if err := m.Gate(fixedTime); !IsNotActivated(err) {
		t.Fatalf("未激活应返回 errNotActivated，实际 %v", err)
	}
}

func TestUnconfiguredBuildFailsClosed(t *testing.T) {
	v, err := NewVerifier("", "")
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	if v.Configured() {
		t.Fatal("空配置不应报告为已配置")
	}
	m := NewManager(t.TempDir(), v, 1)
	if err := m.Gate(fixedTime); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("未配置构建应失败关闭，实际 %v", err)
	}
	if _, _, err := v.Parse("anything"); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("未配置构建解析应返回 ErrNotConfigured，实际 %v", err)
	}
}

// TestShortOnlyBuildRejectsStrongKey is the specific migration hazard: a build
// compiled with only the HMAC key must say "this build cannot check strong keys"
// rather than "bad key".
func TestShortOnlyBuildRejectsStrongKey(t *testing.T) {
	master, _ := newIssuer(t)
	v, err := NewVerifier(EncodeHMACKey(master), "")
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	strong, _, err := IssueStrong(master, 0, 30)
	if err != nil {
		t.Fatalf("IssueStrong: %v", err)
	}
	if _, _, err := v.Parse(strong); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("应返回 ErrNotConfigured，实际 %v", err)
	}
}

func TestDeviceCodeStable(t *testing.T) {
	a := DeviceCode("android", "fp-1")
	b := DeviceCode("android", "fp-1")
	if a != b {
		t.Fatal("相同输入应得到相同设备码")
	}
	if DeviceCode("android", "fp-2") == a {
		t.Fatal("不同输入应得到不同设备码")
	}
	// Field boundaries must matter.
	if DeviceCode("ab", "c") == DeviceCode("a", "bc") {
		t.Fatal("字段拼接不应产生碰撞")
	}
	if a > maxDevice {
		t.Fatalf("设备码超出 40 位：%x", a)
	}
}

func TestDeviceCodeFormatRoundTrip(t *testing.T) {
	for _, v := range []uint64{0, 1, 12345, maxDevice} {
		s := FormatDeviceCode(v)
		if len(s) != DeviceCodeLen {
			t.Fatalf("%d 渲染成 %q，长度 %d", v, s, len(s))
		}
		back, err := ParseDeviceCode(s)
		if err != nil {
			t.Fatalf("ParseDeviceCode(%q): %v", s, err)
		}
		if back != v {
			t.Fatalf("往返不一致: %d -> %q -> %d", v, s, back)
		}
	}
}

func TestMasterKeyRoundTrip(t *testing.T) {
	m, err := NewMasterKey()
	if err != nil {
		t.Fatalf("NewMasterKey: %v", err)
	}
	s := EncodeMaster(m)
	back, err := ParseMaster(s)
	if err != nil {
		t.Fatalf("ParseMaster: %v", err)
	}
	if string(back) != string(m) {
		t.Fatal("主密钥往返不一致")
	}
	if _, err := ParseMaster("nope"); err == nil {
		t.Fatal("非法主密钥应被拒绝")
	}
}

// TestClearAllowsReactivation covers the support case "customer bought a new
// key": clearing must not leave state that blocks the next activation.
func TestClearAllowsReactivation(t *testing.T) {
	master, ver := newIssuer(t)
	dev := DeviceCode("phone")
	first, _, err := IssueShort(master, dev, 30)
	if err != nil {
		t.Fatalf("IssueShort: %v", err)
	}
	m := NewManager(t.TempDir(), ver, dev)
	if _, err := m.Activate(first, fixedTime); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if err := m.Clear(); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if err := m.Gate(fixedTime); !IsNotActivated(err) {
		t.Fatalf("清除后应回到未激活，实际 %v", err)
	}
	if _, err := m.Activate(first, fixedTime); err != nil {
		t.Fatalf("清除后重新激活应成功：%v", err)
	}
}

// TestStrongKeyTamperRejected is the counterpart of TestTamperedKeyRejected for
// the Ed25519 path.
func TestStrongKeyTamperRejected(t *testing.T) {
	master, ver := newIssuer(t)
	key, _, err := IssueStrong(master, 42, 30)
	if err != nil {
		t.Fatalf("IssueStrong: %v", err)
	}
	body := []byte(stripSeparators(strings.TrimPrefix(key, StrongPrefix)))
	idx := 5
	for i := 0; i < len(alphabet); i++ {
		if alphabet[i] != body[idx] {
			body[idx] = alphabet[i]
			break
		}
	}
	if _, _, err := ver.Parse(StrongPrefix + string(body)); !errors.Is(err, ErrSignature) {
		t.Fatalf("篡改强密钥应返回 ErrSignature，实际 %v", err)
	}
}

// TestSignatureIsActuallyChecked guards against the classic bug where a verifier
// is wired up but never consulted: a random 40-character body must fail.
func TestSignatureIsActuallyChecked(t *testing.T) {
	_, ver := newIssuer(t)
	// "04" encodes version 1 (00000001 in the first eight bits) so the shape is
	// legal and only the signature can reject it.
	body := "04" + strings.Repeat("7", ShortLen-2)
	if _, _, err := ver.Parse(body); !errors.Is(err, ErrSignature) {
		t.Fatalf("随机 body 应因签名失败被拒，实际 %v", err)
	}
}

var _ = ed25519.PublicKeySize
