# Getting Started with freebuff-proxy (5-Minute Guide)

This guide takes you from zero to a working OpenAI-compatible proxy connected to FreeBuff.

---

## What is freebuff-proxy?

`freebuff-proxy` is a local bridge server. It sits between your favorite coding tools (like Continue, Cursor, aider, or opencode) and FreeBuff's free AI models. Your tools talk standard OpenAI API to `localhost:3457`, and the proxy manages sessions and tokens behind the scenes.

```
+-------------------+      OpenAI API      +-------------------+      FreeBuff      +-------------------+
| Your AI Client    | -------------------> | freebuff-proxy    | -----------------> | codebuff.com      |
| (Continue/Cursor) | <------------------- | (localhost:3457)  | <----------------- | (Free Models)     |
+-------------------+      SSE Streams     +-------------------+     CLI Envelope   +-------------------+
```

---

## Important Safety Warning

Using this proxy conflicts with Codebuff's terms of service. Upstream abuse detection scans for automation patterns and suspends accounts.
- **Keep `SAFE_MODE=true`** — it is the default and is set explicitly in `.env.example`; it enables anti-ban stealth (TLS fingerprint, header sanitization, request jitter, idle rotation, daily cap).
- Do **not** run 24/7 on heavy unattended automated tasks.
- Keep one modest account; do not create spam clusters of accounts.

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
Run `npm i -g freebuff` and `freebuff` to log in via browser. The CLI saves your `authToken` in `~/.config/manicode/credentials.json` (Windows: `C:\Users\<you>\.config\manicode\credentials.json`).
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
# Diagnostic doctor check:
./freebuff-proxy -doctor

# Quick health check (JSON: status, uptime, model count, per-token snapshot):
curl http://localhost:3457/healthz

# Prometheus metrics scrape endpoint:
curl http://localhost:3457/metrics

# List available models:
curl http://localhost:3457/v1/models
```

`/healthz` returning status `200` means your proxy setup is **100% correct**.

`/healthz` also reports each token's live per-model quota (`quota` map) when the last session admission carried it.

## Step 3: Connect Your Favorite AI Client

Point your AI tool to:
- **Base URL:** `http://localhost:3457/v1`
- **API Key:** `not-needed` (or your token in bridge mode)
- **Model:** `deepseek/deepseek-v4-flash` (or `z-ai/glm-5.2`)

Fastest path: run `./freebuff-proxy -setup` to write the config for Continue, opencode, or aider automatically.

See the [Client Integration Guide](client-integration.md) for copy-paste config for Continue, Cursor, aider, opencode, and more.

---

## Troubleshooting

Run `./freebuff-proxy -doctor` to diagnose problems automatically.

| Error / Symptom | Cause & Fix |
|---|---|
| `502` + `403 free_mode_cli_required` | The request was missing the CLI system prompt marker or envelope. The proxy injects this automatically — update to the latest version. |
| `502` + `401 Invalid API key` | Token in `.env` is expired or invalid. Re-run `freebuff` to log in and update `AUTH_TOKENS`. |
| Connection refused | Proxy is not running, or in Docker without `LISTEN_ADDR=:3457`. |
| `403 account_banned` | Account suspended upstream. Token is dead; use a new established account. |

---

## Related docs

- [Client Integration](client-integration.md) — Continue, Cursor, aider, opencode, SDK configs
- [9router Integration](9router-integration.md) — wiring the proxy into 9router
- [README](../../README.md) — overview, config reference, quick start
