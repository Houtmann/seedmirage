package web

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"seedmirage/internal/config"
	"seedmirage/internal/engine"
)

// maxSettingsBody bounds the size of a settings form.
const maxSettingsBody = 64 << 10

// configBackend is the optional part of the engine the settings page needs
// (*engine.Engine implements it). Kept apart from Backend so that backends
// without configuration support still work.
type configBackend interface {
	ApplyConfig(cfg config.Config) error
	ProfileChoices() []engine.ProfileChoice
}

// fieldDef describes one input of the form. Name is the JSON key of the field
// in config.json ("caps.lineMax" for the nested ones): the test of coverage
// checks that every key of config.Config has an input.
type fieldDef struct {
	Name, Label, Help, Kind string // Kind: number | text | select | checkbox | textarea | hourly
	Step                    string
	Options                 []optionView // for select (profile: filled at render time)
	get                     func(c *config.Config) string
	set                     func(c *config.Config, raw string) error
}

type optionView struct {
	Value, Label string
	Selected     bool
}

type fieldView struct {
	Name, Label, Help, Kind, Step string
	Value, Error                  string
	Checked                       bool
	Options                       []optionView
	Hours                         []hourView   // hourly: one slider per hour
	Presets                       []presetView // hourly: ready-made curves
}

// hourView is one slider of the 24-hour grid.
type hourView struct {
	Label, Value string
}

// presetView is a ready-made hourly curve; Values is the comma-separated list
// of the 24 multipliers.
type presetView struct {
	Label, Values string
}

type sectionView struct {
	ID, Title, Intro string
	Fields           []fieldView
}

type settingsView struct {
	Sections []sectionView
	Error    string // global error (not tied to a field)
}

type sectionDef struct {
	ID, Title, Intro string
	Fields           []fieldDef
}

func intField(name, label, help string, get func(*config.Config) *int) fieldDef {
	return fieldDef{
		Name: name, Label: label, Help: help, Kind: "number", Step: "1",
		get: func(c *config.Config) string { return strconv.Itoa(*get(c)) },
		set: func(c *config.Config, raw string) error {
			n, err := strconv.Atoi(strings.TrimSpace(raw))
			if err != nil {
				return errors.New("nombre entier attendu")
			}
			*get(c) = n
			return nil
		},
	}
}

func floatField(name, label, help string, get func(*config.Config) *float64) fieldDef {
	return fieldDef{
		Name: name, Label: label, Help: help, Kind: "number", Step: "any",
		get: func(c *config.Config) string { return strconv.FormatFloat(*get(c), 'f', -1, 64) },
		set: func(c *config.Config, raw string) error {
			f, err := strconv.ParseFloat(strings.ReplaceAll(strings.TrimSpace(raw), ",", "."), 64)
			if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
				return errors.New("nombre attendu")
			}
			*get(c) = f
			return nil
		},
	}
}

func boolField(name, label, help string, get func(*config.Config) *bool) fieldDef {
	return fieldDef{
		Name: name, Label: label, Help: help, Kind: "checkbox",
		get: func(c *config.Config) string {
			if *get(c) {
				return "on"
			}
			return ""
		},
		set: func(c *config.Config, raw string) error { *get(c) = raw != ""; return nil },
	}
}

func selectField(name, label, help string, opts []optionView, get func(*config.Config) *string) fieldDef {
	return fieldDef{
		Name: name, Label: label, Help: help, Kind: "select", Options: opts,
		get: func(c *config.Config) string { return *get(c) },
		set: func(c *config.Config, raw string) error { *get(c) = raw; return nil },
	}
}

