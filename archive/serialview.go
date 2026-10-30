package archive

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/iyear/tdl/archive/store"
)

// The object store is content-addressed (objects/ab/cd/<sha256>), which is the
// right shape for dedup, integrity and garbage collection — but it is hostile to
// a human: nothing there tells you "this is the 42nd thing my account received".
//
// by-serial/ is a hard-linked view over the same bytes, named by receive number,
// so `ls objects/by-serial/000/` answers that question directly. It costs no
// extra disk (hard link, same filesystem) and it is derived data: deleting the
// whole tree is always safe, the next GC-aware pass or an explicit re-link
// rebuilds it. That property is what keeps the ledger the source of truth.
//
// Layout:
//
//	objects/by-serial/000/000042-photo.jpg
//	objects/by-serial/001/001337-video.mp4
//
// Serial numbers are 1-based, so 000 is a real bucket (1..999).

const serialViewDir = "by-serial"

// SerialViewPath is where the receive-number view of one media item lives.
func SerialViewPath(root string, serial int64, kind, mime string) string {
	if serial <= 0 {
		return ""
	}
	bucket := fmt.Sprintf("%03d", serial/1000)
	name := fmt.Sprintf("%06d-%s%s", serial, safeKind(kind), extForMIME(mime))
	return filepath.Join(root, "objects", serialViewDir, bucket, name)
}

// LinkSerialView exposes an already-stored object under its receive number.
//
// A hard link is preferred because it is free and atomic. When the archive root
// spans filesystems (or the link is refused for any other reason) it falls back
// to a copy so the view still appears; only a total failure is reported, and the
// caller treats that as non-fatal — the ledger and the content-addressed object
// remain correct either way.
func LinkSerialView(root string, serial int64, kind, mime, objPath string) (string, error) {
	if serial <= 0 || objPath == "" {
		return "", nil
	}
	dst := SerialViewPath(root, serial, kind, mime)
	if dst == "" {
		return "", nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return "", err
	}
	if st, err := os.Stat(dst); err == nil && st.Size() > 0 {
		return dst, nil // already linked
	}
	if err := os.Link(objPath, dst); err == nil {
		return dst, nil
	}
	if err := copyFile(objPath, dst); err != nil {
		return "", err
	}
	return dst, nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

// linkSerialViewBestEffort is the call site wrapper: a naming failure must never
// fail a download that already succeeded.
func linkSerialViewBestEffort(root string, serial int64, kind, mime, objPath string) {
	path, err := LinkSerialView(root, serial, kind, mime, objPath)
	if err != nil {
		slog.Default().Warn("serial view link failed",
			slog.Int64("serial", serial), slog.String("error", err.Error()))
		return
	}
	if path != "" {
		slog.Default().Debug("serial view linked",
			slog.Int64("serial", serial), slog.String("path", path))
	}
}

// safeKind keeps a media kind usable as a filename component: the kind comes
// from Telegram payloads and is normally a short token, but it is not trusted.
func safeKind(kind string) string {
	kind = strings.TrimSpace(kind)
	if kind == "" {
		return "media"
	}
	var b strings.Builder
	for _, r := range kind {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + 32)
		}
	}
	if b.Len() == 0 {
		return "media"
	}
	if b.Len() > 16 {
		return b.String()[:16]
	}
	return b.String()
}

// extForMIME gives the view a clickable extension. The content-addressed object
// has no extension at all, which is why per-file lookups there are unusable for
// a person opening things in a file manager.
func extForMIME(mime string) string {
	mime = strings.ToLower(strings.TrimSpace(strings.Split(mime, ";")[0]))
	switch mime {
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/webp":
		return ".webp"
	case "image/gif":
		return ".gif"
	case "video/mp4":
		return ".mp4"
	case "video/webm":
		return ".webm"
	case "video/quicktime":
		return ".mov"
	case "audio/mpeg":
		return ".mp3"
	case "audio/ogg":
		return ".ogg"
	case "audio/mp4":
		return ".m4a"
	case "application/pdf":
		return ".pdf"
	case "application/zip":
		return ".zip"
	}
	if strings.HasPrefix(mime, "image/") {
		return ".img"
	}
	if strings.HasPrefix(mime, "video/") {
		return ".vid"
	}
	if strings.HasPrefix(mime, "audio/") {
		return ".aud"
	}
	return ".bin"
}

// RelinkAll rebuilds the receive-number view for every completed media item.
// It is idempotent and resumable: LinkSerialView skips an existing link, and the
// pass pages by media id so an interrupted run continues where it stopped.
// Returns the number of items examined and the number of links newly created.
func RelinkAll(ctx context.Context, s *store.Store, root string, pageSize int) (examined, created int, err error) {
	var after int64
	for {
		batch, e := s.CompletedForLink(ctx, after, pageSize)
		if e != nil {
			return examined, created, e
		}
		if len(batch) == 0 {
			return examined, created, nil
		}
		for _, t := range batch {
			examined++
			after = t.MediaID
			dst := SerialViewPath(root, t.Serial, t.Kind, t.MIME)
			if dst == "" {
				continue
			}
			if st, e := os.Stat(dst); e == nil && st.Size() > 0 {
				continue
			}
			if _, e := LinkSerialView(root, t.Serial, t.Kind, t.MIME, t.Path); e != nil {
				slog.Default().Warn("relink: link failed",
					slog.Int64("serial", t.Serial), slog.String("error", e.Error()))
				continue
			}
			created++
		}
	}
}
