package archive

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-faster/errors"
	"github.com/gotd/td/telegram/auth/qrlogin"
	"github.com/gotd/td/telegram/updates"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	cstorage "github.com/iyear/tdl/core/storage"
	tclientcore "github.com/iyear/tdl/core/tclient"
	"github.com/iyear/tdl/pkg/consts"
	"github.com/spf13/viper"

	"github.com/iyear/tdl/archive/store"
)

// Controller gives the web UI control-plane capabilities: login (QR + 2FA),
// monitor management (add/remove/sync with background backfill jobs) and a
// supervised connection that also acts as a realtime archiver while serve runs.
// A WebServer without a Controller stays a read-only viewer.
type Controller struct {
	Store   *store.Store
	KV      cstorage.Storage
	AppID   int
	AppHash string

	ctx    context.Context
	cancel context.CancelFunc

	mu        sync.Mutex
	rt        *Runtime
	collector *Collector
	supCancel context.CancelFunc

	// One clicker and one queue worker live for the whole controller: the
	// supervisor reconnects repeatedly, and a per-connection clicker would
	// spawn a second worker every time it did.
	clicker   *AutoClicker
	queueOnce sync.Once

	stateMu sync.RWMutex
	state   CtrlState

	loginMu      sync.Mutex
	qrToken      atomic.Value // string
	qrCancel     context.CancelFunc
	qrPwd        chan string
	qrDone       chan struct{}
	passwordMode bool
	qrErr        string

	jobsMu sync.Mutex
	jobs   map[int64]*Job
	jobSeq int64

	// All media downloads share one process-wide gate. A MediaWorker's Threads
	// value only limits concurrency inside that worker; without this gate, two
	// simultaneous web backfill/sync jobs could each start their own downloader
	// and still saturate disk/network while the Web UI is being used.
	mediaGate    chan struct{}
	mediaPaused  atomic.Bool
	mediaRunning atomic.Bool
}

type CtrlState struct {
	State      string   `json:"state"` // connecting|logged_out|authorized|error
	Authorized bool     `json:"authorized"`
	User       *WebUser `json:"user,omitempty"`
	Error      string   `json:"error,omitempty"`
}

type WebUser struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
	Name     string `json:"name"`
}

const (
	StateConnecting = "connecting"
	StateLoggedOut  = "logged_out"
	StateAuthorized = "authorized"
	StateError      = "error"

	QRIdle           = "idle"
	QRPending        = "pending"
	QRPasswordNeeded = "password_needed"
	QRAuthorized     = "authorized"
	QRError          = "error"
)

type Job struct {
	ID            int64      `json:"id"`
	ChatID        int64      `json:"chat_id"`
	Target        string     `json:"target"`
	Kind          string     `json:"kind"`
	State         string     `json:"state"`
	Archived      int        `json:"archived"`
	ProgressDone  int        `json:"progress_done"`
	ProgressTotal *int       `json:"progress_total,omitempty"`
	Error         string     `json:"error,omitempty"`
	StartedAt     time.Time  `json:"started_at"`
	FinishedAt    *time.Time `json:"finished_at,omitempty"`
	cancel        context.CancelFunc
}

func NewController(ctx context.Context, kvd cstorage.Storage, s *store.Store, appID int, appHash string) *Controller {
	c := &Controller{
		Store: s, KV: kvd, AppID: appID, AppHash: appHash,
		jobs:      map[int64]*Job{},
		mediaGate: make(chan struct{}, 1),
	}
	c.ctx, c.cancel = context.WithCancel(ctx)
	if _, err := c.Store.MarkInterruptedJobs(c.ctx); err != nil {
		fmt.Fprintf(os.Stderr, "archive jobs: mark interrupted: %v\n", err)
	}
	if appID <= 0 || strings.TrimSpace(appHash) == "" {
		c.setState(CtrlState{State: StateError, Error: "telegram.app-id and telegram.app-hash are required"})
		return c
	}
	c.setState(CtrlState{State: StateConnecting})
	c.ensureSupervisor()
	go c.taskLoop()
	go c.superviseLoop()
	return c
}

