package emulation

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func baseJSON(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("profiles/qbittorrent-5.3.0.json")
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestParseRejects(t *testing.T) {
	base := baseJSON(t)
	// mutate décode le JSON de base en map, applique f, ré-encode.
	mutate := func(f func(m map[string]any)) []byte {
		var m map[string]any
		if err := json.Unmarshal(base, &m); err != nil {
			t.Fatal(err)
		}
		f(m)
		out, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	query := func(m map[string]any) []any { return m["query"].([]any) }

	cases := []struct {
		name string
		data []byte
		want string
	}{
		{"champ inconnu", mutate(func(m map[string]any) { m["surprise"] = 1 }), "unknown field"},
		{"données après l'objet", append(append([]byte{}, base...), []byte(" {}")...), "après"},
		{"schema", mutate(func(m map[string]any) { m["schema"] = 2 }), "schema"},
		{"name", mutate(func(m map[string]any) { m["name"] = "autre-1" }), "name"},
		{"released", mutate(func(m map[string]any) { m["released"] = "hier" }), "released"},
		{"source", mutate(func(m map[string]any) { m["source"] = "imported" }), "source"},
		{"references vides", mutate(func(m map[string]any) { m["references"] = []any{} }), "references"},
		{"peer_id trop court", mutate(func(m map[string]any) {
			m["peerId"].(map[string]any)["suffix"] = "[a-z]{11}"
		}), "octets"},
		{"peer_id longueur variable", mutate(func(m map[string]any) {
			m["peerId"].(map[string]any)["suffix"] = "[a-z]{11,12}"
		}), "octets"},
		{"suffixe non compilable", mutate(func(m map[string]any) {
			m["peerId"].(map[string]any)["suffix"] = "(["
		}), "suffix"},
		{"prefix et prefixHex", mutate(func(m map[string]any) {
			m["peerId"].(map[string]any)["prefixHex"] = "2d"
		}), "exclusifs"},
		{"portée inconnue", mutate(func(m map[string]any) {
			m["peerId"].(map[string]any)["renewal"] = map[string]any{"scope": "toujours"}
		}), "scope"},
		{"checksum inconnu", mutate(func(m map[string]any) {
			m["peerId"].(map[string]any)["checksum"] = "crc"
		}), "checksum"},
		{"key motif invalide", mutate(func(m map[string]any) {
			m["key"].(map[string]any)["pattern"] = "(["
		}), "key.pattern"},
		{"hex", mutate(func(m map[string]any) { m["encoding"].(map[string]any)["hex"] = "mixed" }), "encoding.hex"},
		{"keep alphanumérique", mutate(func(m map[string]any) { m["encoding"].(map[string]any)["keep"] = "a" }), "encoding.keep"},
		{"keep pourcent", mutate(func(m map[string]any) { m["encoding"].(map[string]any)["keep"] = "%" }), "encoding.keep"},
		{"paramètre en double", mutate(func(m map[string]any) {
			m["query"] = append(query(m), map[string]any{"name": "port", "value": "1"})
		}), "double"},
		{"variable inconnue", mutate(func(m map[string]any) {
			m["query"] = append(query(m), map[string]any{"name": "x", "value": "{nope}"})
		}), "inconnue"},
		{"omission inconnue", mutate(func(m map[string]any) {
			m["query"] = append(query(m), map[string]any{"name": "x", "value": "1", "omit": []any{"jamais"}})
		}), "omission"},
		{"event sans omission", mutate(func(m map[string]any) {
			m["query"] = append(query(m), map[string]any{"name": "e2", "value": "{event}"})
		}), "exige"},
		{"gabarit non fermé", mutate(func(m map[string]any) {
			m["query"] = append(query(m), map[string]any{"name": "x", "value": "{port"})
		}), "fermée"},
		{"modificateur inconnu", mutate(func(m map[string]any) {
			m["query"] = append(query(m), map[string]any{"name": "x", "value": "{port:hex}"})
		}), "modificateur"},
		{"info_hash raw", mutate(func(m map[string]any) {
			query(m)[0].(map[string]any)["value"] = "{info_hash:raw}"
		}), "raw"},
		{"sans info_hash", mutate(func(m map[string]any) { m["query"] = query(m)[1:] }), "info_hash"},
		{"Host interdit", mutate(func(m map[string]any) {
			m["headers"] = append(m["headers"].([]any), map[string]any{"name": "host", "value": "x"})
		}), "Host"},
		{"Host sans {host}", mutate(func(m map[string]any) {
			m["headers"] = append(m["headers"].([]any), map[string]any{"name": "Host", "value": "{port}"})
		}), "Host"},
		{"en-tête en double", mutate(func(m map[string]any) {
			m["headers"] = append(m["headers"].([]any), map[string]any{"name": "user-agent", "value": "x"})
		}), "double"},
		{"numwant négatif", mutate(func(m map[string]any) { m["numwant"].(map[string]any)["started"] = -2 }), "numwant"},
		{"numwant manquant", mutate(func(m map[string]any) { delete(m["numwant"].(map[string]any), "stopped") }), "numwant"},
		{"numwant clé en trop", mutate(func(m map[string]any) { m["numwant"].(map[string]any)["paused"] = 1 }), "numwant"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse(c.data)
			if err == nil {
				t.Fatal("profil accepté")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("erreur %q, attendu un message contenant %q", err, c.want)
			}
		})
	}
}

func TestKeyVariableNeedsKeyOrOmission(t *testing.T) {
	var m map[string]any
	if err := json.Unmarshal(baseJSON(t), &m); err != nil {
		t.Fatal(err)
	}
	delete(m, "key") // le paramètre key utilise toujours {key}, sans omission
	data, _ := json.Marshal(m)
	if _, err := Parse(data); err == nil || !strings.Contains(err.Error(), "no-key") {
		t.Errorf("erreur : %v", err)
	}
}

func TestPrefixHex(t *testing.T) {
	p := variant(t, load(t, "qbittorrent-5.3.0"), func(p *Profile) {
		p.PeerID.Prefix = ""
		p.PeerID.PrefixHex = "2d7142" + "3533" + "30302d" // -qB5300-
	})
	e, _ := newEmu(t, p, 1)
	a, err := e.BuildAnnounce(AnnounceParams{Port: 1})
	if err != nil || !strings.HasPrefix(string(a.PeerID), "-qB5300-") {
		t.Errorf("%q %v", a.PeerID, err)
	}
}

func TestNewRequiresDependencies(t *testing.T) {
	if _, err := New(nil, nil, nil, Options{}); err == nil {
		t.Error("New sans dépendances accepté")
	}
}

func TestParseTemplate(t *testing.T) {
	tpl, err := parseTemplate("a{x}b{y:raw}")
	if err != nil || len(tpl) != 4 || tpl[1].variable != "x" || !tpl[3].raw {
		t.Fatalf("%+v %v", tpl, err)
	}
	for _, bad := range []string{"}", "{", "{}", "{a{b}", "{:raw}"} {
		if _, err := parseTemplate(bad); err == nil {
			t.Errorf("%q accepté", bad)
		}
	}
}
