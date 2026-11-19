//go:build !windows

package main

import "syscall"

// execNew replaces the process image: same PID, same arguments, same environment.
func execNew(exe string, args, env []string) error {
	return syscall.Exec(exe, args, env) //nolint:gosec // exe is the running binary's own path, just replaced after a verified download
}
