package tracker

import (
	"bufio"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"seedmirage/internal/bencode"
	"seedmirage/internal/emulation"
	"seedmirage/internal/torrent"
)

// Bornes par défaut (connexion 10 s, réponse 15 s).
const (
	DefaultDialTimeout = 10 * time.Second
	DefaultTimeout     = 15 * time.Second
	maxBody            = 1 << 20
	maxRedirects       = 2
	maxFailureReason   = 256
)

// HTTPOptions règle un HTTPClient. Les zéros donnent les valeurs par défaut.
type HTTPOptions struct {
	DialTimeout time.Duration // défaut 10 s
	Timeout     time.Duration // durée totale d'une annonce, redirections comprises ; défaut 15 s
	// RootCAs remplace les autorités système (tests).
	RootCAs *x509.CertPool
	// DialContext remplace net.Dialer (tests) ; sinon dial TCP avec DialTimeout.
	DialContext func(ctx context.Context, network, addr string) (net.Conn, error)
	// ScrapeHeaders : en-têtes envoyés pour un scrape (le scrape n'a pas de profil).
	// Modifiables à chaud par SetScrapeProfile.
	ScrapeHeaders []emulation.Header
	// ScrapeEncoding : encodage de l'info_hash d'un scrape ; nil = octets non
	// réservés tels quels, hexadécimal majuscule. Modifiable par SetScrapeProfile.
	ScrapeEncoding *emulation.Encoding
}

// MaxHTTPScrapeHashes borne le nombre d'info_hash par requête de scrape HTTP
// (URL de longueur raisonnable : 50 × 62 octets au pire). Le client ne découpe
// pas : c'est à l'appelant de respecter cette borne.
const MaxHTTPScrapeHashes = 50

// HTTPClient annonce à un tracker HTTP(S). La requête est écrite à la main :
// ligne de requête, Host, puis les en-têtes du profil dans l'ordre, rien d'autre.
// Une connexion par requête (pas de keep-alive). Sûr en concurrence.
type HTTPClient struct {
	opts HTTPOptions

	mu            sync.RWMutex // protège scrapeHeaders et scrapeEnc
	scrapeHeaders []emulation.Header
	scrapeEnc     *emulation.Encoding
}

var _ Client = (*HTTPClient)(nil)

// NewHTTP crée un client.
func NewHTTP(opts HTTPOptions) *HTTPClient {
	if opts.DialTimeout <= 0 {
		opts.DialTimeout = DefaultDialTimeout
	}
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultTimeout
	}
	c := &HTTPClient{opts: opts}
	c.SetScrapeProfile(opts.ScrapeHeaders, opts.ScrapeEncoding)
	return c
}

// SetScrapeProfile remplace les en-têtes et l'encodage de l'info_hash des
// scrapes suivants (changement de profil à chaud). enc nil : encodage par défaut.
func (c *HTTPClient) SetScrapeProfile(headers []emulation.Header, enc *emulation.Encoding) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.scrapeHeaders = append([]emulation.Header(nil), headers...)
	c.scrapeEnc = nil
	if enc != nil {
		e := *enc
		c.scrapeEnc = &e
	}
}

// Announce envoie l'annonce. Un « failure reason » donne un *FailureError.
func (c *HTTPClient) Announce(ctx context.Context, u *url.URL, a emulation.Announce, p emulation.AnnounceParams) (*Response, error) {
	_ = p // l'état d'annonce est déjà dans a (query) ; conservé pour l'interface commune
	body, err := c.get(ctx, u, a.RawQuery, a.Headers)
	if err != nil {
		return nil, fmt.Errorf("announce %s: %w", RedactURL(u), err)
	}
	r, err := parseAnnounce(body)
	if err != nil {
		return nil, fmt.Errorf("announce %s: %w", RedactURL(u), err)
	}
	return r, nil
}

