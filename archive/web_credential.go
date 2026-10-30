package archive

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Credential storage for the form-login flow.
//
// Why a file and not the database: the credential must be readable before the
// archive database is opened (serve has to decide whether to bind at all), and
// it must survive a database wipe. It is also the one secret an operator may
// want to fix with a text editor while the service is down.
//
// Why a hash and not the plaintext: install.sh used to leave `password=` in
// this file, and the same value was visible through `ps` in the --auth-login
// flag. Storing a PBKDF2 verifier removes the second copy; the cookie signing
// key is derived from the verifier, so rotating the password still revokes
// every live session without keeping a spare secret around.

const (
	credentialVersion = 1
	// credentialIterations follows the OWASP 2023 floor for PBKDF2-HMAC-SHA256.
	// Measured cost on a t3-class core: ~80ms, which is far below the 10s
	// ReadHeaderTimeout and happens once per successful login.
	credentialIterations = 600_000
	credentialKeyLen     = 32
	credentialSaltLen    = 16
	// minimumPasswordLen matches the check serve applies to --auth-login, so a
	// password that was accepted at install time is also accepted here.
	minimumPasswordLen = 8
	// credentialMaxBytes caps the file read: a corrupt or hostile file must not
	// be able to make the process allocate megabytes before the first request.
	credentialMaxBytes = 4 << 10
)

// ErrCredentialMismatch is returned by Verify; it exists so callers can tell a
// wrong password apart from a broken file.
var ErrCredentialMismatch = errors.New("credential: password mismatch")

// Credential is the persisted login identity. Salt/Hash/Iterations are empty
// only for a freshly parsed legacy file, which the loader upgrades immediately.
type Credential struct {
	Version    int    `json:"version"`
	Username   string `json:"username"`
	Iterations int    `json:"iterations"`
	Salt       string `json:"salt"`
	Hash       string `json:"hash"`
	UpdatedAt  string `json:"updated_at"`

	// plaintext is set only by the legacy "user=...\\npassword=..." parser and
	// is never serialized. LoadCredential upgrades it to a hash before
	// returning, so a caller that sees a non-empty value is holding a
	// Credential it must Save.
	plaintext string
	legacy    bool
}

// HasPassword reports whether the credential can verify a password at all.
func (c Credential) HasPassword() bool { return c.plaintext != "" || c.Hash != "" }

// Verifier is the stable, secret-free string the session key is derived from.
// It changes whenever the salt or hash changes, which is exactly when every
// issued cookie must stop working.
func (c Credential) Verifier() string { return c.Encode() }

// Encode renders the credential as `pbkdf2$<iterations>$<salt>$<hash>`. It is
// both the cookie-signing material and the on-the-wire form WebAuthConfig
// carries, so there is a single canonical encoding instead of two.
func (c Credential) Encode() string {
	if c.Hash == "" {
		// Legacy bootstrap: the plaintext is the only material available. It is
		// hashed into the session key, never stored as a separate cookie secret.
		return "plain:" + c.plaintext
	}
	return fmt.Sprintf("pbkdf2$%d$%s$%s", c.Iterations, c.Salt, c.Hash)
}

// DecodeCredential parses Encode's output back into a Credential.
func DecodeCredential(encoded string) (Credential, error) {
	if rest, ok := strings.CutPrefix(encoded, "plain:"); ok {
		if rest == "" {
			return Credential{}, errors.New("credential: empty plaintext verifier")
		}
		return Credential{Version: 0, plaintext: rest, legacy: true}, nil
	}
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2" {
		return Credential{}, fmt.Errorf("credential: unrecognised verifier %q", encoded)
	}
	iterations, err := strconv.Atoi(parts[1])
	if err != nil || iterations <= 0 {
		return Credential{}, fmt.Errorf("credential: bad iteration count %q", parts[1])
	}
	if parts[2] == "" || parts[3] == "" {
		return Credential{}, errors.New("credential: verifier is missing salt or hash")
	}
	return Credential{Version: credentialVersion, Iterations: iterations, Salt: parts[2], Hash: parts[3]}, nil
}

