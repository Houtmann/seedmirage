package tracker

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/fnv"
	"math/rand/v2"
	"net"
	"net/url"
	"sync"
	"time"

	"seedmirage/internal/clock"
	"seedmirage/internal/emulation"
	"seedmirage/internal/torrent"
)

// Constantes du protocole BEP 15 (tous les entiers sont big-endian).
const (
	udpProtocolID uint64 = 0x41727101980

	actionConnect  uint32 = 0
	actionAnnounce uint32 = 1
	actionScrape   uint32 = 2
	actionError    uint32 = 3

	udpEventNone      uint32 = 0
	udpEventCompleted uint32 = 1
	udpEventStarted   uint32 = 2
	udpEventStopped   uint32 = 3

	announceReqLen  = 98
	announceRespMin = 20
	scrapeEntryLen  = 12
	// MaxScrapeHashes : nombre maximal de hashes par paquet de scrape (BEP 15).
	MaxScrapeHashes = 74

	optionURLData byte = 2 // BEP 41

	// ConnectionIDTTL : durée de validité d'un connection id (BEP 15).
	ConnectionIDTTL = 60 * time.Second
	// DefaultUDPBaseTimeout : délai de la première attente (15 × 2^n s).
	DefaultUDPBaseTimeout = 15 * time.Second
	// DefaultUDPMaxRetries : retransmissions après l'envoi initial (BEP 15 : 8).
	DefaultUDPMaxRetries = 2

	maxUDPPacket = 65535
)

// ErrTimeout : aucune réponse valide du tracker après toutes les retransmissions
// (erreur réessayable, par opposition à *FailureError).
var ErrTimeout = errors.New("tracker: délai dépassé")

// UDPOptions règle un UDPClient. Les zéros donnent les valeurs par défaut.
type UDPOptions struct {
	// Clock : horloge des retransmissions et du cache de connection id ; défaut clock.Real().
	Clock clock.Clock
	// Rand : source des transaction id ; défaut une source seedée aléatoirement.
	Rand *rand.Rand
	// BaseTimeout : attente avant la première retransmission ; défaut 15 s.
	BaseTimeout time.Duration
	// MaxRetries : retransmissions après l'envoi initial (donc 1+MaxRetries envois) ; défaut 2.
	MaxRetries int
	// Resolve remplace la résolution DNS (tests).
	Resolve func(ctx context.Context, host string) ([]net.IP, error)
}

type connEntry struct {
	id      uint64
	expires time.Time
}

// UDPClient annonce et « scrape » un tracker UDP (BEP 15, extension BEP 41).
// Une socket par appel, connection id mis en cache 60 s par tracker. Sûr en concurrence.
type UDPClient struct {
	opts UDPOptions

	rngMu sync.Mutex
	rng   *rand.Rand

	mu    sync.Mutex
	conns map[string]connEntry
}

var _ Client = (*UDPClient)(nil)

// NewUDP crée un client.
func NewUDP(opts UDPOptions) *UDPClient {
	if opts.Clock == nil {
		opts.Clock = clock.Real()
	}
	if opts.BaseTimeout <= 0 {
		opts.BaseTimeout = DefaultUDPBaseTimeout
	}
	if opts.MaxRetries <= 0 {
		opts.MaxRetries = DefaultUDPMaxRetries
	}
	rng := opts.Rand
	if rng == nil {
		rng = rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64())) // #nosec G404 -- transaction id, pas un secret
	}
	return &UDPClient{opts: opts, rng: rng, conns: make(map[string]connEntry)}
}

func (c *UDPClient) newTx() uint32 {
	c.rngMu.Lock()
	defer c.rngMu.Unlock()
	return c.rng.Uint32()
}

// Announce envoie l'annonce (connect si nécessaire, puis announce v4).
func (c *UDPClient) Announce(ctx context.Context, u *url.URL, a emulation.Announce, p emulation.AnnounceParams) (*Response, error) {
	resp, err := c.announce(ctx, u, a, p)
	if err != nil {
		return nil, fmt.Errorf("announce %s: %w", RedactURL(u), err)
	}
	return resp, nil
}

// Scrape interroge le tracker (par paquets de 74 hashes au plus).
func (c *UDPClient) Scrape(ctx context.Context, u *url.URL, hashes []torrent.InfoHash) (map[torrent.InfoHash]ScrapeStats, error) {
	out, err := c.scrape(ctx, u, hashes)
	if err != nil {
		return nil, fmt.Errorf("scrape %s: %w", RedactURL(u), err)
	}
	return out, nil
}

