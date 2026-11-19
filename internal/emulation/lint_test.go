package emulation

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// freshProfile renvoie une copie indépendante d'un profil embarqué.
func freshProfile(t *testing.T, name string) *Profile {
	t.Helper()
	data, err := EmbeddedData(name)
	if err != nil {
		t.Fatal(err)
	}
	var p Profile
	if err := json.Unmarshal(data, &p); err != nil {
		t.Fatal(err)
	}
	return &p
}

func hasFinding(fs []Finding, level, code string) bool {
	for _, f := range fs {
		if f.Level == level && f.Code == code {
			return true
		}
	}
	return false
}

func setHeader(p *Profile, name, value string) {
	for i := range p.Headers {
		if strings.EqualFold(p.Headers[i].Name, name) {
			p.Headers[i].Value = value
			return
		}
	}
	p.Headers = append(p.Headers, Header{Name: name, Value: value})
}

func dropHeader(p *Profile, name string) {
	var hs []Header
	for _, h := range p.Headers {
		if !strings.EqualFold(h.Name, name) {
			hs = append(hs, h)
		}
	}
	p.Headers = hs
}

func TestLintRules(t *testing.T) {
	tests := []struct {
		name      string
		base      string
		mutate    func(p *Profile)
		wantLevel string
		wantCode  string
	}{
		{"invalid", "qbittorrent-5.3.0", func(p *Profile) { p.Source = "bogus" }, LevelError, CodeInvalid},
		{"host header", "qbittorrent-5.3.0", func(p *Profile) { p.Headers = append(p.Headers, Header{"Host", "x"}) }, LevelError, CodeHostHeader},
		{"duplicate param", "qbittorrent-5.3.0", func(p *Profile) { p.Query = append(p.Query, Param{Name: "port", Value: "1"}) }, LevelError, CodeDuplicateParam},
		{"no user-agent", "qbittorrent-5.3.0", func(p *Profile) { dropHeader(p, "User-Agent") }, LevelWarn, CodeNoUserAgent},
		{"ua version", "qbittorrent-5.3.0", func(p *Profile) { setHeader(p, "User-Agent", "qBittorrent/4.6.7") }, LevelError, CodeUAVersion},
		{"qb prefix", "qbittorrent-5.3.0", func(p *Profile) { p.PeerID.Prefix = "-qB4670-" }, LevelError, CodePeerIDPrefix},
		{"transmission prefix", "transmission-3.00", func(p *Profile) { p.PeerID.Prefix = "-TR4140-" }, LevelError, CodePeerIDPrefix},
		{"deluge prefix", "deluge-2.2.0", func(p *Profile) { p.PeerID.Prefix = "-DE210s-" }, LevelError, CodePeerIDPrefix},
		{"numwant", "qbittorrent-5.3.0", func(p *Profile) { p.Numwant["started"] = 50 }, LevelWarn, CodeNumwant},
		{"accept-encoding other", "qbittorrent-5.3.0", func(p *Profile) { setHeader(p, "Accept-Encoding", "gzip, deflate") }, LevelWarn, CodeAcceptEncoding},
		{"accept-encoding absent", "transmission-4.1.4", func(p *Profile) { dropHeader(p, "Accept-Encoding") }, LevelWarn, CodeAcceptEncoding},
		{"accept-decode", "qbittorrent-5.3.0", func(p *Profile) { setHeader(p, "Accept-Encoding", "gzip, br;q=0.5") }, LevelWarn, CodeAcceptDecode},
		{"outdated", "qbittorrent-5.3.0", func(p *Profile) { p.Version, p.Name = "1.0.0", "qbittorrent-1.0.0" }, LevelWarn, CodeOutdated},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := freshProfile(t, tt.base)
			tt.mutate(p)
			fs := Lint(p)
			if !hasFinding(fs, tt.wantLevel, tt.wantCode) {
				t.Errorf("findings = %v, want %s %s", fs, tt.wantLevel, tt.wantCode)
			}
		})
	}
}

func TestLintAcceptEncodingNotSentByFamily(t *testing.T) {
	old := families["qbittorrent"]
	t.Cleanup(func() { families["qbittorrent"] = old })
	tr := old
	tr.acceptEncoding = ""
	families["qbittorrent"] = tr
	fs := Lint(freshProfile(t, "qbittorrent-5.3.0"))
	if !hasFinding(fs, LevelWarn, CodeAcceptEncoding) {
		t.Errorf("findings = %v", fs)
	}
}

func TestLintSpecificRulesReplaceInvalid(t *testing.T) {
	p := freshProfile(t, "qbittorrent-5.3.0")
	p.Headers = append(p.Headers, Header{"host", "x"})
	if fs := Lint(p); hasFinding(fs, LevelError, CodeInvalid) {
		t.Errorf("Validate redit le même défaut : %v", fs)
	}
}

func TestLintVersionPrefixSkipped(t *testing.T) {
	// Transmission 4.10.0 : composante > 9 (base 62), la règle est sautée.
	p := freshProfile(t, "transmission-4.1.4")
	p.Version, p.Name = "4.10.0", "transmission-4.10.0"
	setHeader(p, "User-Agent", "Transmission/4.10.0")
	p.PeerID.Prefix = "-TR4A00-"
	if fs := Lint(p); hasFinding(fs, LevelError, CodePeerIDPrefix) {
		t.Errorf("findings = %v", fs)
	}
}

func TestLintEmbeddedProfilesClean(t *testing.T) {
	for _, name := range EmbeddedNames() {
		t.Run(name, func(t *testing.T) {
			p, err := LoadEmbedded(name)
			if err != nil {
				t.Fatal(err)
			}
			if fs := Lint(p); len(fs) != 0 {
				t.Errorf("findings = %v", fs)
			}
		})
	}
}

func TestLintData(t *testing.T) {
	if fs := LintData([]byte(`{"schema": 1, "bogus": 2}`)); !hasFinding(fs, LevelError, CodeParse) {
		t.Errorf("findings = %v", fs)
	}
	data, err := EmbeddedData("deluge-2.2.0")
	if err != nil {
		t.Fatal(err)
	}
	if fs := LintData(data); len(fs) != 0 {
		t.Errorf("findings = %v", fs)
	}
	if HasError([]Finding{{LevelWarn, "x", ""}}) || !HasError([]Finding{{LevelError, "x", ""}}) {
		t.Error("HasError")
	}
}

func TestLintHostPlacement(t *testing.T) {
	p := freshProfile(t, "qbittorrent-5.3.0")
	p.Headers = slices.Insert(p.Headers, 1, Header{"Host", HostPlaceholder})
	if fs := Lint(p); hasFinding(fs, LevelError, CodeHostHeader) || hasFinding(fs, LevelError, CodeInvalid) {
		t.Errorf("Host {host} doit être permis : %v", fs)
	}
	p.Headers[1].Value = "x"
	if fs := Lint(p); !hasFinding(fs, LevelError, CodeHostHeader) {
		t.Errorf("Host avec une autre valeur doit être refusé : %v", fs)
	}
}
