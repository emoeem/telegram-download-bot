package archive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// External video download is delegated to yt-dlp on purpose.
//
// Writing extractors for YouTube, Bilibili, X, TikTok and the rest is not a
// wheel worth rebuilding: each site breaks its own player several times a year
// and yt-dlp absorbs that maintenance. This file is therefore an argv contract
// and a result parser, nothing more — if a site stops working, the fix belongs
// upstream, in the binary, not here.
//
// One deliberate consequence of the environment: ffmpeg is not assumed to
// exist. Video+audio are muxed by ffmpeg on YouTube's DASH streams, so asking
// for the "best" format would produce a file yt-dlp cannot finish. The format
// selector below prefers a single progressive stream and only merges when
// ffmpeg is actually usable.

// ErrVideoToolMissing reports that the external downloader is not installed.
// It is distinct from a download failure: the job is parked rather than
// retried, because retrying cannot install a binary.
var ErrVideoToolMissing = errors.New("yt-dlp not found in PATH")

// ErrVideoUnsupported marks a URL the downloader refuses. Retrying is
// pointless: the site is either unsupported or requires an account.
var ErrVideoUnsupported = errors.New("video URL not supported by downloader")

// ErrVideoTooLarge reports a file the size cap excluded. It is a policy limit,
// not a property of the URL: raising --max-filesize for the source makes the
// same job succeed, so it must not be recorded as permanently unsupported.
var ErrVideoTooLarge = errors.New("video exceeds the configured size limit")

// VideoInfo is the subset of yt-dlp's metadata that the archive actually uses.
type VideoInfo struct {
	ID          string  `json:"id"`
	Title       string  `json:"title"`
	Uploader    string  `json:"uploader"`
	Extractor   string  `json:"extractor_key"`
	Description string  `json:"description"`
	Thumbnail   string  `json:"thumbnail"`
	Ext         string  `json:"ext"`
	WebpageURL  string  `json:"webpage_url"`
	Duration    float64 `json:"duration"`
	Width       int     `json:"width"`
	Height      int     `json:"height"`
	Filesize    int64   `json:"filesize"`
	ApproxSize  int64   `json:"filesize_approx"`
}

// VideoFetcher shells out to yt-dlp.
type VideoFetcher struct {
	// Binary is resolved from PATH when empty.
	Binary  string
	Timeout time.Duration
	// MaxSize skips anything larger, in bytes; 0 disables the cap.
	MaxSize int64
	// HasFFmpeg enables merging separate video/audio streams. It is probed once
	// rather than assumed: a broken ffmpeg makes the merge path fail *after* a
	// full download, which is the most expensive possible way to find out.
	HasFFmpeg bool
}

// NewVideoFetcher returns a fetcher with archive-appropriate defaults.
func NewVideoFetcher() *VideoFetcher {
	return &VideoFetcher{
		Binary:    "yt-dlp",
		Timeout:   15 * time.Minute,
		HasFFmpeg: ffmpegUsable(),
	}
}

// ffmpegUsable reports whether a working ffmpeg is on PATH. Probed by running
// it: a binary that exists but cannot load its shared libraries (a real state
// on this machine) is worse than a missing one, because it changes yt-dlp's
// behaviour while failing at runtime.
func ffmpegUsable() bool {
	path, err := exec.LookPath("ffmpeg")
	if err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, path, "-version").Run() == nil
}

// Available reports whether the downloader can run at all.
func (f *VideoFetcher) Available() bool {
	bin := f.binary()
	if bin == "" {
		return false
	}
	_, err := exec.LookPath(bin)
	return err == nil
}

func (f *VideoFetcher) binary() string {
	if f.Binary == "" {
		return "yt-dlp"
	}
	return f.Binary
}

func (f *VideoFetcher) timeout() time.Duration {
	if f.Timeout <= 0 {
		return 15 * time.Minute
	}
	return f.Timeout
}

// baseArgs are the flags every invocation shares. --no-playlist is the
// important one: a channel or playlist URL would otherwise pull hundreds of
// videos from a single archived message.
func (f *VideoFetcher) baseArgs() []string {
	return []string{
		"--no-playlist",
		"--no-progress",
		"--no-warnings",
		// "--no-call-home" used to sit here. yt-dlp removed the option, and a
		// removed option is not ignored: it exits non-zero with
		// "Deprecated Feature: ...", which made every link job fail while the
		// extractor itself was perfectly healthy. Anything added here must be
		// re-checked against the installed yt-dlp, not against memory.
		"--socket-timeout", "20",
	}
}

// Probe reads metadata without downloading. It is what turns a bare URL into a
// material with a title, a duration and a thumbnail.
func (f *VideoFetcher) Probe(ctx context.Context, rawURL string) (*VideoInfo, error) {
	if !f.Available() {
		return nil, ErrVideoToolMissing
	}
	args := append(f.baseArgs(), "-J", "--", rawURL)
	out, err := f.run(ctx, args...)
	if err != nil {
		return nil, err
	}
	var info VideoInfo
	// -J prints one JSON object; decoding from the stream avoids buffering a
	// multi-megabyte formats array twice.
	if err := json.Unmarshal(out, &info); err != nil {
		return nil, fmt.Errorf("parse yt-dlp metadata: %w", err)
	}
	if info.WebpageURL == "" {
		info.WebpageURL = rawURL
	}
	return &info, nil
}

// VideoDownload is the outcome of a successful fetch.
type VideoDownload struct {
	Path      string
	Title     string
	Duration  int
	Width     int
	Height    int
	MIME      string
	Thumbnail []byte // best-effort cover art; nil when none could be fetched
}

