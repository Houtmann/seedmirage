// Package datadir creates and describes the data directory layout.
package datadir

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	mrand "math/rand/v2"
	"os"
	"path/filepath"

	"seedmirage/internal/config"
)

// Layout lists the paths inside the data directory.
type Layout struct {
	Root     string
	Config   string // config.json
	Clients  string // clients/
	Torrents string // torrents/
	Archived string // torrents/archived/
	State    string // state.json
	UIToken  string // ui-token
	Lock     string // .lock (single-instance lock, see internal/instance)
}

// NewLayout computes the layout for root without touching the disk.
func NewLayout(root string) Layout {
	return Layout{
		Root:     root,
		Config:   filepath.Join(root, "config.json"),
		Clients:  filepath.Join(root, "clients"),
		Torrents: filepath.Join(root, "torrents"),
		Archived: filepath.Join(root, "torrents", "archived"),
		State:    filepath.Join(root, "state.json"),
		UIToken:  filepath.Join(root, "ui-token"),
		Lock:     filepath.Join(root, ".lock"),
	}
}

// Ensure creates the directories (0700) and a default config.json (with a
// drawn port) when missing. An existing config without port gets one drawn and
// saved.
func Ensure(root string) (Layout, error) {
	var seed [16]byte
	if _, err := rand.Read(seed[:]); err != nil {
		return Layout{}, fmt.Errorf("datadir: seed: %w", err)
	}
	rng := mrand.New(mrand.NewPCG(binary.LittleEndian.Uint64(seed[:8]), binary.LittleEndian.Uint64(seed[8:]))) //nolint:gosec // port choice, not a secret
	return EnsureWith(root, rng)
}

// EnsureWith is Ensure with an injectable random source.
func EnsureWith(root string, rng *mrand.Rand) (Layout, error) {
	if root == "" {
		return Layout{}, errors.New("datadir: empty data directory")
	}
	l := NewLayout(root)
	for _, d := range []string{l.Root, l.Clients, l.Torrents, l.Archived} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return Layout{}, fmt.Errorf("datadir: create %s: %w", d, err)
		}
	}
	if err := InstallEmbeddedProfiles(l.Clients); err != nil {
		return Layout{}, err
	}
	cfg, err := config.Load(l.Config)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		cfg = config.Defaults()
		cfg.EnsurePort(rng)
		if err := cfg.Save(l.Config); err != nil {
			return Layout{}, fmt.Errorf("datadir: %w", err)
		}
	case err != nil:
		return Layout{}, fmt.Errorf("datadir: %w", err)
	default:
		if cfg.EnsurePort(rng) {
			if err := cfg.Save(l.Config); err != nil {
				return Layout{}, fmt.Errorf("datadir: %w", err)
			}
		}
	}
	return l, nil
}
