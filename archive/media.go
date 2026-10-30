package archive

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/gotd/td/telegram/downloader"
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
	runJob := func(fn func() error) func() error {
		return func() error {
			if err := fn(); err != nil {
				slog.Default().Warn("media job ended with an error",
					slog.String("error", err.Error()))
			}
			return nil
		}
	}
	for _, chatID := range order {
		group := byChat[chatID]
		located := w.locateFiles(gctx, chatID, group)
		for _, job := range group {
			j := job
			lm, ok := located[j.MessageID]
			g.Go(runJob(func() error { return w.process(gctx, j, lm, ok) }))
		}
	}
	for _, job := range links {
		j := job
		g.Go(runJob(func() error { return w.processLink(gctx, j) }))
	}
	for _, job := range tmeJobs {
		j := job
		g.Go(runJob(func() error { return w.processTMe(gctx, j) }))
	}
	return g.Wait()
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

// locateFiles resolves the downloadable file of many messages of one chat with a
// single history walk. Message ids that the walk cannot confirm within
// historyScanCap are simply absent from the result; the caller then falls back
// to the per-message lookup.
func (w *MediaWorker) locateFiles(ctx context.Context, chatID int64, jobs []store.MediaJob) map[int]locatedMedia {
	out := make(map[int]locatedMedia, len(jobs))
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
		// The deadline is our own per-job budget, not a server rate limit:
		// pausing the whole queue for 10 minutes turned one slow file into a
		// stalled queue. Record it and stay on the normal retry ladder.
		next := time.Now().Add(retryDelay(kind, attempt))
		_ = w.Store.MarkMediaFailed(wctx, j.ID, err.Error(), kind, detail, attempt, next)
		logMediaFailure(j.ID, kind, detail, attempt, &next)
		return err
	}
	if permanent || kind == "not_found" || kind == "permission" || kind == "disk_full" || attempt >= mediaMaxAttempts {
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

// thumbLocation returns the download location of the largest thumbnail a
// message's document carries, or nil when it has none.
//
// Photos are excluded on purpose: a photo's main file *is* the image, and
// BuildMediaPreviews already derives both variants from it. Only formats whose
// bytes are not an image need a poster pulled out of the thumbnail list.
func thumbLocation(notEmpty tg.NotEmptyMessage) tg.InputFileLocationClass {
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
	if !ok {
		return nil
	}
	d, ok := doc.Document.(*tg.Document)
	if !ok {
		return nil
	}
	thumbs, has := d.GetThumbs()
	if !has || len(thumbs) == 0 {
		return nil
	}
	best, bestArea := "", 0
	for _, th := range thumbs {
		sized, ok := th.AsNotEmpty()
		if !ok {
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
	loc := d.AsInputDocumentFileLocation()
	loc.ThumbSize = best
	return loc
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

func (w *MediaWorker) process(ctx context.Context, j store.MediaJob, located locatedMedia, locatedOK bool) error {
	ctx, cancel := context.WithTimeout(ctx, mediaTimeoutFor(j.Size))
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
		p, err := ResolveChatID(ctx, w.Runtime, j.ChatID)
		if err != nil {
			return w.fail(ctx, j, err, false)
		}
		it := query.Messages(w.Runtime.API).GetHistory(p.InputPeer()).BatchSize(20).OffsetID(j.MessageID + 1).Iter()
		for it.Next(ctx) {
			if it.Value().Msg.GetID() != j.MessageID {
				continue
			}
			if f, ok := it.Value().File(); ok {
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
	defer os.Remove(tmp)
	fmt.Fprintf(os.Stderr, "archive media: downloading job=%d chat=%d msg=%d type=%s\n", j.ID, j.ChatID, j.MessageID, j.Type)
	if _, err := downloader.NewDownloader().WithPartSize(1024*1024).Download(w.Runtime.API, file.Location).WithThreads(2).ToPath(ctx, tmp); err != nil {
		return w.fail(ctx, j, err, false)
	}
	if err := w.ingestObject(ctx, j, tmp, file.MIMEType, t.MediaMaxSize, lm.Thumb); err != nil {
		return w.fail(ctx, j, err, false)
	}
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
