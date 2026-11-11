package archive

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/gotd/td/telegram/downloader"
	"github.com/gotd/td/telegram/peers"
	"github.com/gotd/td/telegram/query"
	"github.com/gotd/td/telegram/query/messages"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"github.com/iyear/tdl/archive/store"
	"github.com/iyear/tdl/pkg/consts"
	"golang.org/x/sync/errgroup"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// mediaBatch is how many jobs one worker pass takes. It is deliberately small:
// a pass is paced by the download concurrency below, so a huge batch only
// produces a burst of API calls (and then FLOOD_WAIT).
const mediaBatch = 100

const (
	// mediaDefaultThreads is how many media jobs one worker pass downloads at
	// once. Per-file bandwidth is a separate knob (the downloader splits one
	// file into byte ranges), so the real connection count is
	// threads x ranges-per-file.
	//
	// This used to be 1, which serialised the whole queue: a 1000-item backfill
	// finished one file before starting the next, so a 2 MB video blocked every
	// photo behind it. Four keeps the machine interactive (the web UI shares it)
	// while giving the queue real overlap on small files, where per-request
	// latency dominates. Raise it with TDL_MEDIA_THREADS if the link is fast and
	// the archive is the only thing running.
	mediaDefaultThreads = 4
	// mediaMaxThreads caps the env override. Telegram rate-limits getFile per
	// datacentre, and FLOOD_WAIT is a wall-clock penalty far larger than the
	// extra throughput is worth, so an accidental 200 is refused rather than
	// obeyed.
	mediaMaxThreads = 16
)