// schedulePresets are the ready-made curves of the schedule section. Values
// are the multipliers reached at 00h, 01h ... 23h (0 = silent, 2 = twice the
// speed of the model), interpolated linearly in between.
//   - constant: 1 all day.
//   - soirée: quiet night (0,3), low daytime (0,6-0,8), evening peak 19h-22h (1,6).
//   - bureau: machine on at the office: low outside 8h-18h, two peaks 10h-12h and 14h-17h (1,4).
func schedulePresets() []presetView {
	return []presetView{
		{"Constant", "1,1,1,1,1,1,1,1,1,1,1,1,1,1,1,1,1,1,1,1,1,1,1,1"},
		{"Soirée", "0.8,0.5,0.3,0.3,0.3,0.3,0.3,0.5,0.6,0.6,0.6,0.6,0.8,0.8,0.8,0.8,0.8,0.8,1.2,1.6,1.6,1.6,1.6,1.2"},
		{"Bureau", "0.2,0.2,0.2,0.2,0.2,0.2,0.2,0.4,1,1.4,1.4,1.4,1,1,1.4,1.4,1.4,1.4,0.8,0.4,0.4,0.4,0.2,0.2"},
	}
}

func hourlyField() fieldDef {
	return fieldDef{
		Name: "schedule.hourly", Label: "Coefficient par heure", Kind: "hourly",
		Help: "Multiplie la vitesse du modèle (0 = silence, 1 = inchangée, 2 = double). Atteint à l'heure pile, interpolé entre deux heures.",
		get: func(c *config.Config) string {
			parts := make([]string, len(c.Schedule.Hourly))
			for i, v := range c.Schedule.Hourly {
				parts[i] = strconv.FormatFloat(v, 'f', -1, 64)
			}
			return strings.Join(parts, ",")
		},
		set: func(c *config.Config, raw string) error {
			parts := strings.Split(raw, ",")
			if len(parts) != len(c.Schedule.Hourly) {
				return errors.New("24 coefficients attendus")
			}
			var h [24]float64
			for i, p := range parts {
				f, err := strconv.ParseFloat(strings.TrimSpace(p), 64)
				if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
					return errors.New("coefficients numériques attendus")
				}
				h[i] = f
			}
			c.Schedule.Hourly = h
			return nil
		},
	}
}

func pausesField() fieldDef {
	return fieldDef{
		Name: "schedule.pauses", Label: "Pauses", Kind: "textarea",
		Help: "Une par ligne : jour (lun, mar, mer, jeu, ven, sam, dim) puis plage HH:MM-HH:MM, par exemple « lun 22:00-23:30 ». Une plage qui passe minuit (« ven 23:00-02:00 ») se termine le lendemain. Les torrents sont arrêtés proprement pendant la pause, puis repris.",
		get: func(c *config.Config) string {
			lines := make([]string, len(c.Schedule.Pauses))
			for i, p := range c.Schedule.Pauses {
				lines[i] = p.Day + " " + p.Range
			}
			return strings.Join(lines, "\n")
		},
		set: func(c *config.Config, raw string) error {
			var out []config.Pause
			for i, line := range strings.Split(strings.ReplaceAll(raw, "\r", ""), "\n") {
				if strings.TrimSpace(line) == "" {
					continue
				}
				f := strings.Fields(line)
				if len(f) != 2 {
					return fmt.Errorf("ligne %d : « jour HH:MM-HH:MM » attendu", i+1)
				}
				day, ok := config.CanonicalDay(f[0])
				if !ok {
					return fmt.Errorf("ligne %d : jour inconnu « %s »", i+1, f[0])
				}
				if _, _, err := config.ParseRange(f[1]); err != nil {
					return fmt.Errorf("ligne %d : plage HH:MM-HH:MM attendue", i+1)
				}
				out = append(out, config.Pause{Day: day, Range: f[1]})
			}
			c.Schedule.Pauses = out
			return nil
		},
	}
}