func (c *UDPClient) announce(ctx context.Context, u *url.URL, a emulation.Announce, p emulation.AnnounceParams) (*Response, error) {
	s, err := c.open(ctx, u)
	if err != nil {
		return nil, err
	}
	defer s.close()
	key := udpKey(a.Key)
	var resp *Response
	err = s.exchangeAuth(ctx,
		func(connID uint64, tx uint32) []byte { return buildAnnounce(connID, tx, a, p, key, u) },
		func(action uint32, pkt []byte) bool {
			r, ok := decodeAnnounce(action, pkt)
			if ok {
				resp = r
			}
			return ok
		})
	if err != nil {
		s.forget()
		return nil, err
	}
	return resp, nil
}

func (c *UDPClient) scrape(ctx context.Context, u *url.URL, hashes []torrent.InfoHash) (map[torrent.InfoHash]ScrapeStats, error) {
	out := make(map[torrent.InfoHash]ScrapeStats, len(hashes))
	if len(hashes) == 0 {
		return out, nil
	}
	s, err := c.open(ctx, u)
	if err != nil {
		return nil, err
	}
	defer s.close()
	for start := 0; start < len(hashes); start += MaxScrapeHashes {
		end := min(start+MaxScrapeHashes, len(hashes))
		batch := hashes[start:end]
		var stats []ScrapeStats
		err := s.exchangeAuth(ctx,
			func(connID uint64, tx uint32) []byte { return buildScrape(connID, tx, batch) },
			func(action uint32, pkt []byte) bool {
				st, ok := decodeScrape(action, pkt, len(batch))
				if ok {
					stats = st
				}
				return ok
			})
		if err != nil {
			s.forget()
			return nil, err
		}
		for i, st := range stats {
			out[batch[i]] = st
		}
	}
	return out, nil
}

// udpKey convertit la key de l'annonce : hexadécimal sur 8 caractères → uint32,
// sinon FNV-32a de la chaîne.
func udpKey(k string) uint32 {
	if len(k) == 8 {
		if b, err := hex.DecodeString(k); err == nil {
			return binary.BigEndian.Uint32(b)
		}
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(k))
	return h.Sum32()
}

func udpEvent(e emulation.Event) uint32 {
	switch e {
	case emulation.EventCompleted:
		return udpEventCompleted
	case emulation.EventStarted:
		return udpEventStarted
	case emulation.EventStopped:
		return udpEventStopped
	default:
		return udpEventNone
	}
}

func buildConnect(tx uint32) []byte {
	b := make([]byte, 16)
	binary.BigEndian.PutUint64(b[0:], udpProtocolID)
	binary.BigEndian.PutUint32(b[8:], actionConnect)
	binary.BigEndian.PutUint32(b[12:], tx)
	return b
}

func buildAnnounce(connID uint64, tx uint32, a emulation.Announce, p emulation.AnnounceParams, key uint32, u *url.URL) []byte {
	b := make([]byte, announceReqLen, announceReqLen+32)
	binary.BigEndian.PutUint64(b[0:], connID)
	binary.BigEndian.PutUint32(b[8:], actionAnnounce)
	binary.BigEndian.PutUint32(b[12:], tx)
	copy(b[16:36], p.InfoHash[:])
	copy(b[36:56], a.PeerID) // 20 octets bruts
	binary.BigEndian.PutUint64(b[56:], uint64(max(p.Downloaded, 0)))
	binary.BigEndian.PutUint64(b[64:], uint64(max(p.Left, 0)))
	binary.BigEndian.PutUint64(b[72:], uint64(max(p.Uploaded, 0)))
	binary.BigEndian.PutUint32(b[80:], udpEvent(p.Event))
	// b[84:88] : adresse IP, 0 = celle de l'expéditeur.
	binary.BigEndian.PutUint32(b[88:], key)
	numwant := uint32(0xFFFFFFFF) // -1 : défaut du tracker
	if a.Numwant >= 0 {           // 0 compris : Transmission envoie num_want = 0 pour « stopped » (capturé)
		numwant = uint32(min(a.Numwant, 1<<30)) // #nosec G115 -- borné
	}
	binary.BigEndian.PutUint32(b[92:], numwant)
	binary.BigEndian.PutUint16(b[96:], uint16(max(min(p.Port, 65535), 0))) // #nosec G115 -- borné
	if !a.UDPURLData {
		return b
	}
	return appendURLData(b, u)
}

// appendURLData ajoute l'option BEP 41 URLData (chemin + query de l'URL, découpée
// en blocs de 255 octets) quand l'URL a un chemin autre que « / ».
func appendURLData(b []byte, u *url.URL) []byte {
	ru := u.RequestURI()
	if ru == "" || ru == "/" {
		return b
	}
	for data := []byte(ru); len(data) > 0; {
		n := min(len(data), 255)
		b = append(b, optionURLData, byte(n)) // #nosec G115 -- n <= 255
		b = append(b, data[:n]...)
		data = data[n:]
	}
	return b
}