// MediaThreads reports how many media jobs one worker pass may download at once.
// An out-of-range or unparsable TDL_MEDIA_THREADS falls back to the default
// instead of failing: a typo in an optional tuning knob must not stop archiving.
func MediaThreads() int {
	v := strings.TrimSpace(os.Getenv("TDL_MEDIA_THREADS"))
	if v == "" {
		return mediaDefaultThreads
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > mediaMaxThreads {
		slog.Default().Warn("ignoring out-of-range TDL_MEDIA_THREADS",
			slog.String("value", v),
			slog.Int("max", mediaMaxThreads),
			slog.Int("using", mediaDefaultThreads))
		return mediaDefaultThreads
	}
	return n
}

type MediaWorker struct {
	Store        *store.Store
	Runtime      *Runtime
	Root         string
	Threads      int
	Exporter     *Exporter
	MinFreeSpace int64 // pause downloads below this many free bytes
	// Video downloads external links (Kind=="link" jobs). It is an injected
	// dependency so tests can substitute a fake and so a missing yt-dlp degrades
	// to "link not downloaded" instead of taking the queue down.
	Video *VideoFetcher
}

func NewMediaWorker(s *store.Store, rt *Runtime, root string) *MediaWorker {
	if root == "" {
		root = filepath.Join(consts.DataDir, "archive")
	}
	// The web UI shares this machine and should stay interactive while media is
	// fetched, so the default is modest rather than "as many as possible". See
	// mediaDefaultThreads for why it is no longer 1.
	return &MediaWorker{Store: s, Runtime: rt, Root: root, Threads: MediaThreads(), MinFreeSpace: DefaultMinFreeSpace}
}
func (w *MediaWorker) Run(ctx context.Context) error {
	if w.MinFreeSpace > 0 {
		if free, err := FreeDiskBytes(filepath.Join(w.Root, "objects")); err != nil {
			// A missing/unreadable filesystem must not block message archiving.
			fmt.Printf("archive media: disk check unavailable: %v\n", err)
		} else if free < uint64(w.MinFreeSpace) {
			fmt.Printf("archive media: free disk space %d bytes below watermark %d; pausing downloads (message archiving continues)\n", free, w.MinFreeSpace)
			return nil
		}
	}
	jobs, err := w.Store.ClaimPendingMedia(ctx, mediaBatch)
	if err != nil {
		return err
	}
	if len(jobs) == 0 {
		return nil
	}
	// A graceful stop must not leave claimed rows 'running' until the lease
	// expires: put anything that did not reach a terminal state back in the
	// queue. The write uses a detached context so a canceled pass still
	// releases its claims.
	claimed := make([]int64, 0, len(jobs))
	for _, j := range jobs {
		claimed = append(claimed, j.ID)
	}
	defer func() {
		rctx, cancel := stateCtx(ctx)
		defer cancel()
		_ = w.Store.ReleaseMediaClaims(rctx, claimed)
	}()
	// Resolve file locations per chat, not per file: one history walk serves
	// every pending file of that chat. One request per file is what turns a
	// backlog into a FLOOD_WAIT.
	byChat := make(map[int64][]store.MediaJob)
	order := make([]int64, 0, len(jobs))
	links := make([]store.MediaJob, 0, len(jobs))
	tmeJobs := make([]store.MediaJob, 0, len(jobs))
	for _, j := range jobs {
		// A link job has no Telegram file to resolve: folding it into the
		// per-chat group would send it through a history walk that can never
		// match, and then through the per-message fallback, for nothing.
		switch j.Type {
		case "link":
			links = append(links, j)
			continue
		case store.KindTMe:
			// A quoted post lives under *another* peer, so the chat grouping
			// below would walk the wrong history entirely.
			tmeJobs = append(tmeJobs, j)
			continue
		}
		if _, seen := byChat[j.ChatID]; !seen {
			order = append(order, j.ChatID)
		}
		byChat[j.ChatID] = append(byChat[j.ChatID], j)
	}
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(w.Threads)
	// One bad job must not cancel the batch. errgroup cancels the shared context
	// as soon as any member returns an error, so returning a per-job failure here
	// used to turn one unsupported link into "context canceled" for every other
	// job in the same pass. Each handler already records its own outcome in the
	// database; the log line is the only thing left to do.
	for _, chatID := range order {
		group := byChat[chatID]
		located := w.locateFiles(gctx, chatID, group)
		for _, job := range group {
			j := job
			lm, ok := located[j.MessageID]
			g.Go(w.runJob(gctx, j, func() error { return w.process(gctx, j, lm, ok) }))
		}
	}
	for _, job := range links {
		j := job
		g.Go(w.runJob(gctx, j, func() error { return w.processLink(gctx, j) }))
	}
	for _, job := range tmeJobs {
		j := job
		g.Go(w.runJob(gctx, j, func() error { return w.processTMe(gctx, j) }))
	}
	return g.Wait()
}

// runJob wraps one media job as an errgroup task. It never returns an error:
// one bad job must not cancel the batch, and each handler already records its
// own outcome in the database.
//
// The recover is not decoration. These tasks run in errgroup goroutines, and
// nothing between them and main recovers, so a single malformed payload used to
// take down the whole archive process. Containment alone would not be enough
// either: a job that only logs its panic stays pending and panics again on the
// next pass, so it is parked as a terminal failure instead.
func (w *MediaWorker) runJob(ctx context.Context, j store.MediaJob, fn func() error) func() error {
	return func() (runErr error) {
		defer func() {
			if r := recover(); r != nil {
				slog.Default().Error("media job panicked and was contained",
					slog.Int64("media_id", j.ID),
					slog.String("panic", fmt.Sprint(r)),
					slog.String("stack", string(debug.Stack())))
				wctx, cancel := stateCtx(ctx)
				_ = w.Store.MarkMediaSkippedWithError(wctx, j.ID,
					fmt.Sprintf("panicked while processing: %v", r), "panic", fmt.Sprint(r), j.Attempts+1)
				cancel()
				runErr = nil
			}
		}()
		if err := fn(); err != nil {
			slog.Default().Warn("media job ended with an error",
				slog.String("error", err.Error()))
		}
		return nil
	}
}

// historyScanCap bounds how many history messages one chat scan may read. A
// backlog spanning more ids than this falls back to a per-job lookup.
const historyScanCap = 500

// locatedMedia is what one history walk produced for a message: the primary
// file plus, when the payload carries thumbnails, the location of the largest
// one. Both come out of the same walk on purpose — a second scan just to fetch
// a poster would double the API calls for every video in the archive.
type locatedMedia struct {
	File  messages.File
	Thumb tg.InputFileLocationClass
}

// resolveChat resolves a chat for media work, falling back to the public
// username when the id-based resolve fails.
//
// Without the fallback a monitored bot can never be downloaded. A bot is a
// *tg.PeerUser whose access_hash the archive never learned (Telegram does not
// repeat the bot's User object on every update), so peers.ResolveUserID asks the
// server with AccessHash:0 and gets 400 CONNECTION_LAYER_INVALID — surfacing as
// "get users: ..." on every job. Channel-backed chats are unaffected because
// their access_hash is delivered with the channel itself.
func (w *MediaWorker) resolveChat(ctx context.Context, chatID int64) (peers.Peer, error) {
	username := ""
	if w.Store != nil {
		if t, err := w.Store.Target(ctx, chatID); err == nil {
			username = t.Username
		}
		if username == "" {
			_, username = w.Store.ChatIdentity(ctx, chatID)
		}
	}
	return ResolveChatWithUsername(ctx, w.Runtime, chatID, username)
}

// locateFiles resolves the downloadable file of many messages of one chat with a
// single history walk. Message ids that the walk cannot confirm within
// historyScanCap are simply absent from the result; the caller then falls back
// to the per-message lookup.
func (w *MediaWorker) locateFiles(ctx context.Context, chatID int64, jobs []store.MediaJob) map[int]locatedMedia {
	out := make(map[int]locatedMedia, len(jobs))
	p, err := w.resolveChat(ctx, chatID)
	if err != nil {
		// Returning silently here used to hide the whole failure: every job then
		// fell back to a per-message lookup that failed the same way, and the
		// queue looked idle instead of broken.
		slog.WarnContext(ctx, "archive media: chat resolve failed", "chat", chatID, "error", err)
		return out
	}
	wanted := make(map[int]bool, len(jobs))
	maxID := 0
	for _, j := range jobs {
		wanted[j.MessageID] = true
		if j.MessageID > maxID {
			maxID = j.MessageID
		}
	}
	it := query.Messages(w.Runtime.API).GetHistory(p.InputPeer()).BatchSize(100).OffsetID(maxID + 1).Iter()
	scanned := 0
	for it.Next(ctx) {
		scanned++
		if scanned > historyScanCap {
			break
		}
		id := it.Value().Msg.GetID()
		if !wanted[id] {
			continue
		}
		if _, done := out[id]; done {
			continue
		}
		if f, ok := messageFile(it.Value().Msg); ok {
			out[id] = locatedMedia{File: f, Thumb: thumbLocation(it.Value().Msg)}
			if len(out) == len(wanted) {
				break
			}
		}
	}
	return out
}

// mediaMaxAttempts is how many times a media job is retried before it is parked
// as skipped. Without a terminal state a job whose file is gone from the server
// (or whose chat is no longer accessible) stays 'pending' forever, is re-fetched
// on every pass and silently occupies the queue.
const mediaMaxAttempts = 5

// floodPause is how long the media queue stands down after a timeout that most
// likely hides a flood wait.
const floodPause = 10 * time.Minute

// pause parks the whole media queue for d so a rate limit is not hammered by
// per-job retries. Best effort: failing to record it only costs extra requests.
func (w *MediaWorker) pause(ctx context.Context, d time.Duration) {
	if d < floodPause {
		d = floodPause
	}
	_ = w.Store.SetMediaPausedUntil(ctx, time.Now().Add(d))
}

// stateCtx detaches a terminal state write from the job context. A job that
// failed *because* its context expired is exactly the case where the write must
// still land — using the canceled context here silently dropped every failure
// record, leaving jobs pending with attempts=0 and retried forever.
func stateCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
}

