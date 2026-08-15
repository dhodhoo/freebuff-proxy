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
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"os"
	"strconv"
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
	case "config":
		return d.configData()
	default:
		return nil
	}
}

// RenderConfigResult renders the htmx response fragment after a config save
// attempt (success or validation failure).
func (d *Dashboard) RenderConfigResult(w http.ResponseWriter, r *http.Request, ok bool, message string) {
	d.render(w, r, "config_result", configResultData{OK: ok, Message: message})
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

// --- config editor ---

type configData struct {
	EnvContent string     // current .env text (or a commented template)
	HasEnvFile bool       // whether ./.env existed on disk
	Effective  []configKV // effective values, secrets redacted
}

type configKV struct {
	Key    string
	Value  string
	Secret bool // rendered as a redacted summary, never the raw value
}

type configResultData struct {
	OK      bool
	Message string
}

func (d *Dashboard) configData() configData {
	cfg := d.cfg()
	cd := configData{}
	if raw, err := os.ReadFile(".env"); err == nil {
		cd.HasEnvFile = true
		cd.EnvContent = string(raw)
	} else {
		cd.EnvContent = defaultEnvTemplate
	}
	cd.Effective = []configKV{
		{Key: "LISTEN_ADDR", Value: cfg.ListenAddr},
		{Key: "UPSTREAM_BASE_URL", Value: cfg.UpstreamBaseURL},
		{Key: "AUTH_TOKENS", Value: fmt.Sprintf("%d token(s)", len(cfg.AuthTokens)), Secret: true},
		{Key: "API_KEYS", Value: fmt.Sprintf("%d key(s)", len(cfg.APIKeys)), Secret: true},
		{Key: "ADMIN_TOKEN", Value: boolWord(cfg.AdminToken != ""), Secret: true},
		{Key: "ROTATION_INTERVAL", Value: cfg.RotationInterval.String()},
		{Key: "REQUEST_TIMEOUT", Value: cfg.RequestTimeout.String()},
		{Key: "SESSION_CALL_TIMEOUT", Value: cfg.SessionCallTimeout.String()},
		{Key: "PROXY_ROTATION", Value: cfg.ProxyRotation},
		{Key: "COST_MODE", Value: cfg.CostMode},
		{Key: "TLS_FINGERPRINT", Value: cfg.TLSFingerprint},
		{Key: "REGISTRY_REFRESH", Value: cfg.RegistryRefresh.String()},
		{Key: "DEBUG_DUMP", Value: strconv.FormatBool(cfg.DebugDump)},
		{Key: "LOG_FILE", Value: cfg.LogFile},
		{Key: "LOG_LEVEL", Value: cfg.LogLevel},
		{Key: "MAX_MESSAGES_PER_DAY", Value: strconv.Itoa(cfg.MaxMessagesPerDay)},
		{Key: "IDLE_ROTATION_TIMEOUT", Value: cfg.IdleRotationTimeout.String()},
		{Key: "SAFE_MODE", Value: strconv.FormatBool(cfg.SafeMode)},
		{Key: "REQUEST_JITTER", Value: cfg.RequestJitter.String()},
		{Key: "CLI_VERSION", Value: cfg.CLIVersion},
		{Key: "MODEL_ALIASES", Value: fmt.Sprintf("%d alias(es)", len(cfg.ModelAliases)), Secret: true},
		{Key: "TRANSIENT_RETRIES", Value: strconv.Itoa(cfg.TransientRetries)},
		{Key: "HTTP_PROXY", Value: cfg.HTTPProxy},
		{Key: "SOCKS5_PROXY", Value: boolWord(cfg.SOCKS5Proxy != ""), Secret: true},
		{Key: "SOCKS5_PROXIES", Value: fmt.Sprintf("%d proxy(es)", len(cfg.SOCKS5Proxies)), Secret: true},
	}
	return cd
}

func boolWord(v bool) string {
	if v {
		return "set"
	}
	return "unset"
}

// defaultEnvTemplate seeds the editor when no ./.env file exists yet. Values
// mirror the loader defaults; uncomment and edit what you need.
const defaultEnvTemplate = `# freebuff-proxy configuration (.env)
# Keys mirror the environment variables; leave commented to keep the default.
# See the README and docs/guides for the full reference.

#LISTEN_ADDR=127.0.0.1:3457
#UPSTREAM_BASE_URL=https://www.codebuff.com
#AUTH_TOKENS=token1,token2
#API_KEYS=sk-local-...
#ADMIN_TOKEN=change-me
#ROTATION_INTERVAL=24h
#REQUEST_TIMEOUT=15m
#SESSION_CALL_TIMEOUT=5s
#PROXY_ROTATION=per-token
#COST_MODE=free
#TLS_FINGERPRINT=chrome120
#REGISTRY_REFRESH=6h
#DEBUG_DUMP=false
#LOG_FILE=
#LOG_LEVEL=info
#MAX_MESSAGES_PER_DAY=0
#IDLE_ROTATION_TIMEOUT=0
#SAFE_MODE=true
#REQUEST_JITTER=0s
#CLI_VERSION=0.10.7
#MODEL_ALIASES=
#TRANSIENT_RETRIES=1
#HTTP_PROXY=
#SOCKS5_PROXY=
#SOCKS5_PROXIES=
`

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