// settingsSections is the form layout.
func settingsSections() []sectionDef {
	return []sectionDef{
		{ID: "speed", Title: "Vitesse", Intro: "Débit d'upload annoncé, en ko/s. Appliqué immédiatement.", Fields: []fieldDef{
			intField("uploadRateMin", "Débit minimum (ko/s)", "Borne basse de la vitesse globale.", func(c *config.Config) *int { return &c.UploadRateMin }),
			intField("uploadRateMax", "Débit maximum (ko/s)", "Borne haute ; doit être au moins égale au minimum.", func(c *config.Config) *int { return &c.UploadRateMax }),
			selectField("speedModel", "Modèle de vitesse", "Lissé : variation continue et bruit par torrent ; uniforme : tirage aléatoire entre les bornes.",
				[]optionView{{Value: config.SpeedModelSmooth, Label: "Lissé"}, {Value: config.SpeedModelUniform, Label: "Uniforme"}},
				func(c *config.Config) *string { return &c.SpeedModel }),
		}},
		{ID: "torrents", Title: "Torrents", Intro: "Combien de torrents sont seedés et comment ils sont choisis.", Fields: []fieldDef{
			intField("maxActiveTorrents", "Torrents actifs", "Nombre maximal de torrents annoncés en même temps. Le réduire arrête les plus récents.", func(c *config.Config) *int { return &c.MaxActiveTorrents }),
			selectField("selection", "Choix des torrents", "Essaim : privilégie les torrents qui ont des leechers (scrape) ; aléatoire : tirage simple.",
				[]optionView{{Value: config.SelectionSwarm, Label: "Selon l'essaim"}, {Value: config.SelectionRandom, Label: "Aléatoire"}},
				func(c *config.Config) *string { return &c.Selection }),
			boolField("keepIdleTorrents", "Garder les torrents sans leecher", "Sinon, un torrent sans leecher est écarté.", func(c *config.Config) *bool { return &c.KeepIdleTorrents }),
			{
				Name: "rotateIdleAfter", Label: "Rotation des torrents inactifs", Kind: "text",
				Help: "Durée sans leecher (« 6h », « 90m ») après laquelle un torrent est remplacé ; 0 = jamais. Demande « garder les torrents sans leecher » et le choix selon l'essaim.",
				get:  func(c *config.Config) string { return time.Duration(c.RotateIdleAfter).String() },
				set: func(c *config.Config, raw string) error {
					raw = strings.TrimSpace(raw)
					if raw == "0" {
						c.RotateIdleAfter = 0
						return nil
					}
					d, err := time.ParseDuration(raw)
					if err != nil {
						return errors.New("durée attendue, par exemple 6h ou 90m")
					}
					c.RotateIdleAfter = config.Duration(d)
					return nil
				},
			},
			floatField("ratioTarget", "Ratio cible", "Un torrent qui atteint ce ratio est arrêté ; 0 = désactivé.", func(c *config.Config) *float64 { return &c.RatioTarget }),
			selectField("ratioScope", "Ratio calculé sur", "La session en cours ou l'upload cumulé de tous les lancements.",
				[]optionView{{Value: config.RatioScopeSession, Label: "La session"}, {Value: config.RatioScopeTotal, Label: "Le cumul"}},
				func(c *config.Config) *string { return &c.RatioScope }),
			boolField("autoStart", "Démarrer au lancement", "Commence à seeder dès que « serve » démarre (au prochain lancement).", func(c *config.Config) *bool { return &c.AutoStart }),
		}},
		{ID: "client", Title: "Client émulé", Intro: "Changer de profil redémarre les annonces (un « stopped » est envoyé avec l'ancien client).", Fields: []fieldDef{
			{
				Name: "profile", Label: "Profil", Kind: "select", Help: "Famille et version ; les plus récentes en premier.",
				get: func(c *config.Config) string { return c.Profile },
				set: func(c *config.Config, raw string) error { c.Profile = strings.TrimSpace(raw); return nil },
			},
		}},
		{ID: "network", Title: "Réseau", Intro: "Changer le port ou l'IP annoncée redémarre les annonces.", Fields: []fieldDef{
			intField("port", "Port annoncé", "Entre 1 et 65535 ; 0 conserve le port actuel.", func(c *config.Config) *int { return &c.Port }),
			{
				Name: "announceIP", Label: "IP annoncée", Kind: "text",
				Help: "Vide : non envoyée (le tracker voit déjà votre IP).",
				get:  func(c *config.Config) string { return c.AnnounceIP },
				set:  func(c *config.Config, raw string) error { c.AnnounceIP = strings.TrimSpace(raw); return nil },
			},
		}},
		{ID: "realism", Title: "Réalisme", Intro: "Plafonds plausibles par torrent et téléchargement simulé.", Fields: []fieldDef{
			intField("caps.perLeecherMax", "Plafond par leecher (ko/s)", "Vitesse maximale rapportée à un leecher ; 0 = sans plafond.", func(c *config.Config) *int { return &c.Caps.PerLeecherMax }),
			intField("caps.lineMax", "Ligne montante déclarée (ko/s)", "Plafond de la vitesse globale ; 0 = illimité.", func(c *config.Config) *int { return &c.Caps.LineMax }),
			floatField("caps.dailyRatioMax", "Ratio maximal par 24 h", "L'upload sur 24 h reste sous ce ratio multiplié par la taille ; 0 = désactivé.", func(c *config.Config) *float64 { return &c.Caps.DailyRatioMax }),
			floatField("caps.swarmShare", "Part maximale de l'essaim", "Part de la demande de l'essaim qu'un torrent peut absorber, dans ]0 ; 1].", func(c *config.Config) *float64 { return &c.Caps.SwarmShare }),
			boolField("simulateDownload", "Simuler un téléchargement", "Un torrent vu pour la première fois « télécharge » d'abord, puis seede. Ne vaut que pour les torrents ajoutés ensuite.", func(c *config.Config) *bool { return &c.SimulateDownload }),
			intField("minDownloadRate", "Téléchargement minimum (ko/s)", "Vitesse de téléchargement simulée, borne basse.", func(c *config.Config) *int { return &c.MinDownloadRate }),
			intField("maxDownloadRate", "Téléchargement maximum (ko/s)", "Vitesse de téléchargement simulée, borne haute.", func(c *config.Config) *int { return &c.MaxDownloadRate }),
		}},
		{ID: "schedule", Title: "Planning horaire", Intro: "Un rythme de vie : plus de vitesse le soir, moins la nuit, et des pauses pendant lesquelles le seed est arrêté. Appliqué immédiatement.", Fields: []fieldDef{
			boolField("schedule.enabled", "Activer le planning", "Désactivé : vitesse constante et aucune pause.", func(c *config.Config) *bool { return &c.Schedule.Enabled }),
			{
				Name: "schedule.timezone", Label: "Fuseau horaire", Kind: "text",
				Help: "Nom IANA (« Europe/Paris ») ; vide : fuseau de la machine (variable TZ en Docker).",
				get:  func(c *config.Config) string { return c.Schedule.Timezone },
				set:  func(c *config.Config, raw string) error { c.Schedule.Timezone = strings.TrimSpace(raw); return nil },
			},
			hourlyField(),
			pausesField(),
		}},
		{ID: "advanced", Title: "Avancé", Fields: []fieldDef{
			intField("maxTrackerFailures", "Échecs de tracker tolérés", "Au-delà, le torrent est écarté. Ne vaut que pour les torrents démarrés ensuite : les torrents en cours gardent l'ancienne valeur.", func(c *config.Config) *int { return &c.MaxTrackerFailures }),
		}},
	}
}

