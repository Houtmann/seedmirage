// Command demo-tracker lance un tracker HTTP factice local (internal/trackertest)
// et crée quelques .torrent de démonstration qui le pointent, pour essayer
// l'interface de seedmirage sans aucun tracker réel.
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
	listen := flag.String("listen", "127.0.0.1:6969", "adresse du tracker factice")
	dir := flag.String("dir", "data/torrents", "dossier où écrire les .torrent de démonstration (vide : n'en crée pas)")
	count := flag.Int("count", 4, "nombre de .torrent de démonstration")
	interval := flag.Int("interval", 30, "interval d'annonce renvoyé, en secondes")
	flag.Parse()

	tr, err := trackertest.ListenHTTP(*listen)
	if err != nil {
		log.Fatalf("demo-tracker: %v", err)
	}
	defer tr.Close()

	n := 0
	tr.Respond(func(r trackertest.Request) trackertest.Response {
		n++
		// Seeders et leechers varient un peu pour que le tableau de bord vive.
		leechers := 1 + n%7
		if n%11 == 0 {
			leechers = 0 // un essaim sans leecher de temps en temps
		}
		log.Printf("announce #%d %s", n, r.Path)
		return trackertest.OK(*interval, 3+n%4, leechers)
	})

	if *dir != "" {
		if err := os.MkdirAll(*dir, 0o755); err != nil { //nolint:gosec // dossier de démonstration local
			log.Fatalf("demo-tracker: %v", err)
		}
		for i := 1; i <= *count; i++ {
			path := filepath.Join(*dir, fmt.Sprintf("demo-%d.torrent", i))
			if _, err := os.Stat(path); err == nil {
				continue // jamais d'écrasement
			}
			size := int64(i) * (700 << 20)
			if err := os.WriteFile(path, demoTorrent(tr.AnnounceURL(), i, size), 0o600); err != nil {
				log.Fatalf("demo-tracker: %v", err)
			}
		}
	}

	fmt.Printf("demo-tracker : %s (torrents de démonstration dans %q)\n", tr.AnnounceURL(), *dir)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()
}

// demoTorrent fabrique un .torrent valide (contenu fictif, jamais transféré).
func demoTorrent(announce string, i int, size int64) []byte {
	pieces := make([]byte, 20*((size+pieceLen-1)/pieceLen))
	for j := range pieces {
		pieces[j] = byte(uint(i+j) & 0xff)
	}
	info := bencode.NewDict(map[string]bencode.Value{
		"name":         bencode.String(fmt.Sprintf("Démo %d (fictif)", i)),
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
