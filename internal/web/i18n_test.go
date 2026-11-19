package web

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"seedmirage/internal/config"
	"seedmirage/internal/i18n"
)

// get performs an authenticated GET with the given language headers.
func (e *setEnv) get(target, acceptLang string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", target, nil)
	r.Host = "127.0.0.1:7070"
	r.AddCookie(&http.Cookie{Name: SessionCookie, Value: e.srv.auth.sessionValue()}) //nolint:gosec // test cookie
	if acceptLang != "" {
		r.Header.Set("Accept-Language", acceptLang)
	}
	for _, c := range cookies {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	return w
}

func TestLanguageNegotiation(t *testing.T) {
	e := newSetEnv(t)
	for _, tc := range []struct {
		name, accept string
		cookie       *http.Cookie
		wantLang     string
		wantText     string
	}{
		{"no header: English default", "", nil, "en", "Dashboard"},
		{"Accept-Language fr", "fr-FR,fr;q=0.9,en;q=0.8", nil, "fr", "Tableau de bord"},
		{"q-values: en preferred", "fr;q=0.4,en;q=0.9", nil, "en", "Dashboard"},
		{"unsupported language: default", "de-DE,de;q=0.9", nil, "en", "Dashboard"},
		{"cookie en wins over Accept-Language fr", "fr", langCookie("en"), "en", "Dashboard"},
		{"cookie fr wins over Accept-Language en", "en", langCookie("fr"), "fr", "Tableau de bord"},
		{"invalid cookie ignored", "fr", langCookie("xx"), "fr", "Tableau de bord"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var cs []*http.Cookie
			if tc.cookie != nil {
				cs = append(cs, tc.cookie)
			}
			w := e.get("/settings", tc.accept, cs...)
			body := w.Body.String()
			if w.Code != 200 {
				t.Fatalf("status %d", w.Code)
			}
			if !strings.Contains(body, `<html lang="`+tc.wantLang+`">`) {
				t.Errorf("html lang is not %q", tc.wantLang)
			}
			if !strings.Contains(body, tc.wantText) {
				t.Errorf("page lacks %q", tc.wantText)
			}
		})
	}
}

func TestSettingsErrorsTranslatedByCode(t *testing.T) {
	e := newSetEnv(t)
	form := formFor(e.be.snap.Config)
	form.Set("uploadRateMin", "900")
	form.Set("uploadRateMax", "100")
	for lang, want := range map[string]string{"en": "must be at least the minimum rate", "fr": "doit être au moins égal au débit minimum"} {
		r := httptest.NewRequest("POST", "/settings", strings.NewReader(form.Encode()))
		r.Host = "127.0.0.1:7070"
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set(CSRFHeader, e.csrf())
		r.Header.Set("Accept-Language", lang)
		r.AddCookie(&http.Cookie{Name: SessionCookie, Value: e.srv.auth.sessionValue()}) //nolint:gosec // test cookie
		w := httptest.NewRecorder()
		e.h.ServeHTTP(w, r)
		if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), want) {
			t.Errorf("%s: status %d, body lacks %q", lang, w.Code, want)
		}
	}
}

func TestConfigIssuesCoverEveryCode(t *testing.T) {
	bad := config.Defaults()
	bad.UploadRateMin, bad.UploadRateMax, bad.MaxActiveTorrents = -1, -2, 0
	bad.Profile, bad.RatioTarget, bad.RatioScope, bad.Port, bad.MaxTrackerFailures = "", -1, "x", -1, 0
	bad.Selection, bad.RotateIdleAfter, bad.SpeedModel = "x", -1, "x"
	bad.MinDownloadRate, bad.MaxDownloadRate = 5, 1
	bad.Caps.PerLeecherMax, bad.Caps.LineMax, bad.Caps.DailyRatioMax, bad.Caps.SwarmShare = -1, -1, -1, 5
	bad.Schedule.Timezone = "Nowhere/Land"
	bad.Schedule.Hourly[3] = 9
	bad.Schedule.Pauses = []config.Pause{{Day: "zzz", Range: "10:00-11:00"}}
	issues := configIssues(bad)
	if len(issues) < 15 {
		t.Fatalf("only %d issues: %v", len(issues), issues)
	}
	bad.Schedule.Pauses = []config.Pause{{Day: "mon", Range: "nope"}}
	for k, m := range configIssues(bad) {
		issues[k] = m
	}
	for field, m := range issues {
		for _, l := range i18n.Supported() {
			if !i18n.Has(l, m.Key) {
				t.Errorf("%s: %s lacks key %s", field, l, m.Key)
			}
		}
	}
}

