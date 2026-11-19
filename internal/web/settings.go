package web

import (
	"errors"
	"math"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"seedmirage/internal/config"
	"seedmirage/internal/engine"
	"seedmirage/internal/i18n"
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
// checks that every key of config.Config has an input. Its label and help are
// the catalog keys "settings.<Name>.label" and "settings.<Name>.help".
type fieldDef struct {
	Name, Kind string // Kind: number | text | select | checkbox | textarea | hourly
	Step       string
	Options    []optionView // for select (profile: filled at render time)
	get        func(c *config.Config) string
	set        func(c *config.Config, raw string) error
}

// fieldMsg is a translatable field error: a catalog key and its arguments.
type fieldMsg struct {
	Key  string
	Args []any
}

func (m fieldMsg) Error() string { return m.Key }

type optionView struct {
	Value, Label string
	Key          string // catalog key of the label ("" : Label is shown as is)
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
	ID     string
	Fields []fieldDef
}

func intField(name string, get func(*config.Config) *int) fieldDef {
	return fieldDef{
		Name: name, Kind: "number", Step: "1",
		get: func(c *config.Config) string { return strconv.Itoa(*get(c)) },
		set: func(c *config.Config, raw string) error {
			n, err := strconv.Atoi(strings.TrimSpace(raw))
			if err != nil {
				return fieldMsg{Key: "err.int"}
			}
			*get(c) = n
			return nil
		},
	}
}

func floatField(name string, get func(*config.Config) *float64) fieldDef {
	return fieldDef{
		Name: name, Kind: "number", Step: "any",
		get: func(c *config.Config) string { return strconv.FormatFloat(*get(c), 'f', -1, 64) },
		set: func(c *config.Config, raw string) error {
			f, err := strconv.ParseFloat(strings.ReplaceAll(strings.TrimSpace(raw), ",", "."), 64)
			if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
				return fieldMsg{Key: "err.number"}
			}
			*get(c) = f
			return nil
		},
	}
}

func boolField(name string, get func(*config.Config) *bool) fieldDef {
	return fieldDef{
		Name: name, Kind: "checkbox",
		get: func(c *config.Config) string {
			if *get(c) {
				return "on"
			}
			return ""
		},
		set: func(c *config.Config, raw string) error { *get(c) = raw != ""; return nil },
	}
}

func selectField(name string, opts []optionView, get func(*config.Config) *string) fieldDef {
	return fieldDef{
		Name: name, Kind: "select", Options: opts,
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
func schedulePresets(lang i18n.Lang) []presetView {
	return []presetView{
		{i18n.T(lang, "settings.preset.constant"), "1,1,1,1,1,1,1,1,1,1,1,1,1,1,1,1,1,1,1,1,1,1,1,1"},
		{i18n.T(lang, "settings.preset.evening"), "0.8,0.5,0.3,0.3,0.3,0.3,0.3,0.5,0.6,0.6,0.6,0.6,0.8,0.8,0.8,0.8,0.8,0.8,1.2,1.6,1.6,1.6,1.6,1.2"},
		{i18n.T(lang, "settings.preset.office"), "0.2,0.2,0.2,0.2,0.2,0.2,0.2,0.4,1,1.4,1.4,1.4,1,1,1.4,1.4,1.4,1.4,0.8,0.4,0.4,0.4,0.2,0.2"},
	}
}

func hourlyField() fieldDef {
	return fieldDef{
		Name: "schedule.hourly", Kind: "hourly",
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
				return fieldMsg{Key: "err.hours24"}
			}
			var h [24]float64
			for i, p := range parts {
				f, err := strconv.ParseFloat(strings.TrimSpace(p), 64)
				if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
					return fieldMsg{Key: "err.hoursNumeric"}
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
		Name: "schedule.pauses", Kind: "textarea",
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
					return fieldMsg{Key: "err.pauseLine", Args: []any{i + 1}}
				}
				day, ok := config.CanonicalDay(f[0])
				if !ok {
					return fieldMsg{Key: "err.pauseDay", Args: []any{i + 1, f[0]}}
				}
				if _, _, err := config.ParseRange(f[1]); err != nil {
					return fieldMsg{Key: "err.pauseRange", Args: []any{i + 1}}
				}
				out = append(out, config.Pause{Day: day, Range: f[1]})
			}
			c.Schedule.Pauses = out
			return nil
		},
	}
}