func classifyMediaErrorKind(err error) string {
	kind, _ := classifyMediaError(err)
	return kind
}

func classifyMediaError(err error) (kind, detail string) {
	detail = err.Error()
	if _, ok := tgerr.AsFloodWait(err); ok || strings.Contains(strings.ToUpper(detail), "FLOOD_WAIT") {
		return "flood_wait", detail
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout", detail
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "timeout", detail
	}
	upper := strings.ToUpper(detail)
	if strings.Contains(upper, "FILE_REFERENCE_EXPIRED") || strings.Contains(upper, "FILE_ID_INVALID") || strings.Contains(upper, "MEDIA MESSAGE") && strings.Contains(upper, "NOT FOUND") || os.IsNotExist(err) {
		return "not_found", detail
	}
	if strings.Contains(upper, "CHAT_ADMIN_REQUIRED") || strings.Contains(upper, "CHANNEL_PRIVATE") || strings.Contains(upper, "PERMISSION") || strings.Contains(upper, "FORBIDDEN") {
		return "permission", detail
	}
	if errors.Is(err, syscall.ENOSPC) || strings.Contains(strings.ToLower(detail), "no space left on device") {
		return "disk_full", detail
	}
	var pe *os.PathError
	if errors.As(err, &pe) {
		return "io", detail
	}
	return "unknown", detail
}

func floodWaitDuration(err error) (time.Duration, bool) {
	if d, ok := tgerr.AsFloodWait(err); ok {
		return d, true
	}
	s := strings.ToUpper(err.Error())
	const p = "FLOOD_WAIT_"
	if i := strings.Index(s, p); i >= 0 {
		if n, e := strconv.Atoi(strings.TrimSpace(s[i+len(p):])); e == nil && n > 0 {
			return time.Duration(n) * time.Second, true
		}
	}
	return 0, false
}

func retryDelay(kind string, attempts int) time.Duration {
	base := time.Duration(1<<min(attempts, 6)) * time.Minute
	if base > time.Hour {
		base = time.Hour
	}
	jitter := time.Duration(time.Now().UnixNano()%30) * time.Second
	return base + jitter
}

// fail records a classified media failure. Terminal categories are parked as
// skipped so the existing pending queue cannot accidentally retry them.
func (w *MediaWorker) fail(ctx context.Context, j store.MediaJob, err error, permanent bool) error {
	// A shutdown is not a media failure: consuming an attempt (or worse, hitting
	// the give-up branch) would park a healthy job because the process stopped.
	if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
		return err
	}
	// Terminal failures drop the staging bytes: no later attempt will ever
	// consume them, so keeping them would only leak disk.
	terminal := permanent || classifyMediaErrorKind(err) == "not_found" ||
		classifyMediaErrorKind(err) == "permission" || classifyMediaErrorKind(err) == "disk_full" ||
		j.Attempts+1 >= mediaMaxAttempts
	if terminal {
		_ = os.Remove(filepath.Join(w.Root, "staging", fmt.Sprintf("%d.tmp", j.ID)))
	}
	wctx, cancel := stateCtx(ctx)
	defer cancel()
	kind, detail := classifyMediaError(err)
	attempt := j.Attempts + 1
	if d, ok := floodWaitDuration(err); ok {
		d += time.Duration(time.Now().UnixNano()%30) * time.Second
		next := time.Now().Add(d)
		_ = w.Store.MarkMediaRetryAfter(wctx, j.ID, err.Error(), kind, detail, attempt, &next)
		logMediaFailure(j.ID, kind, detail, attempt, &next)
		w.pause(wctx, d)
		return err
	}
	if kind == "timeout" {
		// A slow transfer keeps its staging file: the next attempt may find a
		// complete file and skip the transfer entirely.
		// The deadline is our own per-job budget, not a server rate limit:
		// pausing the whole queue for 10 minutes turned one slow file into a
		// stalled queue. Record it and stay on the normal retry ladder.
		next := time.Now().Add(retryDelay(kind, attempt))
		_ = w.Store.MarkMediaFailed(wctx, j.ID, err.Error(), kind, detail, attempt, next)
		logMediaFailure(j.ID, kind, detail, attempt, &next)
		return err
	}
	if terminal {
		_ = w.Store.MarkMediaSkippedWithError(wctx, j.ID, fmt.Sprintf("giving up after %d attempt(s): %v", attempt, err), kind, detail, attempt)
		logMediaFailure(j.ID, kind, detail, attempt, nil)
		return nil
	}
	next := time.Now().Add(retryDelay(kind, attempt))
	_ = w.Store.MarkMediaFailed(wctx, j.ID, err.Error(), kind, detail, attempt, next)
	logMediaFailure(j.ID, kind, detail, attempt, &next)
	return err
}

