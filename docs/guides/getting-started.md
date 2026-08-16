# Getting Started with freebuff-proxy (5-Minute Guide)

This guide takes you from zero to a working OpenAI-compatible proxy connected to FreeBuff.

---

## What is freebuff-proxy?

`freebuff-proxy` is a local bridge server. It sits between your favorite coding tools (OpenCode, pi, 9router, LiteLLM, or your own scripts) and FreeBuff's free AI models. Your tools talk standard OpenAI API to `localhost:3457`, and the proxy manages sessions and tokens behind the scenes.

```
+-------------------+      OpenAI API      +-------------------+      FreeBuff      +-------------------+
| Your AI Client    | -------------------> | freebuff-proxy    | -----------------> | codebuff.com      |
| (OpenCode / pi)  | <------------------- | (localhost:3457)  | <----------------- | (Free Models)     |
+-------------------+      SSE Streams     +-------------------+     CLI Envelope   +-------------------+
```

---

## Important Safety Warning

Using this proxy conflicts with Codebuff's terms of service. Upstream abuse detection scans for automation patterns and suspends accounts. Detection is documented in the open-source FreeBuff client: per-request IP scoring, per-account trust levels with sticky caps, daily spend ceilings, and mass sweeps against known farm shapes. The rules below are the evidence-backed dos and don'ts:
- **Keep `SAFE_MODE=true`** (it is the default, set explicitly in `.env.example`). It enables anti-ban stealth (TLS fingerprint, header sanitization, request jitter, idle rotation).
- Do **not** run 24/7 on heavy unattended automated tasks.
- **Do not route through a VPN.** VPN / proxy / Tor / hosting egress is a hard-block signal: it demotes the account to the limited tier or a terminal `country_blocked`, and restricted cohorts get a $0.50/day spend ceiling (≈1 session/day). Stealth masks TLS fingerprints and headers, not your public IP — use a normal residential connection.
- **Only request models your account's tier and region offers.** Out-of-tier picks are refused or silently downgraded to `deepseek/deepseek-v4-flash`, and the requested model id is correlated with your egress IP's region.
- Keep one modest account; do not create spam clusters of accounts. Upstream caps distinct active sessions per egress IP (`ip_capped`), and accounts from the same signup network (≥8 per /24) or mailbox (≥3) are permanently capped at lower trust levels.
- **Use one key until it is rate-limited.** The proxy prefers the token with a live session and only switches when it is exhausted. Don't rotate several healthy keys aggressively (farming signals).
- **For 24h of coding, budget 4–5 keys**, each registered with a **real email address** (e.g. Gmail). Temp-mail registrations are a documented ban cohort: 6,699 of 7,129 accounts on flagged domains were already banned.
- **429 ≠ ban.** `429` is quota/waiting room (resets at Pacific midnight) — the proxy locks the token locally and answers in `<1ms`. Only `403` with `banned` / `country_blocked` means the account is gone; use a new established account.

---

## Step 1: Install & Start the Proxy

### Option A: One-Command Installer (Recommended)

**Linux / macOS Terminal:**
```bash
curl -sSL https://raw.githubusercontent.com/trefeon/freebuff-proxy/main/scripts/install-freebuff-proxy.sh | bash
```

**Windows PowerShell:**
```powershell
irm https://raw.githubusercontent.com/trefeon/freebuff-proxy/main/scripts/install-freebuff-proxy.ps1 | iex
```

Follow the prompts to pick your token or enable bridge mode.

#### How to obtain your FreeBuff token (`authToken`):

Fastest path: run the bundled gen script. It opens a browser OAuth login and prints the token to the terminal without saving it:

- Linux / macOS: `./scripts/gen-token.sh --clipboard`
- Windows (PowerShell): `.\scripts\gen-token.ps1 -ToClipboard`

`gen-token.*` is an alias for `gen-freebuff-token.*`, which also supports `--save` (store in the CLI credentials file), `--append` (add to `.env` `AUTH_TOKENS`), and `--env <path>`.

Alternatively, log in with the official CLI: `npm i -g freebuff` and run `freebuff`. The CLI saves your `authToken` in `~/.config/manicode/credentials.json` (Windows: `C:\Users\<you>\.config\manicode\credentials.json`).

---

### Option B: Docker Compose

```bash
cp .env.example .env
# Edit .env and set AUTH_TOKENS=your_token
docker compose up -d --build
```

---

## Step 2: Verify It Works

Run the diagnostic tool or curl:

```bash
# Diagnostic doctor check: config, port, DNS/TLS, and a real session
# handshake per token (catches expired tokens before the first chat 401):
./freebuff-proxy -doctor

# Standalone token probe: real upstream session handshake, exit 0/1
# (handy for installers and scripts):
./freebuff-proxy -test-token

# Quick health check (JSON: status, uptime, model count, per-token snapshot):
curl http://localhost:3457/healthz

# Prometheus metrics scrape endpoint:
curl http://localhost:3457/metrics

# List available models:
curl http://localhost:3457/v1/models
```

`/healthz` returning status `200` means the proxy is running and reachable. It does **not** validate your token. Use `./freebuff-proxy -test-token` (or the dashboard smoke test on the Overview page) to prove a token is valid before your first chat; `-doctor` runs the same per-token session probe among its checks.

`/healthz` also reports each token's live per-model quota (`quota` map) when the last session admission carried it.

## Step 3: Connect Your Favorite AI Client

Point your AI tool to:
- **Base URL:** `http://localhost:3457/v1`
- **API Key:** `not-needed` (or your token in bridge mode)
- **Model:** `deepseek/deepseek-v4-flash`

Fastest path: run `./freebuff-proxy -setup` to write the client config automatically.

See the [Client Integration Guide](client-integration.md) for copy-paste config for OpenCode, pi, 9router, LiteLLM, and more.

---

## Troubleshooting

Run `./freebuff-proxy -doctor` to diagnose problems automatically.

| Error / Symptom | Cause & Fix |
|---|---|
| `502` + `403 free_mode_cli_required` | The request was missing the CLI system prompt marker or envelope. The proxy injects this automatically. Update to the latest version. |
| `502` + `401 Invalid API key` | Token in `.env` is expired or invalid. Catch it before the first chat: `./freebuff-proxy -test-token` (or `-doctor`) probes with a real session handshake and fails with a clear message. Then re-run `freebuff` to log in and update `AUTH_TOKENS`, or swap the token live on the dashboard Tokens page (no restart). |
| Connection refused | Proxy is not running, or in Docker without `LISTEN_ADDR=:3457`. |
| `403 account_banned` | Account suspended upstream. Token is dead; use a new established account. |

---

## Related docs

- [Client Integration](client-integration.md): OpenCode, pi, 9router, LiteLLM, or your own scripts
- [9router Integration](9router-integration.md): wiring the proxy into 9router
- [README](../../README.md): overview, config reference, quick start
