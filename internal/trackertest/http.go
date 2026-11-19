// Package trackertest fournit un tracker HTTP factice pour les tests (et le mode capture).
// Il lit les requêtes sur un listener TCP maison afin de conserver les lignes
// brutes : ordre des en-têtes, query exacte (octets %xx intacts).
package trackertest

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/textproto"
	"strconv"
	"strings"
	"sync"
	"time"
)

const maxHeaderLines = 100

// TB est le sous-ensemble de testing.TB utilisé par les trackers factices :
// *testing.T et *testing.B le satisfont, et le mode capture (hors tests) peut
// fournir sa propre implémentation sans importer « testing ».
type TB interface {
	Helper()
	Fatalf(format string, args ...any)
	Cleanup(func())
}

// Header est un en-tête tel que reçu (nom et valeur bruts).
type Header struct {
	Name  string
	Value string
}

// Request est une requête reçue, sans aucune normalisation.
type Request struct {
	Method   string
	RawURI   string // cible de la requête telle qu'envoyée (chemin + ?query)
	Path     string
	RawQuery string // sans le '?', octets %xx conservés
	Proto    string
	Headers  []Header // dans l'ordre de réception
	// Lines : request line puis lignes d'en-tête brutes, sans CRLF.
	Lines      []string
	Body       []byte
	RemoteAddr string
}

// HeaderValue renvoie la première valeur de l'en-tête name (insensible à la casse).
func (r Request) HeaderValue(name string) string {
	for _, h := range r.Headers {
		if strings.EqualFold(h.Name, name) {
			return h.Value
		}
	}
	return ""
}

// HeaderNames renvoie les noms d'en-têtes dans l'ordre reçu.
func (r Request) HeaderNames() []string {
	names := make([]string, len(r.Headers))
	for i, h := range r.Headers {
		names[i] = h.Name
	}
	return names
}

// Response décrit la réponse à envoyer.
type Response struct {
	Status  int      // 0 = 200
	Headers []Header // en-têtes supplémentaires
	Body    []byte
	// DeclaredLength : valeur de Content-Length si elle doit différer de len(Body)
	// (corps tronqué) ; 0 = len(Body).
	DeclaredLength int
	// Hang : ne jamais répondre (simule un timeout) ; la connexion est fermée à l'arrêt du tracker.
	Hang bool
	// Close : fermer la connexion sans répondre.
	Close bool
}

// OK renvoie une réponse bencodée standard.
func OK(interval, complete, incomplete int) Response {
	return Response{Body: fmt.Appendf(nil, "d8:completei%de10:incompletei%de8:intervali%dee", complete, incomplete, interval)}
}

// Failure renvoie une réponse HTTP 200 portant un "failure reason".
func Failure(reason string) Response {
	return Response{Body: fmt.Appendf(nil, "d14:failure reason%d:%se", len(reason), reason)}
}

// ServerError renvoie un HTTP 500.
func ServerError() Response {
	return Response{Status: http.StatusInternalServerError, Body: []byte("internal error")}
}

// Timeout renvoie une réponse qui ne vient jamais.
func Timeout() Response { return Response{Hang: true} }

// HTTPTracker est un tracker HTTP factice.
type HTTPTracker struct {
	t  TB // nil hors tests (ListenHTTP) : seul WaitRequests s'en sert
	ln net.Listener

	mu      sync.Mutex
	reqs    []Request
	handler func(Request) Response
	changed chan struct{} // fermé et remplacé à chaque requête reçue
	conns   map[net.Conn]struct{}
	closed  bool

	done chan struct{}
	wg   sync.WaitGroup
}

// NewHTTP démarre un tracker sur 127.0.0.1 (port libre) arrêté par t.Cleanup.
func NewHTTP(t TB) *HTTPTracker {
	t.Helper()
	h, err := ListenHTTP("127.0.0.1:0")
	if err != nil {
		t.Fatalf("trackertest: listen: %v", err)
		return nil
	}
	h.t = t
	t.Cleanup(h.Close)
	return h
}

// ListenHTTP démarre un tracker sur addr (mode capture, hors tests) ; l'appelant
// doit appeler Close. Le tracker répond par défaut interval=1800.
func ListenHTTP(addr string) (*HTTPTracker, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	h := &HTTPTracker{
		ln:      ln,
		handler: func(Request) Response { return OK(1800, 10, 5) },
		changed: make(chan struct{}),
		conns:   make(map[net.Conn]struct{}),
		done:    make(chan struct{}),
	}
	h.wg.Add(1)
	go h.acceptLoop()
	return h, nil
}

// Close arrête le tracker et attend toutes ses goroutines. Idempotent.
func (h *HTTPTracker) Close() {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.closed = true
	close(h.done)
	_ = h.ln.Close()
	for c := range h.conns {
		_ = c.Close()
	}
	h.mu.Unlock()
	h.wg.Wait()
}

// Addr renvoie "127.0.0.1:port".
func (h *HTTPTracker) Addr() string { return h.ln.Addr().String() }