// noTargetBackoff is how long a media row sleeps when its chat is not a
// monitored target yet.
//
// This state is revivable, not terminal: a chat can be archived by a rescan, a
// sync or an autoclick reply before anyone adds it as a target, and the target
// may be created minutes or days later. Writing the row off as skipped lost
// those downloads for good (production lost 1153 rows that way), so park it as
// pending with a long backoff and let Store.WakeTargetMedia wake it the moment
// the target appears.
const noTargetBackoff = 24 * time.Hour

// deferNoTarget parks a job whose chat has no monitor_targets row.
//
// Attempts are deliberately left untouched. The media itself has never been
// attempted, so consuming the retry ladder here would let an unrelated condition
// spend a download's budget and eventually park it for real.
func (w *MediaWorker) deferNoTarget(ctx context.Context, j store.MediaJob) error {
	wctx, cancel := stateCtx(ctx)
	defer cancel()
	next := time.Now().Add(noTargetBackoff)
	if err := w.Store.MarkMediaTargetMissing(wctx, j.ID, next); err != nil {
		return err
	}
	slog.Default().Warn("media parked: chat is not a monitored target yet",
		slog.Int64("media_id", j.ID),
		slog.Int64("chat_id", j.ChatID),
		slog.Int("message_id", j.MessageID),
		slog.Time("retry_at", next),
	)
	return nil
}

// mediaJobTimeout bounds one media job so a stuck network call cannot hold the
// whole pass (and the next tick) hostage. mediaTimeoutFor raises it for large
// files: a fixed cap makes anything above a few hundred MB fail forever on a
// slow link.
const mediaJobTimeout = 5 * time.Minute

// mediaTimeoutFor scales the per-job budget with the known size, assuming a
// pessimistic 256 KiB/s floor and capping at two hours.
func mediaTimeoutFor(size int64) time.Duration {
	d := mediaJobTimeout
	if size > 0 {
		d += time.Duration(size/(256*1024)) * time.Second
	}
	if d > 2*time.Hour {
		d = 2 * time.Hour
	}
	return d
}

// linkJobTimeout is deliberately far larger than mediaJobTimeout: a link job
// runs an external extractor over the public internet, where a single video can
// legitimately take minutes. The two timeouts exist separately so a slow video
// never lengthens the window a Telegram file download may occupy.
const linkJobTimeout = 25 * time.Minute