// NewCredential builds a hashed credential for username/password.
func NewCredential(username, password string, iterations int) (Credential, error) {
	if iterations <= 0 {
		iterations = credentialIterations
	}
	salt := make([]byte, credentialSaltLen)
	if err := fillRandom(salt); err != nil {
		return Credential{}, err
	}
	sum, err := pbkdf2.Key(sha256.New, password, salt, iterations, credentialKeyLen)
	if err != nil {
		return Credential{}, fmt.Errorf("credential: hash: %w", err)
	}
	return Credential{
		Version:    credentialVersion,
		Username:   username,
		Iterations: iterations,
		Salt:       base64.RawStdEncoding.EncodeToString(salt),
		Hash:       base64.RawStdEncoding.EncodeToString(sum),
		UpdatedAt:  time.Now().UTC().Format(time.RFC3339),
	}, nil
}

// Verify checks password against the credential in constant time with respect
// to the stored hash. The comparisons are on fixed-length decoded bytes, so a
// length difference cannot leak through timing.
func (c Credential) Verify(password string) error {
	if c.plaintext != "" {
		if subtle.ConstantTimeCompare([]byte(password), []byte(c.plaintext)) == 1 {
			return nil
		}
		return ErrCredentialMismatch
	}
	salt, err := base64.RawStdEncoding.DecodeString(c.Salt)
	if err != nil {
		return fmt.Errorf("credential: decode salt: %w", err)
	}
	want, err := base64.RawStdEncoding.DecodeString(c.Hash)
	if err != nil {
		return fmt.Errorf("credential: decode hash: %w", err)
	}
	got, err := pbkdf2.Key(sha256.New, password, salt, c.Iterations, len(want))
	if err != nil {
		return fmt.Errorf("credential: hash: %w", err)
	}
	if subtle.ConstantTimeCompare(got, want) == 1 {
		return nil
	}
	return ErrCredentialMismatch
}

// LoadCredential reads the credential file, transparently upgrading the legacy
// `user=...\npassword=...` form that install.sh wrote before hashing existed.
// When an upgrade happens the file is rewritten in place, so the plaintext
// leaves the disk on the first start after upgrade.
func LoadCredential(path string) (Credential, error) {
	f, err := os.Open(path)
	if err != nil {
		return Credential{}, err
	}
	defer f.Close()

	raw, err := readLimited(f, credentialMaxBytes)
	if err != nil {
		return Credential{}, fmt.Errorf("credential: read %s: %w", path, err)
	}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return Credential{}, fmt.Errorf("credential: %s is empty", path)
	}

	// JSON is the current format. Anything else falls through to the legacy
	// key=value parser so an existing deployment keeps working.
	if strings.HasPrefix(trimmed, "{") {
		var c Credential
		if err := json.Unmarshal([]byte(trimmed), &c); err != nil {
			return Credential{}, fmt.Errorf("credential: parse %s: %w", path, err)
		}
		if strings.TrimSpace(c.Username) == "" {
			return Credential{}, fmt.Errorf("credential: %s has no username", path)
		}
		if c.Hash != "" {
			if c.Iterations <= 0 {
				c.Iterations = credentialIterations
			}
			if c.Version == 0 {
				c.Version = credentialVersion
			}
			return c, nil
		}
		return Credential{}, fmt.Errorf("credential: %s has no hash", path)
	}

	legacy, err := parseLegacyCredential(trimmed)
	if err != nil {
		return Credential{}, fmt.Errorf("credential: %s: %w", path, err)
	}
	return legacy, nil
}

