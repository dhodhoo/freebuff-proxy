# Client Integration Guide

Connect any OpenAI-compatible AI tool or coding assistant to `freebuff-proxy`.

**Server Base URL:** `http://localhost:3457/v1`  
**API Key:** `not-needed` (or your FreeBuff token in bridge mode)

---

## Bridge Mode vs Pooled Mode

+ **Pooled Mode (Default):** Set `AUTH_TOKENS=token1,token2` in the proxy's `.env`. The proxy manages token rotation, and clients can use any placeholder API key.
+ **Bridge Mode (Routers & Multi-User):** Leave `AUTH_TOKENS=` empty in `.env`. The proxy acts as a zero-storage relay. **API Routers ([9router](9router-integration.md), OmniRouter, One API, LiteLLM) send actual FreeBuff tokens in `Authorization: Bearer <freebuff-token>`.** The proxy lazily creates sessions per client token with LRU caching, rate limits, and health tracking; live per-model quota is visible in `GET /healthz`.
---

## 1. Continue (VS Code & JetBrains Extension)

Edit `~/.continue/config.json`:

```json
{
  "models": [
    {
      "title": "FreeBuff DeepSeek Flash",
      "provider": "openai",
      "model": "deepseek/deepseek-v4-flash",
      "apiBase": "http://localhost:3457/v1",
      "apiKey": "not-needed"
    },
    {
      "title": "FreeBuff GLM 5.2",
      "provider": "openai",
      "model": "z-ai/glm-5.2",
      "apiBase": "http://localhost:3457/v1",
      "apiKey": "not-needed"
    }
  ]
}
```

---

## 2. Cursor IDE

1. Open **Cursor Settings** -> **Models**
2. Scroll to **OpenAI API Key** or **Custom OpenAI Compatible Provider**
3. Override **Base URL**: `http://localhost:3457/v1`
4. Enter **API Key**: `not-needed`
5. Add Model: `deepseek/deepseek-v4-flash`

---

## 3. aider (CLI AI Pair Programmer)

Run `aider` with environment variables or CLI flags:

```bash
export OPENAI_API_BASE="http://localhost:3457/v1"
export OPENAI_API_KEY="not-needed"

aider --model openai/deepseek/deepseek-v4-flash
```

---

## 4. opencode

Add to `opencode.json` or `~/.config/opencode/opencode.json`:

```json
{
  "providers": {
    "freebuff": {
      "type": "openai",
      "options": {
        "baseURL": "http://localhost:3457/v1",
        "apiKey": "not-needed"
      },
      "models": [
        { "id": "deepseek/deepseek-v4-flash", "name": "DeepSeek Flash" },
        { "id": "z-ai/glm-5.2", "name": "GLM 5.2" }
      ]
    }
  }
}
```

---

## 5. Python (Official OpenAI SDK)

```python
from openai import OpenAI

client = OpenAI(
    base_url="http://localhost:3457/v1",
    api_key="not-needed"
)

response = client.chat.completions.create(
    model="deepseek/deepseek-v4-flash",
    messages=[{"role": "user", "content": "Write a python function to check prime numbers."}],
    stream=True
)

for chunk in response:
    if chunk.choices[0].delta.content:
        print(chunk.choices[0].delta.content, end="")
```

---

## 6. Node.js (Official OpenAI SDK)

```javascript
import OpenAI from 'openai';

const openai = new OpenAI({
  baseURL: 'http://localhost:3457/v1',
  apiKey: 'not-needed',
});

const response = await openai.chat.completions.create({
  model: 'deepseek/deepseek-v4-flash',
  messages: [{ role: 'user', content: 'Say hello in TypeScript!' }],
  stream: true,
});

for await (const chunk of response) {
  process.stdout.write(chunk.choices[0]?.delta?.content || '');
}
```

---

## 7. API Routers & Aggregators (9router, OmniRouter, One API, LiteLLM)

For multi-account management or multi-user API routing:

1. **Proxy Setup:** Run the proxy in **Bridge Mode** (leave `AUTH_TOKENS=` empty in `.env`).
2. **Router Setup (9router / OmniRouter):**
   + **Provider Type:** OpenAI Compatible
   + **Base URL:** `http://localhost:3457/v1` (or container host `http://host.docker.internal:3457/v1`)
   + **API Keys:** Add your actual **auth token(s)** as the node API keys in 9router or OmniRouter.
3. **Routing Behavior:** When 9router or OmniRouter routes a request, it sends the key as `Authorization: Bearer <token>`. The proxy lazily creates and caches upstream free sessions for each token without saving any token to disk.

---

## Recommended Models

Query `http://localhost:3457/v1/models` for the full live list.

| Model ID | Provider | Best for |
|---|---|---|
| `deepseek/deepseek-v4-flash` | DeepSeek | Fast coding; limited + full tiers |
| `deepseek/deepseek-v4-pro` | DeepSeek | Deep reasoning; full tier |
| `openai/gpt-5.6-luna` | OpenAI | Deep reasoning + image; full tier |
| `minimax/minimax-m3` | MiniMax | Fast + image; full tier |
| `mimo/mimo-v2.5` | Xiaomi | Balanced; limited + full tiers |
| `z-ai/glm-5.2` | Zhipu AI | Earned sessions, 5 per 20h (429 rate_limited) |
| `anthropic/claude-fable-5` | Anthropic | Catalog addition; tier-dependent |
| `poolside/laguna-s-2.1` | Poolside | Recent catalog addition; tier pending |
| `openrouter/poolside/laguna-s-2.1` | OpenRouter | Recent catalog addition; tier pending |
| `inclusionai/ling-3.0-flash:free` | Inclusion AI | Catalog addition; tier pending |
| `crof/greg-2-ultra` | CROF | Catalog addition; tier pending |
| `crof/greg-2-super` | CROF | Catalog addition; tier pending |
| `google/gemini-2.5-flash-lite` | Google | Specialist subagents (file finding/research); not general chat |
| `google/gemini-3.1-flash-lite` | Google | Specialist subagents (file finding/research); not general chat |
| `google/gemini-3.5-flash-lite` | Google | Specialist subagents (file finding/research); not general chat |

---

## Related docs

- [Getting Started](getting-started.md) — 5-minute setup walkthrough
- [9router Integration](9router-integration.md) — wiring the proxy into 9router
- [README](../../README.md) — overview, config reference, quick start