// buildSettings builds the view. values are the raw inputs to show (nil: the
// values of cfg), errs the field errors.
func buildSettings(cfg config.Config, values map[string]string, errs map[string]string, profiles []engine.ProfileChoice) settingsView {
	var v settingsView
	for _, sd := range settingsSections() {
		sec := sectionView{ID: sd.ID, Title: sd.Title, Intro: sd.Intro}
		for _, fd := range sd.Fields {
			val := fd.get(&cfg)
			if raw, ok := values[fd.Name]; ok {
				val = raw
			}
			f := fieldView{Name: fd.Name, Label: fd.Label, Help: fd.Help, Kind: fd.Kind, Step: fd.Step, Value: val, Error: errs[fd.Name]}
			switch fd.Kind {
			case "hourly":
				f.Presets = schedulePresets()
				parts := strings.Split(val, ",")
				for i := range 24 {
					hv := hourView{Label: fmt.Sprintf("%02dh", i), Value: "1"}
					if i < len(parts) {
						hv.Value = strings.TrimSpace(parts[i])
					}
					f.Hours = append(f.Hours, hv)
				}
			case "checkbox":
				f.Checked = val != ""
			case "select":
				opts := fd.Options
				if fd.Name == "profile" {
					opts = nil
					known := false
					for _, p := range profiles {
						opts = append(opts, optionView{Value: p.Name, Label: p.Family + " " + p.Version})
						known = known || p.Name == val
					}
					if !known && val != "" {
						opts = append([]optionView{{Value: val, Label: val}}, opts...)
					}
				}
				f.Options = make([]optionView, len(opts))
				for i, o := range opts {
					o.Selected = o.Value == val
					f.Options[i] = o
				}
			}
			sec.Fields = append(sec.Fields, f)
		}
		v.Sections = append(v.Sections, sec)
	}
	return v
}