func TestPostLang(t *testing.T) {
	e := newSetEnv(t)
	post := func(form url.Values, hdr map[string]string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/lang", strings.NewReader(form.Encode()))
		r.Host = "127.0.0.1:7070"
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.AddCookie(&http.Cookie{Name: SessionCookie, Value: e.srv.auth.sessionValue()}) //nolint:gosec // test cookie
		for k, v := range hdr {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		e.h.ServeHTTP(w, r)
		return w
	}
	csrf := map[string]string{CSRFHeader: e.csrf()}

	if w := post(url.Values{"lang": {"fr"}}, nil); w.Code != http.StatusForbidden {
		t.Errorf("without CSRF token: status %d, want 403", w.Code)
	}
	if w := post(url.Values{"lang": {"fr"}}, map[string]string{CSRFHeader: "nope"}); w.Code != http.StatusForbidden {
		t.Errorf("bad CSRF token: status %d, want 403", w.Code)
	}
	if w := post(url.Values{"lang": {"fr"}}, map[string]string{CSRFHeader: e.csrf(), "Origin": "http://evil.example"}); w.Code != http.StatusForbidden {
		t.Errorf("cross-origin: status %d, want 403", w.Code)
	}
	if w := post(url.Values{"lang": {"xx"}}, csrf); w.Code != http.StatusBadRequest {
		t.Errorf("unknown language: status %d, want 400", w.Code)
	}

	w := post(url.Values{"lang": {"fr"}, "next": {"/torrents?tab=archived"}}, csrf)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/torrents?tab=archived" {
		t.Errorf("redirect: %d %q", w.Code, w.Header().Get("Location"))
	}
	var lc *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == LangCookie {
			lc = c
		}
	}
	if lc == nil || lc.Value != "fr" || lc.Path != "/" || lc.SameSite != http.SameSiteStrictMode {
		t.Errorf("lang cookie = %+v", lc)
	}
	if w := post(url.Values{"lang": {"en"}, "next": {"//evil.example/x"}}, csrf); w.Header().Get("Location") != "/" {
		t.Errorf("open redirect: %q", w.Header().Get("Location"))
	}
	hx := post(url.Values{"lang": {"en"}}, map[string]string{CSRFHeader: e.csrf(), "HX-Request": "true"})
	if hx.Code != http.StatusOK || hx.Header().Get("HX-Refresh") != "true" {
		t.Errorf("htmx: %d, HX-Refresh %q", hx.Code, hx.Header().Get("HX-Refresh"))
	}
}

func TestSafeNext(t *testing.T) {
	for next, want := range map[string]string{
		"":                       "/",
		"/":                      "/",
		"/torrents?tab=archived": "/torrents?tab=archived",
		"//evil.example":         "/",
		"/\\evil.example":        "/",
		"https://evil.example/":  "/",
		"javascript:alert(1)":    "/",
		"torrents":               "/",
		"/a\nb":                  "/",
		"/ok/path#frag":          "/ok/path#frag",
	} {
		if got := safeNext(next); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", next, got, want)
		}
	}
}

func TestSelectorPresentAndMarksCurrent(t *testing.T) {
	e := newSetEnv(t)
	body := e.get("/settings", "fr").Body.String()
	for _, want := range []string{`hx-post="/lang"`, `name="lang" value="fr"`, `name="lang" value="en"`, `name="next" value="/settings"`} {
		if !strings.Contains(body, want) {
			t.Errorf("language selector lacks %q", want)
		}
	}
	if !strings.Contains(body, `value="fr" lang="fr" class="btn btn-ghost btn-xs join-item btn-active" aria-pressed="true"`) {
		t.Error("current language not marked")
	}
}

// --- catalog consistency ---------------------------------------------------

