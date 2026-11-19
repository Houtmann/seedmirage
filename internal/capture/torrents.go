package capture

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"strconv"

	"seedmirage/internal/torrenttest"
)

// HardBytes sont les octets d'info_hash « difficiles » à encoder que la
// capture cherche à couvrir : ceux qui diffèrent d'un client à l'autre (restent
// bruts ou passent en %XX), plus les octets de contrôle et hors ASCII.
var HardBytes = []byte{0x00, 0x20, '%', '+', '!', '\'', '(', ')', '*', '-', '.', '_', '~', 0x80, 0xFF}

// TorrentInfo décrit un .torrent factice de la capture.
type TorrentInfo struct {
	File        string `json:"file"`
	Name        string `json:"name"`
	InfoHash    string `json:"infoHash"` // hexadécimal
	Length      int64  `json:"length"`
	Private     bool   `json:"private"`
	Transport   string `json:"transport"` // http | udp
	AnnounceURL string `json:"announceUrl"`
}

// Manifest est capture/manifest.json : les torrents générés.
type Manifest struct {
	Torrents []TorrentInfo `json:"torrents"`
}

// PrivateFunc renvoie la fonction « ce torrent est-il privé ? » pour Options.Private.
func (m Manifest) PrivateFunc() func([20]byte) bool {
	priv := map[[20]byte]bool{}
	for _, t := range m.Torrents {
		b, err := hex.DecodeString(t.InfoHash)
		if err != nil || len(b) != 20 {
			continue
		}
		var ih [20]byte
		copy(ih[:], b)
		priv[ih] = t.Private
	}
	return func(ih [20]byte) bool { return priv[ih] }
}

// ParseManifest relit manifest.json.
func ParseManifest(data []byte) (Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return Manifest{}, fmt.Errorf("manifest.json : %w", err)
	}
	return m, nil
}

// GeneratedTorrent est un .torrent prêt à écrire.
type GeneratedTorrent struct {
	Info TorrentInfo
	Data []byte
}

// TorrentConfig règle la génération.
type TorrentConfig struct {
	HTTP, UDP int
	// Host : « hôte:port » que les clients doivent joindre (dans les URL d'annonce).
	Host string
	Rand *rand.Rand
}

const candidatePool = 20000

func candidate(salt uint64, j int) torrenttest.Options {
	sizes := []int64{350 << 20, 1200 << 20, 700 << 20, 2300 << 20, 90 << 20, 4400 << 20}
	length := sizes[j%len(sizes)] + int64(j)*4096
	piece := int64(1 << 20)
	for length/piece > 32 {
		piece <<= 1
	}
	return torrenttest.Options{
		Name:        "seedmirage-capture-" + strconv.FormatUint(salt, 16) + "-" + strconv.Itoa(j),
		Length:      length,
		PieceLength: piece,
	}
}

func hardMask(ih [20]byte) uint32 {
	var m uint32
	for _, b := range ih {
		for i, h := range HardBytes {
			if b == h {
				m |= 1 << i
			}
		}
	}
	return m
}

// GenerateTorrents fabrique les .torrent factices. Les info_hash des torrents
// HTTP sont choisis (parmi 20 000 candidats, glouton) pour couvrir un maximum
// d'octets de HardBytes à eux tous : c'est ce qui permet de déduire l'encodage.
// Le dernier torrent HTTP est privé quand il y en a au moins deux (certains
// clients n'envoient certains paramètres que pour les torrents privés).
func GenerateTorrents(cfg TorrentConfig) ([]GeneratedTorrent, error) {
	if cfg.HTTP < 1 {
		return nil, fmt.Errorf("au moins un torrent HTTP est nécessaire")
	}
	if cfg.UDP < 0 {
		return nil, fmt.Errorf("nombre de torrents UDP négatif")
	}
	if cfg.Host == "" || cfg.Rand == nil {
		return nil, fmt.Errorf("hôte d'annonce et source aléatoire obligatoires")
	}
	salt := cfg.Rand.Uint64()
	// L'indicateur private est dans le dictionnaire info : il change l'info_hash.
	// Les candidats sont donc hachés dans les deux variantes.
	var masks, masksP [candidatePool]uint32
	for j := range candidatePool {
		o := candidate(salt, j)
		masks[j] = hardMask(torrenttest.InfoHash(o))
		o.Private = true
		masksP[j] = hardMask(torrenttest.InfoHash(o))
	}
	used := map[int]bool{}
	var covered uint32
	// pick choisit un candidat inutilisé : le meilleur gain de couverture si
	// greedy, sinon le premier libre.
	pick := func(greedy, private bool) int {
		ms := &masks
		if private {
			ms = &masksP
		}
		best, bestGain := -1, -1
		for j := range candidatePool {
			if used[j] {
				continue
			}
			gain := 0
			if greedy {
				for m := ms[j] &^ covered; m != 0; m &= m - 1 {
					gain++
				}
			}
			if gain > bestGain {
				best, bestGain = j, gain
			}
			if !greedy {
				break
			}
		}
		used[best] = true
		covered |= ms[best]
		return best
	}

	var out []GeneratedTorrent
	mk := func(j int, transport string, idx int, private bool) {
		o := candidate(salt, j)
		o.Private = private
		o.Announce = transport + "://" + cfg.Host + "/announce"
		ih := torrenttest.InfoHash(o)
		out = append(out, GeneratedTorrent{
			Info: TorrentInfo{
				File: fmt.Sprintf("capture-%02d-%s.torrent", idx, transport), Name: o.Name,
				InfoHash: hex.EncodeToString(ih[:]), Length: o.Length, Private: private,
				Transport: transport, AnnounceURL: o.Announce,
			},
			Data: torrenttest.Build(o),
		})
	}
	// Le torrent privé est choisi en premier : il a moins de candidats utiles.
	privIdx := -1
	if cfg.HTTP >= 2 {
		privIdx = cfg.HTTP - 1
	}
	picked := make([]int, cfg.HTTP)
	if privIdx >= 0 {
		picked[privIdx] = pick(true, true)
	}
	for i := 0; i < cfg.HTTP; i++ {
		if i != privIdx {
			picked[i] = pick(true, false)
		}
	}
	for i := 0; i < cfg.HTTP; i++ {
		mk(picked[i], "http", i+1, i == privIdx)
	}
	for i := 0; i < cfg.UDP; i++ {
		mk(pick(false, false), "udp", i+1, false)
	}
	return out, nil
}