// Scrape interroge l'URL de scrape déduite de l'URL d'annonce (/announce -> /scrape).
func (c *HTTPClient) Scrape(ctx context.Context, u *url.URL, hashes []torrent.InfoHash) (map[torrent.InfoHash]ScrapeStats, error) {
	su, err := scrapeURL(u)
	if err != nil {
		return nil, fmt.Errorf("scrape %s: %w", RedactURL(u), err)
	}
	c.mu.RLock()
	headers, enc := c.scrapeHeaders, c.scrapeEnc
	c.mu.RUnlock()
	var q strings.Builder
	for i, h := range hashes {
		if i > 0 {
			q.WriteByte('&')
		}
		q.WriteString("info_hash=")
		if enc != nil {
			q.WriteString(emulation.Encode(h[:], *enc))
			continue
		}
		for _, b := range h {
			if isUnreserved(b) {
				q.WriteByte(b)
			} else {
				fmt.Fprintf(&q, "%%%02X", b)
			}
		}
	}
	body, err := c.get(ctx, su, q.String(), headers)
	if err != nil {
		return nil, fmt.Errorf("scrape %s: %w", RedactURL(u), err)
	}
	out, err := parseScrape(body)
	if err != nil {
		return nil, fmt.Errorf("scrape %s: %w", RedactURL(u), err)
	}
	return out, nil
}

func isUnreserved(b byte) bool {
	return b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' ||
		b == '-' || b == '_' || b == '.' || b == '~'
}

// scrapeURL remplace le dernier segment « announce… » par « scrape… » (convention BEP 48).
func scrapeURL(u *url.URL) (*url.URL, error) {
	p := u.EscapedPath()
	i := strings.LastIndexByte(p, '/')
	if i < 0 || !strings.HasPrefix(p[i+1:], "announce") {
		return nil, ErrScrapeUnsupported
	}
	su := *u
	np := p[:i+1] + "scrape" + strings.TrimPrefix(p[i+1:], "announce")
	su.RawPath = ""
	dp, err := url.PathUnescape(np)
	if err != nil {
		return nil, ErrScrapeUnsupported
	}
	su.Path = dp
	su.RawPath = np
	return &su, nil
}

// target construit la cible de requête : chemin, puis la query de l'URL
// d'annonce (passkey) jointe à extra par « & » ; « ? » sinon. Jamais réencodée.
func target(u *url.URL, extra string) string {
	p := u.EscapedPath()
	if p == "" {
		p = "/"
	}
	q := u.RawQuery
	switch {
	case q != "" && extra != "":
		q += "&" + extra
	case extra != "":
		q = extra
	}
	if q == "" {
		return p
	}
	return p + "?" + q
}

// hostHeader : hôte, avec le port seulement s'il n'est pas celui par défaut du schéma.
func hostHeader(u *url.URL) string {
	host := u.Hostname()
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	port := u.Port()
	if port == "" || port == defaultPort(u.Scheme) {
		return host
	}
	return host + ":" + port
}

func defaultPort(scheme string) string {
	if scheme == "https" {
		return "443"
	}
	return "80"
}

func hostPort(u *url.URL) string {
	port := u.Port()
	if port == "" {
		port = defaultPort(u.Scheme)
	}
	return net.JoinHostPort(u.Hostname(), port)
}

// writeRequest produit la requête exacte : request line, puis les en-têtes du
// profil. Host (hôte de l'URL) est écrit en premier, sauf si le profil porte une
// entrée Host {host} qui en fixe la position.
func writeRequest(u *url.URL, tgt string, headers []emulation.Header) ([]byte, error) {
	var b bytes.Buffer
	b.WriteString("GET " + tgt + " HTTP/1.1\r\n")
	hostListed := slices.ContainsFunc(headers, func(h emulation.Header) bool { return strings.EqualFold(h.Name, "Host") })
	if !hostListed {
		b.WriteString("Host: " + hostHeader(u) + "\r\n")
	}
	for _, h := range headers {
		if strings.EqualFold(h.Name, "Host") {
			b.WriteString("Host: " + hostHeader(u) + "\r\n")
			continue
		}
		if strings.ContainsAny(h.Name+h.Value, "\r\n") || h.Name == "" {
			return nil, fmt.Errorf("%w: en-tête invalide %q", ErrBadResponse, h.Name)
		}
		b.WriteString(h.Name + ": " + h.Value + "\r\n")
	}
	b.WriteString("\r\n")
	return b.Bytes(), nil
}

