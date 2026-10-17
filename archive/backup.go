package archive

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/iyear/tdl/archive/store"
)

const (
	// DefaultMinFreeSpace pauses media downloads when free disk space drops
	// below this many bytes; message archiving is never throttled.
	DefaultMinFreeSpace int64 = 1 << 30 // 1 GiB

	backupKeep   = 7
	backupPeriod = 24 * time.Hour
	backupPrefix = "tgarchive-"
)

// FreeDiskBytes reports available bytes under the archive root.
func FreeDiskBytes(root string) (uint64, error) {
	if err := os.MkdirAll(root, 0755); err != nil {
		return 0, err
	}
	return freeDiskBytes(root)
}

// backupDir returns the snapshot directory next to the given database file.
func backupDir(dbPath string) string {
	return filepath.Join(filepath.Dir(dbPath), "backups")
}

// MaybeBackup takes a daily VACUUM INTO snapshot of the store and prunes old
// ones, keeping the newest backupKeep files. It is a no-op when the latest
// snapshot is younger than backupPeriod.
func MaybeBackup(ctx context.Context, s *store.Store) (string, error) {
	dir := backupDir(s.Path())
	entries, err := filepath.Glob(filepath.Join(dir, backupPrefix+"*.db"))
	if err != nil {
		return "", err
	}
	sort.Strings(entries) // timestamped names sort chronologically
	if len(entries) > 0 {
		last := entries[len(entries)-1]
		if fi, err := os.Stat(last); err == nil && time.Since(fi.ModTime()) < backupPeriod {
			return "", nil // fresh enough
		}
	}
	base := filepath.Join(dir, backupPrefix+time.Now().Format("20060102-150405"))
	target := base + ".db"
	for i := 1; ; i++ { // same-second collisions get a numeric suffix
		if _, err := os.Stat(target); os.IsNotExist(err) {
			break
		}
		target = fmt.Sprintf("%s-%d.db", base, i)
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", err
	}
	if _, err := s.DB().ExecContext(ctx, "VACUUM INTO ?", target); err != nil {
		return "", fmt.Errorf("vacuum into %s: %w", target, err)
	}
	entries = append(entries, target)
	sort.Strings(entries)
	for i := 0; i < len(entries)-backupKeep; i++ {
		_ = os.Remove(entries[i])
	}
	return target, nil
}

// LatestBackup returns the path of the newest snapshot, if any.
func LatestBackup(dbPath string) string {
	entries, _ := filepath.Glob(filepath.Join(backupDir(dbPath), backupPrefix+"*.db"))
	if len(entries) == 0 {
		return ""
	}
	sort.Strings(entries)
	return entries[len(entries)-1]
}
