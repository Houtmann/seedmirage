// Package instance guarantees a single running seedmirage per data directory.
//
// The lock is an OS-level lock on <data>/.lock that disappears with the
// process (no stale lock after a crash). The file holds the PID and the UI
// listen address of the owner, never the access token.
package instance

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// ErrLocked means another instance already holds the lock.
var ErrLocked = errors.New("instance: another seedmirage is already running on this data directory")

// Info is what the lock file records.
type Info struct {
	PID    int
	Listen string // host:port of the UI
}

// Lock is a held instance lock.
type Lock struct {
	f *os.File
}

// Acquire takes the lock at path and records info in it. It returns ErrLocked
// when another process (or another Acquire in this process) holds it.
func Acquire(path string, info Info) (*Lock, error) {
	f, err := lockFile(path)
	if err != nil {
		return nil, err
	}
	l := &Lock{f: f}
	if err := l.Update(info); err != nil {
		_ = l.Release()
		return nil, err
	}
	return l, nil
}

// Update rewrites the recorded info (e.g. once the real listen address is known).
func (l *Lock) Update(info Info) error {
	if err := l.f.Truncate(0); err != nil {
		return fmt.Errorf("instance: %w", err)
	}
	if _, err := l.f.WriteAt([]byte(format(info)), 0); err != nil {
		return fmt.Errorf("instance: %w", err)
	}
	return nil
}

// Release drops the lock. The file is left in place: removing it would let two
// processes lock two different inodes.
func (l *Lock) Release() error {
	unlockFile(l.f)
	err := l.f.Close()
	return err
}

// Read parses the lock file of the running instance.
func Read(path string) (Info, error) {
	b, err := os.ReadFile(path) //nolint:gosec // <data>/.lock
	if err != nil {
		return Info{}, fmt.Errorf("instance: %w", err)
	}
	return parse(string(b))
}

func format(i Info) string {
	return fmt.Sprintf("pid=%d\nlisten=%s\n", i.PID, i.Listen)
}

func parse(s string) (Info, error) {
	var i Info
	for _, line := range strings.Split(s, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch k {
		case "pid":
			i.PID, _ = strconv.Atoi(v)
		case "listen":
			i.Listen = v
		}
	}
	if i.Listen == "" {
		return Info{}, errors.New("instance: lock file has no listen address")
	}
	return i, nil
}
