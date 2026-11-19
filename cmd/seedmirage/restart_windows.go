//go:build windows

package main

import (
	"os"
	"os/exec"
)

// execNew starts the new process; the caller then exits.
func execNew(exe string, args, env []string) error {
	cmd := exec.Command(exe, args[1:]...) //nolint:gosec // exe is the running binary's own path, just replaced after a verified download
	cmd.Env = env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Start()
}
