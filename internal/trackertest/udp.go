package trackertest

import (
	"encoding/binary"
	"net"
	"strconv"
	"sync"
)

// Actions BEP 15.
const (
	UDPActionConnect  uint32 = 0
	UDPActionAnnounce uint32 = 1
	UDPActionScrape   uint32 = 2
	UDPActionError    uint32 = 3
)

// UDPPacket est un datagramme reçu, tel quel.
type UDPPacket struct {
	Raw    []byte
	Action uint32 // 0xFFFFFFFF si le paquet est trop court pour porter une action
	// ConnectionID n'a de sens que pour announce et scrape (premiers 8 octets).
	ConnectionID  uint64
	TransactionID uint32
}

// UDPAnnounceReply est la réponse scriptée à une annonce.
type UDPAnnounceReply struct {
	Interval, Leechers, Seeders uint32
	// Peers : pairs compacts bruts (6 octets IPv4 ou 18 octets IPv6 chacun), optionnels.
	Peers []byte
}

// UDPScrapeEntry est une entrée de réponse de scrape.
type UDPScrapeEntry struct {
	Seeders, Completed, Leechers uint32
}

// UDPTracker est un tracker UDP factice (connect, announce, scrape, erreur).
type UDPTracker struct {
	t    TB // nil hors tests (ListenUDP)
	conn *net.UDPConn

	mu         sync.Mutex
	cond       *sync.Cond // signalé à chaque paquet reçu
	pkts       []UDPPacket
	drop       int
	errMsg     string
	hasErr     bool
	nextConnID uint64
	validConns map[uint64]bool
	announce   UDPAnnounceReply
	scrape     map[[20]byte]UDPScrapeEntry
	closed     bool
	onPacket   func(UDPPacket)

	wg sync.WaitGroup
}

// NewUDP démarre un tracker sur 127.0.0.1 (port libre) arrêté par t.Cleanup.
func NewUDP(t TB) *UDPTracker {
	t.Helper()
	u, err := ListenUDP("127.0.0.1:0")
	if err != nil {
		t.Fatalf("trackertest: listen udp: %v", err)
		return nil
	}
	u.t = t
	t.Cleanup(u.Close)
	return u
}

// ListenUDP démarre un tracker UDP sur addr (mode capture, hors tests) ; l'appelant
// doit appeler Close.
func ListenUDP(addr string) (*UDPTracker, error) {
	ua, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, err
	}
	conn, err := net.ListenUDP("udp", ua)
	if err != nil {
		return nil, err
	}
	u := &UDPTracker{
		conn:       conn,
		nextConnID: 0x1122334455667700,
		validConns: make(map[uint64]bool),
		announce:   UDPAnnounceReply{Interval: 1800, Leechers: 5, Seeders: 10},
		scrape:     make(map[[20]byte]UDPScrapeEntry),
	}
	u.cond = sync.NewCond(&u.mu)
	u.wg.Add(1)
	go u.loop()
	return u, nil
}

// OnPacket enregistre un rappel appelé pour chaque datagramme reçu, dans
// l'ordre, le verrou du tracker tenu : il ne doit pas rappeler le tracker.
func (u *UDPTracker) OnPacket(fn func(UDPPacket)) {
	u.mu.Lock()
	u.onPacket = fn
	u.mu.Unlock()
}

// Addr renvoie « 127.0.0.1:port ».
func (u *UDPTracker) Addr() string { return u.conn.LocalAddr().String() }

// URL renvoie udp://127.0.0.1:port suivi de path (ex. « /announce »).
func (u *UDPTracker) URL(path string) string {
	_, port, _ := net.SplitHostPort(u.Addr())
	if _, err := strconv.Atoi(port); err != nil {
		u.t.Fatalf("trackertest: port udp invalide %q", port)
	}
	return "udp://127.0.0.1:" + port + path
}

// Close arrête le tracker et attend sa goroutine. Idempotent.
func (u *UDPTracker) Close() {
	u.mu.Lock()
	if u.closed {
		u.mu.Unlock()
		return
	}
	u.closed = true
	u.cond.Broadcast()
	u.mu.Unlock()
	_ = u.conn.Close()
	u.wg.Wait()
}

// Drop ignore (sans répondre) les n prochains paquets reçus, tous types confondus.
func (u *UDPTracker) Drop(n int) {
	u.mu.Lock()
	u.drop = n
	u.mu.Unlock()
}

// FailWith répond désormais par une erreur (action 3) à tout connect, announce et scrape.
// Un message vide rétablit les réponses normales.
func (u *UDPTracker) FailWith(msg string) {
	u.mu.Lock()
	u.errMsg, u.hasErr = msg, msg != ""
	u.mu.Unlock()
}

// SetAnnounce règle la réponse aux annonces.
func (u *UDPTracker) SetAnnounce(r UDPAnnounceReply) {
	u.mu.Lock()
	u.announce = r
	u.mu.Unlock()
}