func TestCatalogsSameKeysAndNoEmptyValue(t *testing.T) {
	en, fr := i18n.Keys(i18n.EN), i18n.Keys(i18n.FR)
	if len(en) == 0 {
		t.Fatal("empty English catalog")
	}
	if strings.Join(en, "\n") != strings.Join(fr, "\n") {
		inFr := map[string]bool{}
		for _, k := range fr {
			inFr[k] = true
		}
		for _, k := range en {
			if !inFr[k] {
				t.Errorf("key %q missing in fr", k)
			}
		}
		inEn := map[string]bool{}
		for _, k := range en {
			inEn[k] = true
		}
		for _, k := range fr {
			if !inEn[k] {
				t.Errorf("key %q missing in en", k)
			}
		}
	}
	for _, l := range i18n.Supported() {
		for _, k := range i18n.Keys(l) {
			if strings.TrimSpace(i18n.T(l, k)) == "" {
				t.Errorf("%s: empty value for %q", l, k)
			}
		}
	}
}

var templateKey = regexp.MustCompile(`\{\{-?\s*(?:t|tn|th)\s+"([^"]+)"`)

// catalogHas reports whether key (or one of its plural / derived forms) exists.
func catalogHas(key string) bool {
	if i18n.Has(i18n.EN, key) || i18n.Has(i18n.EN, key+".one") || i18n.Has(i18n.EN, key+".label") {
		return true
	}
	for _, k := range i18n.Keys(i18n.EN) {
		if strings.HasPrefix(k, key+".") {
			return true
		}
	}
	return false
}

func readTemplates(t *testing.T) map[string]string {
	t.Helper()
	files, err := filepath.Glob("templates/*.html")
	if err != nil || len(files) == 0 {
		t.Fatalf("templates: %v", err)
	}
	out := map[string]string{}
	for _, f := range files {
		b, err := os.ReadFile(f) //nolint:gosec // fixed glob of the package templates
		if err != nil {
			t.Fatal(err)
		}
		out[f] = string(b)
	}
	return out
}

func TestTemplateKeysExist(t *testing.T) {
	n := 0
	for f, src := range readTemplates(t) {
		for _, m := range templateKey.FindAllStringSubmatch(src, -1) {
			n++
			if !i18n.Has(i18n.EN, m[1]) && !i18n.Has(i18n.EN, m[1]+".other") {
				t.Errorf("%s: unknown key %q", f, m[1])
			}
		}
	}
	if n < 100 {
		t.Errorf("only %d keys found in the templates: extraction broken?", n)
	}
}

// goKeyNamespaces are the catalog namespaces whose keys the Go code writes as
// string literals (field names such as "schedule.pauses" are not among them).
var goKeyNamespaces = []string{"settings.", "toast.", "err.", "reason.", "status.", "upload.", "torrent.", "torrents.", "profiles.", "profile.", "stats.", "time.", "common.", "dash."}

func TestGoKeysExist(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	n := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(af, func(nd ast.Node) bool {
			bl, ok := nd.(*ast.BasicLit)
			if !ok || bl.Kind != token.STRING {
				return true
			}
			v, err := strconv.Unquote(bl.Value)
			if err != nil || strings.ContainsAny(v, " \t\n") {
				return true
			}
			for _, ns := range goKeyNamespaces {
				if strings.HasPrefix(v, ns) && len(v) > len(ns) {
					n++
					if !catalogHas(strings.TrimSuffix(v, ".")) {
						t.Errorf("%s: unknown catalog key %q", fset.Position(bl.Pos()), v)
					}
				}
			}
			return true
		})
	}
	if n < 40 {
		t.Errorf("only %d Go keys found: extraction broken?", n)
	}
}

