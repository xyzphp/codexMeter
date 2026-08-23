# ChatGPT & Codex 使用额度面板

这是一个使用 Go 编写的单文件后端应用，内嵌 HTML 前端页面，适用于 LX04 Android WebView 额度面板。

默认使用 ChatGPT 的只读接口：`GET https://chatgpt.com/backend-api/wham/usage`。

默认 `wham` 模式不会提交模型提示词。旧版 `probe` 模式会调用 `POST /backend-api/codex/responses`，可能消耗额度，因此默认关闭。

## 主要功能

- 查询 ChatGPT OAuth 账号额度、Token 使用量和重置时间
- 显示本周剩余额度、近七天使用量和各模型占比
- 查询最近七个已完成日期的 Token、对话轮次和模型使用情况
- 对接公开的 Codex Reset 预测接口
- 支持自动刷新、额度区间提示音、HTTP Basic Auth、API Key 和代理
- 前端页面内嵌到 Go 二进制文件中

## 运行

复制 `config.example.json` 为 `config.json`，填写 OAuth Access Token 和 ChatGPT Account ID，然后执行 `go run .`。

默认页面地址：

```text
额度页面：http://127.0.0.1:8080/
配置页面：http://127.0.0.1:8080/settings
```

默认只监听本机地址。配置 `app_api_key` 或 `APP_API_KEY` 后，需要在配置页面输入相同的 Key。

## 前端页面与构建方式

前端页面使用原生 HTML、CSS 和 JavaScript，源文件位于：

```text
web/index.html       额度首页
web/settings.html    配置页面
```

Go 服务通过 `//go:embed` 将这两个 HTML 文件嵌入可执行文件中，并分别提供 `/` 和 `/settings` 页面。因此，Release 压缩包不需要额外携带 `web` 目录，解压后直接运行对应平台的可执行文件即可访问页面。

页面修改后需要重新执行 Go 构建，修改内容才会进入新的可执行文件。运行时的 `config.json` 不会嵌入程序，OAuth Token、账号 ID、代理和 Basic Auth 等配置需要在服务器上单独创建或通过环境变量提供。

## GitHub Actions 自动发布

仓库已配置 GitHub Actions 发布工作流。推送版本标签后，会自动运行测试、静态检查并构建发布包：

`git tag v1.0.0 && git push origin v1.0.0`

工作流会自动创建 GitHub Release，并上传 Windows amd64、Linux amd64 和 Linux arm64 三个平台的压缩包。每个发布包包含对应平台的可执行文件、内嵌的 HTML 页面、`config.example.json` 和中文 README，不包含独立的 `web` 目录和真实的 `config.json`。

## 配置项

配置可以写入 `config.json`，也可以通过环境变量提供；`CONFIG_FILE` 可以指定其他 JSON 配置文件。

| 环境变量 | JSON 配置项 | 说明 |
|---|---|---|
| `OPENAI_ACCESS_TOKEN` | `openai.access_token` | OpenAI OAuth Access Token |
| `CHATGPT_ACCOUNT_ID` | `openai.chatgpt_account_id` | ChatGPT 账号 ID |
| `USAGE_MODE` | `usage_mode` | `wham`（推荐）或 `probe` |
| `OPENAI_USER_AGENT` | `openai.user_agent` | 上游请求 User-Agent |
| `OPENAI_FEDRAMP` | `openai.fedramp` | 是否发送 FedRAMP 请求头 |
| `UPSTREAM_PROXY` | `proxy.url` | HTTP/HTTPS 代理地址 |
| `APP_API_KEY` | `app_api_key` | 保护本地 API 接口 |
| `BASIC_AUTH_ENABLED` | `basic_auth.enabled` | 是否启用 HTTP Basic Auth |
| `BASIC_AUTH_USER` | `basic_auth.username` | Basic Auth 用户名 |
| `BASIC_AUTH_PASSWORD` | `basic_auth.password` | Basic Auth 密码 |
| `BIND_ADDR` | `bind_addr` | 监听地址，默认 `127.0.0.1:8080` |
| `USAGE_CACHE_TTL` | `cache_ttl` | 缓存时长，默认 `10m` |
| `CODEX_MODEL` | `openai.model` | 页面显示模型名称 |

配置页面支持修改 Token、账号 ID、查询模式、User-Agent、代理和缓存时长。保存配置时，在支持的系统上会使用 `0600` 文件权限。

## 数据接口

面板后端使用服务器端 OAuth 凭证请求：

```text
/backend-api/wham/usage
/backend-api/wham/usage/daily-token-usage-breakdown
/backend-api/wham/analytics/daily-workspace-usage-counts
```

`/api/usage/analytics` 会合并每日 Token、工作区对话轮次和模型使用数据，返回最近七个已完成日期；当前未完成日期会被排除。

预测页面通过代理请求：

```text
https://codex-resets.com/api/v1/status
```

预测数据仅供参考，不会修改账号额度，也不会消耗 OpenAI 使用额度。

## 本地 API

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

## Basic Auth 和 Nginx

启用 Basic Auth 后，额度页面、配置页面、健康检查和 API 都需要输入配置的用户名和密码。使用 Nginx 反向代理时，需要透传 `Authorization` 请求头：

```nginx
location /codex/ {
    proxy_pass http://127.0.0.1:8123/;
    proxy_http_version 1.1;
    proxy_set_header Host $host;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;
    proxy_set_header Authorization $http_authorization;
}
```

## 安全注意事项

- 不要把 OAuth Token 写入 HTML、APK 或前端 JavaScript。
- 不要提交真实的 `config.json`，该文件已加入 `.gitignore`。
- 服务对外提供时请使用 HTTPS 和访问控制。
- `chatgpt.com/backend-api` 属于内部接口，返回结构可能随时变化。
- 请妥善保管 Basic Auth 密码、API Key 和 OAuth 凭证。