// SetScrape règle les statistiques renvoyées pour un info_hash (inconnu : zéros).
func (u *UDPTracker) SetScrape(h [20]byte, e UDPScrapeEntry) {
	u.mu.Lock()
	u.scrape[h] = e
	u.mu.Unlock()
}

// Packets renvoie une copie des paquets reçus, dans l'ordre.
func (u *UDPTracker) Packets() []UDPPacket {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]UDPPacket(nil), u.pkts...)
}

// WaitPackets bloque jusqu'à ce que n paquets au moins aient été reçus
// (condition, sans attente active).
func (u *UDPTracker) WaitPackets(n int) []UDPPacket {
	u.mu.Lock()
	defer u.mu.Unlock()
	for len(u.pkts) < n && !u.closed {
		u.cond.Wait()
	}
	return append([]UDPPacket(nil), u.pkts...)
}

// ConnectionIDs renvoie les connection id émis, dans l'ordre.
func (u *UDPTracker) ConnectionIDs() []uint64 {
	u.mu.Lock()
	defer u.mu.Unlock()
	ids := make([]uint64, 0, len(u.validConns))
	for id := uint64(0x1122334455667700); id < u.nextConnID; id++ {
		ids = append(ids, id)
	}
	return ids
}

func (u *UDPTracker) loop() {
	defer u.wg.Done()
	buf := make([]byte, 65535)
	for {
		n, addr, err := u.conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		raw := append([]byte(nil), buf[:n]...)
		if reply := u.handle(raw); reply != nil {
			_, _ = u.conn.WriteToUDP(reply, addr)
		}
	}
}

func parseUDPPacket(raw []byte) UDPPacket {
	p := UDPPacket{Raw: raw, Action: 0xFFFFFFFF}
	if len(raw) >= 16 {
		p.ConnectionID = binary.BigEndian.Uint64(raw[0:])
		p.Action = binary.BigEndian.Uint32(raw[8:])
		p.TransactionID = binary.BigEndian.Uint32(raw[12:])
	}
	return p
}

func errorReply(tx uint32, msg string) []byte {
	b := make([]byte, 8, 8+len(msg))
	binary.BigEndian.PutUint32(b[0:], UDPActionError)
	binary.BigEndian.PutUint32(b[4:], tx)
	return append(b, msg...)
}

// handle enregistre le paquet et renvoie la réponse (nil : silence).
func (u *UDPTracker) handle(raw []byte) []byte {
	p := parseUDPPacket(raw)
	u.mu.Lock()
	defer u.mu.Unlock()
	u.pkts = append(u.pkts, p)
	if u.onPacket != nil {
		u.onPacket(p)
	}
	u.cond.Broadcast()
	if u.drop > 0 {
		u.drop--
		return nil
	}
	if p.Action == 0xFFFFFFFF {
		return nil
	}
	if u.hasErr {
		return errorReply(p.TransactionID, u.errMsg)
	}
	switch p.Action {
	case UDPActionConnect:
		// Le connect porte le protocol id à la place du connection id.
		if p.ConnectionID != 0x41727101980 {
			return nil
		}
		id := u.nextConnID
		u.nextConnID++
		u.validConns[id] = true
		b := make([]byte, 16)
		binary.BigEndian.PutUint32(b[0:], UDPActionConnect)
		binary.BigEndian.PutUint32(b[4:], p.TransactionID)
		binary.BigEndian.PutUint64(b[8:], id)
		return b
	case UDPActionAnnounce:
		if !u.validConns[p.ConnectionID] || len(raw) < 98 {
			return errorReply(p.TransactionID, "invalid connection id")
		}
		r := u.announce
		b := make([]byte, 20, 20+len(r.Peers))
		binary.BigEndian.PutUint32(b[0:], UDPActionAnnounce)
		binary.BigEndian.PutUint32(b[4:], p.TransactionID)
		binary.BigEndian.PutUint32(b[8:], r.Interval)
		binary.BigEndian.PutUint32(b[12:], r.Leechers)
		binary.BigEndian.PutUint32(b[16:], r.Seeders)
		return append(b, r.Peers...)
	case UDPActionScrape:
		if !u.validConns[p.ConnectionID] {
			return errorReply(p.TransactionID, "invalid connection id")
		}
		hashes := raw[16:]
		b := make([]byte, 8, 8+12*(len(hashes)/20))
		binary.BigEndian.PutUint32(b[0:], UDPActionScrape)
		binary.BigEndian.PutUint32(b[4:], p.TransactionID)
		for i := 0; i+20 <= len(hashes); i += 20 {
			var h [20]byte
			copy(h[:], hashes[i:i+20])
			e := u.scrape[h]
			b = binary.BigEndian.AppendUint32(b, e.Seeders)
			b = binary.BigEndian.AppendUint32(b, e.Completed)
			b = binary.BigEndian.AppendUint32(b, e.Leechers)
		}
		return b
	}
	return nil
}
