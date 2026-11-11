package consts

import (
	"os"
	"path/filepath"
)

func init() {
	dir, err := os.UserHomeDir()
	if err != nil || dir == "" {
		// No usable $HOME. Android app processes and minimal containers do not
		// set it, so deriving paths here would create them in a location the
		// platform does not allow writing to. Leave the paths empty: the caller
		// that knows a writable directory (the gomobile bridge, passing the
		// app's private files dir) calls Rebase before it touches the archive.
		return
	}

	// Deriving the data directory from $HOME is a desktop convenience, not a
	// requirement. Android app processes inherit an unwritable HOME (typically
	// "/"), and some containers do too. A failed MkdirAll there is an expected
	// condition on those platforms, NOT a reason to abort: a panic in init runs
	// before any code can log it, and inside gomobile it surfaces only as
	// SIGABRT with no message at all. The platform entry point calls Rebase
	// explicitly with a directory it knows is writable.
	_ = Rebase(filepath.Join(dir, ".tdl"))
}

// Rebase points HomeDir and every derived path at dataDir, then creates the
// directories. On desktop it runs once from init with $HOME/.tdl; the gomobile
// bridge calls it explicitly with the Android app's private files directory,
// which is the only location the platform guarantees is writable.
//
// Directory creation happens before any assignment, so a failed Rebase leaves
// the previous values untouched instead of pointing the package at paths that
// do not exist.
//
// It is not safe to call concurrently with code that is already reading the
// paths, so the bridge calls it once, before opening any store.
func Rebase(dataDir string) error {
	extensions := filepath.Join(dataDir, "extensions")
	extensionsData := filepath.Join(extensions, "data")

	for _, p := range []string{dataDir, extensions, extensionsData} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			return err
		}
	}

	HomeDir = filepath.Dir(dataDir)
	DataDir = dataDir
	LogPath = filepath.Join(dataDir, "log")
	ExtensionsPath = extensions
	ExtensionsDataPath = extensionsData

	return nil
}