func buildScrape(connID uint64, tx uint32, hashes []torrent.InfoHash) []byte {
	b := make([]byte, 16, 16+20*len(hashes))
	binary.BigEndian.PutUint64(b[0:], connID)
	binary.BigEndian.PutUint32(b[8:], actionScrape)
	binary.BigEndian.PutUint32(b[12:], tx)
	for _, h := range hashes {
		b = append(b, h[:]...)
	}
	return b
}

// decodeHeader lit action et transaction id ; ok=false si le paquet est trop court.
func decodeHeader(pkt []byte) (action, tx uint32, ok bool) {
	if len(pkt) < 8 {
		return 0, 0, false
	}
	return binary.BigEndian.Uint32(pkt[0:]), binary.BigEndian.Uint32(pkt[4:]), true
}

func decodeConnect(action uint32, pkt []byte) (uint64, bool) {
	if action != actionConnect || len(pkt) < 16 {
		return 0, false
	}
	return binary.BigEndian.Uint64(pkt[8:]), true
}

func decodeAnnounce(action uint32, pkt []byte) (*Response, bool) {
	if action != actionAnnounce || len(pkt) < announceRespMin {
		return nil, false
	}
	return &Response{
		Interval: time.Duration(binary.BigEndian.Uint32(pkt[8:])) * time.Second,
		Leechers: int(binary.BigEndian.Uint32(pkt[12:])),
		Seeders:  int(binary.BigEndian.Uint32(pkt[16:])),
	}, true
}

// decodeScrape attend au moins n entrées de 12 octets (seeders, completed,
// leechers) ; l'excédent est ignoré, un paquet plus court est rejeté.
func decodeScrape(action uint32, pkt []byte, n int) ([]ScrapeStats, bool) {
	if action != actionScrape || len(pkt) < 8+scrapeEntryLen*n {
		return nil, false
	}
	out := make([]ScrapeStats, n)
	for i := range out {
		o := 8 + i*scrapeEntryLen
		out[i] = ScrapeStats{
			Seeders:    int(binary.BigEndian.Uint32(pkt[o:])),
			Downloaded: int(binary.BigEndian.Uint32(pkt[o+4:])),
			Leechers:   int(binary.BigEndian.Uint32(pkt[o+8:])),
		}
	}
	return out, true
}

func decodeError(pkt []byte) *FailureError {
	reason := ""
	if len(pkt) > 8 {
		reason = string(pkt[8:])
	}
	if len(reason) > maxFailureReason {
		reason = reason[:maxFailureReason]
	}
	return &FailureError{Reason: reason}
}

// udpSession : une socket connectée vers un tracker, pour la durée d'un appel.
type udpSession struct {
	c    *UDPClient
	addr string // clé du cache : host:port de l'URL
	conn net.Conn
	pkts chan []byte
	done chan struct{}
	wg   sync.WaitGroup
}

func (c *UDPClient) open(ctx context.Context, u *url.URL) (*udpSession, error) {
	host := u.Hostname()
	port := u.Port()
	if host == "" || port == "" {
		return nil, fmt.Errorf("%w: hôte ou port manquant", ErrUnsupportedScheme)
	}
	ip, err := c.resolve(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("résolution de %s: %w", host, err)
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "udp", net.JoinHostPort(ip.String(), port))
	if err != nil {
		return nil, fmt.Errorf("udp: %w", err)
	}
	s := &udpSession{
		c:    c,
		addr: net.JoinHostPort(host, port),
		conn: conn,
		pkts: make(chan []byte, 16),
		done: make(chan struct{}),
	}
	s.wg.Add(1)
	go s.readLoop()
	return s, nil
}

// resolve renvoie une adresse IP du tracker, IPv4 de préférence.
func (c *UDPClient) resolve(ctx context.Context, host string) (net.IP, error) {
	if ip := net.ParseIP(host); ip != nil {
		return ip, nil
	}
	var ips []net.IP
	var err error
	if c.opts.Resolve != nil {
		ips, err = c.opts.Resolve(ctx, host)
	} else {
		var addrs []net.IPAddr
		addrs, err = net.DefaultResolver.LookupIPAddr(ctx, host)
		for _, a := range addrs {
			ips = append(ips, a.IP)
		}
	}
	if err != nil {
		return nil, err
	}
	if len(ips) == 0 {
		return nil, errors.New("aucune adresse")
	}
	for _, ip := range ips {
		if ip.To4() != nil {
			return ip, nil
		}
	}
	return ips[0], nil
}

