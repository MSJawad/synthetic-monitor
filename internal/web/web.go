// Package web serves the public status page and its JSON equivalent.
//
// The page is rendered on the server and runs no JavaScript: charts are
// inline SVG, tooltips are SVG titles and the page refreshes itself. It
// names checks but never shows their targets or raw errors, which can
// reveal internal hostnames and addresses.
package web

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/MSJawad/synthetic-monitor/internal/config"
	"github.com/MSJawad/synthetic-monitor/internal/probe"
	"github.com/MSJawad/synthetic-monitor/internal/slo"
	"github.com/MSJawad/synthetic-monitor/internal/store"
)

// refreshSeconds is how often the page reloads itself.
const refreshSeconds = 30

var (
	//go:embed templates/status.html
	pageTemplate string
	//go:embed static
	staticFiles embed.FS

	tmpl = template.Must(template.New("status").Parse(pageTemplate))
	// cssURL carries a hash of the stylesheet, so browsers can cache it for
	// good and still pick up every change.
	cssURL = func() string {
		css, err := staticFiles.ReadFile("static/style.css")
		if err != nil {
			panic(err)
		}
		sum := sha256.Sum256(css)
		return "/static/style.css?v=" + hex.EncodeToString(sum[:6])
	}()
)

// Source supplies what the status page shows.
type Source struct {
	// Version identifies the running build, reported by /api/status so a
	// deploy can be confirmed from outside.
	Version string
	// Checks returns the configured checks, in config order.
	Checks func() []config.Check
	// Recent returns a check's last n probe results, oldest first.
	Recent func(ctx context.Context, check string, n int) ([]store.Sample, error)
	// SLOs returns the latest SLO status of each check that has one.
	SLOs func() map[string]slo.Status
	// Firing returns when each firing alert's failures began.
	Firing func() map[string]time.Time
	// LastPing returns when a heartbeat check was last pinged. Nil means
	// never.
	LastPing func(check string) (time.Time, bool)
	// Days returns a check's daily probe totals from the day containing
	// since, oldest first.
	Days func(ctx context.Context, check string, since time.Time) ([]store.Day, error)
	// Incidents returns the incidents open or ended since a time: the
	// open ones first, then the rest, each newest first.
	Incidents func(ctx context.Context, since time.Time) ([]store.Incident, error)
}

type handler struct {
	src    Source
	logger *slog.Logger
	now    func() time.Time
}

// Handler serves the status page at /, its JSON form at /api/status, and
// the stylesheet under /static/.
func Handler(src Source, logger *slog.Logger) http.Handler {
	return newHandler(src, logger, time.Now)
}

func newHandler(src Source, logger *slog.Logger, now func() time.Time) http.Handler {
	h := &handler{src: src, logger: logger, now: now}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", h.page)
	mux.HandleFunc("GET /api/status", h.api)
	mux.Handle("GET /static/", immutable(http.FileServerFS(staticFiles)))
	return secure(mux)
}