func TestSettingsFieldsHaveCatalogText(t *testing.T) {
	for _, sd := range settingsSections() {
		for _, l := range i18n.Supported() {
			if !i18n.Has(l, "settings.sec."+sd.ID+".title") {
				t.Errorf("%s: section %s has no title", l, sd.ID)
			}
			for _, fd := range sd.Fields {
				for _, suffix := range []string{".label", ".help"} {
					if !i18n.Has(l, "settings."+fd.Name+suffix) {
						t.Errorf("%s: field %s lacks %s", l, fd.Name, suffix)
					}
				}
				for _, o := range fd.Options {
					if !i18n.Has(l, o.Key) {
						t.Errorf("%s: option %s lacks key %s", l, o.Value, o.Key)
					}
				}
			}
		}
	}
	for _, r := range []string{"ratio", "idle", "failures", "startFailed"} {
		if !i18n.Has(i18n.FR, "reason."+r) {
			t.Errorf("reason.%s missing", r)
		}
	}
	for r, k := range reasonKeys {
		if !i18n.Has(i18n.EN, k) {
			t.Errorf("reason %s: key %s missing", r, k)
		}
	}
}

// --- no hard-coded text in the templates ------------------------------------

var (
	actionRe = regexp.MustCompile(`(?s)\{\{.*?\}\}`)
	tagRe    = regexp.MustCompile(`(?s)<[^>]*>`)
	attrRe   = regexp.MustCompile(`\s(?:aria-label|title|placeholder|alt|hx-confirm)="([^"]*)"`)
	letterRe = regexp.MustCompile(`\p{L}`)
	entityRe = regexp.MustCompile(`&[a-z]+;`)
)

// textExceptions are the words that may stay in the templates: the product
// name, protocol abbreviations (S / L: seeders / leechers), identifiers and
// the /metrics route shown as a link.
var textExceptions = []string{"seedmirage", "info_hash", "URL", "/metrics"}

// visibleText returns the letters-bearing text nodes and attribute values of
// src that are not template actions.
func visibleText(src string) []string {
	var out []string
	for _, m := range attrRe.FindAllStringSubmatch(src, -1) {
		out = append(out, m[1])
	}
	noAction := actionRe.ReplaceAllString(src, "\x00") // an action splits text nodes
	for _, node := range strings.Split(tagRe.ReplaceAllString(noAction, "\x01"), "\x01") {
		out = append(out, strings.Split(node, "\x00")...)
	}
	return out
}

func TestNoHardcodedTextInTemplates(t *testing.T) {
	for f, src := range readTemplates(t) {
		// attribute values that are template actions or contain one are fine
		for _, s := range visibleText(src) {
			if strings.Contains(s, "{{") {
				continue
			}
			s = entityRe.ReplaceAllString(s, "")
			for _, w := range textExceptions {
				s = strings.ReplaceAll(s, w, "")
			}
			// "S / L" and "S / <a>L</a>": single letters standing for seeders / leechers
			s = strings.NewReplacer("S /", "", " L", "", "L ", "").Replace(s)
			if strings.TrimSpace(s) == "L" {
				continue
			}
			if letterRe.MatchString(s) {
				t.Errorf("%s: hard-coded text %q (use t / tn / th)", f, strings.TrimSpace(s))
			}
		}
	}
}

func TestHardcodedTextDetectorCatchesText(t *testing.T) {
	for _, src := range []string{`<p>Hello</p>`, `<button aria-label="Close">x</button>`, `<a>{{.X}} Bonjour</a>`} {
		found := false
		for _, s := range visibleText(src) {
			if !strings.Contains(s, "{{") && letterRe.MatchString(s) {
				found = true
			}
		}
		if !found {
			t.Errorf("detector missed text in %s", src)
		}
	}
}

func TestEveryPageRendersInBothLanguages(t *testing.T) {
	e := newSetEnv(t)
	var missing []string
	for _, lang := range []string{"en", "fr"} {
		for _, p := range []string{"/", "/settings", "/status"} {
			w := e.get(p, lang)
			if w.Code != 200 {
				t.Errorf("%s %s: status %d", lang, p, w.Code)
			}
			b, _ := io.ReadAll(w.Body)
			// an unresolved key is rendered as itself: look for catalog-like tokens
			for _, m := range regexp.MustCompile(`>\s*((?:nav|dash|settings|status|toast)\.[A-Za-z.]+)\s*<`).FindAllStringSubmatch(string(b), -1) {
				missing = append(missing, lang+" "+p+" "+m[1])
			}
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("untranslated keys displayed: %v", missing)
	}
}

// langCookie is a request cookie as the browser would send it.
func langCookie(v string) *http.Cookie {
	return &http.Cookie{Name: LangCookie, Value: v, Path: "/", HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode}
}