// Download fetches the video into dir and returns its real path. yt-dlp picks
// the extension, so the path is read back from its output rather than guessed.
func (f *VideoFetcher) Download(ctx context.Context, rawURL, dir string) (*VideoDownload, error) {
	if !f.Available() {
		return nil, ErrVideoToolMissing
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	// A stable stem keeps retries from accumulating orphan files.
	stem := fmt.Sprintf("link-%d", time.Now().UnixNano())
	tmpl := filepath.Join(dir, stem+".%(ext)s")

	args := f.baseArgs()
	if f.MaxSize > 0 {
		// yt-dlp enforces this before writing large bodies, which is the only
		// place the cap is cheap. It must come *before* the "--" terminator:
		// everything after that is a positional argument, so appending the flag
		// later made yt-dlp read "--max-filesize" as the URL of a second video.
		args = append(args, "--max-filesize", strconv.FormatInt(f.MaxSize, 10))
	}
	args = append(args,
		"-f", f.formatSelector(),
		"--print", "after_move:filepath",
		"--print", "after_move:title",
		"-o", tmpl,
		"--", rawURL,
	)
	out, err := f.run(ctx, args...)
	if err != nil {
		return nil, err
	}
	lines := nonEmptyLines(string(out))
	if len(lines) == 0 {
		// --max-filesize makes yt-dlp skip a file with exit 0 and no output at
		// all, which is indistinguishable from a silent extractor failure unless
		// the cap is named here.
		if f.MaxSize > 0 {
			return nil, fmt.Errorf("%w: %d bytes, url %s", ErrVideoTooLarge, f.MaxSize, rawURL)
		}
		return nil, fmt.Errorf("yt-dlp produced no output for %s", rawURL)
	}
	path := lines[0]
	if _, statErr := os.Stat(path); statErr != nil {
		return nil, fmt.Errorf("yt-dlp reported %q but the file is missing: %w", path, statErr)
	}
	res := &VideoDownload{Path: path, MIME: mimeForExt(filepath.Ext(path))}
	if len(lines) > 1 {
		res.Title = lines[1]
	}
	// Metadata is a second, cheap call: the download output does not carry
	// duration or dimensions in a parseable form.
	if info, probeErr := f.Probe(ctx, rawURL); probeErr == nil {
		res.Duration = int(info.Duration)
		res.Width, res.Height = info.Width, info.Height
		if res.Title == "" {
			res.Title = info.Title
		}
		res.Thumbnail = f.fetchThumbnail(ctx, info.Thumbnail)
	}
	return res, nil
}

// formatSelector prefers one progressive file so no muxer is required, and only
// asks for separate streams when a working ffmpeg can join them.
func (f *VideoFetcher) formatSelector() string {
	if f.HasFFmpeg {
		return "bv*[ext=mp4]+ba[ext=m4a]/b[ext=mp4]/bv*+ba/b"
	}
	return "b[ext=mp4]/b"
}

// maxThumbBytes bounds cover art. A "thumbnail" is occasionally a full frame.
const maxThumbBytes = 4 << 20

// fetchThumbnail downloads the cover URL. Failure is silent: a video without a
// poster is still a usable material, and this runs inside the download path
// where an error would fail the whole job.
func (f *VideoFetcher) fetchThumbnail(ctx context.Context, url string) []byte {
	if url == "" {
		return nil
	}
	reqCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return nil
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxThumbBytes))
	if err != nil || len(body) == 0 {
		return nil
	}
	return body
}

// run executes yt-dlp and classifies its failures. The classification matters
// more than the message: a missing binary or an unsupported site must not be
// retried five times, while a network timeout should be.
func (f *VideoFetcher) run(ctx context.Context, args ...string) ([]byte, error) {
	runCtx, cancel := context.WithTimeout(ctx, f.timeout())
	defer cancel()
	cmd := exec.CommandContext(runCtx, f.binary(), args...)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return []byte(stdout.String()), nil
	}
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		return nil, fmt.Errorf("yt-dlp timed out after %s", f.timeout())
	}
	msg := strings.TrimSpace(stderr.String())
	if msg == "" {
		msg = err.Error()
	}
	switch {
	case strings.Contains(msg, "Unsupported URL"),
		strings.Contains(msg, "No video could be found"),
		strings.Contains(msg, "is not a valid URL"),
		strings.Contains(msg, "No video formats found"),
		strings.Contains(msg, "This video is unavailable"),
		strings.Contains(msg, "Private video"),
		strings.Contains(msg, "requires authentication"):
		return nil, fmt.Errorf("%w: %s", ErrVideoUnsupported, firstLine(msg))
	}
	return nil, fmt.Errorf("yt-dlp: %s", firstLine(msg))
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func nonEmptyLines(s string) []string {
	out := make([]string, 0, 2)
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out
}

// mimeForExt maps the extension yt-dlp chose onto the mime type the archive
// stores, so the browse filter and the export see a video, not an unknown blob.
func mimeForExt(ext string) string {
	switch strings.ToLower(ext) {
	case ".mp4", ".m4v":
		return "video/mp4"
	case ".webm":
		return "video/webm"
	case ".mkv":
		return "video/x-matroska"
	case ".mov":
		return "video/quicktime"
	case ".m4a":
		return "audio/mp4"
	case ".mp3":
		return "audio/mpeg"
	case ".opus":
		return "audio/opus"
	case ".flv":
		return "video/x-flv"
	}
	return "application/octet-stream"
}
