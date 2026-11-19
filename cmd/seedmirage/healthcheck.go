package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"

	"seedmirage/internal/config"
)

// setupHealthcheck registers `healthcheck`: GET /healthz on the local listen
// address (flag > SEEDMIRAGE_LISTEN > default), exit 0 if 200, 1 otherwise.
// No token is needed (/healthz is public) and nothing sensitive is printed.
func setupHealthcheck(fs *flag.FlagSet) func([]string, io.Writer, io.Writer) int {
	def := config.DefaultListen
	if v := os.Getenv(config.EnvListen); v != "" {
		def = v
	}
	listen := fs.String("listen", def, "UI listen address (env "+config.EnvListen+")")
	timeout := fs.Duration("timeout", 3*time.Second, "request timeout")
	return func(_ []string, _, stderr io.Writer) int {
		if err := checkHealth(context.Background(), *listen, *timeout); err != nil {
			_, _ = fmt.Fprintf(stderr, "seedmirage healthcheck: %v\n", err)
			return 1
		}
		return exitOK
	}
}

// healthURL turns a listen address into the local /healthz URL (an
// unspecified host such as 0.0.0.0 or :7070 becomes 127.0.0.1).
func healthURL(listen string) (string, error) {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "", fmt.Errorf("invalid listen address %q: %w", listen, err)
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/healthz", nil
}

func checkHealth(ctx context.Context, listen string, timeout time.Duration) error {
	u, err := healthURL(listen)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil) //nolint:gosec // local address chosen by the operator
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req) //nolint:gosec // idem: local /healthz only
	if err != nil {
		return fmt.Errorf("GET /healthz: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET /healthz: status %d", resp.StatusCode)
	}
	return nil
}