func (h *handler) page(w http.ResponseWriter, r *http.Request) {
	v := buildPage(h.inputs(r.Context()), h.now())
	data := struct {
		pageView
		CSS     string
		Refresh int
		Favicon template.URL
	}{v, cssURL, refreshSeconds, favicon(v.State)}

	// Render fully before writing, so a template error cannot send half a
	// page with a 200 status.
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		h.logger.Error("rendering status page", "err", err)
		http.Error(w, "status page unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = buf.WriteTo(w)
}

// apiStatus is the JSON form of the status page.
type apiStatus struct {
	Version   string     `json:"version"`
	Status    state      `json:"status"`
	Generated time.Time  `json:"generated"`
	Checks    []apiCheck `json:"checks"`
	// Incidents are those open or ended over the days the page shows:
	// the open ones first, then the rest, each newest first.
	Incidents []apiIncident `json:"incidents"`
}

type apiCheck struct {
	Name         string    `json:"name"`
	Type         string    `json:"type"`
	State        state     `json:"state"`
	FailingSince time.Time `json:"failing_since,omitzero"`
	LastPing     time.Time `json:"last_ping,omitzero"` // heartbeat checks only
	LastProbe    *apiProbe `json:"last_probe,omitempty"`
	SLO          *apiSLO   `json:"slo,omitempty"`
	// Days are the daily probe totals over the days the page shows, oldest
	// first; days without probes are left out.
	Days []apiDay `json:"days"`
}

type apiDay struct {
	Date   string `json:"date"` // in UTC, like 2026-10-21
	Probes int    `json:"probes"`
	Failed int    `json:"failed"`
}

type apiIncident struct {
	Check string `json:"check"`
	// Kind is "down" for an incident raised by failures in a row, or
	// "flapping" for one raised by probes failing on and off.
	Kind   string       `json:"kind"`
	Start  time.Time    `json:"start"`
	End    time.Time    `json:"end,omitzero"` // absent while the incident is open
	Reason probe.Reason `json:"reason,omitempty"`
}

type apiProbe struct {
	At        time.Time    `json:"at"`
	OK        bool         `json:"ok"`
	LatencyMS float64      `json:"latency_ms"`
	Reason    probe.Reason `json:"reason,omitempty"`
}

type apiSLO struct {
	Target     float64 `json:"target"`
	WindowDays int     `json:"window_days"`
	// Since is when the period measured begins: the window's start, or
	// the check's first probe while its history is shorter than the window.
	Since                time.Time `json:"since"`
	Probes               int       `json:"probes"`
	FailedProbes         int       `json:"failed_probes"`
	Availability         float64   `json:"availability"`
	ErrorBudgetRemaining float64   `json:"error_budget_remaining"`
}

func (h *handler) api(w http.ResponseWriter, r *http.Request) {
	inputs := h.inputs(r.Context())
	now := h.now()
	v := buildPage(inputs, now)
	out := apiStatus{Version: h.src.Version, Status: v.State, Generated: now.UTC(), Checks: []apiCheck{},
		Incidents: []apiIncident{}}
	for i, in := range inputs.checks {
		c := apiCheck{Name: in.check.Name, Type: in.check.Type(), State: v.Checks[i].State,
			FailingSince: in.firingSince.UTC(), LastPing: in.lastPing.UTC(), Days: []apiDay{}}
		for _, d := range in.days {
			c.Days = append(c.Days, apiDay{Date: d.Start.UTC().Format(time.DateOnly), Probes: d.Probes, Failed: d.Failed})
		}
		if n := len(in.samples); n > 0 {
			last := in.samples[n-1]
			c.LastProbe = &apiProbe{At: last.Start, OK: last.OK,
				LatencyMS: float64(last.Duration) / float64(time.Millisecond), Reason: last.Reason}
		}
		if in.slo != nil && in.check.SLO != nil {
			c.SLO = &apiSLO{Target: in.slo.Target, WindowDays: in.check.SLO.WindowDays, Since: in.slo.Since.UTC(),
				Probes: in.slo.Total, FailedProbes: in.slo.Failed, Availability: in.slo.Availability,
				ErrorBudgetRemaining: in.slo.BudgetRemaining}
		}
		out.Checks = append(out.Checks, c)
	}
	for _, in := range inputs.incidents {
		kind := "down"
		if in.Burn {
			kind = "flapping"
		}
		out.Incidents = append(out.Incidents, apiIncident{Check: in.Check, Kind: kind, Start: in.Start.UTC(),
			End: in.End.UTC(), Reason: in.Reason})
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(out); err != nil {
		h.logger.Warn("writing status JSON", "err", err)
	}
}

// inputs gathers everything known about each configured check.
func (h *handler) inputs(ctx context.Context) pageInput {
	slos, firing := h.src.SLOs(), h.src.Firing()
	checks := h.src.Checks()
	// The history starts at midnight on the first day shown.
	since := midnight(h.now()).AddDate(0, 0, 1-historyDays)
	var page pageInput
	incidents, err := h.src.Incidents(ctx, since)
	if err != nil {
		h.logger.Error("reading incidents for the status page", "err", err)
		page.incidentsUnavailable = true
	}
	byCheck := make(map[string][]store.Incident)
	for _, in := range incidents {
		byCheck[in.Check] = append(byCheck[in.Check], in)
	}
	configured := make(map[string]bool, len(checks))
	for _, c := range checks {
		configured[c.Name] = true
	}
	// Incidents of checks since removed from the config are left out.
	for _, in := range incidents {
		if configured[in.Check] {
			page.incidents = append(page.incidents, in)
		}
	}
	for _, c := range checks {
		in := checkInput{check: c, firingSince: firing[c.Name]}
		if c.Heartbeat != nil && h.src.LastPing != nil {
			in.lastPing, in.pinged = h.src.LastPing(c.Name)
		}
		if s, ok := slos[c.Name]; ok {
			in.slo = &s
		}
		samples, err := h.src.Recent(ctx, c.Name, recentProbes)
		if err != nil {
			h.logger.Error("reading recent probes for the status page", "check", c.Name, "err", err)
			in.unavailable = true
		}
		in.samples = samples
		days, err := h.src.Days(ctx, c.Name, since)
		if err != nil {
			h.logger.Error("reading daily totals for the status page", "check", c.Name, "err", err)
			in.daysUnavailable = true
		}
		in.days, in.incidents = days, byCheck[c.Name]
		page.checks = append(page.checks, in)
	}
	return page
}

// secure adds headers that stop the page being framed, sniffed or used to
// run injected content: with no scripts and no inline styles, its
// Content-Security-Policy can forbid both outright.
func secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy",
			"default-src 'none'; style-src 'self'; img-src data:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

// immutable lets browsers cache static files for a year; their URLs change
// whenever their contents do.
func immutable(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		next.ServeHTTP(w, r)
	})
}

// favicon returns a dot in the overall state's color, so a browser tab
// shows trouble at a glance.
func favicon(s state) template.URL {
	color := map[state]string{stateUp: "#0ca30c", stateFailing: "#fab219", stateRecovering: "#fab219",
		stateDown: "#d03b3b", statePending: "#898781"}[s]
	svg := `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 16 16"><circle cx="8" cy="8" r="7" fill="` + color + `"/></svg>`
	return template.URL("data:image/svg+xml," + url.PathEscape(svg)) //nolint:gosec // built from constants, not input
}