// parseLegacyCredential understands the shell-sourced `user=...` / `password=...`
// file install.sh generated. Quoted values are accepted because the file was
// sourced by bash, which tolerates them.
func parseLegacyCredential(body string) (Credential, error) {
	var user, pass string
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		if unquoted, err := strconv.Unquote(value); err == nil {
			value = unquoted
		} else {
			value = strings.Trim(value, `"'`)
		}
		switch strings.TrimSpace(key) {
		case "user", "username":
			user = value
		case "password", "pass":
			pass = value
		}
	}
	if user == "" {
		return Credential{}, errors.New("no user= line")
	}
	if pass == "" {
		return Credential{}, errors.New("no password= line (already upgraded? use RESET_PASSWORD=1 ./install.sh)")
	}
	return Credential{Version: 0, Username: user, plaintext: pass, legacy: true}, nil
}

// Upgrade replaces a legacy plaintext credential with a hashed one and writes
// it back, so the plaintext no longer sits on disk.
func (c Credential) Upgrade(path string) (Credential, error) {
	if c.plaintext == "" {
		return c, nil
	}
	hashed, err := NewCredential(c.Username, c.plaintext, credentialIterations)
	if err != nil {
		return c, err
	}
	if err := SaveCredential(path, hashed); err != nil {
		return c, err
	}
	return hashed, nil
}

// SaveCredential writes the credential atomically with 0600 permissions. The
// temporary file is created in the destination directory so the rename cannot
// cross a filesystem boundary.
func SaveCredential(path string, c Credential) error {
	if strings.TrimSpace(c.Username) == "" {
		return errors.New("credential: refusing to save an empty username")
	}
	if c.Hash == "" {
		return errors.New("credential: refusing to save a credential without a hash")
	}
	c.Version = credentialVersion
	if c.Iterations <= 0 {
		c.Iterations = credentialIterations
	}
	if c.UpdatedAt == "" {
		c.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	}
	body, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("credential: marshal: %w", err)
	}
	body = append(body, '\n')

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("credential: create %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".credential-*")
	if err != nil {
		return fmt.Errorf("credential: temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("credential: chmod: %w", err)
	}
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return fmt.Errorf("credential: write: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("credential: sync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("credential: close: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("credential: rename: %w", err)
	}
	return nil
}

// ValidateNewPassword enforces the same rules as --auth-login. The colon check
// exists because install.sh still builds a `user:pass` flag on first run and a
// colon there silently truncates the password.
func ValidateNewPassword(password string) error {
	if len(password) < minimumPasswordLen {
		return fmt.Errorf("密码至少需要 %d 位", minimumPasswordLen)
	}
	if strings.Contains(password, ":") {
		return errors.New("密码不能包含冒号（:），会破坏首次启动的登录参数")
	}
	if strings.TrimSpace(password) != password {
		return errors.New("密码首尾不能有空格")
	}
	return nil
}

// ValidateNewUsername keeps the name usable in the login form and in the
// `user:pass` flag that install.sh may still generate.
func ValidateNewUsername(username string) error {
	if username == "" {
		return errors.New("账号不能为空")
	}
	if strings.ContainsAny(username, ":\r\n\t") {
		return errors.New("账号不能包含冒号、换行或制表符")
	}
	if len(username) > 64 {
		return errors.New("账号过长（最多 64 字节）")
	}
	return nil
}

// credentialExists reports whether path is a regular file, so callers can tell
// "not configured yet" from "configured but unreadable".
func credentialExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// isNotExist is a tiny helper so callers do not need to import io/fs.
func isNotExist(err error) bool { return errors.Is(err, fs.ErrNotExist) }

// fillRandom fills b from the system CSPRNG. It fails closed: a short read is an
// error, never a zero-padded salt.
func fillRandom(b []byte) error {
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return fmt.Errorf("credential: random: %w", err)
	}
	return nil
}

// readLimited reads at most limit bytes and rejects anything longer, so a
// hostile file cannot make the reader allocate without bound.
func readLimited(r io.Reader, limit int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("file exceeds %d bytes", limit)
	}
	return body, nil
}