// readLoop lit la socket jusqu'à sa fermeture (close()) et transmet des copies.
func (s *udpSession) readLoop() {
	defer s.wg.Done()
	buf := make([]byte, maxUDPPacket)
	for {
		n, err := s.conn.Read(buf)
		if err != nil {
			return
		}
		pkt := append([]byte(nil), buf[:n]...)
		select {
		case s.pkts <- pkt:
		case <-s.done:
			return
		default: // file pleine : paquet perdu, comme sur le réseau
		}
	}
}

// close ferme la socket (ce qui interrompt la lecture) et attend la goroutine.
func (s *udpSession) close() {
	close(s.done)
	_ = s.conn.Close()
	s.wg.Wait()
}

// forget oublie le connection id de ce tracker (après un échec).
func (s *udpSession) forget() {
	s.c.mu.Lock()
	delete(s.c.conns, s.addr)
	s.c.mu.Unlock()
}

// connectionID renvoie le connection id en cache s'il a moins de 60 s, sinon en négocie un.
func (s *udpSession) connectionID(ctx context.Context) (uint64, error) {
	now := s.c.opts.Clock.Now()
	s.c.mu.Lock()
	e, ok := s.c.conns[s.addr]
	s.c.mu.Unlock()
	if ok && now.Before(e.expires) {
		return e.id, nil
	}
	var id uint64
	err := s.exchange(ctx, buildConnect, func(action uint32, pkt []byte) bool {
		v, ok := decodeConnect(action, pkt)
		if ok {
			id = v
		}
		return ok
	})
	if err != nil {
		return 0, fmt.Errorf("connect: %w", err)
	}
	s.c.mu.Lock()
	s.c.conns[s.addr] = connEntry{id: id, expires: s.c.opts.Clock.Now().Add(ConnectionIDTTL)}
	s.c.mu.Unlock()
	return id, nil
}

// exchange envoie la requête construite avec un transaction id aléatoire et attend
// une réponse valide (voir run).
func (s *udpSession) exchange(ctx context.Context, build func(tx uint32) []byte, decode func(action uint32, pkt []byte) bool) error {
	tx := s.c.newTx()
	req := build(tx)
	return s.run(ctx, func(context.Context) ([]byte, uint32, error) { return req, tx, nil }, decode)
}

// exchangeAuth est exchange pour les requêtes qui portent un connection id
// (announce, scrape) : avant chaque envoi et retransmission, un id expiré est
// renégocié (connect) et le paquet reconstruit avec un nouveau transaction id,
// car un id de plus de 60 s serait refusé par le tracker.
func (s *udpSession) exchangeAuth(ctx context.Context, build func(connID uint64, tx uint32) []byte, decode func(action uint32, pkt []byte) bool) error {
	var (
		req  []byte
		tx   uint32
		last uint64
		have bool
	)
	return s.run(ctx, func(ctx context.Context) ([]byte, uint32, error) {
		id, err := s.connectionID(ctx)
		if err != nil {
			return nil, 0, err
		}
		if !have || id != last {
			last, have = id, true
			tx = s.c.newTx()
			req = build(id, tx)
		}
		return req, tx, nil
	}, decode)
}

// run envoie le paquet fourni par prepare puis attend une réponse valide.
// Réponses trop courtes, d'un autre transaction id ou mal formées : ignorées.
// Action 3 : *FailureError. Sans réponse, retransmission après 15 × 2^n s
// (n = 0..MaxRetries) puis ErrTimeout. decode renvoie true si la réponse est acceptée.
func (s *udpSession) run(ctx context.Context, prepare func(context.Context) ([]byte, uint32, error), decode func(action uint32, pkt []byte) bool) error {
	clk := s.c.opts.Clock
	for attempt := 0; attempt <= s.c.opts.MaxRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		req, tx, err := prepare(ctx)
		if err != nil {
			return err
		}
		if _, err := s.conn.Write(req); err != nil {
			return fmt.Errorf("udp: %w", err)
		}
		timer := clk.NewTimer(s.c.opts.BaseTimeout << attempt)
	wait:
		for {
			select {
			case pkt := <-s.pkts:
				action, rtx, ok := decodeHeader(pkt)
				if !ok || rtx != tx {
					continue
				}
				if action == actionError {
					timer.Stop()
					return decodeError(pkt)
				}
				if decode(action, pkt) {
					timer.Stop()
					return nil
				}
			case <-timer.C():
				break wait
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			}
		}
	}
	return ErrTimeout
}