// superviseLoop keeps a managed session connected. The Telegram read loop can
// die on its own (network blip, server-side restart, "read: EOF") and nothing
// else brings it back: the supervisor goroutine clears the runtime on error and
// ensureSupervisor was only ever called from a control-plane request or the QR
// login path, so a browser-managed serve silently stopped archiving until
// somebody clicked something in the UI.
func (c *Controller) superviseLoop() {
	t := time.NewTicker(superviseInterval)
	defer t.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-t.C:
			st := c.State()
			if st.State != StateError {
				continue
			}
			fmt.Fprintf(os.Stderr, "archive: connection lost (%s), reconnecting\n", st.Error)
			// Move out of the error state first so the next tick does not fire
			// again while the client is still coming up.
			c.setState(CtrlState{State: StateConnecting})
			c.ensureSupervisor()
		}
	}
}

// superviseInterval is how often a broken managed connection is retried.
const superviseInterval = 30 * time.Second

const (
	// MediaTick is how often the media queue is drained: a managed serve
	// session and `archive run` share this cadence (see DrainMediaQueue).
	MediaTick = 2 * time.Minute
	// sweepTick is how often a managed session re-scans targets to close gaps
	// and mirrors pending exports, matching `archive run`.
	sweepTick = 10 * time.Minute
	// passTimeout bounds a single maintenance pass so one hung download cannot
	// stall every later pass.
	passTimeout = 15 * time.Minute
	// mediaBatches is how many 100-job media batches one pass may drain before
	// yielding, so a large backlog does not monopolise the connection.
	mediaBatches = 5
)

// taskLoop gives a browser-managed serve session the same maintenance the
// `archive run` command performs: the media queue is drained every mediaTick
// and a full sweep (backfill + media + exports + avatars + daily backup) runs
// every sweepTick. Without it, serve archives message rows but never fetches
// their media.
func (c *Controller) taskLoop() {
	media := time.NewTimer(30 * time.Second)
	sweep := time.NewTimer(3 * time.Minute)
	defer media.Stop()
	defer sweep.Stop()
	// The media queue and the gap sweep get independent slots: a slow download
	// pass must never starve the gap check (and vice versa).
	mediaBusy := make(chan struct{}, 1)
	sweepBusy := make(chan struct{}, 1)
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-media.C:
			c.mediaPass(mediaBusy)
			media.Reset(MediaTick)
		case <-sweep.C:
			c.sweepPass(sweepBusy)
			sweep.Reset(sweepTick)
		}
	}
}

// runtimeForTask returns the live runtime, or nil when the session is not
// authorized yet / the supervisor has not connected.
func (c *Controller) runtimeForTask() (*Runtime, *Collector) {
	if st := c.State(); !st.Authorized {
		return nil, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.rt, c.collector
}

// mediaPass drains the media queue via the shared DrainMediaQueue helper.
func (c *Controller) mediaPass(busy chan struct{}) {
	select {
	case busy <- struct{}{}:
	default:
		return // a media pass is already running
	}
	defer func() { <-busy }()

	rt, _ := c.runtimeForTask()
	if rt == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.ctx), passTimeout)
	defer cancel()

	if n, err := DrainMediaQueue(ctx, c.Store, mediaBatches, func(ctx context.Context) error {
		return c.runMediaWorker(ctx, rt)
	}); err != nil {
		fmt.Fprintf(os.Stderr, "archive media pass: %v\n", err)
	} else if n > 0 {
		fmt.Fprintf(os.Stderr, "archive media pass: ran %d batch(es)\n", n)
	}
}

