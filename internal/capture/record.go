// Package capture enregistre les annonces d'un vrai client BitTorrent sur un
// tracker local (HTTP et UDP) et en déduit un profil seedmirage (schema 1).
//
// Tout ce qui est enregistré l'est sans normalisation : la requête HTTP est
// conservée ligne par ligne, dans l'ordre reçu, et les paquets UDP tels quels.
// info_hash, peer_id et key sont des octets : ils ne passent jamais par une
// string interprétée comme de l'UTF-8.
package capture

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"seedmirage/internal/trackertest"
)

// Transports d'un Record.
const (
	TransportHTTP = "http"
	TransportUDP  = "udp"
)

// Record est une ligne de raw.jsonl : une requête HTTP ou un paquet UDP reçu.
type Record struct {
	Time      time.Time `json:"time"`
	Transport string    `json:"transport"`
	Remote    string    `json:"remote,omitempty"`

	// HTTP. Head est la référence : la ligne de requête et les en-têtes bruts
	// séparés par CRLF (sans la ligne vide finale), en base64 car ils peuvent
	// contenir des octets hors UTF-8. Lines n'est qu'une copie lisible.
	Head  string   `json:"head,omitempty"`
	Lines []string `json:"lines,omitempty"`

	// UDP : le datagramme en hexadécimal.
	Packet string `json:"packet,omitempty"`
}

// FromHTTP convertit une requête du tracker factice.
func FromHTTP(at time.Time, r trackertest.Request) Record {
	return Record{
		Time:      at,
		Transport: TransportHTTP,
		Remote:    r.RemoteAddr,
		Head:      base64.StdEncoding.EncodeToString([]byte(strings.Join(r.Lines, "\r\n"))),
		Lines:     append([]string(nil), r.Lines...),
	}
}

// FromUDP convertit un datagramme du tracker factice.
func FromUDP(at time.Time, p trackertest.UDPPacket) Record {
	return Record{Time: at, Transport: TransportUDP, Packet: hex.EncodeToString(p.Raw)}
}

// HTTPHeader est un en-tête tel que reçu.
type HTTPHeader struct{ Name, Value string }

// HTTPRequest est une requête HTTP enregistrée, relue sans normalisation.
type HTTPRequest struct {
	Method, RawURI, Proto string
	Path, RawQuery        string
	Headers               []HTTPHeader // dans l'ordre reçu, Host compris
}

// HTTP relit la requête enregistrée.
func (r Record) HTTP() (HTTPRequest, error) {
	if r.Transport != TransportHTTP {
		return HTTPRequest{}, fmt.Errorf("enregistrement %q : pas une requête HTTP", r.Transport)
	}
	var head []byte
	if r.Head != "" {
		b, err := base64.StdEncoding.DecodeString(r.Head)
		if err != nil {
			return HTTPRequest{}, fmt.Errorf("head : %w", err)
		}
		head = b
	} else {
		head = []byte(strings.Join(r.Lines, "\r\n"))
	}
	lines := strings.Split(string(head), "\r\n")
	parts := strings.SplitN(lines[0], " ", 3)
	if len(parts) != 3 {
		return HTTPRequest{}, fmt.Errorf("ligne de requête invalide %q", lines[0])
	}
	req := HTTPRequest{Method: parts[0], RawURI: parts[1], Proto: parts[2]}
	req.Path, req.RawQuery, _ = strings.Cut(req.RawURI, "?")
	for _, l := range lines[1:] {
		if l == "" {
			continue
		}
		name, value, ok := strings.Cut(l, ":")
		if !ok {
			return HTTPRequest{}, fmt.Errorf("en-tête invalide %q", l)
		}
		req.Headers = append(req.Headers, HTTPHeader{Name: name, Value: strings.TrimSpace(value)})
	}
	return req, nil
}

// QueryParam est un paramètre de query brut (valeur non décodée).
type QueryParam struct{ Name, Raw string }

// Params découpe la query brute en paramètres, dans l'ordre. Un paramètre sans
// « = » a une valeur vide.
func (r HTTPRequest) Params() []QueryParam {
	if r.RawQuery == "" {
		return nil
	}
	var out []QueryParam
	for _, kv := range strings.Split(r.RawQuery, "&") {
		n, v, _ := strings.Cut(kv, "=")
		out = append(out, QueryParam{Name: n, Raw: v})
	}
	return out
}