// processLink downloads a video that lives outside Telegram. The post that
// carried it is already archived as a message; this adds the bytes.
//
// Failure policy differs from a Telegram file download on purpose: a missing
// tool or an unsupported site is terminal (retrying cannot fix either), while a
// network error keeps the normal retry ladder.
func (w *MediaWorker) processLink(ctx context.Context, j store.MediaJob) error {
	ctx, cancel := context.WithTimeout(ctx, linkJobTimeout)
	defer cancel()
	started := time.Now()
	log := slog.Default().With(
		slog.Int64("media_id", j.ID),
		slog.Int64("chat_id", j.ChatID),
		slog.Int("message_id", j.MessageID),
		slog.String("url", j.URL),
	)
	t, err := w.Store.Target(ctx, j.ChatID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return w.deferNoTarget(ctx, j)
		}
		return w.fail(ctx, j, err, true)
	}
	if j.URL == "" {
		return w.Store.MarkMediaSkipped(ctx, j.ID, "link job has no source URL")
	}
	fetcher := w.Video
	if fetcher == nil {
		fetcher = NewVideoFetcher()
	}
	if !fetcher.Available() {
		// Park instead of retrying: no attempt limit will install yt-dlp, and a
		// failing job would be re-fetched on every pass forever.
		wctx, cancelState := stateCtx(ctx)
		defer cancelState()
		return w.Store.MarkMediaSkippedWithError(wctx, j.ID,
			"external downloader unavailable: install yt-dlp to archive link videos",
			"tool_missing", fetcher.Binary, j.Attempts)
	}
	// The per-target cap applies to external media too; yt-dlp enforces it
	// before writing the body, which is the only place the limit is cheap.
	if t.MediaMaxSize > 0 {
		fetcher.MaxSize = t.MediaMaxSize
	}
	staging := filepath.Join(w.Root, "staging")
	if err := os.MkdirAll(staging, 0o755); err != nil {
		return w.fail(ctx, j, err, false)
	}
	fmt.Fprintf(os.Stderr, "archive media: fetching link job=%d chat=%d msg=%d url=%s\n", j.ID, j.ChatID, j.MessageID, j.URL)

	dl, err := fetcher.Download(ctx, j.URL, staging)
	if err != nil {
		switch {
		case errors.Is(err, ErrVideoToolMissing):
			wctx, cancelState := stateCtx(ctx)
			defer cancelState()
			return w.Store.MarkMediaSkippedWithError(wctx, j.ID, err.Error(), "tool_missing", "", j.Attempts)
		case errors.Is(err, ErrVideoTooLarge):
			wctx, cancelState := stateCtx(ctx)
			defer cancelState()
			// Policy limit, not a dead URL: park it with the reason so raising the
			// cap and re-queueing works, instead of burning attempts on retries.
			return w.Store.MarkMediaSkippedWithError(wctx, j.ID, err.Error(), "too_large", j.URL, j.Attempts)
		case errors.Is(err, ErrVideoUnsupported):
			wctx, cancelState := stateCtx(ctx)
			defer cancelState()
			// Terminal on purpose: an unsupported or private URL will not become
			// supported by waiting, and each retry costs a full extraction run.
			return w.Store.MarkMediaTerminal(wctx, j.ID, err.Error(), "unsupported", firstLine(err.Error()), j.Attempts)
		}
		return w.fail(ctx, j, err, false)
	}
	defer os.Remove(dl.Path)

	sha, size, err := hashFile(dl.Path)
	if err != nil {
		return w.fail(ctx, j, err, false)
	}
	obj := filepath.Join(w.Root, "objects", sha[:2], sha[2:4], sha)
	if err := os.MkdirAll(filepath.Dir(obj), 0o755); err != nil {
		return w.fail(ctx, j, err, false)
	}
	if _, statErr := os.Stat(obj); os.IsNotExist(statErr) {
		if err := os.Rename(dl.Path, obj); err != nil {
			return w.fail(ctx, j, err, false)
		}
	}
	// Cover art: yt-dlp hands back a still frame, and the shared preview builder
	// turns it into the same 320/1280 pair a Telegram photo gets. Doing it here
	// rather than with ffmpeg keeps a video poster working on a machine where
	// ffmpeg cannot load its own libraries.
	thumb, preview := "", ""
	if len(dl.Thumbnail) > 0 {
		coverSHA := sha256Hex(dl.Thumbnail)
		coverPath := filepath.Join(w.Root, "covers", coverSHA[:2], coverSHA[2:4], coverSHA+".jpg")
		if err := os.MkdirAll(filepath.Dir(coverPath), 0o755); err != nil {
			return w.fail(ctx, j, err, false)
		}
		if _, statErr := os.Stat(coverPath); os.IsNotExist(statErr) {
			if err := os.WriteFile(coverPath, dl.Thumbnail, 0o644); err != nil {
				return w.fail(ctx, j, err, false)
			}
		}
		if th, pv, e := BuildMediaPreviews(ctx, coverPath, "image/jpeg", w.Root, coverSHA); e == nil {
			thumb, preview = th, pv
		}
	}
	wctx, cancelState := stateCtx(ctx)
	defer cancelState()
	if err := w.Store.MarkMediaCompleted(wctx, j.ID, sha, size, dl.MIME, obj, thumb, preview); err != nil {
		return w.fail(ctx, j, err, false)
	}
	log.Info("media worker link fetched",
		slog.String("sha256", sha),
		slog.Int64("size", size),
		slog.String("title", dl.Title),
		slog.Int("duration", dl.Duration),
		slog.Int64("duration_ms", time.Since(started).Milliseconds()),
	)
	if w.Exporter != nil {
		return w.Exporter.ExportMedia(ctx, j.ChatID, j.MessageID)
	}
	return nil
}

// fetchThumb downloads the largest server-side thumbnail of a payload. key
// names the staging file so concurrent jobs cannot collide.
func (w *MediaWorker) fetchThumb(ctx context.Context, loc tg.InputFileLocationClass, key string) (string, error) {
	staging := filepath.Join(w.Root, "staging")
	if err := os.MkdirAll(staging, 0o755); err != nil {
		return "", err
	}
	dst := filepath.Join(staging, key+".thumb")
	tctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	if _, err := downloader.NewDownloader().Download(w.Runtime.API, loc).ToPath(tctx, dst); err != nil {
		return "", err
	}
	return dst, nil
}

