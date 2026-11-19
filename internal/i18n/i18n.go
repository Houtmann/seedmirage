// Package i18n translates the web interface: embedded catalogs (en, fr),
// language negotiation and locale-aware formatting. It is pure (no I/O).
package i18n

import (
	"embed"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
)

//go:embed locales/*.json
var localesFS embed.FS

// Lang is a supported interface language.
type Lang string

// Supported languages; English is the default.
const (
	EN      Lang = "en"
	FR      Lang = "fr"
	Default      = EN
)

// Supported returns the languages in display order.
func Supported() []Lang { return []Lang{EN, FR} }

// Parse returns the language for a code such as "fr" or "fr-CH".
func Parse(code string) (Lang, bool) {
	base := strings.ToLower(strings.TrimSpace(code))
	if i := strings.IndexAny(base, "-_"); i >= 0 {
		base = base[:i]
	}
	for _, l := range Supported() {
		if string(l) == base {
			return l, true
		}
	}
	return Default, false
}

// Match negotiates a language from an Accept-Language header (q-values
// honoured, region ignored). It returns Default when nothing matches.
func Match(accept string) Lang {
	type cand struct {
		lang Lang
		q    float64
	}
	var cands []cand
	for _, part := range strings.Split(accept, ",") {
		fields := strings.Split(part, ";")
		tag := strings.TrimSpace(fields[0])
		if tag == "" {
			continue
		}
		q := 1.0
		for _, p := range fields[1:] {
			p = strings.TrimSpace(p)
			if v, ok := strings.CutPrefix(p, "q="); ok {
				f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
				if err != nil || f < 0 || f > 1 {
					f = 0
				}
				q = f
			}
		}
		if q <= 0 {
			continue
		}
		if tag == "*" {
			cands = append(cands, cand{Default, q})
			continue
		}
		if l, ok := Parse(tag); ok {
			cands = append(cands, cand{l, q})
		}
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].q > cands[j].q })
	if len(cands) == 0 {
		return Default
	}
	return cands[0].lang
}

type catalogs map[Lang]map[string]string

// loadCatalogs parses the embedded catalogs.
func loadCatalogs() (catalogs, error) {
	out := catalogs{}
	for _, l := range Supported() {
		b, err := localesFS.ReadFile("locales/" + string(l) + ".json")
		if err != nil {
			return nil, fmt.Errorf("i18n %s: %w", l, err)
		}
		m := map[string]string{}
		if err := json.Unmarshal(b, &m); err != nil {
			return nil, fmt.Errorf("i18n %s: %w", l, err)
		}
		out[l] = m
	}
	return out, nil
}

var loaded = sync.OnceValue(func() catalogs {
	c, err := loadCatalogs()
	if err != nil {
		return catalogs{} // never panic: keys are returned as is (the tests catch a broken catalog)
	}
	return c
})

// Has reports whether the catalog of lang defines key.
func Has(lang Lang, key string) bool {
	_, ok := loaded()[lang][key]
	return ok
}

// Keys returns the sorted keys of the catalog of lang.
func Keys(lang Lang) []string {
	m := loaded()[lang]
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func lookup(lang Lang, key string) (string, bool) {
	if s, ok := loaded()[lang][key]; ok {
		return s, true
	}
	if s, ok := loaded()[Default][key]; ok {
		return s, true
	}
	return "", false
}

// T translates key into lang. With arguments the text is a fmt format
// (positional verbs). Unknown key: English, then the key itself.
func T(lang Lang, key string, args ...any) string {
	s, ok := lookup(lang, key)
	if !ok {
		return key
	}
	if len(args) == 0 {
		return s
	}
	return fmt.Sprintf(s, args...)
}

// pluralForm is "one" or "other" for n in lang (French treats 0 and 1 as singular).
func pluralForm(lang Lang, n int) string {
	if lang == FR {
		if n == 0 || n == 1 {
			return "one"
		}
		return "other"
	}
	if n == 1 {
		return "one"
	}
	return "other"
}

// TN translates the plural family key.one / key.other according to n. The
// arguments are those of T (n is usually repeated as the first one).
func TN(lang Lang, key string, n int, args ...any) string {
	if s, ok := lookup(lang, key+"."+pluralForm(lang, n)); ok {
		if len(args) == 0 {
			return s
		}
		return fmt.Sprintf(s, args...)
	}
	if s, ok := lookup(lang, key+".other"); ok {
		return fmt.Sprintf(s, args...)
	}
	return T(lang, key, args...)
}
