package update

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// The "update-pending" marker lives in the data directory. The installing
// process writes "pending" just before restarting; the new process turns it
// into "started" at launch and removes it once its HTTP server is ready. A
// marker found as "started" means the previous launch never became ready.
const (
	pendingFile  = "update-pending"
	markPending  = "pending"
	markStarted  = "started"
	pendingPerms = 0o600
)

// PendingPath is the marker file of a data directory.
func PendingPath(dataDir string) string { return filepath.Join(dataDir, pendingFile) }

// MarkPending records that a restart into a new binary is about to happen.
func MarkPending(dataDir string) error {
	return os.WriteFile(PendingPath(dataDir), []byte(markPending+"\n"), pendingPerms)
}

// ClearPending removes the marker (the new process is ready).
func ClearPending(dataDir string) error {
	err := os.Remove(PendingPath(dataDir))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// Recovery is what Begin found.
type Recovery struct {
	// RolledBack: the previous launch after an update never became ready and
	// the previous binary was put back in place for the next launch.
	RolledBack bool
	// FirstLaunch: this process is the first launch after an update.
	FirstLaunch bool
}

// Begin runs at startup. Marker "pending": this is the first launch after an
// update, it becomes "started". Marker "started" with an exe+".old": the first
// launch never got ready, so the old binary is restored (once: the marker is
// removed) for the next launch. Any other state just clears a stale marker.
func Begin(dataDir, exe string) (Recovery, error) {
	b, err := os.ReadFile(PendingPath(dataDir)) //nolint:gosec // <data>/update-pending
	if errors.Is(err, os.ErrNotExist) {
		return Recovery{}, nil
	}
	if err != nil {
		return Recovery{}, err
	}
	if strings.TrimSpace(string(b)) == markPending {
		return Recovery{FirstLaunch: true}, os.WriteFile(PendingPath(dataDir), []byte(markStarted+"\n"), pendingPerms)
	}
	if err := ClearPending(dataDir); err != nil {
		return Recovery{}, err
	}
	old := exe + ".old"
	if _, err := os.Stat(old); err != nil {
		return Recovery{}, nil
	}
	if runtime.GOOS == "windows" { // the running file cannot be overwritten: move it aside first
		bad := exe + ".bad"
		_ = os.Remove(bad)
		if err := os.Rename(exe, bad); err != nil {
			return Recovery{}, fmt.Errorf("restore previous binary: %w", err)
		}
	}
	if err := os.Rename(old, exe); err != nil {
		return Recovery{}, fmt.Errorf("restore previous binary: %w", err)
	}
	return Recovery{RolledBack: true}, nil
}