// sweepPass closes tail gaps, refreshes view counters, mirrors pending exports,
// refreshes avatars and takes the daily backup.
func (c *Controller) sweepPass(busy chan struct{}) {
	select {
	case busy <- struct{}{}:
	default:
		return // a sweep is already running
	}
	defer func() { <-busy }()

	rt, col := c.runtimeForTask()
	if rt == nil || col == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.ctx), passTimeout)
	defer cancel()

	started := time.Now()
	c.backfillTargets(ctx, rt, col)
	RefreshStatsTargets(ctx, c.Store, rt)
	if col.Exporter != nil {
		if ts, err := c.Store.Targets(ctx); err == nil {
			for _, t := range ts {
				if t.Enabled {
					_ = col.Exporter.ExportPending(ctx, t.ChatID, 200)
				}
			}
		}
	}
	_ = NewAvatarWorker(c.Store, rt, "").Run(ctx)
	if _, err := MaybeBackup(ctx, c.Store); err != nil {
		fmt.Fprintf(os.Stderr, "archive backup: %v\n", err)
	}
	// Stale staging partials are pure waste; reclaiming them is always safe.
	if n, err := CleanStaging(filepath.Join(consts.DataDir, "archive"), DefaultStagingMaxAge); err == nil && n > 0 {
		fmt.Fprintf(os.Stderr, "archive gc: removed %d stale staging file(s)\n", n)
	}
	fmt.Fprintf(os.Stderr, "archive maintenance: sweep done in %s\n", time.Since(started).Round(time.Millisecond))
}

func (c *Controller) runMediaWorker(ctx context.Context, rt *Runtime) error {
	for c.mediaPaused.Load() {
		t := time.NewTicker(500 * time.Millisecond)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
			t.Stop()
		}
	}
	select {
	case c.mediaGate <- struct{}{}:
		defer func() { <-c.mediaGate }()
	case <-ctx.Done():
		return ctx.Err()
	}
	c.mediaRunning.Store(true)
	defer c.mediaRunning.Store(false)
	return NewMediaWorker(c.Store, rt, "").Run(ctx)
}

func (c *Controller) SetMediaPaused(paused bool) { c.mediaPaused.Store(paused) }
func (c *Controller) MediaPaused() bool          { return c.mediaPaused.Load() }

// backfillTargets closes tail gaps and re-scans every enabled target.
func (c *Controller) backfillTargets(ctx context.Context, rt *Runtime, col *Collector) {
	RescanTargets(ctx, c.Store, rt, col)
	// The material layer feeds export and publish, so it is refreshed on the
	// same cadence as the rescan that produced new raw messages. A failure here
	// must not abort the backfill: the archive itself is already consistent.
	if _, err := RefreshMaterialLayer(ctx, c.Store); err != nil {
		fmt.Fprintf(os.Stderr, "archive material: %v\n", err)
	}
}

func (c *Controller) Close() {
	c.cancel()
	c.stopQR()
}

func (c *Controller) setState(s CtrlState) {
	c.stateMu.Lock()
	c.state = s
	c.stateMu.Unlock()
}

func (c *Controller) State() CtrlState {
	c.stateMu.RLock()
	defer c.stateMu.RUnlock()
	return c.state
}

