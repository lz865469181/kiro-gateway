# Kiro Gateway Go

Cross-platform Go implementation of Kiro Gateway. It provides OpenAI- and Anthropic-compatible APIs over Kiro and preserves the original AGPL-3.0 license.

## Features

- OpenAI `GET /v1/models` and `POST /v1/chat/completions`
- Anthropic `POST /v1/messages` and `POST /v1/messages/count_tokens`
- Streaming and non-streaming responses
- Kiro Desktop, AWS SSO OIDC, Enterprise, JSON, refresh-token, and Kiro CLI SQLite credentials
- Multi-account sticky selection, failover, cooldown, model cache, and atomic state persistence
- Tool calls, tool results, JSON Schema sanitation, multimodal base64 images, and conversation repair
- Extended-thinking injection and all response handling modes
- Truncated content/tool detection and one-shot recovery notices
- Native and emulated Kiro MCP web search
- HTTP, HTTPS, SOCKS5, and SOCKS5H proxy support with authentication and `NO_PROXY`
- Retry/backoff, first-token timeout retries, 403 token refresh, network/error classification, CORS, debug captures, and graceful shutdown
- Pure-Go SQLite and static cross-platform builds

## Build

Go 1.23 or newer is required.

```bash
go test ./...
go build -o kiro-gateway ./cmd/kiro-gateway
```

Windows:

```powershell
go build -o kiro-gateway.exe ./cmd/kiro-gateway
.\kiro-gateway.exe
```

Linux and macOS:

```bash
go build -o kiro-gateway ./cmd/kiro-gateway
./kiro-gateway
```

The server defaults to `http://localhost:8000`. CLI flags override environment settings:

```bash
./kiro-gateway --host 127.0.0.1 --port 9000
./kiro-gateway --version
./kiro-gateway healthcheck
./kiro-gateway --healthcheck
```

## Configuration

The Go implementation uses the same environment variables, `.env`, `credentials.json`, credential JSON files, and Kiro CLI SQLite database formats as the original runtime. Set a unique, nonempty `PROXY_API_KEY` (startup rejects common example placeholders) and one credential source:

```env
PROXY_API_KEY="your-gateway-password"
REFRESH_TOKEN="your-kiro-refresh-token"
```

Alternatively use `KIRO_CREDS_FILE`, `KIRO_CLI_DB_FILE`, or multi-account `credentials.json`. `ACCOUNT_SYSTEM=false` uses only the first configured account with no failover or cooldown rotation; `ACCOUNT_SYSTEM=true` enables sticky multi-account failover. Refreshed tokens remain usable in memory if write-back fails; set `JSON_READONLY=true` or `SQLITE_READONLY=true` for intentionally read-only credential stores.

## Docker

```bash
docker compose up --build
```

The default `Dockerfile` builds a static Go binary and runs it as a non-root user in a distroless image. Generated credentials, account state, and debug captures live under the writable `/data` volume by default (`ACCOUNTS_CONFIG_FILE`, `ACCOUNTS_STATE_FILE`, and `DEBUG_DIR`).

## Verification

CI runs tests and vet on Windows, Linux, and macOS, and cross-builds:

- `windows/amd64`
- `linux/amd64`
- `linux/arm64`
- `darwin/amd64`
- `darwin/arm64`

## License

AGPL-3.0-or-later. This implementation is derived from Kiro Gateway by Jwadow.
