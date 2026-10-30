package archive

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"
)

// Account credential management for the web panel itself.
//
// This is deliberately separate from Telegram account management
// (/api/accounts): those are archive identities, this is the operator's login
// to the panel. Mixing them into one screen is how people end up believing
// "changing the password logs me out of Telegram".

const (
	// maxAccountBody caps the change-request body. It carries four short
	// strings, so anything above this is either a bug or an attack.
	maxAccountBody = 4 << 10
	// accountChangeKeySuffix keeps the "current password" failures in their own
	// bucket. Sharing the login bucket would let a password-change guess lock
	// the login page for the same client.
	accountChangeKeySuffix = "|account-change"
)

type accountAPIResponse struct {
	Username string `json:"username"`
	// CanChange is false when the credential came from flags only, because
	// then there is no file to write and a change would be lost on restart.
	CanChange  bool   `json:"can_change"`
	File       string `json:"file,omitempty"`
	Source     string `json:"source"`
	PasswordOK bool   `json:"password_set"`
	UpdatedAt  string `json:"updated_at,omitempty"`
	// Scheme tells the UI whether the stored secret is a hash. Anything else
	// means the file has not been upgraded yet.
	Scheme string `json:"scheme"`
}

type accountChangeRequest struct {
	CurrentPassword string `json:"current_password"`
	NewUsername     string `json:"new_username"`
	NewPassword     string `json:"new_password"`
	ConfirmPassword string `json:"confirm_password"`
}