// parseSettings applies the posted form to a copy of cur. It returns the new
// configuration, the raw values (to redisplay) and the errors by field.
func parseSettings(cur config.Config, form map[string][]string) (config.Config, map[string]string, map[string]string) {
	next := cur
	values := map[string]string{}
	errs := map[string]string{}
	for _, sd := range settingsSections() {
		for _, fd := range sd.Fields {
			raw := ""
			if vs := form[fd.Name]; len(vs) > 0 {
				raw = vs[0]
				if fd.Kind == "hourly" {
					raw = strings.Join(vs, ",") // one slider per hour, same name
				}
			}
			values[fd.Name] = raw
			if fd.Kind != "checkbox" && !has(form, fd.Name) {
				continue // absent field: unchanged
			}
			if err := fd.set(&next, raw); err != nil {
				errs[fd.Name] = err.Error()
			}
		}
	}
	// An absent field keeps its current value in the redisplay too.
	for _, sd := range settingsSections() {
		for _, fd := range sd.Fields {
			if fd.Kind != "checkbox" && !has(form, fd.Name) {
				values[fd.Name] = fd.get(&cur)
			}
		}
	}
	if next.Port == 0 {
		next.Port = cur.Port // 0 = keep the current port
	}
	if next.AnnounceIP != "" {
		if _, err := netip.ParseAddr(next.AnnounceIP); err != nil {
			errs["announceIP"] = "adresse IP invalide"
		}
	}
	return next, values, errs
}

func has(form map[string][]string, k string) bool { _, ok := form[k]; return ok }

// settingsHandlers groups the routes of the settings page. mu serialises the
// "validate, apply, save" sequence so two tabs cannot interleave.
type settingsHandlers struct {
	s    *Server
	be   configBackend
	snap func() config.Config
	path string
	mu   sync.Mutex
}

func (s *Server) registerSettings(mux *http.ServeMux, be configBackend) {
	h := &settingsHandlers{s: s, be: be, snap: func() config.Config { return s.deps.Engine.Snapshot().Config }, path: s.deps.ConfigPath}
	mux.Handle("GET /settings", s.protect(http.HandlerFunc(h.page)))
	mux.Handle("GET /settings/form", s.protect(http.HandlerFunc(h.formFragment)))
	mux.Handle("POST /settings", s.protect(http.HandlerFunc(h.save)))
}

