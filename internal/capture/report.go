package capture

import (
	"fmt"
	"strings"
)

// Markdown rend le rapport de capture (capture/report.md) : d'abord les
// incertitudes (confiance basse puis moyenne), puis les déductions sûres.
func (r *Result) Markdown() string {
	var b strings.Builder
	b.WriteString("# Rapport de capture\n\n")
	if r.Profile != nil {
		fmt.Fprintf(&b, "Profil déduit : `%s` (source `capture`).\n\n", r.Profile.Name)
	} else {
		b.WriteString("Aucun profil produit (voir les incertitudes).\n\n")
	}
	fmt.Fprintf(&b, "- Requêtes HTTP : %d\n- Annonces UDP : %d\n", r.HTTPRequests, r.UDPAnnounces)
	if r.PeerID.Pattern != "" {
		prefix := fmt.Sprintf("%q", string(r.PeerID.Prefix))
		if !printable(r.PeerID.Prefix) {
			prefix = fmt.Sprintf("hex %x", r.PeerID.Prefix)
		}
		fmt.Fprintf(&b, "- peer_id : préfixe %s, suffixe `%s`", prefix, r.PeerID.Pattern)
		if r.PeerID.Checksum != "" {
			fmt.Fprintf(&b, " + somme %s", r.PeerID.Checksum)
		}
		fmt.Fprintf(&b, ", portée `%s` (confiance %s)\n", r.PeerID.Scope, r.PeerID.Confidence)
	}
	if r.HasKey {
		fmt.Fprintf(&b, "- key : `%s`, portée `%s` (confiance %s)\n", r.Key.Pattern, r.Key.Scope, r.Key.Confidence)
	} else {
		b.WriteString("- key : aucune key dans les annonces HTTP\n")
	}

	section := func(title string, confs ...Confidence) {
		var rows []Finding
		for _, f := range r.Findings {
			for _, c := range confs {
				if f.Confidence == c {
					rows = append(rows, f)
				}
			}
		}
		if len(rows) == 0 {
			return
		}
		fmt.Fprintf(&b, "\n## %s\n\n| Champ | Confiance | Détail |\n|---|---|---|\n", title)
		for _, f := range rows {
			fmt.Fprintf(&b, "| `%s` | %s | %s |\n", f.Field, f.Confidence, strings.ReplaceAll(f.Message, "|", `\|`))
		}
	}
	section("Incertitudes (à vérifier avant d'utiliser le profil)", ConfLow, ConfMedium)
	section("Déductions sûres", ConfHigh)

	if len(r.Lint) > 0 {
		b.WriteString("\n## Linter (`seedmirage check-profile`)\n\n")
		for _, f := range r.Lint {
			fmt.Fprintf(&b, "- %s\n", f)
		}
	} else if r.Profile != nil {
		b.WriteString("\n## Linter (`seedmirage check-profile`)\n\nok\n")
	}
	return b.String()
}