// Param renvoie la valeur brute du premier paramètre name.
func (r HTTPRequest) Param(name string) (string, bool) {
	for _, p := range r.Params() {
		if p.Name == name {
			return p.Raw, true
		}
	}
	return "", false
}

// PercentDecode décode %XX octet par octet. Un « % » mal formé est conservé
// tel quel (ok = false).
func PercentDecode(s string) (b []byte, ok bool) {
	ok = true
	for i := 0; i < len(s); i++ {
		if s[i] != '%' {
			b = append(b, s[i])
			continue
		}
		if i+2 < len(s) && isHex(s[i+1]) && isHex(s[i+2]) {
			b = append(b, unhex(s[i+1])<<4|unhex(s[i+2]))
			i += 2
			continue
		}
		ok = false
		b = append(b, '%')
	}
	return b, ok
}

func isHex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

func unhex(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	default:
		return c - 'A' + 10
	}
}

// UDPAnnounce est une annonce BEP 15 décodée (octets bruts conservés).
type UDPAnnounce struct {
	InfoHash, PeerID            [20]byte
	Downloaded, Left, Uploaded  int64
	Event                       uint32 // 0 aucun, 1 completed, 2 started, 3 stopped
	Key                         uint32
	Numwant                     int32
	Port                        uint16
	ConnectionID, TransactionID uint64
	// Options : octets après les 98 octets fixes (options BEP 41), vide sinon.
	Options []byte
}

// UDPAnnounce décode un paquet d'annonce ; ok = false pour les autres paquets
// (connect, scrape, paquets trop courts).
func (r Record) UDPAnnounce() (UDPAnnounce, bool) {
	if r.Transport != TransportUDP {
		return UDPAnnounce{}, false
	}
	raw, err := hex.DecodeString(r.Packet)
	if err != nil || len(raw) < 98 || binary.BigEndian.Uint32(raw[8:]) != trackertest.UDPActionAnnounce {
		return UDPAnnounce{}, false
	}
	var a UDPAnnounce
	a.ConnectionID = binary.BigEndian.Uint64(raw[0:])
	a.TransactionID = uint64(binary.BigEndian.Uint32(raw[12:]))
	copy(a.InfoHash[:], raw[16:36])
	copy(a.PeerID[:], raw[36:56])
	a.Downloaded = int64(binary.BigEndian.Uint64(raw[56:])) //nolint:gosec // compteur BEP 15 : 64 bits signés
	a.Left = int64(binary.BigEndian.Uint64(raw[64:]))       //nolint:gosec // compteur BEP 15 : 64 bits signés
	a.Uploaded = int64(binary.BigEndian.Uint64(raw[72:]))   //nolint:gosec // compteur BEP 15 : 64 bits signés
	a.Event = binary.BigEndian.Uint32(raw[80:])
	a.Key = binary.BigEndian.Uint32(raw[88:])
	a.Numwant = int32(binary.BigEndian.Uint32(raw[92:])) //nolint:gosec // num_want signé (-1 = défaut)
	a.Port = binary.BigEndian.Uint16(raw[96:])
	a.Options = append([]byte(nil), raw[98:]...)
	return a, true
}

// Writer écrit les enregistrements en JSON lines, une ligne par enregistrement,
// écrite et vidée aussitôt (une capture interrompue reste lisible). Sûr en concurrence.
type Writer struct {
	mu  sync.Mutex
	w   io.Writer
	err error
}

// NewWriter écrit dans w.
func NewWriter(w io.Writer) *Writer { return &Writer{w: w} }

// Write ajoute un enregistrement. La première erreur d'écriture est conservée.
func (w *Writer) Write(r Record) {
	b, err := json.Marshal(r)
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return
	}
	if err != nil {
		w.err = err
		return
	}
	_, w.err = w.w.Write(append(b, '\n'))
}

// Err renvoie la première erreur d'écriture.
func (w *Writer) Err() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.err
}

// ReadRecords relit un raw.jsonl.
func ReadRecords(r io.Reader) ([]Record, error) {
	var out []Record
	br := bufio.NewReaderSize(r, 1<<16)
	for n := 1; ; n++ {
		line, err := br.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			var rec Record
			if jerr := json.Unmarshal(line, &rec); jerr != nil {
				return nil, fmt.Errorf("raw.jsonl ligne %d : %w", n, jerr)
			}
			out = append(out, rec)
		}
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
	}
}