func (h *settingsHandlers) page(w http.ResponseWriter, r *http.Request) {
	pd := h.s.newPageData(r, true)
	pd.Data = buildSettings(h.snap(), nil, nil, h.be.ProfileChoices())
	h.s.render(w, http.StatusOK, "settings", pd)
}

func (h *settingsHandlers) formFragment(w http.ResponseWriter, _ *http.Request) {
	h.write(w, http.StatusOK, buildSettings(h.snap(), nil, nil, h.be.ProfileChoices()), nil)
}

// write renders the form fragment, followed by out-of-band toasts.
func (h *settingsHandlers) write(w http.ResponseWriter, status int, v settingsView, toasts []toastView) {
	var sb strings.Builder
	if err := h.s.pages["settings"].ExecuteTemplate(&sb, "settingsform", v); err != nil {
		h.s.log.Error("render settings", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	for _, t := range toasts {
		toast, terr := h.s.renderFragment("toast", t)
		if terr != nil {
			continue
		}
		sb.WriteString(`<div hx-swap-oob="beforeend:#toasts">` + toast + `</div>`)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(sb.String()))
}

// save handles POST /settings. Order: parse and validate, then ApplyConfig,
// then Save. ApplyConfig can still refuse a validated configuration (unknown
// profile): saving last means a refused change never reaches config.json, so
// the file and the running engine cannot disagree about a rejected edit. If
// the write itself fails after a successful apply, the change is live but not
// persisted, which the user is told.
func (h *settingsHandlers) save(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxSettingsBody)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	cur := h.snap()
	next, values, errs := parseSettings(cur, r.PostForm)
	// Validate even when some fields failed to parse: the other fields still
	// get their message (a field that failed to parse keeps its own).
	var fe config.FieldErrors
	if err := next.Validate(); errors.As(err, &fe) {
		for k, m := range fe {
			if _, ok := errs[k]; !ok {
				errs[k] = m
			}
		}
	}
	profiles := h.be.ProfileChoices()
	if len(errs) > 0 {
		h.write(w, http.StatusUnprocessableEntity, buildSettings(cur, values, errs, profiles),
			[]toastView{{Kind: "error", Text: "Réglages invalides : corrigez les champs signalés."}})
		return
	}
	if err := h.be.ApplyConfig(next); err != nil {
		msg := sanitizeError(err.Error())
		var fe config.FieldErrors
		if errors.As(err, &fe) {
			h.write(w, http.StatusUnprocessableEntity, buildSettings(cur, values, fe, profiles),
				[]toastView{{Kind: "error", Text: "Réglages invalides : corrigez les champs signalés."}})
			return
		}
		h.s.log.Warn("settings: apply refused", "err", err)
		v := buildSettings(cur, values, nil, profiles)
		v.Error = msg
		h.write(w, http.StatusUnprocessableEntity, v, []toastView{{Kind: "error", Text: "Réglages refusés : " + msg}})
		return
	}
	toasts := []toastView{{Kind: "info", Text: "Réglages appliqués."}}
	if err := next.Save(h.path); err != nil {
		h.s.log.Error("settings: save", "err", err)
		toasts = []toastView{{Kind: "warning", Text: "Réglages appliqués mais non enregistrés : " + sanitizeError(err.Error())}}
	}
	if next.Profile != cur.Profile || next.Port != cur.Port || next.AnnounceIP != cur.AnnounceIP {
		toasts = append(toasts, toastView{Kind: "warning", Text: "Les annonces redémarrent avec ce changement (profil, port ou IP)."})
	}
	if next.MaxTrackerFailures != cur.MaxTrackerFailures {
		toasts = append(toasts, toastView{Kind: "info", Text: "Échecs de tracker tolérés : valable pour les torrents démarrés ensuite."})
	}
	h.write(w, http.StatusOK, buildSettings(next, nil, nil, profiles), toasts)
}