// get exécute la requête (redirections comprises) et renvoie le corps décompressé.
// Les erreurs retournées ne contiennent pas l'URL : l'appelant ajoute RedactURL.
func (c *HTTPClient) get(ctx context.Context, u *url.URL, extraQuery string, headers []emulation.Header) ([]byte, error) {
	if u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return nil, ErrUnsupportedScheme
	}
	ctx, cancel := context.WithTimeout(ctx, c.opts.Timeout)
	defer cancel()

	cur := u
	tgt := target(u, extraQuery)
	for hop := 0; ; hop++ {
		resp, body, err := c.roundTrip(ctx, cur, tgt, headers)
		if err != nil {
			return nil, err
		}
		switch resp.StatusCode {
		case http.StatusMovedPermanently, http.StatusFound, http.StatusTemporaryRedirect:
			if hop >= maxRedirects {
				return nil, fmt.Errorf("%w: plus de %d redirections", ErrRedirect, maxRedirects)
			}
			loc, err := cur.Parse(resp.Header.Get("Location"))
			if err != nil || resp.Header.Get("Location") == "" {
				return nil, fmt.Errorf("%w: Location invalide", ErrRedirect)
			}
			if loc.Scheme != cur.Scheme || !strings.EqualFold(hostPort(loc), hostPort(cur)) {
				return nil, fmt.Errorf("%w: vers %s", ErrRedirect, RedactURL(&url.URL{Scheme: loc.Scheme, Host: loc.Host}))
			}
			cur = loc
			tgt = target(loc, "")
			continue
		}
		if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			return nil, fmt.Errorf("%w: code %d", ErrRedirect, resp.StatusCode)
		}
		if resp.StatusCode >= 400 {
			// Un corps bencodé (failure reason) reste exploitable ; sinon erreur réseau.
			if d, derr := bencode.Decode(body); derr == nil {
				if _, ok := d.(bencode.Dict); ok {
					return body, nil
				}
			}
			return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
		}
		return body, nil
	}
}

func (c *HTTPClient) dial(ctx context.Context, u *url.URL) (net.Conn, error) {
	addr := hostPort(u)
	var conn net.Conn
	var err error
	if c.opts.DialContext != nil {
		conn, err = c.opts.DialContext(ctx, "tcp", addr)
	} else {
		d := net.Dialer{Timeout: c.opts.DialTimeout}
		conn, err = d.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return nil, fmt.Errorf("connexion: %w", err)
	}
	if u.Scheme != "https" {
		return conn, nil
	}
	tc := tls.Client(conn, &tls.Config{
		ServerName: u.Hostname(),
		NextProtos: []string{"http/1.1"},
		RootCAs:    c.opts.RootCAs,
		MinVersion: tls.VersionTLS12,
	})
	if err := tc.HandshakeContext(ctx); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("TLS: %w", err)
	}
	return tc, nil
}

// roundTrip : une requête sur une connexion neuve ; renvoie la réponse (corps déjà lu et décompressé).
func (c *HTTPClient) roundTrip(ctx context.Context, u *url.URL, tgt string, headers []emulation.Header) (*http.Response, []byte, error) {
	req, err := writeRequest(u, tgt, headers)
	if err != nil {
		return nil, nil, err
	}
	conn, err := c.dial(ctx, u)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = conn.Close() }()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()

	if _, err := conn.Write(req); err != nil {
		return nil, nil, ctxErr(ctx, fmt.Errorf("envoi: %w", err))
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodGet})
	if err != nil {
		return nil, nil, ctxErr(ctx, fmt.Errorf("%w: %w", ErrBadResponse, err))
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return nil, nil, ctxErr(ctx, fmt.Errorf("lecture du corps: %w", err))
	}
	if len(raw) > maxBody {
		return nil, nil, fmt.Errorf("%w: corps supérieur à %d octets", ErrBadResponse, maxBody)
	}
	body, err := decode(resp.Header.Get("Content-Encoding"), raw)
	if err != nil {
		return nil, nil, err
	}
	return resp, body, nil
}

// ctxErr privilégie l'erreur du contexte (timeout, annulation) sur l'erreur de socket qui en découle.
func ctxErr(ctx context.Context, err error) error {
	if cerr := ctx.Err(); cerr != nil {
		return fmt.Errorf("%w (%v)", cerr, err)
	}
	// L'échéance de la connexion (posée à celle du contexte) peut sonner juste
	// avant que ctx.Err() ne soit renseigné : c'est bien un dépassement du contexte.
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return fmt.Errorf("%w (%v)", context.DeadlineExceeded, err)
	}
	return err
}

