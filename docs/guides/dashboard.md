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
| `ADMIN_TOKEN` set | Login required. Enter the token; a signed `HttpOnly` + `SameSite=Strict` cookie unlocks the dashboard for 24h. Failed logins are rate-limited per IP (5 fails → 1 minute lockout). |
| `ADMIN_TOKEN` unset | Dashboard is open (legacy behavior, matching `/admin/reload`). A startup warning reminds you to set it if the proxy is reachable beyond loopback. |

The session cookie is stateless (HMAC-signed expiry, per-process random key):
restarting the proxy signs everyone out, which is the safe default.

## Pages

- **Overview** — pooled/bridge mode, model count, uptime, safe-mode flag, and
  per-token cards: session status, ban/429 risk level (`low`/`moderate`/`high`/
  `critical`), active runs, requests, 24h messages vs `MAX_MESSAGES_PER_DAY`,
  queue position, cooldown end, transient retries. Refreshes every 5s.
- **Tokens** — the same per-token state in detail (session instance id) plus
  the live **per-model session quota** table (`limit`, `recent`, `period`,
  `reset`, `entitlement`) parsed from the last upstream admission. The token
  pool is built at startup, so adding/removing `AUTH_TOKENS` requires a restart
  — the page says so rather than pretending otherwise.
- **Config** — a raw `.env` editor next to the effective-value table (secrets
  redacted to set/unset + counts). Save validates with the same pipeline as
  startup (durations, URLs, `Config.Validate`) and hot-reloads via the atomic
  config swap; invalid input is rejected **with the previous file restored**.
  When no `.env` exists yet, the editor is seeded with a commented template of
  every key and its default.
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
   `SOCKS5_PROXY(S)`, and `ADMIN_TOKEN` show only set/unset + counts.

## What is deliberately not in v1

- Runtime add/remove of pool tokens (the pool is construction-fixed; see the
  Tokens page note) — manage via `AUTH_TOKENS` + restart.
- Multi-user/role separation — one `ADMIN_TOKEN`, one session model.

Related: [README](../README.md), [Getting Started](getting-started.md),
[Client Integration](client-integration.md), [9router Integration](9router-integration.md).
