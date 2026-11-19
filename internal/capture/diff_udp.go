package capture

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"time"

	"seedmirage/internal/clock"
	"seedmirage/internal/emulation"
	"seedmirage/internal/regexgen"
)

// udpOptionURLData : option BEP 41 « URLData ».
const udpOptionURLData = 2

// DiffUDP compare une annonce UDP (BEP 15) capturée à celle que produirait le
// profil pour le même torrent, le même événement et les mêmes compteurs.
// Sont neutralisés : connection id, transaction id, peer_id et key (qui doivent
// seulement respecter le motif du profil). Le reste (action, info_hash,
// compteurs, événement, adresse IP à 0, num_want, port, options BEP 41) doit
// être identique à l'octet près. Liste vide = identique à l'aléa près.
//
// opts.URLData : chemin + query de l'URL du tracker (ex. « /announce »), dont
// le client envoie l'option BEP 41 quand ce n'est pas « / » et que le profil
// l'annonce (udp.urlData, vrai par défaut) ; vide = aucune option.
func DiffUDP(p *emulation.Profile, rec Record, opts DiffOptions) ([]Mismatch, error) {
	if rec.Transport != TransportUDP {
		return nil, fmt.Errorf("enregistrement %q : pas un paquet UDP", rec.Transport)
	}
	raw, err := hex.DecodeString(rec.Packet)
	if err != nil {
		return nil, fmt.Errorf("packet : %w", err)
	}
	a, ok := rec.UDPAnnounce()
	if !ok {
		return nil, fmt.Errorf("paquet UDP : pas une annonce BEP 15 (%d octets)", len(raw))
	}
	clk := clock.NewFake(time.Unix(1_700_000_000, 0))
	em, err := emulation.New(p, clk, rand.New(rand.NewPCG(1, 2)), emulation.Options{}) //nolint:gosec // valeurs neutralisées
	if err != nil {
		return nil, err
	}
	ev := udpEventToEvent(a.Event)
	ann, err := em.BuildAnnounce(emulation.AnnounceParams{
		InfoHash: a.InfoHash, Port: int(a.Port), Uploaded: a.Uploaded, Downloaded: a.Downloaded,
		Left: a.Left, Event: ev, Private: opts.Private, PublicIP: opts.PublicIP,
	})
	if err != nil {
		return nil, err
	}

	var out []Mismatch
	add := func(field, want, got string) { out = append(out, Mismatch{field, want, got}) }

	if m := checkPeerIDBytes(p, a.PeerID[:]); m != "" {
		add("udp.peer_id", m, hex.EncodeToString(a.PeerID[:]))
	}
	if p.Key != nil && len(ann.Key) == 8 {
		k := fmt.Sprintf("%08x", a.Key)
		if !regexgen.Matches(p.Key.Pattern, []byte(k)) && !regexgen.Matches(p.Key.Pattern, []byte(strings.ToUpper(k))) {
			add("udp.key", "motif "+p.Key.Pattern, k)
		}
	}
	if ip := raw[84:88]; !bytes.Equal(ip, []byte{0, 0, 0, 0}) {
		add("udp.ip", "00000000", hex.EncodeToString(ip))
	}
	wantNW := int32(-1)
	if ann.Numwant >= 0 {
		wantNW = int32(min(ann.Numwant, 1<<30)) //nolint:gosec // borné
	}
	if a.Numwant != wantNW {
		add("udp.numwant", fmt.Sprint(wantNW), fmt.Sprint(a.Numwant))
	}
	if a.Event > 3 {
		add("udp.event", "0 à 3", fmt.Sprint(a.Event))
	}
	var wantOpts []byte
	if p.SendsUDPURLData() && opts.URLData != "" && opts.URLData != "/" {
		for data := []byte(opts.URLData); len(data) > 0; {
			n := min(len(data), 255)
			wantOpts = append(wantOpts, udpOptionURLData, byte(n)) //nolint:gosec // n <= 255
			wantOpts = append(wantOpts, data[:n]...)
			data = data[n:]
		}
	}
	if got := raw[98:]; !slices.Equal(got, wantOpts) {
		add("udp.options", hex.EncodeToString(wantOpts), hex.EncodeToString(got))
	}
	return out, nil
}

func udpEventToEvent(e uint32) emulation.Event {
	switch e {
	case 1:
		return emulation.EventCompleted
	case 2:
		return emulation.EventStarted
	case 3:
		return emulation.EventStopped
	default:
		return emulation.EventNone
	}
}
