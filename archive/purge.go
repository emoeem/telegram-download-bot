package archive

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/iyear/tdl/archive/store"
)

// Deleting is split across two layers on purpose: the store removes rows and
// reports which content-addressed objects lost their last reference; this file
// removes the bytes and the receive-number view.
//
// The order matters. Unlinking first and failing to delete the row would leave
// an archive pointing at a missing file — the worst of the three states, because
// it looks intact. Rows first means the worst case is an orphan object, which
// the GC pass already knows how to reclaim (see gc.go).

// ApplyPurge deletes the object files and the by-serial views named by a purge
// report. It never fails the caller on a missing file: the row is already gone,
// so a file that vanished earlier is the expected outcome, not an error.
func ApplyPurge(root string, rep store.PurgeReport) (removed int, bytesFreed int64) {
	for _, path := range rep.Files {
		st, err := os.Stat(path)
		if err != nil {
			continue
		}
		size := st.Size()
		if err := os.Remove(path); err != nil {
			slog.Default().Warn("purge: unlink failed", slog.String("path", path), slog.String("error", err.Error()))
			continue
		}
		removed++
		bytesFreed += size
	}
	for _, serial := range rep.Serials {
		removeSerialView(root, serial)
	}
	return removed, bytesFreed
}

// removeSerialView drops every file in the serial's bucket whose name starts
// with that number. The view has one file per number, so the prefix match is
// exact; it also covers a view whose extension differs from what this build
// would choose today (mime type refined by a later pass).
func removeSerialView(root string, serial int64) {
	if serial <= 0 {
		return
	}
	bucket := filepath.Join(root, "objects", serialViewDir, fmt.Sprintf("%03d", serial/1000))
	entries, err := os.ReadDir(bucket)
	if err != nil {
		return
	}
	prefix := fmt.Sprintf("%06d-", serial)
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), prefix) {
			continue
		}
		_ = os.Remove(filepath.Join(bucket, e.Name()))
	}
	// Leave an empty bucket alone: removing directories during a purge creates
	// spurious write errors on read-only mounts and buys nothing.
}

// SerialViewRoot explains where the view lives, for CLI output.
func SerialViewRoot(root string) string {
	return filepath.Join(root, "objects", serialViewDir)
}
