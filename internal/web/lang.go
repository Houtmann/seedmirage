package web

import (
	"html/template"
	"net/http"
	"net/url"
	"strings"

	"seedmirage/internal/i18n"
)

// LangCookie is the cookie holding the interface language. It is a
// preference, not a secret: no HttpOnly needed, but it never leaves the site.
const LangCookie = "lang"

// langOption is one entry of the language selector.
type langOption struct {
	Code   string // "en"
	Label  string // "EN"
	Active bool
}

func langOptions(cur i18n.Lang) []langOption {
	var out []langOption
	for _, l := range i18n.Supported() {
		out = append(out, langOption{Code: string(l), Label: strings.ToUpper(string(l)), Active: l == cur})
	}
	return out
}

// langOf negotiates the language of a request: the lang cookie, then
// Accept-Language, then English.
func (s *Server) langOf(r *http.Request) i18n.Lang {
	if c, err := r.Cookie(LangCookie); err == nil {
		if l, ok := i18n.Parse(c.Value); ok {
			return l
		}
	}
	return i18n.Match(r.Header.Get("Accept-Language"))
}

// setLang handles POST /lang (behind protect: session, CSRF header and Origin
// check). It stores the choice in a cookie; htmx clients reload the page, the
// others are sent back to a validated local path.
func (s *Server) setLang(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	lang, ok := i18n.Parse(r.PostForm.Get("lang"))
	if !ok || len(r.PostForm.Get("lang")) > 2 {
		http.Error(w, "unsupported language", http.StatusBadRequest)
		return
	}
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // a language preference, not a secret: HttpOnly/Secure add nothing on plain loopback HTTP
		Name:     LangCookie,
		Value:    string(lang),
		Path:     "/",
		SameSite: http.SameSiteStrictMode,
		MaxAge:   365 * 24 * 3600,
	})
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Refresh", "true")
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, safeNext(r.PostForm.Get("next")), http.StatusSeeOther) //nolint:gosec // safeNext only lets local paths through
}

// safeNext returns next when it is a local path (no scheme, no host, no
// protocol-relative form), "/" otherwise: no open redirect.
func safeNext(next string) string {
	if next == "" || next[0] != '/' || strings.HasPrefix(next, "//") || strings.Contains(next, `\`) {
		return "/"
	}
	for _, c := range next {
		if c < 0x20 || c == 0x7f {
			return "/"
		}
	}
	u, err := url.Parse(next)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil {
		return "/"
	}
	return next
}

// tFunc is the "t" template function of a language: {{t "key" args...}}.
func tFunc(lang i18n.Lang) func(string, ...any) string {
	return func(key string, args ...any) string { return i18n.T(lang, key, args...) }
}

// tnFunc is the "tn" template function: {{tn "key" n args...}}.
func tnFunc(lang i18n.Lang) func(string, int, ...any) string {
	return func(key string, n int, args ...any) string { return i18n.TN(lang, key, n, args...) }
}

// thFunc is the "th" template function for catalog entries that carry markup
// (<code>, <strong>): the text comes from the embedded catalog, the arguments
// are HTML-escaped.
func thFunc(lang i18n.Lang) func(string, ...any) template.HTML {
	return func(key string, args ...any) template.HTML {
		esc := make([]any, len(args))
		for i, a := range args {
			if str, ok := a.(string); ok {
				esc[i] = template.HTMLEscapeString(str)
			} else {
				esc[i] = a
			}
		}
		return template.HTML(i18n.T(lang, key, esc...)) //nolint:gosec // trusted embedded catalog, arguments escaped above
	}
}