// settingsSections is the form layout. Titles and intros are the catalog keys
// "settings.sec.<ID>.title" and ".intro".
func settingsSections() []sectionDef {
	return []sectionDef{
		{ID: "speed", Fields: []fieldDef{
			intField("uploadRateMin", func(c *config.Config) *int { return &c.UploadRateMin }),
			intField("uploadRateMax", func(c *config.Config) *int { return &c.UploadRateMax }),
			selectField("speedModel",
				[]optionView{{Value: config.SpeedModelSmooth, Key: "settings.speedModel.opt.smooth"}, {Value: config.SpeedModelUniform, Key: "settings.speedModel.opt.uniform"}},
				func(c *config.Config) *string { return &c.SpeedModel }),
		}},
		{ID: "torrents", Fields: []fieldDef{
			intField("maxActiveTorrents", func(c *config.Config) *int { return &c.MaxActiveTorrents }),
			selectField("selection",
				[]optionView{{Value: config.SelectionSwarm, Key: "settings.selection.opt.swarm"}, {Value: config.SelectionRandom, Key: "settings.selection.opt.random"}},
				func(c *config.Config) *string { return &c.Selection }),
			boolField("keepIdleTorrents", func(c *config.Config) *bool { return &c.KeepIdleTorrents }),
			{
				Name: "rotateIdleAfter", Kind: "text",
				get: func(c *config.Config) string { return time.Duration(c.RotateIdleAfter).String() },
				set: func(c *config.Config, raw string) error {
					raw = strings.TrimSpace(raw)
					if raw == "0" {
						c.RotateIdleAfter = 0
						return nil
					}
					d, err := time.ParseDuration(raw)
					if err != nil {
						return fieldMsg{Key: "err.duration"}
					}
					c.RotateIdleAfter = config.Duration(d)
					return nil
				},
			},
			floatField("ratioTarget", func(c *config.Config) *float64 { return &c.RatioTarget }),
			selectField("ratioScope",
				[]optionView{{Value: config.RatioScopeSession, Key: "settings.ratioScope.opt.session"}, {Value: config.RatioScopeTotal, Key: "settings.ratioScope.opt.total"}},
				func(c *config.Config) *string { return &c.RatioScope }),
			boolField("autoStart", func(c *config.Config) *bool { return &c.AutoStart }),
		}},
		{ID: "client", Fields: []fieldDef{
			{
				Name: "profile", Kind: "select",
				get: func(c *config.Config) string { return c.Profile },
				set: func(c *config.Config, raw string) error { c.Profile = strings.TrimSpace(raw); return nil },
			},
		}},
		{ID: "network", Fields: []fieldDef{
			intField("port", func(c *config.Config) *int { return &c.Port }),
			{
				Name: "announceIP", Kind: "text",
				get: func(c *config.Config) string { return c.AnnounceIP },
				set: func(c *config.Config, raw string) error { c.AnnounceIP = strings.TrimSpace(raw); return nil },
			},
		}},
		{ID: "realism", Fields: []fieldDef{
			intField("caps.perLeecherMax", func(c *config.Config) *int { return &c.Caps.PerLeecherMax }),
			intField("caps.lineMax", func(c *config.Config) *int { return &c.Caps.LineMax }),
			floatField("caps.dailyRatioMax", func(c *config.Config) *float64 { return &c.Caps.DailyRatioMax }),
			floatField("caps.swarmShare", func(c *config.Config) *float64 { return &c.Caps.SwarmShare }),
			boolField("caps.swarmBudget", func(c *config.Config) *bool { return &c.Caps.SwarmBudget }),
			boolField("simulateDownload", func(c *config.Config) *bool { return &c.SimulateDownload }),
			intField("minDownloadRate", func(c *config.Config) *int { return &c.MinDownloadRate }),
			intField("maxDownloadRate", func(c *config.Config) *int { return &c.MaxDownloadRate }),
		}},
		{ID: "schedule", Fields: []fieldDef{
			boolField("schedule.enabled", func(c *config.Config) *bool { return &c.Schedule.Enabled }),
			{
				Name: "schedule.timezone", Kind: "text",
				get: func(c *config.Config) string { return c.Schedule.Timezone },
				set: func(c *config.Config, raw string) error { c.Schedule.Timezone = strings.TrimSpace(raw); return nil },
			},
			hourlyField(),
			pausesField(),
		}},
		{ID: "advanced", Fields: []fieldDef{
			intField("maxTrackerFailures", func(c *config.Config) *int { return &c.MaxTrackerFailures }),
			boolField("updateCheck", func(c *config.Config) *bool { return &c.UpdateCheck }),
			boolField("autoUpdate", func(c *config.Config) *bool { return &c.AutoUpdate }),
		}},
	}
}