// WaitState blocks until the controller state leaves "connecting" or the
// timeout expires, so API callers get a meaningful answer on first poll.
func (c *Controller) WaitState(timeout time.Duration) CtrlState {
	deadline := time.Now().Add(timeout)
	for {
		st := c.State()
		if st.State != StateConnecting || time.Now().After(deadline) {
			return st
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func (c *Controller) QRState() string {
	c.loginMu.Lock()
	defer c.loginMu.Unlock()
	if c.qrCancel == nil {
		return QRIdle
	}
	if c.passwordMode {
		return QRPasswordNeeded
	}
	return QRPending
}

func (c *Controller) QRError() string {
	c.loginMu.Lock()
	defer c.loginMu.Unlock()
	return c.qrErr
}

func (c *Controller) QRToken() string {
	if v, ok := c.qrToken.Load().(string); ok {
		return v
	}
	return ""
}

// ensureSupervisor starts the shared runtime connection if it is not running.
func (c *Controller) ensureSupervisor() {
	c.mu.Lock()
	if c.rt != nil {
		c.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(c.ctx)
	col := NewCollector(c.Store)
	rt, err := NewRuntime(ctx, c.KV, c.AppID, c.AppHash, col.SaveUpdate, col.SaveUpdateGroup, col.SaveService, col.SaveEdit, col.SaveDelete, col.SaveReactions)
	if err != nil {
		c.mu.Unlock()
		cancel()
		c.setState(CtrlState{State: StateError, Error: err.Error()})
		return
	}
	col.Exporter = NewExporter(rt, c.Store)
	col.Notifier = NewNotifier(c.Store)
	if c.clicker == nil {
		c.clicker = NewAutoClicker(c.Store, col.Notifier, func() *Runtime {
			rt, _ := c.runtimeForTask()
			return rt
		})
	}
	col.AutoClick = c.clicker
	c.queueOnce.Do(func() { c.clicker.StartQueue(c.ctx) })
	c.rt, c.collector, c.supCancel = rt, col, cancel
	c.mu.Unlock()

	go func() {
		runErr := rt.Client.Run(ctx, func(ctx context.Context) error {
			st, err := rt.Client.Auth().Status(ctx)
			if err != nil {
				return err
			}
			if !st.Authorized {
				c.setState(CtrlState{State: StateLoggedOut})
				// Stay connected: the shared session in KV becomes authorized
				// once the QR login flow (a separate client) completes and the
				// supervisor is kicked, so block here until then.
				<-ctx.Done()
				return ctx.Err()
			}
			if err := rt.Peers.Init(ctx); err != nil {
				return err
			}
			self, err := rt.Client.Self(ctx)
			if err != nil {
				return err
			}
			if self.Bot {
				err := errors.New("archive requires a Telegram user account, not a bot")
				c.setState(CtrlState{State: StateError, Error: err.Error()})
				return err
			}
			c.setState(CtrlState{State: StateAuthorized, Authorized: true, User: userFromTG(self)})
			close(rt.Ready)
			return rt.Gaps.Run(ctx, rt.API, self.ID, updates.AuthOptions{IsBot: false})
		})
		if ctx.Err() != nil {
			return // kicked or controller closed; cleanup already done
		}
		c.mu.Lock()
		if c.rt == rt {
			c.rt = nil
			c.collector = nil
			c.supCancel = nil
		}
		c.mu.Unlock()
		if runErr != nil {
			c.setState(CtrlState{State: StateError, Error: runErr.Error()})
		}
	}()
}

// kick drops the supervised connection so the next ensureSupervisor reconnects
// and picks up the freshly authorized session.
func (c *Controller) kick() {
	c.mu.Lock()
	cancel := c.supCancel
	c.rt, c.collector, c.supCancel = nil, nil, nil
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (c *Controller) runtimeForOps() (*Runtime, *Collector, error) {
	c.ensureSupervisor()
	st := c.WaitState(15 * time.Second)
	if !st.Authorized {
		return nil, nil, fmt.Errorf("archive account is not logged in (state: %s)", st.State)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.rt == nil {
		return nil, nil, errors.New("archive connection unavailable")
	}
	return c.rt, c.collector, nil
}

func (c *Controller) checkCreds() error {
	if c.AppID <= 0 || strings.TrimSpace(c.AppHash) == "" {
		return errors.New("telegram.app-id and telegram.app-hash are required")
	}
	return nil
}

// StartQRLogin launches the QR login flow on a dedicated client.
func (c *Controller) StartQRLogin() error {
	if err := c.checkCreds(); err != nil {
		return err
	}
	if st := c.WaitState(15 * time.Second); st.Authorized {
		return errors.New("already logged in")
	}
	c.loginMu.Lock()
	if c.qrCancel != nil {
		c.loginMu.Unlock()
		return nil // already running
	}
	ctx, cancel := context.WithCancel(c.ctx)
	c.qrCancel = cancel
	c.qrPwd = make(chan string, 1)
	c.qrDone = make(chan struct{})
	c.passwordMode = false
	c.qrErr = ""
	c.loginMu.Unlock()
	c.qrToken.Store("")

	go c.runQRLogin(ctx)
	return nil
}

func (c *Controller) runQRLogin(ctx context.Context) {
	defer func() {
		c.loginMu.Lock()
		c.qrCancel = nil
		c.passwordMode = false
		c.loginMu.Unlock()
		close(c.qrDone)
	}()

	d := tg.NewUpdateDispatcher()
	lc, err := tclientcore.New(ctx, tclientcore.Options{
		AppID: c.AppID, AppHash: c.AppHash,
		Session:          cstorage.NewSession(c.KV, false),
		Proxy:            viper.GetString(consts.FlagProxy),
		ProxyInsecure:    viper.GetBool(consts.FlagProxyInsecure),
		ReconnectTimeout: viper.GetDuration(consts.FlagReconnectTimeout),
		UpdateHandler:    d,
	})
	if err != nil {
		c.setQRError(err.Error())
		return
	}

	err = lc.Run(ctx, func(ctx context.Context) error {
		if st, err := lc.Auth().Status(ctx); err == nil && st.Authorized {
			// Session in KV is already authorized (e.g. CLI login raced us).
			return nil
		}
		_, err := lc.QR().Auth(ctx, qrlogin.OnLoginToken(d), func(ctx context.Context, token qrlogin.Token) error {
			c.qrToken.Store(token.URL())
			return nil
		})
		if err != nil {
			if !tgerr.Is(err, "SESSION_PASSWORD_NEEDED") {
				return err
			}
			c.loginMu.Lock()
			c.passwordMode = true
			c.loginMu.Unlock()
			var pwd string
			select {
			case pwd = <-c.qrPwd:
			case <-ctx.Done():
				return ctx.Err()
			}
			c.loginMu.Lock()
			c.passwordMode = false
			c.loginMu.Unlock()
			if _, err = lc.Auth().Password(ctx, pwd); err != nil {
				return errors.Wrap(err, "2FA password")
			}
		}
		user, err := lc.Self(ctx)
		if err != nil {
			return err
		}
		c.setState(CtrlState{State: StateAuthorized, Authorized: true, User: userFromTG(user)})
		return nil
	})
	if err != nil && ctx.Err() == nil {
		if c.State().State != StateAuthorized {
			c.setQRError(err.Error())
		}
	}

	if c.State().Authorized {
		c.kick()
		c.ensureSupervisor() // reconnect with the authorized session
	}
}

func (c *Controller) setQRError(msg string) {
	c.loginMu.Lock()
	c.qrErr = msg
	c.loginMu.Unlock()
}

// SubmitPassword completes 2FA during the QR login flow.
func (c *Controller) SubmitPassword(pwd string) error {
	c.loginMu.Lock()
	defer c.loginMu.Unlock()
	if c.qrCancel == nil || !c.passwordMode {
		return errors.New("no 2FA challenge pending")
	}
	select {
	case c.qrPwd <- pwd:
		return nil
	default:
		return errors.New("password already submitted")
	}
}

func (c *Controller) stopQR() {
	c.loginMu.Lock()
	cancel := c.qrCancel
	c.qrCancel = nil
	c.passwordMode = false
	c.loginMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// CancelQR aborts a running QR login.
func (c *Controller) CancelQR() {
	c.stopQR()
	c.loginMu.Lock()
	c.qrErr = ""
	c.loginMu.Unlock()
}

// AddMonitorOptions carries everything the web add-target form collects.
type AddMonitorOptions struct {
	Target            string
	Since             string
	Limit             int
	Download          bool
	Keywords          []string
	ExcludeKeywords   []string
	MediaMaxSize      int64
	AutoClick         bool
	AutoClickKeywords []string
}

// AddMonitor registers a target and starts a background backfill job.
func (c *Controller) AddMonitor(opts AddMonitorOptions) (*Job, error) {
	target, since, limit, download := opts.Target, opts.Since, opts.Limit, opts.Download
	if err := c.checkCreds(); err != nil {
		return nil, err
	}
	rt, col, err := c.runtimeForOps()
	if err != nil {
		return nil, err
	}
	ctx := c.ctx
	if err := rt.Peers.Init(ctx); err != nil {
		return nil, err
	}
	p, err := Resolve(ctx, rt, target)
	if err != nil {
		return nil, err
	}
	sval, err := parseSinceValue(since)
	if err != nil {
		return nil, err
	}
	u, _ := p.Username()
	t := store.Target{
		ChatID: p.ID(), Username: u, Enabled: true,
		ArchiveText: true, ArchiveMedia: true, ArchiveFiles: true,
		DownloadMedia: download, ArchiveMode: "local", Since: sval,
		Keywords: opts.Keywords, ExcludeKeywords: opts.ExcludeKeywords, MediaMaxSize: opts.MediaMaxSize,
		AutoClick: opts.AutoClick, AutoClickKeywords: opts.AutoClickKeywords,
	}
	if err := c.Store.AddTarget(ctx, t); err != nil {
		return nil, err
	}
	return c.startJob(p.VisibleName(), t, "backfill", limit, true, rt, col)
}

// SyncMonitor re-scans an existing target to close gaps.
func (c *Controller) SyncMonitor(target string, limit int) (*Job, error) {
	if err := c.checkCreds(); err != nil {
		return nil, err
	}
	rt, col, err := c.runtimeForOps()
	if err != nil {
		return nil, err
	}
	ctx := c.ctx
	if err := rt.Peers.Init(ctx); err != nil {
		return nil, err
	}
	p, err := Resolve(ctx, rt, target)
	if err != nil {
		return nil, err
	}
	t, err := c.Store.Target(ctx, p.ID())
	if err != nil {
		return nil, fmt.Errorf("target %s is not monitored", target)
	}
	return c.startJob(p.VisibleName(), t, "sync", limit, false, rt, col)
}

func (c *Controller) RemoveMonitor(chatID int64) error {
	return c.Store.RemoveTarget(c.ctx, chatID)
}

func (c *Controller) startJob(name string, t store.Target, kind string, limit int, resume bool, rt *Runtime, col *Collector) (*Job, error) {
	now := time.Now()
	var total *int
	if limit > 0 {
		total = &limit
	}
	dbJob := store.Job{Kind: kind, Status: "running", ProgressDone: 0, ProgressTotal: total, StartedAt: &now}
	dbJob.ChatID = &t.ChatID
	dbJob.TargetID = &t.ChatID
	id, err := c.Store.CreateJob(c.ctx, dbJob)
	if err != nil {
		return nil, err
	}
	jobLogger := slog.Default().With(
		slog.Int64("job_id", id),
		slog.String("kind", kind),
		slog.Int64("target_id", t.ChatID),
		slog.Int64("chat_id", t.ChatID),
	)
	jobLogger.Info("archive job started")
	job := &Job{ID: id, ChatID: t.ChatID, Target: name, Kind: kind, State: "running", StartedAt: now, ProgressTotal: total}
	jobCtx, cancel := context.WithCancel(c.ctx)
	job.cancel = cancel
	c.jobsMu.Lock()
	c.jobs[id] = job
	c.jobsMu.Unlock()

	update := func(fn func(*Job)) {
		c.jobsMu.Lock()
		if j, ok := c.jobs[id]; ok {
			fn(j)
		}
		c.jobsMu.Unlock()
	}
	lastProgressWrite := time.Now().Add(-time.Second)
	lastPercent := -1
	progress := func(done int) {
		update(func(j *Job) { j.Archived = done; j.ProgressDone = done })
		percent := -1
		if total != nil && *total > 0 {
			percent = done * 100 / *total
		}
		if time.Since(lastProgressWrite) >= 500*time.Millisecond || (percent >= 0 && percent != lastPercent) {
			_ = c.Store.UpdateJobProgress(context.Background(), id, done, total)
			elapsed := time.Since(now)
			rate := 0.0
			if elapsed > 0 {
				rate = float64(done) / elapsed.Seconds()
			}
			jobLogger.Info("archive backfill progress",
				slog.Int("done", done),
				slog.Int("total", valueOrZero(total)),
				slog.Float64("rate_per_sec", rate),
			)
			lastProgressWrite = time.Now()
			lastPercent = percent
		}
	}
	go func() {
		defer cancel()
		n, runErr := col.Backfill(jobCtx, rt, t, BackfillOptions{Limit: limit, Resume: resume, Since: t.Since, Progress: progress})
		update(func(j *Job) { j.Archived = n; j.ProgressDone = n })
		_ = c.Store.UpdateJobProgress(context.Background(), id, n, total)
		if runErr != nil {
			status := "failed"
			if errors.Is(runErr, context.Canceled) {
				status = "canceled"
			}
			_ = c.Store.FinishJob(context.Background(), id, status, runErr.Error())
			finishedAt := time.Now()
			update(func(j *Job) { j.Error = runErr.Error(); j.State = status; j.FinishedAt = &finishedAt })
			logJobFinished(jobLogger, now, status, runErr)
			return
		}
		opCtx, opCancel := context.WithTimeout(jobCtx, 10*time.Minute)
		defer opCancel()
		if err := c.runMediaWorker(opCtx, rt); err != nil {
			update(func(j *Job) {
				if j.Error == "" {
					j.Error = err.Error()
				}
			})
			_ = c.Store.FinishJob(context.Background(), id, "failed", err.Error())
			finishedAt := time.Now()
			update(func(j *Job) { j.State = "failed"; j.FinishedAt = &finishedAt })
			logJobFinished(jobLogger, now, "failed", err)
			return
		}
		if col.Exporter != nil {
			_ = col.Exporter.ExportPending(opCtx, t.ChatID, limit)
		}
		_ = NewAvatarWorker(c.Store, rt, "").Run(opCtx)
		_ = c.Store.FinishJob(context.Background(), id, "done", "")
		finishedAt := time.Now()
		update(func(j *Job) { j.State = "done"; j.FinishedAt = &finishedAt })
		logJobFinished(jobLogger, now, "done", nil)
	}()
	return job, nil
}

func (c *Controller) Jobs() []Job {
	if c.Store == nil {
		return nil
	}
	dbJobs, err := c.Store.ListJobs(c.ctx, store.JobFilter{Limit: 100})
	if err != nil {
		return nil
	}
	out := make([]Job, 0, len(dbJobs))
	for _, dj := range dbJobs {
		j := Job{ID: dj.ID, Kind: dj.Kind, State: dj.Status, ProgressDone: dj.ProgressDone, ProgressTotal: dj.ProgressTotal, Error: dj.Error, StartedAt: time.Time{}, FinishedAt: dj.FinishedAt}
		if dj.StartedAt != nil {
			j.StartedAt = *dj.StartedAt
		} else {
			j.StartedAt = dj.CreatedAt
		}
		if dj.ChatID != nil {
			j.ChatID = *dj.ChatID
			j.Target = fmt.Sprintf("%d", *dj.ChatID)
		}
		c.jobsMu.Lock()
		if live, ok := c.jobs[dj.ID]; ok {
			j.Target = live.Target
			j.Archived = live.Archived
			j.cancel = live.cancel
		}
		c.jobsMu.Unlock()
		out = append(out, j)
	}
	return out
}

func (c *Controller) CancelJob(id int64) error {
	c.jobsMu.Lock()
	j, ok := c.jobs[id]
	c.jobsMu.Unlock()
	if !ok {
		return fmt.Errorf("job %d is not running", id)
	}
	if j.cancel == nil {
		return fmt.Errorf("job %d cannot be canceled", id)
	}
	j.cancel()
	return nil
}

func userFromTG(u *tg.User) *WebUser {
	if u == nil {
		return nil
	}
	name := strings.TrimSpace(u.FirstName + " " + u.LastName)
	return &WebUser{ID: u.ID, Username: u.Username, Name: name}
}

func parseSinceValue(v string) (*time.Time, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil, nil
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02"} {
		if t, err := time.Parse(layout, v); err == nil {
			return &t, nil
		}
	}
	return nil, fmt.Errorf("invalid since %q: use RFC3339 or YYYY-MM-DD", v)
}
