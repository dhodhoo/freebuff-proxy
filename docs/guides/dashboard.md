# Dashboard Guide

The embedded admin web UI gives you live relay state, per-token quota, a `.env`
config editor, logs, and metrics — in the same single binary, with no build
step and no extra process. It is htmx + Pico CSS vendored into the binary at
compile time (`internal/dashboard`); the server is untouched.

## Access

Open `http://127.0.0.1:3457/admin` (your `LISTEN_ADDR`). You land on the login
page unless `ADMIN_TOKEN` is unset.

| Setting | Behavior |
|---|---|
| `ADMIN_TOKEN` set | Login required. Enter the token; a signed `HttpOnly` + `SameSite=Strict` cookie unlocks the dashboard for 24h (`Secure` is added automatically when the proxy listens beyond loopback). Failed logins are rate-limited per IP (5 fails → 1 minute lockout). |
| `ADMIN_TOKEN` unset | Dashboard is open (legacy behavior, matching `/admin/reload`). A startup warning reminds you to set it if the proxy is reachable beyond loopback. **Config and Logs pages additionally require a loopback client** in this mode — a remotely reachable proxy cannot read or rewrite its `.env`, or view logs, without the token. |

The session cookie is stateless (HMAC-signed expiry, per-process random key):
restarting the proxy signs everyone out, which is the safe default.

## Pages

- **Overview** — pooled/bridge mode, model count, uptime, safe-mode flag, and
  per-token cards: session status, ban/429 risk level (`low`/`moderate`/`high`/
  `critical`), active runs, requests, 24h messages vs `MAX_MESSAGES_PER_DAY`,
  queue position, cooldown end, transient retries. Refreshes every 5s.
- **Tokens** — the same per-token state in detail (session instance id) plus
  the live **per-model session quota** table (`limit`, `recent`, `period`,
  `reset`, `entitlement`) parsed from the last upstream admission, now with
  **usage bars and reset countdowns** ("resets in 4h 12m", amber at ≥80%).
  Refreshes every 30s so countdowns stay honest. Three per-token actions:
  - **Test** — a real upstream session handshake (create + end) through that
    token, surfacing validity/network errors — the same idea as 9router's
    per-connection Test button.
  - **Unlock** — clears a cooldown / rate-limit lock / ban window (only shown
    while a lock is active; `hx-confirm` guards it — upstream locks are
    usually correct, and unlocking a banned token resumes traffic).
  - **Finish runs** — finishes the token's active runs without touching
    in-flight requests.
  These are gated like Config (loopback-only when `ADMIN_TOKEN` is unset).
  The token pool is built at startup, so adding/removing `AUTH_TOKENS` still
  requires a restart.
- **Models** — the live registry catalog: every model id with the upstream
  agent that serves it, plus the `MODEL_ALIASES` table.
- **Traces** — recent chat requests and their routing outcome: token chosen,
  model, status (`ok`/`error` + class `rate_limited`/`banned`/`waiting_room`/
  `upstream`), duration, and error string. This is the ban-avoidance
  observability view — see 429/ban events as they happen. Refreshes every 3s.
- **Config** — a raw `.env` editor next to the effective-value table (secrets
  redacted to set/unset + counts). Save validates with the same pipeline as
  startup (durations, URLs, `Config.Validate`) and hot-reloads via the atomic
  config swap; invalid input is rejected **with the previous file restored**.
  When no `.env` exists yet, the editor is seeded with a commented template of
  every key and its default.
- **Setup** — copy-paste client configuration: OpenCode, Continue, aider,
  9router, and a curl smoke test, all generated from the effective config
  (base URL, mode, first catalog model), plus the full model list as chips.
- **Logs** — the last 200 records from an in-memory ring that mirrors the
  process logger (stderr and any `LOG_FILE` still receive everything). No log
  file, no docker access needed. Refreshes every 3s.
- **Metrics** — sampled counter trends (requests, transient retries,
  fingerprint rotations) as server-rendered SVG sparklines. The full Prometheus
  exposition with per-token gauges stays at `/metrics`.

## Docker caveat

The config editor writes `./.env` **relative to the proxy's working
directory**. Inside the container that is the image workdir, not your host
directory — so in Docker, prefer environment variables in `docker-compose.yml`
(or bind-mount your `.env`) and use the dashboard's Config page read-only.
The Overview/Tokens/Logs/Metrics pages work fine in Docker.

## Hardening

1. **Set `ADMIN_TOKEN`** — a random string. Unauthenticated mode is only safe
   when the proxy is bound to loopback and unreachable from the network.
2. Keep `LISTEN_ADDR` on loopback unless you are deliberately exposing the
   proxy; if you expose it, put TLS in front (reverse proxy) — the cookie and
   admin traffic would otherwise cross the wire in the clear.
3. The dashboard never renders token values: `AUTH_TOKENS`, `API_KEYS`,
   `SOCKS5_PROXY(S)`, and `ADMIN_TOKEN` show only set/unset + counts. The
   `.env` file is written atomically with mode `0600`.
4. Saves (and `/admin/reload`) re-apply the JSON config file the proxy was
   started with (`-config`), so JSON overrides survive a dashboard save.

## What is deliberately not in v1

- Runtime add/remove of pool tokens (the pool is construction-fixed; see the
  Tokens page note) — manage via `AUTH_TOKENS` + restart.
- Multi-user/role separation — one `ADMIN_TOKEN`, one session model.

Related: [README](../README.md), [Getting Started](getting-started.md),
[Client Integration](client-integration.md), [9router Integration](9router-integration.md).
