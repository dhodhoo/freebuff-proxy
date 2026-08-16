# FreeBuff Auth Token Generator

`gen-freebuff-token.sh` (Linux / macOS) and `gen-freebuff-token.ps1`
(Windows) generate a FreeBuff auth token (`cb_...`) through a headless
login flow:

1. The script creates a unique `fingerprintId` and asks the FreeBuff API
   for a login URL.
2. Your browser opens for GitHub OAuth login.
3. The script polls until authentication completes (5-minute timeout).
4. The token is printed and stored according to the mode you chose.

Each run uses a fresh `fingerprintId`, so you can generate a token for a
different account by signing into that GitHub account in the browser
first.

> **Warning:** using FreeBuff tokens through a proxy violates FreeBuff /
> Codebuff terms of service. Accounts may be suspended or banned. You
> accept this risk.

## Quick start — just run it

Run the script with no arguments. It shows recommended options and the
default (Enter) appends the token to `.env` in the current directory —
auto-creating it from `.env.example` if it doesn't exist yet.

## Requirements

- **Windows** — PowerShell 5.1+ (built into Windows 10/11)
- **Linux / macOS** — `bash`, `curl`, `jq`, `openssl`

## Windows (PowerShell)

```powershell
# Recommended: interactive menu — Enter appends to .\.env
.\gen-freebuff-token.ps1

# Non-interactive (piped/CI): auto-appends to .\.env
.\gen-freebuff-token.ps1 < nul

# Explicit modes (skip the menu):
.\gen-freebuff-token.ps1 -ToClipboard
.\gen-freebuff-token.ps1 -Save
.\gen-freebuff-token.ps1 -Append
.\gen-freebuff-token.ps1 -Append -EnvFile D:\path\to\.env
```

If PowerShell blocks the script with an execution-policy error, run the
current session with the policy bypassed first:

```powershell
Set-ExecutionPolicy -Scope Process Bypass
```

## Linux / macOS (bash)

```bash
# Recommended: interactive menu — Enter appends to ./.env
./gen-freebuff-token.sh

# Non-interactive (piped/CI): auto-appends to ./.env
echo | ./gen-freebuff-token.sh

# Explicit modes (skip the menu):
./gen-freebuff-token.sh --clipboard
./gen-freebuff-token.sh --save
./gen-freebuff-token.sh --append
./gen-freebuff-token.sh --env /path/to/.env
```

If the script is not executable, run `chmod +x gen-freebuff-token.sh`
first (or invoke it with `bash gen-freebuff-token.sh`).

## Options

| Behavior                     | Windows                 | Linux / macOS      |
| ---------------------------- | ----------------------- | ------------------ |
| Interactive menu (default)   | *(no flags)*            | *(no flags)*       |
| Append to `.env` AUTH_TOKENS | `-Append`               | `--append`         |
| Target a specific `.env`     | `-EnvFile <path>`       | `--env <path>`     |
| Copy to clipboard            | `-ToClipboard`          | `--clipboard`      |
| Save to CLI credentials file | `-Save`                 | `--save`           |
| Print token only             | *(menu: pick 3)*        | `--print`          |

`--append` / `-Append`:
- creates `.env` from `.env.example` (searched next to the script, in
  its parent directory, then in the current directory) when it is
  missing;
- skips appending when the token is already present.

`gen-token.sh` / `gen-token.ps1` are short aliases for the same scripts,
kept in the repository for convenience.

## Next steps

- **Pooled mode** — run the script (recommended default appends
  `AUTH_TOKENS=cb_...` to `.env`), then start `freebuff-proxy`.
- **Bridge mode** — leave `AUTH_TOKENS` empty; the proxy serves tokens
  provided per request.
- Alternatively, log in once with the official CLI (`npm i -g freebuff &&
  freebuff`): the proxy auto-discovers the token from its credentials
  file on startup.
