// Package dashboard serves the embedded admin web UI: a single-binary,
// htmx-driven control panel for the proxy (health, config, tokens, logs,
// metrics). Static assets (htmx, pico, app.css) and templates are vendored
// and embedded via go:embed — no runtime CDN, no node build step, and no
// change to the distribution model.
//
// Every page handler renders either a full page (plain requests) or the bare
// content fragment (htmx requests, detected via the HX-Request header), so
// the same URL serves the initial load and the live-updating polls.
package dashboard

import (
	"bytes"
	"embed"
	"html/template"
	"log/slog"
	"net/http"
	"time"

	"freebuff-proxy/internal/config"
	"freebuff-proxy/internal/pool"
	"freebuff-proxy/internal/registry"
)

//go:embed assets templates
var files embed.FS

// AssetsFS exposes the vendored static assets (htmx, pico, app.css) for the
// server to mount under /admin/assets/. Embedded paths carry the "assets/"
// prefix, so the server strips "/admin/" before serving.
func AssetsFS() embed.FS { return files }

// Dashboard renders the admin UI over the live pool, registry, and config.
type Dashboard struct {
	cfg     func() *config.Config // returns the current (hot-reloadable) config
	pool    *pool.Pool
	reg     *registry.Registry
	logger  *slog.Logger
	started time.Time
	tpl     *template.Template
}

// New builds the dashboard. cfg must return the current configuration — the
// server passes its atomic pointer loader so /admin/reload is reflected
// immediately. A nil logger falls back to slog.Default(). Template parse
// failures panic: the templates are embedded, so a parse error is a build
// invariant violation, not a runtime condition.
func New(cfg func() *config.Config, p *pool.Pool, reg *registry.Registry, logger *slog.Logger) *Dashboard {
	if logger == nil {
		logger = slog.Default()
	}
	tpl, err := template.ParseFS(files, "templates/*.tmpl")
	if err != nil {
		panic("dashboard: embedded template parse failed: " + err.Error())
	}
	return &Dashboard{cfg: cfg, pool: p, reg: reg, logger: logger, started: time.Now(), tpl: tpl}
}

// layoutData carries the pre-rendered page body into the layout shell. The
// body is template.HTML deliberately: it was produced by executing one of
// our own escaped content templates, so no second escaping pass applies.
type layoutData struct {
	Body template.HTML
}

// isHX reports whether the request came from htmx (fragment, not full page).
func isHX(r *http.Request) bool { return r.Header.Get("HX-Request") == "true" }

// render executes content (as a bare fragment for htmx requests, or inside
// the layout for plain navigation).
func (d *Dashboard) render(w http.ResponseWriter, r *http.Request, content string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if isHX(r) {
		if err := d.tpl.ExecuteTemplate(w, content, data); err != nil {
			d.logger.Error("dashboard fragment render failed", "template", content, "err", err)
		}
		return
	}
	var buf bytes.Buffer
	if err := d.tpl.ExecuteTemplate(&buf, content, data); err != nil {
		d.logger.Error("dashboard page render failed", "template", content, "err", err)
		return
	}
	if err := d.tpl.ExecuteTemplate(w, "layout", layoutData{Body: template.HTML(buf.String())}); err != nil {
		d.logger.Error("dashboard layout render failed", "err", err)
	}
}

// Page returns a handler for the named content template, wired to its data.
func (d *Dashboard) Page(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		d.render(w, r, name, d.dataFor(name))
	}
}

// RenderLogin renders the login page with an optional error message.
func (d *Dashboard) RenderLogin(w http.ResponseWriter, r *http.Request, errMsg string) {
	d.render(w, r, "login", loginData{Error: errMsg})
}

// dataFor resolves the page data for a named content template.
func (d *Dashboard) dataFor(name string) any {
	switch name {
	case "overview":
		return d.overviewData()
	default:
		return nil
	}
}

// --- overview ---

type overviewData struct {
	Mode                 string // "bridge" | "pooled"
	InBridge             bool
	BridgeTokens         int
	Models               int
	Uptime               string
	Rotation             string
	SafeMode             bool
	MaxMessagesPerDay    int
	TransientRetries     int64
	FingerprintRotations int64
	Tokens               []tokenCard
	HasTokens            bool
}

type tokenCard struct {
	Index            int
	SessionStatus    string
	QueuePosition    int
	QueueDepth       int
	ActiveRuns       int
	Requests         int
	Messages24h      int
	DailyLimit       int
	UsagePct         int
	RiskLevel        string
	CooldownActive   bool
	CooldownUntil    string
	TransientRetries int64
}

type loginData struct {
	Error string
}

func (d *Dashboard) overviewData() overviewData {
	cfg := d.cfg()
	ps := d.pool.PoolSnapshot()
	od := overviewData{
		Mode:                 "pooled",
		Models:               d.reg.ModelCount(),
		Uptime:               time.Since(d.started).Round(time.Second).String(),
		Rotation:             cfg.ProxyRotation,
		SafeMode:             cfg.SafeMode,
		MaxMessagesPerDay:    cfg.MaxMessagesPerDay,
		TransientRetries:     ps.TransientRetries,
		FingerprintRotations: ps.FingerprintRotations,
		BridgeTokens:         d.pool.BridgeCount(),
	}
	if cfg.BridgeMode() {
		od.Mode = "bridge"
		od.InBridge = true
	}
	for _, t := range ps.Tokens {
		card := tokenCard{
			Index:            t.Token,
			SessionStatus:    t.SessionStatus,
			QueuePosition:    t.SessionQueuePosition,
			QueueDepth:       t.SessionQueueDepth,
			ActiveRuns:       t.ActiveRuns,
			Requests:         t.Requests,
			Messages24h:      t.Messages24h,
			DailyLimit:       t.DailyLimit,
			UsagePct:         t.UsagePct,
			RiskLevel:        t.RiskLevel,
			TransientRetries: t.TransientRetries,
		}
		if !t.CooldownUntil.IsZero() && time.Now().Before(t.CooldownUntil) {
			card.CooldownActive = true
			card.CooldownUntil = t.CooldownUntil.Format(time.RFC3339)
		}
		od.Tokens = append(od.Tokens, card)
	}
	od.HasTokens = len(od.Tokens) > 0
	return od
}