// messageFile resolves the downloadable file of a message without trusting the
// shape of the payload the server sent.
//
// gotd's messages.Elem.File only asks whether the media *kind* matches, never
// whether the object behind it is whole. A *tg.MessageMediaPhoto that is
// present but nil is still matched by its type switch, and reading .Photo then
// faults — SIGSEGV at address 0x18 of a nil struct instead of the documented
// ok=false. The photo branch has the same hole one level deeper, because
// (*tg.Photo).AsNotEmpty carries no receiver check and returns (nil, true), so
// a typed-nil photo survives the ok check and faults in GetID. A history walk
// meets whatever the account can see, so those nil shapes have to be rejected
// here rather than there.
//
// recover is the backstop for shapes not enumerated above: this runs inside a
// media-worker goroutine, where an unrecovered panic takes down the whole
// archive process instead of failing one job.
func messageFile(notEmpty tg.NotEmptyMessage) (f messages.File, ok bool) {
	defer func() {
		if r := recover(); r != nil {
			slog.Default().Warn("media payload panicked while resolving its file",
				slog.String("message_type", fmt.Sprintf("%T", notEmpty)),
				slog.String("panic", fmt.Sprint(r)))
			f, ok = messages.File{}, false
		}
	}()
	m, isMessage := notEmpty.(*tg.Message)
	if !isMessage || m == nil {
		return messages.File{}, false
	}
	switch md := m.Media.(type) {
	case *tg.MessageMediaPhoto:
		if md == nil {
			return rejectMedia(notEmpty, "photo media object is nil")
		}
		if md.Photo == nil {
			return rejectMedia(notEmpty, "photo media carries no photo")
		}
		if p, typed := md.Photo.(*tg.Photo); typed && p == nil {
			return rejectMedia(notEmpty, "photo media carries a typed-nil photo")
		}
	case *tg.MessageMediaDocument:
		if md == nil {
			return rejectMedia(notEmpty, "document media object is nil")
		}
		if md.Document == nil {
			return rejectMedia(notEmpty, "document media carries no document")
		}
		if d, typed := md.Document.(*tg.Document); typed && d == nil {
			return rejectMedia(notEmpty, "document media carries a typed-nil document")
		}
	default:
		return messages.File{}, false
	}
	f, ok = messages.Elem{Msg: notEmpty}.File()
	if !ok || f.Location == nil {
		return messages.File{}, false
	}
	return f, true
}

// rejectMedia records why a payload was refused. It is Debug rather than Warn
// because one chat can hold many such messages and the log would drown; the
// value is in being able to explain, at all, why a job later ends up parked as
// "not found" instead of looking like a lost file.
func rejectMedia(notEmpty tg.NotEmptyMessage, reason string) (messages.File, bool) {
	if slog.Default().Enabled(context.Background(), slog.LevelDebug) {
		slog.Default().Debug("media payload rejected",
			slog.String("reason", reason),
			slog.String("message_type", fmt.Sprintf("%T", notEmpty)))
	}
	return messages.File{}, false
}

// thumbLocation returns the download location of the largest thumbnail a
// message's document carries, or nil when it has none.
//
// Photos are excluded on purpose: a photo's main file *is* the image, and
// BuildMediaPreviews already derives both variants from it. Only formats whose
// bytes are not an image need a poster pulled out of the thumbnail list.
func thumbLocation(notEmpty tg.NotEmptyMessage) (loc tg.InputFileLocationClass) {
	// Same untrusted-payload problem as messageFile: the nil that killed the
	// file resolver also sits on the poster path, and a poster is decoration
	// — losing one must never cost the job.
	defer func() {
		if r := recover(); r != nil {
			slog.Default().Warn("media payload panicked while resolving its thumbnail",
				slog.String("message_type", fmt.Sprintf("%T", notEmpty)),
				slog.String("panic", fmt.Sprint(r)))
			loc = nil
		}
	}()
	// NotEmptyMessage does not expose media; every message the archive walks is
	// the *tg.Message variant, and anything else simply has no poster.
	m, ok := notEmpty.(*tg.Message)
	if !ok || m == nil {
		return nil
	}
	md, ok := m.GetMedia()
	if !ok {
		return nil
	}
	doc, ok := md.(*tg.MessageMediaDocument)
	if !ok || doc == nil {
		return nil
	}
	d, ok := doc.Document.(*tg.Document)
	if !ok || d == nil {
		return nil
	}
	thumbs, has := d.GetThumbs()
	if !has || len(thumbs) == 0 {
		return nil
	}
	best, bestArea := "", 0
	for _, th := range thumbs {
		sized, ok := th.AsNotEmpty()
		if !ok || sized == nil {
			continue
		}
		key := sized.GetType()
		if key == "" {
			continue
		}
		w, h := photoSizeDims(sized)
		if area := w * h; area >= bestArea {
			best, bestArea = key, area
		}
	}
	if best == "" {
		return nil
	}
	l := d.AsInputDocumentFileLocation()
	l.ThumbSize = best
	return l
}

// photoSizeDims reads the dimensions of a thumbnail payload. PhotoStrippedSize
// is deliberately absent: it is a 120-byte placeholder, not an image.
func photoSizeDims(p tg.NotEmptyPhotoSize) (int, int) {
	switch x := p.(type) {
	case *tg.PhotoSize:
		return x.W, x.H
	case *tg.PhotoSizeProgressive:
		return x.W, x.H
	case *tg.PhotoCachedSize:
		return x.W, x.H
	}
	return 0, 0
}

// hashFile returns the lowercase hex sha256 of a file along with its size.
func hashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", 0, err
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), st.Size(), nil
}

// sha256Hex hashes an in-memory buffer (cover art is a few hundred kilobytes).
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// mediaClaimHeartbeat is how often a running download refreshes its lease.
// It must stay well below store's claim lease so a long file is never treated
// as abandoned while it is still being written.
const mediaClaimHeartbeat = 5 * time.Minute

