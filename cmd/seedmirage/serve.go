package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"seedmirage/internal/clock"
	"seedmirage/internal/config"
	"seedmirage/internal/datadir"
	"seedmirage/internal/engine"
	"seedmirage/internal/instance"
	"seedmirage/internal/logring"
	"seedmirage/internal/update"
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
	return serveCore(ctx, st, stderr, open, serveHooks{exec: execNew})
}

// updateURLEnv points the release check and the installer to a local fake
// release server (manual tests). Only loopback hosts are accepted.
const updateURLEnv = "SEEDMIRAGE_UPDATE_URL"

// localUpdateBase validates SEEDMIRAGE_UPDATE_URL: http, loopback host only.
func localUpdateBase(raw string) (base, host string, ok bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "http" || u.User != nil {
		return "", "", false
	}
	switch u.Hostname() {
	case "127.0.0.1", "localhost", "::1":
		return strings.TrimRight(u.String(), "/"), u.Host, true
	}
	return "", "", false
}

// serveHooks are the test seams of serveCore.
type serveHooks struct {
	// exec replaces the process after an installed update (tests never do).
	exec execFunc
	// exe overrides the binary an update replaces ("": the running one).
	exe string
	// clock drives the release check (nil: the system clock).
	clock clock.Clock
}

// serveCore is serveWith with injectable seams.
func serveCore(ctx context.Context, st config.Startup, stderr io.Writer, open opener, hk serveHooks) int {
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
	releaseLock := sync.OnceFunc(func() { _ = lock.Release() })
	defer releaseLock()
	exe, exeErr := hk.exe, error(nil)
	if exe == "" {
		exe, exeErr = update.ExecutablePath()
	}
	var rec update.Recovery
	if exeErr == nil {
		if rec, err = update.Begin(layout.Root, exe); err != nil {
			logger.Warn("update marker", "err", err)
		}
		if rec.RolledBack {
			logger.Warn("the last update did not start correctly: the previous binary was restored for the next launch")
		}
	}
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
		if err := update.ClearPending(layout.Root); err != nil {
			logger.Warn("update marker", "err", err)
		}
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
	// The run context ends on shutdown or when an installed update asks for a restart.
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	hook := &restartHook{cancel: cancelRun}
	// Release check: off for this run with --no-update-check, otherwise governed
	// by the updateCheck setting (applied at once). Stopped with the run context.
	var mgr *update.Manager
	if !st.NoUpdateCheck {
		ucfg := update.Config{
			Version: version, Logger: logger, Enabled: eng.UpdateCheckEnabled, Clock: hk.clock,
			OnAvailable: func(r update.Release) { eng.NotifyUpdate(r.Version) },
		}
		var src update.Source
		if raw := os.Getenv(updateURLEnv); raw != "" {
			if base, host, ok := localUpdateBase(raw); ok {
				ucfg.URL, ucfg.AllowDev = base+"/latest", true
				src = update.Source{Base: base + "/download", LocalHost: host}
			} else {
				logger.Warn(updateURLEnv + " ignored: only http on 127.0.0.1, localhost or ::1 is accepted")
			}
		}
		var chk *update.Checker
		ucfg.AfterCheck = func() { // automatic installation at the next check slot
			if eng.AutoInstallDue() {
				if err := mgr.Start(); err != nil {
					logger.Debug("automatic update not started", "err", err)
				}
			}
		}
		chk = update.New(ucfg)
		block := ""
		switch {
		case update.InDocker():
			block = update.BlockDocker
		case !chk.Supported():
			block = update.BlockDev
		case exeErr != nil || !update.DirWritable(filepath.Dir(exe)):
			block = update.BlockReadOnly
		}
		mgr = update.NewManager(update.ManagerConfig{
			Ctx: runCtx, Latest: chk.Latest, Source: src, ExePath: exe, Block: block, Restarter: hook,
			OnProgress: eng.NotifyUpdateProgress, Logger: logger,
		})
		eng.SetUpdates(chk)
		eng.SetInstaller(mgr)
		wg.Add(1)
		go func() { defer wg.Done(); chk.Run(runCtx) }()
	}
	srv, err := web.New(st, web.Deps{Logger: logger, TokenPath: layout.UIToken, Version: version, Docker: update.InDocker(), RolledBack: rec.RolledBack, Engine: eng, ConfigPath: layout.Config, OnReady: onReady, Logs: ring})
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
	if mgr != nil {
		mgr.Wait()
	}
	if hook.requested() && ctx.Err() == nil {
		// Same process path as a shutdown is done (engine stopped, listener
		// closed); free the instance lock too, or the new process would refuse to start.
		releaseLock()
		if err := update.MarkPending(layout.Root); err != nil {
			logger.Warn("update marker", "err", err)
		}
		logger.Info("restarting on the new version")
		if err := hk.exec(exe, restartArgs(os.Args), restartEnv(os.Environ())); err != nil {
			_ = update.ClearPending(layout.Root)
			_, _ = fmt.Fprintf(stderr, "seedmirage serve: restart: %v\n", err)
			return 1
		}
	}
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
