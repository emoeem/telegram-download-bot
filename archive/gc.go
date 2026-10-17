package archive

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/iyear/tdl/archive/store"
)

// DefaultStagingMaxAge is how long an interrupted download's partial file may
// linger in staging/ before the sweeper removes it.
const DefaultStagingMaxAge = 24 * time.Hour

// GCOptions controls a garbage-collection pass.
type GCOptions struct {
	// Apply false performs a dry run: nothing is deleted, the report still lists
	// what would go.
	Apply bool
	// StagingMaxAge removes staging files older than this. Zero uses the default.
	StagingMaxAge time.Duration
	// Repair requeues media whose object file vanished from the CAS.
	Repair bool
}

// GCReport is the outcome of a pass, suitable for printing or JSON output.
type GCReport struct {
	Applied         bool          `json:"applied"`
	OrphanObjects   []string      `json:"orphan_objects"`
	OrphanBytes     int64         `json:"orphan_bytes"`
	FilesRowsPruned int           `json:"files_rows_pruned"`
	StagingRemoved  int           `json:"staging_removed"`
	RequeuedMedia   []string      `json:"requeued_media,omitempty"`
	Duration        time.Duration `json:"duration"`
}

// objectPath is the CAS location of a sha256.
func objectPath(root, sha string) string {
	return filepath.Join(root, "objects", sha[:2], sha[2:4], sha)
}

// ObjectPath exposes the CAS layout for callers that need to read an object
// (export, repair, tests).
func ObjectPath(root, sha string) string { return objectPath(root, sha) }

// CleanStaging removes partial downloads older than age. This is always safe to
// run periodically: staging only ever holds in-flight temporary files.
func CleanStaging(root string, age time.Duration) (int, error) {
	if age <= 0 {
		age = DefaultStagingMaxAge
	}
	staging := filepath.Join(root, "staging")
	entries, err := os.ReadDir(staging)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	cutoff := time.Now().Add(-age)
	removed := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		if err := os.Remove(filepath.Join(staging, e.Name())); err == nil {
			removed++
		}
	}
	return removed, nil
}

// countStaleStaging reports how many staging partials CleanStaging would remove.
func countStaleStaging(root string, age time.Duration) (int, error) {
	if age <= 0 {
		age = DefaultStagingMaxAge
	}
	entries, err := os.ReadDir(filepath.Join(root, "staging"))
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	cutoff := time.Now().Add(-age)
	n := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if info, err := e.Info(); err == nil && info.ModTime().Before(cutoff) {
			n++
		}
	}
	return n, nil
}

// GarbageCollect reclaims content-addressed objects that nothing references and
// clears stale staging files. The archive is append-only otherwise: without
// this, objects/ only ever grows.
func GarbageCollect(ctx context.Context, s *store.Store, root string, o GCOptions) (GCReport, error) {
	start := time.Now()
	rep := GCReport{Applied: o.Apply}

	orphans, err := s.OrphanFiles(ctx)
	if err != nil {
		return rep, err
	}
	for _, f := range orphans {
		size := f.Size
		if st, err := os.Stat(objectPath(root, f.SHA256)); err == nil {
			size = st.Size()
		}
		rep.OrphanObjects = append(rep.OrphanObjects, f.SHA256)
		rep.OrphanBytes += size
		if !o.Apply {
			continue
		}
		for _, p := range []string{
			objectPath(root, f.SHA256),
			filepath.Join(root, "thumbnails", f.SHA256[:2], f.SHA256[2:4], f.SHA256+"-320.jpg"),
			filepath.Join(root, "thumbnails", f.SHA256[:2], f.SHA256[2:4], f.SHA256+"-1280.jpg"),
		} {
			if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
				return rep, fmt.Errorf("remove %s: %w", p, err)
			}
		}
		if err := s.DeleteFileRow(ctx, f.SHA256); err != nil {
			return rep, err
		}
		rep.FilesRowsPruned++
	}

	if o.Repair {
		shas, err := s.CompletedMedia(ctx)
		if err != nil {
			return rep, err
		}
		for _, sha := range shas {
			if _, err := os.Stat(objectPath(root, sha)); err == nil {
				continue
			}
			rep.RequeuedMedia = append(rep.RequeuedMedia, sha)
			if !o.Apply {
				continue
			}
			if _, err := s.ResetMediaBySHA(ctx, sha); err != nil {
				return rep, err
			}
		}
	}

	if o.Apply {
		removed, err := CleanStaging(root, o.StagingMaxAge)
		if err != nil {
			return rep, err
		}
		rep.StagingRemoved = removed
	} else {
		// Dry run reports count only; nothing on disk is touched.
		removed, err := countStaleStaging(root, o.StagingMaxAge)
		if err != nil {
			return rep, err
		}
		rep.StagingRemoved = removed
	}
	rep.Duration = time.Since(start)
	return rep, nil
}
