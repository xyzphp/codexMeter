# ChatGPT & Codex OAuth Usage Dashboard

This is a single-binary Go backend and embedded HTML frontend for the LX04 Android WebView dashboard. It uses the read-only ChatGPT internal quota endpoint by default:

```text
GET https://chatgpt.com/backend-api/wham/usage
```

The default `wham` mode does not submit a model prompt. The legacy `probe` mode can call `POST /backend-api/codex/responses` and may consume quota, so it is disabled by default in the configuration UI.

## Run

Copy `config.example.json` to `config.json`, fill in the OAuth access token and ChatGPT account ID, then run:

```powershell
go run .
```

Open the dashboard at `http://127.0.0.1:8080/` and the configuration page at `http://127.0.0.1:8080/settings`.

The default bind address is localhost. If `app_api_key` or `APP_API_KEY` is set, enter the same key on `/settings`; it is kept in browser session storage and then used by the dashboard API calls.

## Authentication sent to `wham/usage`

The backend keeps the OAuth token server-side and sends these upstream headers:

```text
Authorization: Bearer <OPENAI_ACCESS_TOKEN>
chatgpt-account-id: <CHATGPT_ACCOUNT_ID>
openai-beta: codex-1
oai-language: zh-CN
originator: Codex Desktop
accept: application/json
sec-fetch-site: none
sec-fetch-mode: no-cors
sec-fetch-dest: empty
priority: u=4, i
User-Agent: <OPENAI_USER_AGENT>
```

When enabled, `x-openai-fedramp: true` is also sent. The OAuth token and account ID are required. Cookies are not required by this implementation.

## Configuration

Configuration can be stored in `config.json` or supplied through environment variables. `CONFIG_FILE` selects another JSON file.

| Variable | JSON field | Meaning |
|---|---|---|
| `OPENAI_ACCESS_TOKEN` | `openai.access_token` | OAuth access token |
| `CHATGPT_ACCOUNT_ID` | `openai.chatgpt_account_id` | ChatGPT account ID |
| `USAGE_MODE` | `usage_mode` | `wham` (recommended) or `probe` |
| `OPENAI_USER_AGENT` | `openai.user_agent` | Upstream User-Agent |
| `OPENAI_FEDRAMP` | `openai.fedramp` | Send the FedRAMP header |
| `UPSTREAM_PROXY` | `proxy.url` | HTTP/HTTPS proxy |
| `APP_API_KEY` | `app_api_key` | Protect local API endpoints |
| `BASIC_AUTH_ENABLED` | `basic_auth.enabled` | Enable HTTP Basic Auth for pages and APIs |
| `BASIC_AUTH_USER` | `basic_auth.username` | Basic Auth username |
| `BASIC_AUTH_PASSWORD` | `basic_auth.password` | Basic Auth password |
| `BIND_ADDR` | `bind_addr` | Listen address, default `127.0.0.1:8080` |
| `USAGE_CACHE_TTL` | `cache_ttl` | Cache duration, default `10m` |
| `CODEX_MODEL` | `openai.model` | Displayed model; used by `probe` mode |

The config page also allows changing the token, account ID, query mode, User-Agent, proxy and cache duration. Saving writes `config.json` with file mode `0600` on supported systems.

The dashboard uses different built-in alert sounds for quota bands: no sound above 80%, the existing sound from 50% to 80%, a warning sound from above 20% through 50%, and a critical sound at or below 20%. The active alert repeats once after each automatic refresh. It does not use TTS; if browser autoplay is blocked, the sound is retried after the next touch or click.

When Basic Auth is enabled, the browser prompts for the configured username and password before serving the dashboard, settings page, health endpoint, or API. The reverse proxy must pass through the `Authorization` header.

## API

```text
GET /healthz
GET /api/usage
GET /api/usage?force=true
GET /api/usage/analytics
GET /api/usage/analytics?force=true
GET /api/prediction
GET /api/prediction?force=true
GET /api/config
PUT /api/config
```

`/api/usage` returns normalized `five_hour` and `seven_day` windows, reset timestamps, cache status, and a small usage history for the frontend trend line.

`/api/usage/analytics` requests ChatGPT's daily token-usage breakdown and daily workspace-usage counts with the server-side OAuth credentials, merges both responses, includes per-model daily usage, and returns the seven completed dates ending yesterday for the detail-page charts. The incomplete current date is intentionally excluded. The result is cached using the configured cache duration.

`/api/prediction` reads the public `https://codex-resets.com/api/v1/status` endpoint through the configured proxy and caches its result using the same cache duration. It returns the latest public reset announcement, any active watch, and aggregate reset statistics. This public signal is informational only; it never changes the account quota and does not consume OpenAI usage.

## Security notes

- Do not put the OAuth token in HTML, an APK, or frontend JavaScript.
- Keep the service on `127.0.0.1` unless it is behind HTTPS and an access-control layer.
- `chatgpt.com/backend-api` and its response schema are internal interfaces and can change without notice.
- The `probe` path is retained only as an explicit compatibility option; use `wham` for quota display.
