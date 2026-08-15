# freebuff-proxy — OpenAI-Compatible Coding Gateway

[![CI](https://img.shields.io/github/actions/workflow/status/trefeon/freebuff-proxy/ci.yml)](https://github.com/trefeon/freebuff-proxy/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/trefeon/freebuff-proxy)](https://github.com/trefeon/freebuff-proxy/releases)
[![License](https://img.shields.io/github/license/trefeon/freebuff-proxy)](https://github.com/trefeon/freebuff-proxy/blob/main/LICENSE)

`freebuff-proxy` is a local gateway that makes the AI coding models behind Codebuff/FreeBuff available to **any** tool that speaks the OpenAI API — OpenCode, Continue, Cursor, aider, 9router, LiteLLM, or your own scripts.

Your coding tools expect an OpenAI-style endpoint (`/v1/chat/completions`). The upstream service is not OpenAI-shaped: it is a CLI coding agent with a proprietary session protocol, and its free-tier access is tied to per-account tokens that carry individual daily quotas and can be rate-limited or banned. `freebuff-proxy` sits between the two and absorbs that friction:

- **Translates** — rewrites standard OpenAI requests into the upstream session protocol (CLI request envelope, model-bound agent runs, tool-schema normalization) and streams the SSE response back as OpenAI `chat.completion.chunk` events.
- **Pools** — spreads requests across multiple tokens with round-robin and failover, so a busy client or router rides out per-account quotas instead of failing.
- **Stealths** — makes egress look like a real browser (TLS fingerprints, header sanitization, request jitter) so upstream abuse detection is less likely to flag your account (see the ToS warning below).

> **⚠️ Terms-of-service risk.** Using your FreeBuff token through this proxy conflicts with FreeBuff/Codebuff terms of service; upstream abuse detection can suspend or permanently ban accounts. Use `SAFE_MODE=true`, keep usage modest, and do not run unattended 24/7. See [Getting Started](docs/guides/getting-started.md).

---

## Table of Contents

- [New here? Start here](#new-here-start-here)
- [Requirements](#requirements)
- [Features](#features)
- [How It Works](#how-it-works)
- [Key Concepts](#key-concepts)
- [Quick Start](#quick-start)
- [Command-Line Interface](#command-line-interface)
- [Configuration Reference](#configuration-reference)
- [Deployment](#deployment)
- [Guides](#guides)
- [Contributing & Security](#contributing--security)
- [Contact & Support](#contact--support)
- [License](#license)

---

## New here? Start here

Freebuff-proxy makes the free AI models behind the FreeBuff/Codebuff CLI available to any OpenAI-compatible tool (opencode, Continue, Cursor, aider, 9router). If you are new:

1. **Get a FreeBuff account + token.** You need a Codebuff/FreeBuff account; the token (`cb_...`) is what the proxy uses upstream. Get one with the official CLI or `scripts/gen-token.*` — see [Obtain an Auth Token](#2-obtain-an-auth-token).
2. **Install the proxy.** One command, no Go or Docker required — see [Quick Start](#quick-start).
3. **Connect your AI tool.** Point opencode/Continue/Cursor/aider at `http://127.0.0.1:3457/v1` — see [Client Integration](docs/guides/client-integration.md).

For a guided walkthrough, read [Getting Started](docs/guides/getting-started.md) (5 minutes).

## Requirements

| Requirement | Details |
|---|---|
| **A FreeBuff/Codebuff account** | Free account at codebuff.com / freebuff.com. The proxy relays your account's token; each account has its own daily session quota. |
| **A token (`cb_...`)** | From the official CLI login or `scripts/gen-token.*` — see [Obtain an Auth Token](#2-obtain-an-auth-token). |
| **OS** | Linux, macOS, or Windows (amd64/arm64). Prebuilt release binaries; no Go toolchain needed. |
| **Docker** | Optional — only for the container deployment path (`docker compose up -d --build`). |
| **Network** | Outbound HTTPS to `codebuff.com` (configurable via `UPSTREAM_BASE_URL`); the proxy listens on loopback `127.0.0.1:3457` by default. |
| **Go 1.26+** | Only if building from source. |

---

## Features

- **OpenAI-Compatible API**: `POST /v1/chat/completions` (stream + non-stream), `GET /v1/models`, `GET /healthz`, Prometheus `GET /metrics`, and hot config reload via `POST /admin/reload`.
- **Dynamic Reasoning Effort**: OpenAI `reasoning_effort` (`low`/`medium`/`high`/`max`) and Codex/Anthropic `reasoning.effort` are normalized and mapped to upstream reasoning engines.
- **Session & Run Lifecycle**: Upstream session handshakes, model-lock recovery (`DELETE` → re-`POST`), grace draining, and idle-run finishing, all automatic.
- **Token Pooling & Bridge Mode**: Round-robin with failover across `AUTH_TOKENS`, or zero-storage relay when clients bring their own token — see [Key Concepts](#key-concepts).
- **Token Auto-Discovery**: With empty `AUTH_TOKENS`, credentials are read from the official CLI login files (`~/.config/manicode/credentials.json`, `~/.config/codebuff/credentials.json`). Disable with `AUTO_DISCOVER_TOKEN=false`.
- **TLS Stealth & Egress Proxies**: `HTTP_PROXY` / `SOCKS5_PROXY`, per-token SOCKS5 routing (`SOCKS5_PROXIES` + `PROXY_ROTATION`), and browser TLS fingerprinting via uTLS (Chrome, Firefox, Safari, Edge).
- **Subagent-Ready Concurrency**: Single-flight session refresh prevents race conditions during high-volume tool-calling loops.
- **Safe Mode**: On by default — anti-ban presets (TLS stealth, header sanitization, jitter, idle rotation, daily cap).
- **Operational Tooling**: `-doctor` diagnostics, `-setup` interactive client configuration, and a SHA-256-verified `-update` self-updater.
- **Quota Transparency**: Live per-model remaining quota (from the upstream `rateLimitsByModel` admission payload) is surfaced in `GET /healthz` (per-token `quota` map) and `GET /metrics` (`freebuff_proxy_quota_recent` / `freebuff_proxy_quota_limit` gauges).

## How It Works

One chat request, end to end:

1. **Your tool calls the proxy.** It POSTs a standard OpenAI request to `http://127.0.0.1:3457/v1/chat/completions` — same shape it would send to any OpenAI-compatible endpoint.
2. **A token is chosen.** The proxy picks the next healthy token from the pool (round-robin, skipping tokens in cooldown or locked by a rate limit), or — in bridge mode — uses the token your client sent in its `Authorization` header.
3. **The request is translated.** The model id is resolved through the catalog to the upstream agent that runs it, the message list is sanitized and re-wrapped in the CLI request envelope, and OpenAI extras (`reasoning_effort`, tool schemas, etc.) are mapped to what upstream expects.
4. **It goes out stealthily.** The upstream call uses a browser-like TLS handshake and sanitized headers, through `HTTP_PROXY` / `SOCKS5_PROXY` if configured.
5. **The stream comes back translated.** The upstream SSE stream is converted into OpenAI `chat.completion.chunk` events and relayed to your client in real time.
6. **State is cleaned up.** When the request finishes, the run is drained; once a run or token ages out (rotation interval, idle timeout), it is rotated or finished so the next request starts clean. A token that hit a quota limit (`429`) is locked locally until its reset time — the proxy answers `429` + `Retry-After` itself, with no traffic sent upstream.

The upstream protocol is not public: the translation layer is built by reverse-engineering the official CLI's wire protocol and session lifecycle. It changes when the upstream changes — the translation lives in `internal/convert`, `internal/upstream`, `internal/stealth`, and `internal/registry`.

```mermaid
graph TD
    Client[AI Client / Router<br/>OpenCode · 9router · Continue · Cursor · aider] -->|POST /v1/chat/completions| Proxy[freebuff-proxy<br/>localhost:3457]
    Proxy -->|1. Session & Run Lifecycle| Pool[Token Pool & Session Cache]
    Proxy -->|2. Inject Envelope + Stealth| Upstream[Upstream Backend API]
    Upstream -->|3. SSE Stream| Proxy
    Proxy -->|4. OpenAI SSE Chunks| Client
    Client -.->|GET /metrics · GET /healthz · POST /admin/reload| Proxy
```

## Key Concepts

| Concept | What it means |
|---|---|
| **Token** | One FreeBuff/Codebuff account credential (`cb_...`). Each token has its own daily quota and can be rate-limited or banned independently. |
| **Session** | Per-token upstream admission state (handshake, model locks). The proxy maintains and reuses it so every request does not pay the handshake cost. |
| **Run** | One upstream agent execution for a model, shared across many requests. Runs start on first use, live for `ROTATION_INTERVAL` (default `6h`), then are rotated (fresh start, old one drained/finished) so no run accumulates suspiciously long-lived activity. Idle tokens get their runs finished too. |
| **Model** | A catalog entry addressed as `provider/model` (e.g. `deepseek/deepseek-v4-flash`, `z-ai/glm-5.2`). The registry serves `/v1/models` and maps each model to the upstream agent that runs it. |
| **Pooled mode** | You configure several tokens in `AUTH_TOKENS`. Requests round-robin across them with automatic failover. Best for one user with several accounts who wants maximum uptime and quota headroom. |
| **Bridge mode** | You configure no tokens. Each client sends its own token as `Authorization: Bearer <token>`, and the proxy relays with it, caching per-client state (LRU, max 32). Best for a shared router (e.g. 9router) serving many users who each bring their own account. |
| **Safe mode** | Default-on anti-ban presets: TLS stealth, proxy-header sanitization, request jitter, idle rotation, and a per-token daily cap. See [Safe Mode](#safe-mode--zero-spam-quota-handling). |
| **Quota lock** | When a token hits its daily limit, the proxy parses the upstream `429` reset timestamp and refuses local requests for that token until reset — fast (`<1ms`), silent, and spam-free. |

---

## Quick Start

### 1. Install

**One-command installer (Linux/macOS):**

```bash
curl -sSL https://raw.githubusercontent.com/trefeon/freebuff-proxy/main/scripts/install-freebuff-proxy.sh | bash
```

**Windows (PowerShell):**

```powershell
irm https://raw.githubusercontent.com/trefeon/freebuff-proxy/main/scripts/install-freebuff-proxy.ps1 | iex
```

The installer prompts for an install method (easy, manual binary, Docker Compose, bridge mode), mints/reads your token, and writes `.env`.

**Alternatively**, run with Docker Compose:

```bash
cp .env.example .env   # then set AUTH_TOKENS
docker compose up -d --build
```

**Or** download a release binary from [Releases](https://github.com/trefeon/freebuff-proxy/releases) (Linux/macOS/Windows × amd64/arm64) and run `./freebuff-proxy`.

### 2. Obtain an Auth Token

Generate one headlessly (opens a browser OAuth login, prints the token to the terminal without saving):

**Windows (PowerShell):**

```powershell
.\scripts\gen-token.ps1 -ToClipboard
```

**Linux / macOS (bash):**

```bash
./scripts/gen-token.sh --clipboard
```

`gen-token.*` are aliases for `gen-freebuff-token.*`, which also supports `--save` (store in the CLI credentials file), `--append` (add to `.env` `AUTH_TOKENS`), and `--env <path>`.

Alternatively, log in with the official CLI (`npm i -g freebuff && freebuff`): the proxy auto-discovers the token from its credentials file on startup.

### 3. Configure

Copy the example and set your token:

```bash
cp .env.example .env
# AUTH_TOKENS=cb_xxx        ← paste your token (comma-separate for pooling)
# SAFE_MODE=true            ← default (set false to disable)
```

Leave `AUTH_TOKENS=` empty for **bridge mode** (clients bring their own tokens). Not sure which to pick? One user with a few accounts → pooled mode; a shared router serving many users → bridge mode. See [Key Concepts](#key-concepts). `config.example.json` shows the equivalent JSON config file, loaded with `-config`; see the [Configuration Reference](#configuration-reference) for every key.

### 4. Run & Verify

```bash
./freebuff-proxy            # or: docker compose up -d
```

Check health and run diagnostics:

```bash
curl http://127.0.0.1:3457/healthz
./freebuff-proxy -doctor    # config, port, DNS/TLS, model registry checks
```

---

## Command-Line Interface

| Flag | Description |
|---|---|
| *(none)* | Run the proxy |
| `-config <path>` | Load an optional JSON config file (keys mirror env names) |
| `-v` | Verbose (debug) logging |
| `-version` | Print version and exit |
| `-doctor` | Run configuration and environment diagnostics |
| `-update` | Self-update from the latest GitHub release (SHA-256 verified against `checksums.txt`) |
| `-setup` | Interactive client setup (Continue, opencode, aider) |
| `-yes` | Auto-confirm `-setup` prompts |

---

## Configuration Reference

All keys can be set via environment variables or a `.env` file (which behaves like the environment), or via the JSON config file passed to `-config`. Precedence, lowest to highest: **built-in defaults < JSON `-config` < `./.env` < environment**. List values (`AUTH_TOKENS`, `API_KEYS`, `SOCKS5_PROXIES`) are comma-separated in env and arrays in JSON.

| Environment Variable | Default | Description |
|---|---|---|
| `LISTEN_ADDR` | `127.0.0.1:3457` | Host and port to bind (loopback; containers set `:3457`) |
| `UPSTREAM_BASE_URL` | `https://codebuff.com` | Upstream API endpoint (normalized to `www.codebuff.com`) |
| `AUTH_TOKENS` | `""` | Comma-separated upstream tokens (empty = bridge mode) |
| `AUTO_DISCOVER_TOKEN` | `true` | When `AUTH_TOKENS` is empty, read credentials from the official CLI login files (`false` disables) |
| `API_KEYS` | `""` | Comma-separated client keys required for `/v1/*` (empty = open; ignored in bridge mode) |
| `ROTATION_INTERVAL` | `6h` | Agent-run rotation interval |
| `REQUEST_TIMEOUT` | `15m` | Upstream request timeout |
| `SESSION_CALL_TIMEOUT` | `30s` | Session call timeout |
| `REGISTRY_REFRESH` | `6h` | Model catalog refresh interval |
| `COST_MODE` | `free` | `free` (free-tier) or paid billing mode |
| `TLS_FINGERPRINT` | `auto` | `auto`, `chrome120`, `chrome126`, `safari17`, `safari18`, `firefox120`, `firefox128`, `edge126`, `random` |
| `HTTP_PROXY` | `""` | Outbound HTTP proxy for upstream requests |
| `SOCKS5_PROXY` | `""` | Outbound SOCKS5 proxy for upstream requests |
| `SOCKS5_PROXIES` | `""` | Per-token SOCKS5 proxies (comma-separated) |
| `PROXY_ROTATION` | `per-token` | `per-token`, `round-robin`, or `random` |
| `DEBUG_DUMP` | `false` | Persist redacted traffic dumps to `./dump/` (mode 0600) |
| `LOG_FILE` | `""` | Append log lines to a file (e.g. `./logs/proxy.log`) |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `MAX_MESSAGES_PER_DAY` | `0` | Per-token daily cap on successful chats (`0` = unlimited; `SAFE_MODE` presets 150 when unset) |
| `IDLE_ROTATION_TIMEOUT` | `0` | Finish runs after this idle period (`0` = disabled; `SAFE_MODE` sets 30m when unset) |
| `SAFE_MODE` | `true` | Apply anti-ban presets (see below; set `false` to disable) |
| `REQUEST_JITTER` | `0s` | Random delay range `[0, REQUEST_JITTER)` before upstream calls (`SAFE_MODE` sets 2s when unset) |
| `CLI_VERSION` | `0.10.7` | Upstream CLI version string used in the request envelope |
| `MODEL_ALIASES` | `""` | Map aliases to real model IDs, e.g. `gpt-4o:deepseek/deepseek-v4-flash,glm:z-ai/glm-5.2` |
| `TRANSIENT_RETRIES` | `1` | Max additional attempts after a transient transport failure; `0` disables |

### Safe Mode & Zero-Spam Quota Handling

`SAFE_MODE=true` is the **default** for all setups (set `SAFE_MODE=false` to
opt out). It enables essential anti-ban protections and presets:

- **JA3 TLS Stealth**: Mimics real browser handshakes (Chrome 120/126, Safari 17/18, Firefox 120/128, Edge 126) via `uTLS` to prevent WAF / CDN bot detection.
- **Proxy Header Sanitization**: Strips 25 proxy-identifying headers (`X-Forwarded-For`, `Via`, `CF-Connecting-IP`, etc.).
- **Request Jitter**: Injects randomized 0–2s delay jitter to break robotic, machine-like cadence.
- **Idle Rotation**: Finishes runs after 30 minutes of inactivity.
- **Daily Cap**: Presets `MAX_MESSAGES_PER_DAY=150` when the key is unset.

**Why `MAX_MESSAGES_PER_DAY=0` (Unlimited) is Recommended:**

- Since `SAFE_MODE` now defaults on and presets an **unset** cap to 150, set
  `MAX_MESSAGES_PER_DAY=0` explicitly (as `.env.example` does) to utilize
  100% of your FreeBuff/Codebuff free-tier allowance. Explicit values always
  win over the preset.
- **Zero-Spam Guarantee**: When an account reaches its daily quota or upstream capacity limit, the upstream returns a `429` with a Pacific midnight reset timestamp (`resetAt: 07:00:00Z`).
- The proxy parses this timestamp and **locks the token locally in memory**.
- Any subsequent request for that token returns `429` locally in `<1ms` without sending any network traffic upstream.
- Upstream routers (e.g. 9router) receive standard `429` + `Retry-After` headers and automatically rotate to your next available account without failing user prompts.

### HTTP Endpoints

| Endpoint | Auth | Description |
|---|---|---|
| `POST /v1/chat/completions` | `API_KEYS` (when set) | OpenAI-compatible chat, streaming and non-streaming |
| `GET /v1/models` | `API_KEYS` (when set) | Model catalog from the registry (fallback at boot + live refresh) |
| `GET /healthz` | none | JSON: `status`, `uptime_seconds`, `models`, per-token snapshot (incl. per-model `quota` map when the last admission carried it), `bridge_tokens` |
| `GET /metrics` | none | Prometheus text format: uptime, model count, per-token 24h messages / requests / active runs / cooldown, per-model quota (`freebuff_proxy_quota_recent` / `freebuff_proxy_quota_limit`) |
| `POST /admin/reload` | `API_KEYS` (when set) | Hot-reload configuration from disk without restart |

---

## Deployment

- **Docker**: `docker-compose.yml` + `Dockerfile` — runs as an unprivileged user, healthchecked on `/healthz`, `LISTEN_ADDR=:3457` inside the container.
- **Systemd**: `scripts/freebuff-proxy.service` (Linux).
- **macOS launchd**: `scripts/com.freebuff-proxy.plist` (macOS).
- **Docker + 9router helper**: `scripts/setup-proxy-docker.sh`.

## Guides

- [Getting Started](docs/guides/getting-started.md) — 5-minute setup walkthrough
- [Client Integration](docs/guides/client-integration.md) — OpenCode, 9router, Continue, Cursor, aider, OpenAI SDKs
- [9router Integration](docs/guides/9router-integration.md) — router dashboard setup in bridge mode

---

## Contributing & Security

- [Contributing](CONTRIBUTING.md) — filing issues, opening PRs, what to expect
- [Security](.github/SECURITY.md) — supported versions and how to report a vulnerability

## Contact & Support

- **Questions, bugs, feature requests**: [GitHub Issues](https://github.com/trefeon/freebuff-proxy/issues)
- **Security reports**: [SECURITY.md](.github/SECURITY.md)
- **Contributing**: [CONTRIBUTING.md](CONTRIBUTING.md)

## License

[MIT](LICENSE)
