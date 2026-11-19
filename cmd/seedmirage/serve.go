package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	"seedmirage/internal/config"
	"seedmirage/internal/datadir"
	"seedmirage/internal/engine"
	"seedmirage/internal/instance"
	"seedmirage/internal/logring"
	"seedmirage/internal/web"
)

// statusLogs is the number of warn+ records kept for /status.
const statusLogs = 100

// setupServe registers the startup flags of `serve` (flag > env > default).
func setupServe(fs *flag.FlagSet) func([]string, io.Writer, io.Writer) int {
	st := config.RegisterStartupFlags(fs, os.Getenv)
	return func(_ []string, _, stderr io.Writer) int {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return serveWith(ctx, *st, stderr, systemOpener)
	}
}

func newLogger(w io.Writer, st config.Startup) (*slog.Logger, error) {
	var level slog.Level
	if err := level.UnmarshalText([]byte(st.LogLevel)); err != nil {
		return nil, fmt.Errorf("invalid log level %q", st.LogLevel)
	}
	opts := &slog.HandlerOptions{Level: level}
	switch st.LogFormat {
	case "text":
		return slog.New(slog.NewTextHandler(w, opts)), nil
	case "json":
		return slog.New(slog.NewJSONHandler(w, opts)), nil
	}
	return nil, fmt.Errorf("invalid log format %q (text|json)", st.LogFormat)
}

// serve runs the UI until ctx is cancelled (SIGINT/SIGTERM in production).
func serve(ctx context.Context, st config.Startup, stderr io.Writer) int {
	return serveWith(ctx, st, stderr, systemOpener)
}

// uiToken is the token of the UI ("" when auth is off or unknown).
func uiToken(st config.Startup, tokenPath string) string {
	switch {
	case st.NoAuth:
		return ""
	case st.UIToken != "":
		return st.UIToken
	}
	b, err := os.ReadFile(tokenPath) //nolint:gosec // <data>/ui-token
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// serveWith is serve with an injectable browser opener (tests never launch a browser).
func serveWith(ctx context.Context, st config.Startup, stderr io.Writer, open opener) int {
	logger, err := newLogger(stderr, st)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "seedmirage serve: %v\n", err)
		return exitUsage
	}
	// The last warn+ records are kept in memory for the /status page.
	ring := logring.New(statusLogs)
	logger = slog.New(logring.NewHandler(logger.Handler(), ring))
	layout, err := datadir.Ensure(st.DataDir)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "seedmirage serve: %v\n", err)
		return exitUsage
	}
	lock, err := instance.Acquire(layout.Lock, instance.Info{PID: os.Getpid(), Listen: st.Listen})
	if errors.Is(err, instance.ErrLocked) {
		return alreadyRunning(ctx, st, layout, stderr, open)
	}
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "seedmirage serve: %v\n", err)
		return 1
	}
	defer func() { _ = lock.Release() }()
	cfg, err := config.Load(layout.Config)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "seedmirage serve: %v\n", err)
		return exitUsage
	}
	eng, err := engine.New(engine.Options{
		Config: cfg, TorrentsDir: layout.Torrents, ArchiveDir: layout.Archived, ClientsDir: layout.Clients, StatePath: layout.State,
		Logger: logger,
	})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "seedmirage serve: %v\n", err)
		return exitUsage
	}
	var wg sync.WaitGroup
	onReady := func(addr string) {
		if err := lock.Update(instance.Info{PID: os.Getpid(), Listen: addr}); err != nil {
			logger.Warn("instance lock", "err", err)
		}
		if !st.Open {
			return
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := open(ctx, uiURL(addr, uiToken(st, layout.UIToken))); err != nil {
				logger.Warn("could not open the browser", "err", err)
			}
		}()
	}
	srv, err := web.New(st, web.Deps{Logger: logger, TokenPath: layout.UIToken, Version: version, Engine: eng, ConfigPath: layout.Config, OnReady: onReady, Logs: ring})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "seedmirage serve: %v\n", err)
		return exitUsage
	}
	if cfg.AutoStart {
		if err := eng.Start(ctx); err != nil {
			_, _ = fmt.Fprintf(stderr, "seedmirage serve: %v\n", err)
			return 1
		}
	} else {
		logger.Info("autoStart disabled: seeding is not started")
	}

	// The engine stops (one "stopped" per torrent, 10 s at most) as soon as
	// shutdown begins, in parallel with the UI's own graceful shutdown.
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-runCtx.Done()
		if err := eng.Stop(context.WithoutCancel(runCtx)); err != nil {
			logger.Warn("engine stop", "err", err)
		}
	}()
	code := exitOK
	if err := srv.Run(runCtx); err != nil {
		_, _ = fmt.Fprintf(stderr, "seedmirage serve: %v\n", err)
		code = 1
	}
	cancelRun()
	wg.Wait()
	return code
}

// alreadyRunning handles a second launch: it opens the first instance's UI
// (when --open) and exits 0 without starting an engine.
func alreadyRunning(ctx context.Context, st config.Startup, layout datadir.Layout, stderr io.Writer, open opener) int {
	info, err := instance.Read(layout.Lock)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "seedmirage serve: already running, but its address is unknown: %v\n", err)
		return 1
	}
	_, _ = fmt.Fprintf(stderr, "seedmirage is already running (pid %d): %s\n", info.PID, uiURL(info.Listen, ""))
	if st.Open {
		if err := open(ctx, uiURL(info.Listen, uiToken(st, layout.UIToken))); err != nil {
			_, _ = fmt.Fprintf(stderr, "seedmirage serve: %v\n", err)
		}
	}
	return exitOK
}