// keepClaimAlive refreshes the job lease in the background until the returned
// stop function is called or the job context ends.
func (w *MediaWorker) keepClaimAlive(ctx context.Context, id int64) func() {
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(mediaClaimHeartbeat)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				// Detached: the heartbeat must still land if the job context
				// was just canceled, so the next pass can see it expired rather
				// than treating it as abandoned.
				wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
				_ = w.Store.TouchMediaClaim(wctx, id)
				cancel()
			}
		}
	}()
	return func() { close(done) }
}

func (w *MediaWorker) process(ctx context.Context, j store.MediaJob, located locatedMedia, locatedOK bool) error {
	ctx, cancel := context.WithTimeout(ctx, mediaTimeoutFor(j.Size))
	defer cancel()
	defer w.keepClaimAlive(ctx, j.ID)()
	started := time.Now()
	slog.Default().With(
		slog.Int64("media_id", j.ID),
		slog.String("file_id_unique", j.FileUniqueID),
	).Debug("media worker started",
		slog.Int64("chat_id", j.ChatID),
		slog.Int("message_id", j.MessageID),
		slog.String("type", j.Type),
		slog.Int("attempts", j.Attempts),
		slog.Bool("located", locatedOK),
	)
	t, err := w.Store.Target(ctx, j.ChatID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return w.deferNoTarget(ctx, j)
		}
		return w.fail(ctx, j, err, true)
	}
	// Enforce the per-target size cap from message metadata BEFORE spending
	// bandwidth: the size is known at enqueue time for documents and photos.
	if t.MediaMaxSize > 0 && j.Size > 0 && j.Size > t.MediaMaxSize {
		wctx, cancel := stateCtx(ctx)
		defer cancel()
		return w.Store.MarkMediaSkipped(wctx, j.ID, "media exceeds configured max size")
	}
	if j.FileUniqueID != "" {
		if reusable, ok, e := w.Store.FindReusableMedia(ctx, j.FileUniqueID); e == nil && ok {
			obj := filepath.Join(w.Root, "objects", reusable.SHA256[:2], reusable.SHA256[2:4], reusable.SHA256)
			if _, statErr := os.Stat(obj); statErr == nil {
				if e := w.Store.MarkMediaReused(ctx, j.ID, reusable); e != nil {
					return e
				}
				recordDedupHit()
				logger := slog.Default().With(
					slog.String("file_id_unique", j.FileUniqueID),
					slog.String("sha256", reusable.SHA256),
					slog.Int64("media_id", j.ID),
				)
				logger.Info("media CAS dedup hit",
					slog.Int64("duration_ms", time.Since(started).Milliseconds()),
					slog.Bool("dedup_hit", true),
				)
				return nil
			}
		}
	}
	lm, found := located, locatedOK
	if !found {
		// The batched chat scan did not cover this id: fall back to looking up
		// just this message.
		p, err := w.resolveChat(ctx, j.ChatID)
		if err != nil {
			return w.fail(ctx, j, err, false)
		}
		it := query.Messages(w.Runtime.API).GetHistory(p.InputPeer()).BatchSize(20).OffsetID(j.MessageID + 1).Iter()
		// 与 locateFiles 的 historyScanCap 同一款上限：消息被服务端删除时
		// getHistory 会一直走到频道最旧端，20 万条的频道 ≈ 上万次 API 请求，
		// 批量遇到这种任务会触发 FLOOD_WAIT 停掉整个队列。走到上限还没命中
		// 就按未找到终态处理——结果一样，代价有界。
		scanned := 0
		for it.Next(ctx) {
			scanned++
			if scanned > historyScanCap {
				break
			}
			if it.Value().Msg.GetID() != j.MessageID {
				continue
			}
			if f, ok := messageFile(it.Value().Msg); ok {
				lm = locatedMedia{File: f, Thumb: thumbLocation(it.Value().Msg)}
				found = true
				break
			}
		}
		if err := it.Err(); err != nil {
			return w.fail(ctx, j, err, false)
		}
	}
	if !found {
		// Either deleted on the server or outside the visible history window.
		return w.fail(ctx, j, fmt.Errorf("media message %d not found", j.MessageID), false)
	}
	file := lm.File
	staging := filepath.Join(w.Root, "staging")
	if err := os.MkdirAll(staging, 0755); err != nil {
		return w.fail(ctx, j, err, false)
	}
	tmp := filepath.Join(staging, fmt.Sprintf("%d.tmp", j.ID))
	// Keep the staging file across failures instead of deleting it on every
	// retry. Two cases benefit:
	//   - a transfer that completed but failed during ingest (disk full,
	//     preview/export error) is re-ingested without downloading it again;
	//   - a job interrupted by a restart does not lose the bytes it already
	//     fetched.
	// gotd's ToPath truncates, so a partial file cannot be resumed offset-wise;
	// it is discarded and re-fetched. Refusing to delete it before this check
	// is what makes the complete-file case cheap.
	complete := false
	if st, statErr := os.Stat(tmp); statErr == nil {
		if j.Size > 0 && st.Size() == j.Size {
			complete = true
			slog.Default().Info("media staging file already complete; skipping transfer",
				slog.Int64("media_id", j.ID),
				slog.Int64("size", st.Size()))
		} else {
			// Partial or unknown-size leftover: restart cleanly rather than
			// handing a truncated file to the hash/CAS step.
			_ = os.Remove(tmp)
		}
	}
	if !complete {
		fmt.Fprintf(os.Stderr, "archive media: downloading job=%d chat=%d msg=%d type=%s\n", j.ID, j.ChatID, j.MessageID, j.Type)
		if _, err := downloader.NewDownloader().WithPartSize(1024*1024).Download(w.Runtime.API, file.Location).WithThreads(2).ToPath(ctx, tmp); err != nil {
			return w.fail(ctx, j, err, false)
		}
	}
	if err := w.ingestObject(ctx, j, tmp, file.MIMEType, t.MediaMaxSize, lm.Thumb); err != nil {
		return w.fail(ctx, j, err, false)
	}
	// ingestObject either renamed the file into the CAS or found the object
	// already present; either way the staging copy is no longer needed.
	_ = os.Remove(tmp)
	slog.Default().Info("media worker download completed",
		slog.Int64("media_id", j.ID),
		slog.String("file_id_unique", j.FileUniqueID),
		slog.Int64("duration_ms", time.Since(started).Milliseconds()),
		slog.Bool("dedup_hit", false),
	)
	return nil
}