// accountAPI serves GET and POST /api/auth/account.
func (w *WebServer) accountAPI(rw http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		w.accountGet(rw)
	case http.MethodPost:
		w.accountChange(rw, r)
	default:
		rw.Header().Set("Allow", "GET, HEAD, POST")
		writeJSONErr(rw, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (w *WebServer) accountGet(rw http.ResponseWriter) {
	lc := w.cred()
	resp := accountAPIResponse{
		Username:   lc.user,
		CanChange:  lc.file != "",
		File:       lc.file,
		PasswordOK: lc.cred.HasPassword(),
		Scheme:     "flag",
	}
	switch {
	case lc.cred.Hash != "":
		resp.Scheme = "pbkdf2"
		resp.UpdatedAt = lc.cred.UpdatedAt
	case lc.cred.plaintext != "":
		resp.Scheme = "plaintext"
	}
	// Saying "flag" when a file exists would be misleading: the file wins at
	// startup, so that is where the operator has to look after a restart.
	if credentialExists(lc.file) {
		if c, err := LoadCredential(lc.file); err == nil {
			resp.File = lc.file
			if c.UpdatedAt != "" {
				resp.UpdatedAt = c.UpdatedAt
			}
		}
	}
	if lc.file != "" {
		if resp.Scheme == "flag" {
			resp.Scheme = "file"
		}
		resp.Source = "file"
	} else {
		resp.Source = "flag"
	}
	writeJSON(rw, resp)
}

func (w *WebServer) accountChange(rw http.ResponseWriter, r *http.Request) {
	lc := w.cred()
	if lc.file == "" {
		writeJSONErr(rw, http.StatusConflict,
			"当前登录密码来自启动参数，没有配置文件可写入。请用 --auth-file 指定凭据文件后重启。")
		return
	}

	r.Body = http.MaxBytesReader(rw, r.Body, maxAccountBody)
	var req accountChangeRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeJSONErr(rw, http.StatusBadRequest, "请求格式不正确")
		return
	}

	now := time.Now()
	key := clientKey(r) + accountChangeKeySuffix
	if wait, blocked := w.loginLimiter.blocked(key, now); blocked {
		writeJSONErr(rw, http.StatusTooManyRequests,
			"尝试次数过多，请等待 "+humanTTL(wait.Round(time.Second))+" 后再试。")
		return
	}

	// A wrong current password is a credential guess and gets the same
	// backoff as a failed login.
	if lc.cred.Verify(req.CurrentPassword) != nil {
		w.loginLimiter.fail(key, now)
		writeJSONErr(rw, http.StatusUnauthorized, "当前密码不正确")
		return
	}
	w.loginLimiter.reset(key)

	newUser := strings.TrimSpace(req.NewUsername)
	newPass := req.NewPassword
	changeUser := newUser != "" && newUser != lc.user
	changePass := newPass != ""

	if !changeUser && !changePass && req.NewUsername == "" && req.NewPassword == "" {
		writeJSONErr(rw, http.StatusBadRequest, "没有需要修改的内容")
		return
	}
	if changePass {
		if req.ConfirmPassword != "" && req.ConfirmPassword != newPass {
			writeJSONErr(rw, http.StatusBadRequest, "两次输入的新密码不一致")
			return
		}
		if err := ValidateNewPassword(newPass); err != nil {
			writeJSONErr(rw, http.StatusBadRequest, err.Error())
			return
		}
		if lc.cred.Verify(newPass) == nil {
			writeJSONErr(rw, http.StatusBadRequest, "新密码不能与当前密码相同")
			return
		}
	}

	username := lc.user
	if changeUser {
		if err := ValidateNewUsername(newUser); err != nil {
			writeJSONErr(rw, http.StatusBadRequest, err.Error())
			return
		}
		username = newUser
	}

	// Rehashing always regenerates the salt, so even a username-only change
	// rotates the verifier and therefore the cookie signing key. That is
	// intentional: it is the only way a rename can also invalidate sessions
	// that were issued under the old name.
	password := ""
	switch {
	case changePass:
		password = newPass
	case lc.cred.plaintext != "":
		password = lc.cred.plaintext
	default:
		// No plaintext available: re-derive the same verifier without
		// touching the hash. The session key only depends on Verifier(), so
		// this keeps existing cookies valid across a pure rename.
		next := Credential{
			Version:    credentialVersion,
			Username:   username,
			Iterations: lc.cred.Iterations,
			Salt:       lc.cred.Salt,
			Hash:       lc.cred.Hash,
			UpdatedAt:  now.UTC().Format(time.RFC3339),
		}
		if err := SaveCredential(lc.file, next); err != nil {
			slog.Error("archive web: save credential failed", slog.Any("err", err))
			writeJSONErr(rw, http.StatusInternalServerError, "保存失败："+err.Error())
			return
		}
		w.login.Store(&loginCredential{user: username, cred: next, file: lc.file})
		w.reissueSession(rw, r, now)
		writeJSON(rw, map[string]any{"ok": true, "username": username,
			"message": "账号已更新"})
		return
	}

	next, err := NewCredential(username, password, credentialIterations)
	if err != nil {
		slog.Error("archive web: hash credential failed", slog.Any("err", err))
		writeJSONErr(rw, http.StatusInternalServerError, "生成密码失败")
		return
	}
	if err := SaveCredential(lc.file, next); err != nil {
		slog.Error("archive web: save credential failed", slog.Any("err", err))
		writeJSONErr(rw, http.StatusInternalServerError, "保存失败："+err.Error())
		return
	}
	// Publish after the file is durable: a restart must never find the old
	// verifier on disk while the running process already accepted the new one.
	w.login.Store(&loginCredential{user: username, cred: next, file: lc.file})
	w.reissueSession(rw, r, now)

	slog.Info("archive web credentials updated",
		slog.String("username", username),
		slog.Bool("password_changed", changePass),
		slog.String("file", lc.file))

	msg := "密码已更新，其它设备上的登录已失效"
	if !changePass {
		msg = "账号已更新，其它设备上的登录已失效"
	}
	writeJSON(rw, map[string]any{"ok": true, "username": username, "message": msg})
}

// reissueSession hands the caller a cookie signed with the *new* secret.
// Without it, changing your own password would immediately log you out of the
// tab you changed it in — the very session that proved you were allowed to.
func (w *WebServer) reissueSession(rw http.ResponseWriter, r *http.Request, now time.Time) {
	value, exp := w.issueSession(now)
	http.SetCookie(rw, &http.Cookie{
		Name:     sessionCookie,
		Value:    value,
		Path:     "/",
		Expires:  exp,
		MaxAge:   int(w.sessionTTL().Seconds()),
		HttpOnly: true,
		Secure:   r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https"),
		SameSite: http.SameSiteLaxMode,
	})
}

// BootstrapCredentialFile loads the credential file, creating it from the
// startup flags on first run and upgrading a legacy plaintext file in place.
//
// It returns the config the server must actually use, so the caller never has
// to know whether the password came from a flag or a file.
func BootstrapCredentialFile(path string, cfg WebAuthConfig) (WebAuthConfig, error) {
	if strings.TrimSpace(path) == "" {
		return cfg, nil
	}
	cfg.CredentialFile = path

	cred, err := LoadCredential(path)
	switch {
	case err == nil:
		// The file is authoritative. A stale --auth-login must not override it,
		// otherwise `install.sh` re-runs would silently revert a password the
		// operator changed in the panel.
		upgraded, uerr := cred.Upgrade(path)
		if uerr != nil {
			return cfg, uerr
		}
		if upgraded.legacy || cred.plaintext != "" {
			slog.Info("archive web credential upgraded to a hash",
				slog.String("file", path))
		}
		cfg.LoginUser = upgraded.Username
		cfg.PasswordHash = upgraded.Verifier()
		cfg.LoginPassword = ""
		return cfg, nil

	case isNotExist(err) || errors.Is(err, os.ErrNotExist):
		// First run: seed from the flags, then persist as a hash.
		if cfg.LoginUser == "" || cfg.LoginPassword == "" {
			return cfg, nil
		}
		cred, cerr := NewCredential(cfg.LoginUser, cfg.LoginPassword, credentialIterations)
		if cerr != nil {
			return cfg, cerr
		}
		if serr := SaveCredential(path, cred); serr != nil {
			return cfg, serr
		}
		slog.Info("archive web credential file created",
			slog.String("file", path), slog.String("username", cfg.LoginUser))
		cfg.PasswordHash = cred.Verifier()
		cfg.LoginPassword = ""
		return cfg, nil

	default:
		// A corrupt file is fatal on purpose: silently falling back to the flag
		// would re-open a password the operator believes they replaced.
		return cfg, err
	}
}
