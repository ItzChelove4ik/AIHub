# AIHub backend

Go server behind the AIHub app: accounts, AI provider keys, one chat API for every
provider, an OpenAI-compatible API on AIHub's own keys, and OpenFlux channel
management for reaching the server through mobile whitelists. API reference:
[`docs/api.yaml`](../docs/api.yaml).

| Package | What it does |
|---|---|
| `internal/auth` | Email + password (argon2id), 15-minute access JWT, rotating refresh tokens bound to a device, brute-force lock |
| `internal/connections` | Provider keys sealed with AES-256-GCM, key check on save, model catalog (cached 1 h), OpenRouter OAuth PKCE |
| `internal/providers` | Adapters: OpenAI-compatible (OpenAI, OpenRouter, xAI, DeepSeek, Mistral, Groq, custom), Anthropic, Gemini; SSE parser |
| `internal/chat` | Conversations, streaming, titles, usage per user / provider / day, admin limits |
| `internal/apikeys` | `ahk_live_…` keys: hash + prefix stored, scopes, per-key rate limit |
| `internal/tunnel` | Carrier pool, one OpenFlux channel per device, links (`provision.ShareLink`) and QR codes |
| `internal/nodectl`, `cmd/aihub-nodectl` | Privileged helper that only allows `apply / remove / list / status` of `node-install.sh` |
| `internal/store` | Storage interface; `store/sqlite` implements it on SQLite (pure Go, migrations embedded) |

## Run locally

With Docker (build context is the repository root, because the backend imports
OpenFlux from the `third_party/openflux` submodule):

```sh
git clone --recurse-submodules https://github.com/ItzChelove4ik/AIHub
cd AIHub/backend
cp .env.example .env
# put a fresh key into .env: AIHUB_MASTER_KEY=<output of: openssl rand -hex 32>
docker compose up --build
```

Without Docker (Go from `go.mod`):

```sh
cd backend
export AIHUB_MASTER_KEY=$(openssl rand -hex 32)
go run ./cmd/aihub-backend
```

On Windows PowerShell set the variable with
`$env:AIHUB_MASTER_KEY = -join ((1..32) | % { '{0:x2}' -f (Get-Random -Max 256) })`.

The server listens on `http://localhost:8080`; `GET /readyz` answers when it is up.

## Walkthrough: from registration to a streamed answer

```sh
API=http://localhost:8080

# 1. Register (the first account becomes admin) and keep the access token.
curl -s $API/v1/auth/register -H 'content-type: application/json' \
  -d '{"email":"me@example.com","password":"a-long-password","device_id":"laptop","device_name":"Laptop"}'
TOKEN=...   # tokens.access_token from the answer (valid 15 minutes; refresh with /v1/auth/refresh)

# 2. Connect a provider with your API key. The server checks it by listing
#    models, stores it encrypted and only ever shows the last 4 characters.
curl -s $API/v1/connections -H "authorization: Bearer $TOKEN" -H 'content-type: application/json' \
  -d '{"provider":"anthropic","api_key":"<your key>"}'

# 3. The model catalog, straight from the providers' APIs.
curl -s $API/v1/models -H "authorization: Bearer $TOKEN"

# 4. Stream an answer (server-sent events: start, delta..., usage, done).
curl -N $API/v1/chat -H "authorization: Bearer $TOKEN" -H 'content-type: application/json' \
  -d '{"provider":"anthropic","model":"<model id from step 3>","messages":[{"role":"user","content":"Привет!"}]}'

# 5. History stays on the server.
curl -s $API/v1/conversations -H "authorization: Bearer $TOKEN"
```

Other providers work the same way: `openai`, `gemini` (AI Studio key), `openrouter`,
`xai`, `deepseek`, `mistral`, `groq`, or `custom` with `"label"` and `"base_url"` of any
OpenAI-compatible API. "Sign in with OpenRouter" is `GET /v1/oauth/openrouter/start`
(it needs `AIHUB_PUBLIC_URL` to be the server's public https address).

### AIHub API keys (use AIHub from other programs)

```sh
curl -s $API/v1/apikeys -H "authorization: Bearer $TOKEN" -H 'content-type: application/json' -d '{"name":"cli"}'
# → "key": "ahk_live_..."  (shown once)

curl -s $API/openai/v1/chat/completions -H "authorization: Bearer ahk_live_..." -H 'content-type: application/json' \
  -d '{"model":"anthropic/<model id>","messages":[{"role":"user","content":"Hi"}],"stream":true}'
```

Any OpenAI SDK works with `base_url = <server>/openai/v1` and the AIHub key; models are
named `provider/model`.

## OpenFlux channels

Each device gets its own channel (an OpenFlux channel serves one connected device).
`POST /v1/tunnel/channels` creates the node through `aihub-nodectl` and returns the
`openflux://` link once (it carries the channel key). Yandex and Mail.ru documents come
from the pool the admin fills (`/v1/admin/tunnel/carriers`); cups.online rooms are
created automatically; direct to `TUNNEL_PUBLIC_HOST` is always the backup.

`aihub-nodectl` runs as root on the VPS and allows only `apply`, `remove`, `list` and
`status` of `/opt/openflux-node/node-install.sh`; the config goes through a 0600 temp
file, never through arguments. The backend reaches it either through a unix socket
(`aihub-nodectl serve`, `NODECTL_MODE=socket`, for the Docker deployment: no Docker
socket, no sudo in the container) or through a sudoers rule for this one binary
(`NODECTL_MODE=exec`). The VPS setup comes in phase 2 (`deploy/`).

## Tests

```sh
go vet ./...
go test ./...
```

Adapters are tested on recorded provider responses in `internal/providers/testdata`
(no real keys); the helper on a fake `node-install.sh` (Linux/macOS only); channel
links are checked with OpenFlux's own `share.Decode`.

## Provider terms

AIHub uses only official APIs with your own keys and OpenRouter's official OAuth.
Signing in with consumer subscriptions (ChatGPT Plus, Claude Pro/Max, Gemini Advanced)
is not supported: providers forbid it for third-party apps. Some providers do not
serve every region; requests leave from your server, and following the providers'
terms is the server owner's responsibility.
