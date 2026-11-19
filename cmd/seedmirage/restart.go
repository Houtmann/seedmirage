package main

import (
	"context"
	"strings"
	"sync"

	"seedmirage/internal/config"
)

// restartHook is the update.Restarter of `serve`: it does not exec anything
// itself. It records the request and cancels the run context, so the restart
// goes through the normal shutdown path (engine stop with its `stopped`
// announces, HTTP shutdown); only then, with the instance lock released, does
// serve exec the new binary.
type restartHook struct {
	cancel context.CancelFunc
	mu     sync.Mutex
	asked  bool
}

func (h *restartHook) Restart(context.Context) error {
	h.mu.Lock()
	h.asked = true
	h.mu.Unlock()
	h.cancel()
	return nil
}

func (h *restartHook) requested() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.asked
}

// execFunc replaces (Unix) or relaunches (Windows) the process with exe.
type execFunc func(exe string, args, env []string) error

// restartArgs keeps the original arguments but drops --open: the browser tab
// is already there and reloads by itself.
func restartArgs(args []string) []string {
	out := make([]string, 0, len(args))
	for i, a := range args {
		if i > 0 {
			t := strings.TrimLeft(a, "-")
			if len(a)-len(t) <= 2 && (t == "open" || strings.HasPrefix(t, "open=")) {
				continue
			}
		}
		out = append(out, a)
	}
	return out
}

// restartEnv is the environment with SEEDMIRAGE_OPEN forced off.
func restartEnv(env []string) []string {
	out := make([]string, 0, len(env)+1)
	for _, e := range env {
		if !strings.HasPrefix(e, config.EnvOpen+"=") {
			out = append(out, e)
		}
	}
	return append(out, config.EnvOpen+"=false")
}
