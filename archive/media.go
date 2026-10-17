package archive

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/gotd/td/telegram/downloader"
	"github.com/gotd/td/telegram/query"
	"github.com/gotd/td/telegram/query/messages"
	"github.com/gotd/td/tgerr"
	"github.com/iyear/tdl/archive/store"
	"github.com/iyear/tdl/pkg/consts"
	"golang.org/x/sync/errgroup"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// mediaBatch is how many jobs one worker pass takes. It is deliberately small:
// a pass is paced by the two download threads, so a huge batch only produces a
// burst of API calls (and then FLOOD_WAIT).
const mediaBatch = 100

type MediaWorker struct {
	Store        *store.Store
	Runtime      *Runtime
	Root         string
	Threads      int
	Exporter     *Exporter
	MinFreeSpace int64 // pause downloads below this many free bytes
}

func NewMediaWorker(s *store.Store, rt *Runtime, root string) *MediaWorker {
	if root == "" {
		root = filepath.Join(consts.DataDir, "archive")
	}
	// Keep archive downloads deliberately conservative. The web UI shares the same
	// machine and should remain interactive while media is being fetched.
	return &MediaWorker{Store: s, Runtime: rt, Root: root, Threads: 1, MinFreeSpace: DefaultMinFreeSpace}
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
	jobs, err := w.Store.PendingMedia(ctx, mediaBatch)
	if err != nil {
		return err
	}
	if len(jobs) == 0 {
		return nil
	}
	// Resolve file locations per chat, not per file: one history walk serves
	// every pending file of that chat. One request per file is what turns a
	// backlog into a FLOOD_WAIT.
	byChat := make(map[int64][]store.MediaJob)
	order := make([]int64, 0, len(jobs))
	for _, j := range jobs {
		if _, seen := byChat[j.ChatID]; !seen {
			order = append(order, j.ChatID)
		}
		byChat[j.ChatID] = append(byChat[j.ChatID], j)
	}
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(w.Threads)
	for _, chatID := range order {
		group := byChat[chatID]
		located := w.locateFiles(gctx, chatID, group)
		for _, job := range group {
			j := job
			f, ok := located[j.MessageID]
			g.Go(func() error { return w.process(gctx, j, f, ok) })
		}
	}
	return g.Wait()
}

// historyScanCap bounds how many history messages one chat scan may read. A
// backlog spanning more ids than this falls back to a per-job lookup.
const historyScanCap = 500

// locateFiles resolves the downloadable file of many messages of one chat with a
// single history walk. Message ids that the walk cannot confirm within
// historyScanCap are simply absent from the result; the caller then falls back
// to the per-message lookup.
func (w *MediaWorker) locateFiles(ctx context.Context, chatID int64, jobs []store.MediaJob) map[int]messages.File {
	out := make(map[int]messages.File, len(jobs))
	p, err := ResolveChatID(ctx, w.Runtime, chatID)
	if err != nil {
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
		if f, ok := it.Value().File(); ok {
			out[id] = f
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
		d := retryDelay(kind, attempt)
		next := time.Now().Add(d)
		_ = w.Store.MarkMediaFailed(wctx, j.ID, err.Error(), kind, detail, attempt, next)
		logMediaFailure(j.ID, kind, detail, attempt, &next)
		w.pause(wctx, floodPause)
		return err
	}
	if permanent || kind == "not_found" || kind == "permission" || kind == "disk_full" || attempt >= mediaMaxAttempts {
		_ = w.Store.MarkMediaSkippedWithError(wctx, j.ID, fmt.Sprintf("giving up after %d attempt(s): %v", attempt, err), kind, detail, attempt)
		logMediaFailure(j.ID, kind, detail, attempt, nil)
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return err
	}
	d := retryDelay(kind, attempt)
	next := time.Now().Add(d)
	_ = w.Store.MarkMediaFailed(wctx, j.ID, err.Error(), kind, detail, attempt, next)
	logMediaFailure(j.ID, kind, detail, attempt, &next)
	return err
}

// mediaJobTimeout bounds one media job so a stuck network call cannot hold the
// whole pass (and the next tick) hostage.
const mediaJobTimeout = 5 * time.Minute

func (w *MediaWorker) process(ctx context.Context, j store.MediaJob, located messages.File, locatedOK bool) error {
	ctx, cancel := context.WithTimeout(ctx, mediaJobTimeout)
	defer cancel()
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
		// The chat is no longer monitored: nothing to retry.
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
	file, found := located, locatedOK
	if !found {
		// The batched chat scan did not cover this id: fall back to looking up
		// just this message.
		p, err := ResolveChatID(ctx, w.Runtime, j.ChatID)
		if err != nil {
			return w.fail(ctx, j, err, false)
		}
		it := query.Messages(w.Runtime.API).GetHistory(p.InputPeer()).BatchSize(20).OffsetID(j.MessageID + 1).Iter()
		for it.Next(ctx) {
			if it.Value().Msg.GetID() != j.MessageID {
				continue
			}
			file, found = it.Value().File()
			if found {
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
	staging := filepath.Join(w.Root, "staging")
	if err := os.MkdirAll(staging, 0755); err != nil {
		return err
	}
	tmp := filepath.Join(staging, fmt.Sprintf("%d.tmp", j.ID))
	defer os.Remove(tmp)
	fmt.Fprintf(os.Stderr, "archive media: downloading job=%d chat=%d msg=%d type=%s\n", j.ID, j.ChatID, j.MessageID, j.Type)
	if _, err := downloader.NewDownloader().WithPartSize(1024*1024).Download(w.Runtime.API, file.Location).WithThreads(2).ToPath(ctx, tmp); err != nil {
		return w.fail(ctx, j, err, false)
	}
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
	if t.MediaMaxSize > 0 && st.Size() > t.MediaMaxSize {
		_ = w.Store.MarkMediaSkipped(ctx, j.ID, "media exceeds configured max size")
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
	if t, p, e := BuildMediaPreviews(ctx, obj, file.MIMEType, w.Root, sha); e == nil {
		thumb, preview = t, p
	}
	wctx, cancel := stateCtx(ctx)
	defer cancel()
	if err := w.Store.MarkMediaCompleted(wctx, j.ID, sha, st.Size(), file.MIMEType, obj, thumb, preview); err != nil {
		return err
	}
	slog.Default().Info("media worker download completed",
		slog.Int64("media_id", j.ID),
		slog.String("file_id_unique", j.FileUniqueID),
		slog.String("sha256", sha),
		slog.Int64("size", st.Size()),
		slog.Int64("duration_ms", time.Since(started).Milliseconds()),
		slog.Bool("dedup_hit", false),
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
