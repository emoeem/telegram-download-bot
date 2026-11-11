// Package mobile is the gomobile bridge that embeds the archive core in an
// Android app. It deliberately exposes only flat types (string/int/error) so
// `gomobile bind` can generate the JNI shims; all business logic stays in
// archive/ and pkg/ and is unchanged.
//
// The app runs the Go core inside a ForegroundService and points a WebView at
// the loopback HTTP server this package starts. Nothing is exposed off-device:
// the listener is pinned to 127.0.0.1 and the port is chosen by the OS.
package mobile

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/iyear/tdl/archive"
	"github.com/iyear/tdl/core/storage"
	"github.com/iyear/tdl/license"
	"github.com/iyear/tdl/pkg/consts"
	"github.com/iyear/tdl/pkg/kv"
)

// Version is stamped at build time with -ldflags "-X .../mobile.Version=...".
var Version = "dev"

// InstallCrashLog points the Go runtime's crash output at path and returns the
// resolved location.
//
// On Android an app process has fd 2 wired to /dev/null, and many release ROMs
// make logcat unreadable even from a root shell. An unrecovered panic therefore
// aborts the process leaving no trace at all. debug.SetCrashOutput is the
// supported hook for this exact case: when the runtime dies it writes the full
// goroutine dump into a file the app owns, which adb can read afterwards.
//
// The app calls this before touching any other bridge entry point.
func InstallCrashLog(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("crash log path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("create crash log dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return "", fmt.Errorf("open crash log: %w", err)
	}
	if err := debug.SetCrashOutput(f, debug.CrashOptions{}); err != nil {
		_ = f.Close()
		return "", fmt.Errorf("set crash output: %w", err)
	}
	// Best effort: anything the Go side or a cgo library prints to stderr ends
	// up in the same file instead of the void.
	//
	// 只赋值 os.Stderr 是不够的：那只是换掉 Go 包变量，fd 2 仍指向 /dev/null，
	// cgo 输出和 Go runtime 自己的 panic dump 照样消失。dup2 把 fd 2 本身
	// 指向日志文件，上面的承诺才成立。
	//
	// flags 必须是 0：dup3 只接受 O_CLOEXEC，传 O_APPEND 会被内核判为无效参数
	// （实测 EINVAL），于是这一调用每次启动都失败，下面两行永远执行不到，
	// 而 Java 侧只能看到一句 "installCrashLog threw"。
	if dupErr := syscall.Dup3(int(f.Fd()), 2, 0); dupErr != nil {
		_ = f.Close()
		return "", fmt.Errorf("dup stderr: %w", dupErr)
	}
	os.Stderr = f
	return path, nil
}

// AppendLog appends one line to path, creating it if needed. The Kotlin side
// uses it to record boot failures that never reach the Go runtime's crash
// handler (for example a returned error rather than a panic).
func AppendLog(path string, line string) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("log path is empty")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(line + "\n")
	return err
}

// recoverTo turns a panic into an ordinary error so it crosses the gomobile
// boundary as a Java exception with a readable message. Without this an
// unrecovered panic aborts the whole app process from native code, which the
// Kotlin side cannot intercept or report.
func recoverTo(errp *error, where string) {
	if r := recover(); r != nil {
		*errp = fmt.Errorf("%s: panic: %v\n%s", where, r, debug.Stack())
	}
}

// Bridge owns one running archive instance: the KV store, the account manager
// and the loopback web server.
//
// Fields are unexported on purpose: gomobile exposes methods only, and keeping
// the state private stops the generated Java layer from poking at Go structs.
type Bridge struct {
	mu       sync.Mutex
	stg      kv.Storage
	accounts *archive.AccountManager
	srv      *http.Server
	ln       net.Listener
	cancel   context.CancelFunc
	addr     string
	started  bool
	lastErr  string
	// authToken 是本次启动的回环访问凭据：Android 不隔离 127.0.0.1 端口，
	// 同机任意 App 都能连上，服务端必须带 Token 鉴权（B1）。
	authToken string
	// deviceMaterial is the platform identification the app supplies before
	// Start. It is hashed into the licence device code; the raw value never
	// leaves the process.
	deviceMaterial string
}