// ingestObject moves a fully downloaded file into the content-addressed store
// and records it against the job.
//
// Every job kind ends here: an in-chat attachment, and a post quoted through a
// t.me link. Keeping it in one place is what makes the archive treat a quoted
// video exactly like an uploaded one — same hash, same dedup, same previews,
// same export trigger.
func (w *MediaWorker) ingestObject(ctx context.Context, j store.MediaJob, tmp, mime string, maxSize int64, thumbLoc tg.InputFileLocationClass) error {
	f, err := os.Open(tmp)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	h := sha256.New()
	_, err = io.Copy(h, f)
	f.Close()
	if err != nil {
		return err
	}
	sha := hex.EncodeToString(h.Sum(nil))
	if maxSize > 0 && st.Size() > maxSize {
		wctx, cancel := stateCtx(ctx)
		defer cancel()
		_ = w.Store.MarkMediaSkipped(wctx, j.ID, "media exceeds configured max size")
		return nil
	}
	obj := filepath.Join(w.Root, "objects", sha[:2], sha[2:4], sha)
	if err := os.MkdirAll(filepath.Dir(obj), 0755); err != nil {
		return err
	}
	if _, err := os.Stat(obj); os.IsNotExist(err) {
		if err := os.Rename(tmp, obj); err != nil {
			return err
		}
	}
	thumb, preview := "", ""
	if t, p, e := BuildMediaPreviews(ctx, obj, mime, w.Root, sha); e == nil {
		thumb, preview = t, p
	}
	// Poster for anything whose own bytes are not an image. Telegram ships a
	// thumbnail with every video and document, so the cover costs one extra
	// download and no decoder: this is why ffmpeg is not required for it.
	if thumb == "" && thumbLoc != nil {
		if src, e := w.fetchThumb(ctx, thumbLoc, sha); e == nil {
			if t, p, e := BuildMediaPreviews(ctx, src, "image/jpeg", w.Root, sha); e == nil {
				thumb, preview = t, p
			}
			_ = os.Remove(src)
		} else {
			// A missing poster must not fail a downloaded file.
			slog.Default().Debug("media thumbnail unavailable",
				slog.Int64("media_id", j.ID), slog.String("error", e.Error()))
		}
	}
	wctx, cancel := stateCtx(ctx)
	defer cancel()
	if err := w.Store.MarkMediaCompleted(wctx, j.ID, sha, st.Size(), mime, obj, thumb, preview); err != nil {
		return err
	}
	// Publish the receive-number view. Best effort by design: the bytes are
	// already committed under their content hash, so a naming failure here must
	// not turn a successful download into a failed job.
	if serial, e := w.Store.MediaSerial(wctx, j.ID); e == nil {
		linkSerialViewBestEffort(w.Root, serial, j.Type, mime, obj)
	}
	slog.Default().Info("media object stored",
		slog.Int64("media_id", j.ID),
		slog.String("sha256", sha),
		slog.Int64("size", st.Size()),
		slog.String("type", j.Type),
	)
	if w.Exporter != nil {
		return w.Exporter.ExportMedia(ctx, j.ChatID, j.MessageID)
	}
	return nil
}

// DrainMediaQueue is the shared media-queue drain loop used by both the
// managed serve session and `archive run`. It honors the global pause gate
// (media_paused_until, set after a rate limit), then runs up to batches worker
// passes back to back, stopping as soon as the queue is empty. runPass wraps
// one worker pass so each caller keeps its own worker construction and gating.
// It returns the number of passes actually run.
func DrainMediaQueue(ctx context.Context, s *store.Store, batches int, runPass func(context.Context) error) (int, error) {
	if until := s.MediaPausedUntil(ctx); time.Now().Before(until) {
		fmt.Fprintf(os.Stderr, "archive media: paused until %s after a rate limit\n", until.Format(time.RFC3339))
		return 0, nil
	}
	passes := 0
	for i := 0; i < batches; i++ {
		if err := runPass(ctx); err != nil {
			return passes, err
		}
		passes++
		more, err := s.PendingMedia(ctx, 1)
		if err != nil {
			return passes, err
		}
		if len(more) == 0 {
			break
		}
	}
	return passes, nil
}
