// Command demo-tracker runs a local fake HTTP tracker (internal/trackertest)
// and creates a few demo .torrent files pointing at it, to try out the
// seedmirage UI without any real tracker.
//
//	go run ./scripts/demo-tracker --dir ./data/torrents
//	make run
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"seedmirage/internal/bencode"
	"seedmirage/internal/trackertest"
)

const pieceLen = 256 << 10

func main() {
	listen := flag.String("listen", "127.0.0.1:6969", "address of the fake tracker")
	dir := flag.String("dir", "data/torrents", "directory where the demo .torrent files are written (empty: create none)")
	count := flag.Int("count", 4, "number of demo .torrent files")
	interval := flag.Int("interval", 30, "announce interval returned, in seconds")
	flag.Parse()

	tr, err := trackertest.ListenHTTP(*listen)
	if err != nil {
		log.Fatalf("demo-tracker: %v", err)
	}
	defer tr.Close()

	n := 0
	tr.Respond(func(r trackertest.Request) trackertest.Response {
		n++
		// Seeders and leechers vary a little so the dashboard looks alive.
		leechers := 1 + n%7
		if n%11 == 0 {
			leechers = 0 // a swarm without leechers from time to time
		}
		log.Printf("announce #%d %s", n, r.Path)
		return trackertest.OK(*interval, 3+n%4, leechers)
	})

	if *dir != "" {
		if err := os.MkdirAll(*dir, 0o755); err != nil { //nolint:gosec // local demo directory
			log.Fatalf("demo-tracker: %v", err)
		}
		for i := 1; i <= *count; i++ {
			path := filepath.Join(*dir, fmt.Sprintf("demo-%d.torrent", i))
			if _, err := os.Stat(path); err == nil {
				continue // never overwrite
			}
			size := int64(i) * (700 << 20)
			if err := os.WriteFile(path, demoTorrent(tr.AnnounceURL(), i, size), 0o600); err != nil {
				log.Fatalf("demo-tracker: %v", err)
			}
		}
	}

	fmt.Printf("demo-tracker: %s (demo torrents in %q)\n", tr.AnnounceURL(), *dir)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()
}

// demoTorrent builds a valid .torrent (fake content, never transferred).
func demoTorrent(announce string, i int, size int64) []byte {
	pieces := make([]byte, 20*((size+pieceLen-1)/pieceLen))
	for j := range pieces {
		pieces[j] = byte(uint(i+j) & 0xff)
	}
	info := bencode.NewDict(map[string]bencode.Value{
		"name":         bencode.String(fmt.Sprintf("Sample torrent %d", i)),
		"length":       bencode.Int(size),
		"piece length": bencode.Int(pieceLen),
		"pieces":       bencode.String(pieces),
	})
	root := bencode.NewDict(map[string]bencode.Value{
		"announce": bencode.String(announce),
		"info":     info,
	})
	return bencode.Encode(root)
}