// NewBridge prepares the data directory and opens the KV store. It must run
// before Start and after the app has created its private files directory.
//
// dataDir is where everything lives (SQLite archive, session KV, downloaded
// media). On Android that is filesDir, never external storage: the archive
// contains the Telegram session key.
func NewBridge(dataDir string) (bridge *Bridge, err error) {
	defer recoverTo(&err, "new bridge")

	if strings.TrimSpace(dataDir) == "" {
		return nil, fmt.Errorf("data dir is empty")
	}

	// The package-level init left the paths empty because an app process has no
	// $HOME. Point them somewhere writable before anything reads them.
	if err := consts.Rebase(dataDir); err != nil {
		return nil, fmt.Errorf("rebase data dir: %w", err)
	}

	stg, err := kv.NewWithMap(map[string]string{
		"type": "bolt",
		"path": filepath.Join(consts.DataDir, "data"),
	})
	if err != nil {
		return nil, fmt.Errorf("open kv storage: %w", err)
	}

	return &Bridge{stg: stg}, nil
}

// SetDeviceMaterial supplies the platform identification used to derive the
// licence device code.
//
// The app composes ANDROID_ID with the build fingerprint and passes it here
// before Start. It is a separate call rather than a Start parameter so the
// gomobile-generated Java signature of Start stays stable for existing callers.
// An empty value makes the Go side fall back to its own host-based identity,
// which is what a non-Android embedder gets.
func (b *Bridge) SetDeviceMaterial(material string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.deviceMaterial = strings.TrimSpace(material)
}

// licenceGate builds the activation manager for the embedded server.
//
// A build without verifier material returns nil, which leaves the embedded UI
// ungated — the same behaviour the app had before licensing existed. That is
// what makes a developer build usable without the vendor's master key.
func (b *Bridge) licenceGate() (*license.Manager, error) {
	if !license.Injected() {
		return nil, nil
	}
	ver, err := license.BuildVerifier()
	if err != nil {
		return nil, fmt.Errorf("licence verifier: %w", err)
	}
	var device uint64
	if b.deviceMaterial != "" {
		device = license.DeviceCode(b.deviceMaterial)
	} else {
		code, _, derr := license.LocalDeviceCode()
		if derr != nil {
			return nil, fmt.Errorf("licence device: %w", derr)
		}
		device = code
	}
	return license.NewManager(filepath.Join(archiveDataDir(), "license"), ver, device), nil
}

// Start brings up the account manager and the loopback web server.
//
// appID/appHash come from the bundled Telegram application credentials. port 0
// asks the OS for a free port; the chosen address is returned so the WebView
// can navigate to it. Calling Start twice returns the existing address.
func (b *Bridge) Start(appID int, appHash string, port int) (addr string, err error) {
	defer recoverTo(&err, "start bridge")

	b.mu.Lock()
	defer b.mu.Unlock()

	if b.started {
		return b.addr, nil
	}
	// A failed Start must not leave half-initialised state behind, so every
	// error path tears down what was already created.
	defer func() {
		if err != nil {
			b.teardownLocked()
			b.lastErr = err.Error()
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	ctx = kv.With(ctx, b.stg)

	mgr, mgrErr := archive.NewAccountManager(ctx, b.stg, archiveDataDir(),
		appID, strings.TrimSpace(appHash), b.sealer(ctx))
	if mgrErr != nil {
		cancel()
		return "", fmt.Errorf("account manager: %w", mgrErr)
	}
	if mgrErr = mgr.Load(ctx); mgrErr != nil {
		mgr.Close()
		cancel()
		return "", fmt.Errorf("load accounts: %w", mgrErr)
	}
	// A first launch has no account. Create the shell so the UI has somewhere
	// to show the QR login instead of an empty screen with a dead button.
	if mgr.ActiveID() == "" {
		if _, mgrErr = mgr.Add(ctx, "默认账号"); mgrErr != nil {
			mgr.Close()
			cancel()
			return "", fmt.Errorf("create default account: %w", mgrErr)
		}
	}

	var ctl *archive.Controller
	if sess := mgr.Current(); sess != nil {
		ctl = sess.Ctl
	}

	// 每次启动生成一次性 128-bit token：WebView 用它作为 Bearer 凭据访问
	// 回环服务（Java 侧经 AuthToken() 取走，主文档与子资源请求都要带）。
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		mgr.Close()
		cancel()
		return "", fmt.Errorf("generate auth token: %w", err)
	}
	b.authToken = hex.EncodeToString(raw)

	viewer := archive.NewWebServer(nil, archive.WebAuthConfig{Token: b.authToken})
	viewer.Accounts = mgr
	viewer.Ctl = ctl
	gate, gateErr := b.licenceGate()
	if gateErr != nil {
		mgr.Close()
		cancel()
		return "", gateErr
	}
	viewer.Licence = gate

	ln, lnErr := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if lnErr != nil {
		mgr.Close()
		cancel()
		return "", fmt.Errorf("listen loopback: %w", lnErr)
	}

	srv := &http.Server{
		Handler:           viewer.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// Same split as cmd/archive.go: bounded request reads, but a long
		// write budget so a multi-GB export is not cut off mid-stream.
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Minute,
		IdleTimeout:  120 * time.Second,
	}

	b.accounts, b.srv, b.ln, b.cancel = mgr, srv, ln, cancel
	b.addr = ln.Addr().String()
	b.started = true

	go func() {
		if serveErr := srv.Serve(ln); serveErr != nil && serveErr != http.ErrServerClosed {
			b.mu.Lock()
			// 必须完整拆除，不能只把 started 复位：srv/ln/accounts/cancel 不
			// 清的话 Addr() 会继续返回这个已经死掉的地址，而下次 Start() 会
			// 新建 AccountManager 直接覆盖 b.accounts（旧的从没 Close，bolt
			// namespace 与 SQLite 连接都留在那儿），旧 cancel 也再没人调用，
			// supervisor / click queue 的 goroutine 就永久泄漏了。
			b.teardownLocked()
			b.lastErr = serveErr.Error()
			b.mu.Unlock()
		}
	}()

	return b.addr, nil
}

// AuthToken returns the per-boot bearer token the WebView must send on every
// request to the loopback server. Empty until Start succeeds.
func (b *Bridge) AuthToken() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.authToken
}

