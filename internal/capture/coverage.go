package capture

import (
	"fmt"
	"strings"
)

// coverage signale les octets difficiles (HardBytes) absents des info_hash
// capturés : leur encodage n'est alors pas prouvé par la capture.
func coverage(obs []observation, add func(Finding)) {
	seen := map[byte]bool{}
	torrents := map[[20]byte]bool{}
	for _, o := range obs {
		torrents[o.ih] = true
		for _, b := range o.ih {
			seen[b] = true
		}
	}
	var missing []byte
	for _, b := range HardBytes {
		if !seen[b] {
			missing = append(missing, b)
		}
	}
	if len(missing) == 0 {
		add(Finding{"coverage.info_hash", ConfHigh, fmt.Sprintf("les %d octets difficiles sont présents dans les %d info_hash capturés", len(HardBytes), len(torrents))})
		return
	}
	add(Finding{"coverage.info_hash", ConfMedium, fmt.Sprintf("octets difficiles absents des %d info_hash capturés (encodage non prouvé pour eux) : %s", len(torrents), hexList(missing))})
}

func hexList(bs []byte) string {
	parts := make([]string, len(bs))
	for i, b := range bs {
		parts[i] = fmt.Sprintf("0x%02X", b)
	}
	return strings.Join(parts, " ")
}
