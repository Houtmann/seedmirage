package main

import (
	"context"
	"fmt"
	"net/url"
	"os/exec"
	"runtime"
	"time"
)

// opener opens a URL in the user's browser.
type opener func(ctx context.Context, url string) error

// openCommand returns the platform command opening rawURL. The URL is always
// a separate argument: no shell is involved.
func openCommand(goos, rawURL string) (name string, args []string) {
	switch goos {
	case "darwin":
		return "open", []string{rawURL}
	case "windows":
		return "rundll32", []string{"url.dll,FileProtocolHandler", rawURL}
	}
	return "xdg-open", []string{rawURL}
}

// systemOpener launches the platform's URL handler.
func systemOpener(ctx context.Context, rawURL string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	name, args := openCommand(runtime.GOOS, rawURL)
	if err := exec.CommandContext(ctx, name, args...).Run(); err != nil { //nolint:gosec // fixed command names, URL is an argument
		return fmt.Errorf("open browser (%s): %w", name, err)
	}
	return nil
}

// uiURL builds the link to the UI at addr (host:port), with the token when set.
func uiURL(addr, token string) string {
	u := url.URL{Scheme: "http", Host: addr, Path: "/"}
	if token != "" {
		u.RawQuery = url.Values{"token": {token}}.Encode()
	}
	return u.String()
}