// Stop shuts the server and the account manager down. It is safe to call when
// nothing is running, and safe to call twice.
func (b *Bridge) Stop() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.teardownLocked()
}

// Close releases the KV store as well. Call it when the service is destroyed;
// after Close the Bridge cannot be started again.
func (b *Bridge) Close() {
	b.Stop()

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.stg != nil {
		_ = b.stg.Close()
		b.stg = nil
	}
}

// Addr returns the loopback base URL (for example "127.0.0.1:41234"), or an
// empty string when the server is not running.
func (b *Bridge) Addr() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.addr
}

// Status reports the runtime state as JSON so the Kotlin side can render it
// without needing a typed struct across the bridge.
func (b *Bridge) Status() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	st := map[string]any{
		"started":  b.started,
		"addr":     b.addr,
		"data_dir": consts.DataDir,
		"version":  Version,
	}
	if b.lastErr != "" {
		st["last_error"] = b.lastErr
	}

	if b.accounts != nil {
		st["account_count"] = len(b.accounts.List())
		st["active_id"] = b.accounts.ActiveID()
		// Only the active account has a live controller; an inactive one has no
		// state beyond whether a session file exists, which the accounts API
		// covers. Mirror what accountsView does so the bridge and the UI agree.
		if sess := b.accounts.Current(); sess != nil && sess.Ctl != nil {
			cs := sess.Ctl.State()
			st["state"] = cs.State
			st["authorized"] = cs.Authorized
			st["has_session"] = cs.Authorized
			if cs.User != nil {
				st["user"] = cs.User.Username
			}
		}
	}

	out, err := json.Marshal(st)
	if err != nil {
		return `{"error":"status marshal failed"}`
	}
	return string(out)
}

// DataDir reports where the archive lives, so the UI can show storage usage.
func (b *Bridge) DataDir() string { return consts.DataDir }

func (b *Bridge) teardownLocked() {
	if b.srv != nil {
		_ = b.srv.Close()
		b.srv = nil
	}
	if b.ln != nil {
		_ = b.ln.Close()
		b.ln = nil
	}
	if b.accounts != nil {
		b.accounts.Close()
		b.accounts = nil
	}
	if b.cancel != nil {
		b.cancel()
		b.cancel = nil
	}
	b.started = false
	b.addr = ""
}

// sealer mirrors the desktop policy: encrypt each account namespace when a
// passphrase is configured, and refuse to open an encrypted namespace without
// one. On Android TGA_SESSION_PASSPHRASE is normally unset, so sessions stay in
// plaintext inside the app's private directory.
func (b *Bridge) sealer(ctx context.Context) archive.KVWrapper {
	return func(s storage.Storage) (storage.Storage, error) {
		pass := strings.TrimSpace(os.Getenv("TGA_SESSION_PASSPHRASE"))
		if pass == "" {
			enc, err := archive.IsEncrypted(ctx, s)
			if err != nil {
				return nil, err
			}
			if enc {
				return nil, archive.ErrPassphraseRequired
			}
			return s, nil
		}
		return archive.SecureNamespace(ctx, s, pass)
	}
}

func archiveDataDir() string { return filepath.Join(consts.DataDir, "archive") }