// AnnounceURL renvoie l'URL d'annonce, sans query.
func (h *HTTPTracker) AnnounceURL() string { return "http://" + h.Addr() + "/announce" }

// Respond remplace le générateur de réponses (défaut : interval=1800, complete=10, incomplete=5).
func (h *HTTPTracker) Respond(fn func(Request) Response) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.handler = fn
}

// Requests renvoie une copie des requêtes reçues, dans l'ordre d'arrivée.
func (h *HTTPTracker) Requests() []Request {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]Request, len(h.reqs))
	for i, r := range h.reqs {
		out[i] = r.clone()
	}
	return out
}

// WaitRequests attend d'avoir reçu au moins n requêtes ; échoue le test au bout de timeout.
// À appeler depuis la goroutine du test.
func (h *HTTPTracker) WaitRequests(n int, timeout time.Duration) []Request {
	h.t.Helper()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		h.mu.Lock()
		count, ch := len(h.reqs), h.changed
		h.mu.Unlock()
		if count >= n {
			return h.Requests()
		}
		select {
		case <-ch:
		case <-deadline.C:
			h.t.Fatalf("trackertest: %d requête(s) reçue(s) sur %d attendues après %v", count, n, timeout)
			return nil
		}
	}
}

func (r Request) clone() Request {
	r.Headers = append([]Header(nil), r.Headers...)
	r.Lines = append([]string(nil), r.Lines...)
	r.Body = append([]byte(nil), r.Body...)
	return r
}

func (h *HTTPTracker) acceptLoop() {
	defer h.wg.Done()
	for {
		c, err := h.ln.Accept()
		if err != nil {
			return
		}
		h.mu.Lock()
		if h.closed {
			h.mu.Unlock()
			_ = c.Close()
			return
		}
		h.conns[c] = struct{}{}
		h.wg.Add(1)
		h.mu.Unlock()
		go h.serve(c)
	}
}

func (h *HTTPTracker) serve(c net.Conn) {
	defer h.wg.Done()
	defer func() {
		_ = c.Close()
		h.mu.Lock()
		delete(h.conns, c)
		h.mu.Unlock()
	}()
	br := bufio.NewReader(c)
	for {
		req, err := readRequest(br)
		if err != nil {
			return
		}
		req.RemoteAddr = c.RemoteAddr().String()

		h.mu.Lock()
		h.reqs = append(h.reqs, req.clone())
		close(h.changed)
		h.changed = make(chan struct{})
		handler := h.handler
		h.mu.Unlock()

		resp := handler(req)
		if resp.Hang {
			<-h.done
			return
		}
		if resp.Close {
			return
		}
		if err := writeResponse(c, resp); err != nil {
			return
		}
		if strings.EqualFold(req.HeaderValue("Connection"), "close") {
			return
		}
	}
}

// readRequest lit une requête en conservant les lignes brutes.
func readRequest(br *bufio.Reader) (Request, error) {
	tp := textproto.NewReader(br)
	line, err := tp.ReadLine()
	if err != nil {
		return Request{}, err
	}
	parts := strings.SplitN(line, " ", 3)
	if len(parts) != 3 {
		return Request{}, fmt.Errorf("request line invalide %q", line)
	}
	req := Request{Method: parts[0], RawURI: parts[1], Proto: parts[2], Lines: []string{line}}
	req.Path, req.RawQuery, _ = strings.Cut(req.RawURI, "?")
	for {
		l, err := tp.ReadLine()
		if err != nil {
			return Request{}, err
		}
		if l == "" {
			break
		}
		if len(req.Headers) >= maxHeaderLines {
			return Request{}, errors.New("trop d'en-têtes")
		}
		req.Lines = append(req.Lines, l)
		name, value, ok := strings.Cut(l, ":")
		if !ok {
			return Request{}, fmt.Errorf("en-tête invalide %q", l)
		}
		req.Headers = append(req.Headers, Header{Name: name, Value: strings.TrimSpace(value)})
	}
	if cl := req.HeaderValue("Content-Length"); cl != "" {
		n, err := strconv.ParseInt(cl, 10, 64)
		if err != nil || n < 0 || n > 1<<20 {
			return Request{}, fmt.Errorf("Content-Length invalide %q", cl)
		}
		req.Body = make([]byte, n)
		if _, err := io.ReadFull(br, req.Body); err != nil {
			return Request{}, err
		}
	}
	return req, nil
}

func writeResponse(w io.Writer, r Response) error {
	status := r.Status
	if status == 0 {
		status = http.StatusOK
	}
	var b strings.Builder
	fmt.Fprintf(&b, "HTTP/1.1 %d %s\r\n", status, http.StatusText(status))
	b.WriteString("Content-Type: text/plain\r\n")
	for _, h := range r.Headers {
		fmt.Fprintf(&b, "%s: %s\r\n", h.Name, h.Value)
	}
	n := len(r.Body)
	if r.DeclaredLength > 0 {
		n = r.DeclaredLength
	}
	fmt.Fprintf(&b, "Content-Length: %d\r\n\r\n", n)
	b.Write(r.Body)
	_, err := io.WriteString(w, b.String())
	return err
}