// buildSettings builds the view. values are the raw inputs to show (nil: the
// values of cfg), errs the field errors.
func buildSettings(lang i18n.Lang, cfg config.Config, values map[string]string, errs map[string]fieldMsg, profiles []engine.ProfileChoice) settingsView {
	var v settingsView
	for _, sd := range settingsSections() {
		sec := sectionView{ID: sd.ID, Title: i18n.T(lang, "settings.sec."+sd.ID+".title")}
		if i18n.Has(lang, "settings.sec."+sd.ID+".intro") {
			sec.Intro = i18n.T(lang, "settings.sec."+sd.ID+".intro")
		}
		for _, fd := range sd.Fields {
			val := fd.get(&cfg)
			if raw, ok := values[fd.Name]; ok {
				val = raw
			}
			f := fieldView{Name: fd.Name, Label: i18n.T(lang, "settings."+fd.Name+".label"), Help: i18n.T(lang, "settings."+fd.Name+".help"), Kind: fd.Kind, Step: fd.Step, Value: val}
			if m, ok := errs[fd.Name]; ok {
				f.Error = i18n.T(lang, m.Key, m.Args...)
			}
			switch fd.Kind {
			case "hourly":
				f.Presets = schedulePresets(lang)
				parts := strings.Split(val, ",")
				for i := range 24 {
					hv := hourView{Label: i18n.T(lang, "settings.hourLabel", i), Value: "1"}
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
					if o.Key != "" {
						o.Label = i18n.T(lang, o.Key)
					}
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
func parseSettings(cur config.Config, form map[string][]string) (config.Config, map[string]string, map[string]fieldMsg) {
	next := cur
	values := map[string]string{}
	errs := map[string]fieldMsg{}
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
				var m fieldMsg
				if !errors.As(err, &m) {
					m = fieldMsg{Key: "err.number"}
				}
				errs[fd.Name] = m
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
			errs["announceIP"] = fieldMsg{Key: "err.ip"}
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
	pd.Data = buildSettings(h.s.langOf(r), h.snap(), nil, nil, h.be.ProfileChoices())
	h.s.render(w, http.StatusOK, "settings", pd)
}

func (h *settingsHandlers) formFragment(w http.ResponseWriter, r *http.Request) {
	lang := h.s.langOf(r)
	h.write(w, lang, http.StatusOK, buildSettings(lang, h.snap(), nil, nil, h.be.ProfileChoices()), nil)
}

// write renders the form fragment, followed by out-of-band toasts.
func (h *settingsHandlers) write(w http.ResponseWriter, lang i18n.Lang, status int, v settingsView, toasts []toastView) {
	var sb strings.Builder
	if err := h.s.page(lang, "settings").ExecuteTemplate(&sb, "settingsform", v); err != nil {
		h.s.log.Error("render settings", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	for _, t := range toasts {
		toast, terr := h.s.renderFragment(lang, "toast", t)
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
	lang := h.s.langOf(r)
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
	for k, m := range configIssues(next) {
		if _, ok := errs[k]; !ok {
			errs[k] = m
		}
	}
	profiles := h.be.ProfileChoices()
	invalid := []toastView{{Kind: "error", Text: i18n.T(lang, "toast.settingsInvalid")}}
	if len(errs) > 0 {
		h.write(w, lang, http.StatusUnprocessableEntity, buildSettings(lang, cur, values, errs, profiles), invalid)
		return
	}
	if err := h.be.ApplyConfig(next); err != nil {
		msg := sanitizeError(err.Error())
		var fe config.FieldErrors
		if errors.As(err, &fe) {
			h.write(w, lang, http.StatusUnprocessableEntity, buildSettings(lang, cur, values, configIssues(next), profiles), invalid)
			return
		}
		h.s.log.Warn("settings: apply refused", "err", err)
		v := buildSettings(lang, cur, values, nil, profiles)
		v.Error = msg
		h.write(w, lang, http.StatusUnprocessableEntity, v, []toastView{{Kind: "error", Text: i18n.T(lang, "toast.settingsRefused", msg)}})
		return
	}
	toasts := []toastView{{Kind: "info", Text: i18n.T(lang, "toast.settingsApplied")}}
	if err := next.Save(h.path); err != nil {
		h.s.log.Error("settings: save", "err", err)
		toasts = []toastView{{Kind: "warning", Text: i18n.T(lang, "toast.settingsNotSaved", sanitizeError(err.Error()))}}
	}
	if next.Profile != cur.Profile || next.Port != cur.Port || next.AnnounceIP != cur.AnnounceIP {
		toasts = append(toasts, toastView{Kind: "warning", Text: i18n.T(lang, "toast.restart")})
	}
	if next.MaxTrackerFailures != cur.MaxTrackerFailures {
		toasts = append(toasts, toastView{Kind: "info", Text: i18n.T(lang, "toast.trackerFailures")})
	}
	h.write(w, lang, http.StatusOK, buildSettings(lang, next, nil, nil, profiles), toasts)
}

// configIssues turns the validation problems of cfg into translatable field
// errors (by stable code, never by translating the English message).
func configIssues(cfg config.Config) map[string]fieldMsg {
	out := map[string]fieldMsg{}
	for k, is := range cfg.Issues() {
		out[k] = fieldMsg{Key: "err.cfg." + is.Code, Args: is.Args}
	}
	return out
}