// decode applique Content-Encoding : identity, gzip, deflate (zlib, repli sur flux brut).
func decode(enc string, raw []byte) ([]byte, error) {
	enc = strings.ToLower(strings.TrimSpace(enc))
	var r io.Reader
	switch enc {
	case "", "identity":
		return raw, nil
	case "gzip", "x-gzip":
		gr, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			return nil, fmt.Errorf("%w: gzip: %w", ErrBadResponse, err)
		}
		r = gr
	case "deflate":
		zr, err := zlib.NewReader(bytes.NewReader(raw))
		if err != nil {
			r = flate.NewReader(bytes.NewReader(raw))
		} else {
			r = zr
		}
	default:
		return nil, fmt.Errorf("%w: Content-Encoding %q non supporté", ErrBadResponse, enc)
	}
	out, err := io.ReadAll(io.LimitReader(r, maxBody+1))
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrBadResponse, enc, err)
	}
	if len(out) > maxBody {
		return nil, fmt.Errorf("%w: corps décompressé supérieur à %d octets", ErrBadResponse, maxBody)
	}
	return out, nil
}

func parseDict(body []byte) (bencode.Dict, error) {
	v, err := bencode.Decode(body)
	if err != nil {
		return bencode.Dict{}, fmt.Errorf("%w: %w", ErrBadResponse, err)
	}
	d, ok := v.(bencode.Dict)
	if !ok {
		return bencode.Dict{}, fmt.Errorf("%w: la réponse n'est pas un dictionnaire", ErrBadResponse)
	}
	if reason, ok := d.Bytes("failure reason"); ok {
		r := string(reason)
		if len(r) > maxFailureReason {
			r = r[:maxFailureReason]
		}
		return bencode.Dict{}, &FailureError{Reason: r}
	}
	return d, nil
}

func parseAnnounce(body []byte) (*Response, error) {
	d, err := parseDict(body)
	if err != nil {
		return nil, err
	}
	r := &Response{}
	if v, ok := d.Int("interval"); ok && v > 0 {
		r.Interval = time.Duration(min(v, 1<<31)) * time.Second
	}
	if v, ok := d.Int("min interval"); ok && v > 0 {
		r.MinInterval = time.Duration(min(v, 1<<31)) * time.Second
	}
	if v, ok := d.Int("complete"); ok && v > 0 {
		r.Seeders = int(min(v, 1<<31))
	}
	if v, ok := d.Int("incomplete"); ok && v > 0 {
		r.Leechers = int(min(v, 1<<31))
	}
	if v, ok := d.Bytes("tracker id"); ok {
		r.TrackerID = string(v)
	}
	if v, ok := d.Bytes("warning message"); ok {
		r.Warning = string(v)
	}
	return r, nil
}

func parseScrape(body []byte) (map[torrent.InfoHash]ScrapeStats, error) {
	d, err := parseDict(body)
	if err != nil {
		return nil, err
	}
	files, ok := d.Dict("files")
	if !ok {
		return nil, fmt.Errorf("%w: clé files absente", ErrBadResponse)
	}
	var minReq time.Duration
	if fl, ok := d.Dict("flags"); ok {
		if v, ok := fl.Int("min_request_interval"); ok && v > 0 {
			minReq = time.Duration(min(v, 24*3600)) * time.Second
		}
	}
	out := make(map[torrent.InfoHash]ScrapeStats, len(files.Keys))
	for _, k := range files.Keys {
		if len(k) != len(torrent.InfoHash{}) {
			continue
		}
		e, ok := files.Dict(k)
		if !ok {
			continue
		}
		var h torrent.InfoHash
		copy(h[:], k)
		var s ScrapeStats
		if v, ok := e.Int("complete"); ok && v > 0 {
			s.Seeders = int(min(v, 1<<31))
		}
		if v, ok := e.Int("incomplete"); ok && v > 0 {
			s.Leechers = int(min(v, 1<<31))
		}
		if v, ok := e.Int("downloaded"); ok && v > 0 {
			s.Downloaded = int(min(v, 1<<31))
		}
		s.MinRequestInterval = minReq
		out[h] = s
	}
	return out, nil
}
