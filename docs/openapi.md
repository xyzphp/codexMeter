# OpenAPI 接入说明

本项目提供的是一个用于展示 ChatGPT/Codex 额度和统计数据的 Go 服务。接口定义位于仓库根目录的 [`openapi.yaml`](../openapi.yaml)，可以直接导入 Swagger UI、Postman、Apifox 或其他支持 OpenAPI 3.0 的工具。

OpenAI/ChatGPT OAuth 凭证的准备、Account ID 的确认和 `config.json` 填写方法，请先阅读 [OpenAI 配置接入说明](openai-config.md)。

## 1. 接入边界

项目服务端使用配置的 ChatGPT OAuth Bearer Token 请求上游 `wham/usage` 和统计接口，再向调用方返回整理后的数据。调用方不需要、也不应该直接携带 OAuth Token。

本项目的接口不是 OpenAI 官方 API，也不提供聊天补全或 Responses 代理，主要用于：

- 查询当前五小时和七天额度窗口；
- 查询近七天 token、额度、工作区和模型使用统计；
- 查询公开的 Codex Reset 预测信息；
- 读取或更新服务端运行配置；
- 获取前端内置提示音；
- 在受保护的接口文档页面中调试上述接口。

## 2. 导入 OpenAPI 文件

导入文件：

```text
openapi.yaml
```

服务地址按部署方式填写：

```text
本地直连：   http://127.0.0.1:8123
反向代理：   https://你的域名/codex
```

如果 Nginx 使用 `location /codex/`，请确认 `proxy_pass` 的路径转发方式与前端访问地址一致；如果 Nginx 已经去掉 `/codex` 前缀，则使用后端实际收到的路径进行调试。`bind_addr` 和 `base_path` 不在设置页面中配置，需要时通过部署配置文件或环境变量调整。

## 3. 认证方式

认证由服务端配置决定，不能从 OpenAPI 文件推断当前实例是否已启用。

### Basic Auth

启用 Basic Auth 时，所有页面和接口都需要用户名密码。例如：

```bash
curl -u '用户名:密码' \
  'https://你的域名/codex/api/usage'
```

### 管理密钥

如果配置了 `app_api_key`，API 请求可以使用请求头：

```bash
curl \
  -H 'X-App-API-Key: 你的管理密钥' \
  'https://你的域名/codex/api/usage'
```

也可以使用 Bearer 形式：

```bash
curl \
  -H 'Authorization: Bearer 你的管理密钥' \
  'https://你的域名/codex/api/usage'
```

Basic Auth 与管理密钥不要写进前端源码、查询参数、截图或日志。生产环境建议通过 HTTPS 访问。

## 4. 接口文档页面

浏览器访问服务根地址后，点击“接口文档”，或直接访问：

```text
http://127.0.0.1:8123/api-docs
```

页面会展示当前项目维护的接口列表，并提供同源在线调试器。在线调试器可以在已经通过 Basic Auth 的情况下读取当前 App API Key；如果未启用 Basic Auth，则需要手动填写 App API Key，或使用之前会话中保存的密钥。密钥只保存在当前浏览器会话的 `sessionStorage` 中，不会写入 URL。

页面中的“OpenAPI 原文”链接对应：

```text
GET /openapi.yaml
```

该页面适合管理和排查当前服务，不建议直接暴露给不可信用户；配置写入接口仍需在发送前确认请求体。

## 5. 常用接口

| 方法 | 路径 | 用途 |
| --- | --- | --- |
| GET | `/healthz` | 健康检查 |
| GET | `/api/usage` | 当前额度和重置时间 |
| GET | `/api/usage/analytics` | 日期范围统计，包含模型占比 |
| GET | `/api/prediction` | 公共重置预测 |
| GET | `/api/config` | 获取脱敏后的运行配置 |
| GET | `/api/config/app-key` | 在已认证时读取当前管理密钥，供接口调试页使用 |
| POST | `/api/config/test-proxy` | 在填写账号凭证前测试代理或直连 |
| POST | `/api/config/test` | 使用临时配置测试 OpenAI 额度连接，不保存配置 |
| PUT | `/api/config` | 部分更新运行配置 |
| GET | `/audio?kind=normal` | 获取内置提示音 |

额度、统计和预测接口支持：

```text
?force=true
```

该参数会绕过本地缓存立即请求上游。日常轮询建议使用默认缓存；只有手动刷新或需要确认最新数据时才使用 `force=true`。

## 6. 调用示例

### 获取当前额度

```bash
curl -u '用户名:密码' \
  'http://127.0.0.1:8123/api/usage'
```

核心返回结构：

```json
{
  "source": "wham_usage",
  "plan_type": "plus",
  "rate_limit_allowed": true,
  "rate_limit_reached": false,
  "five_hour": {
    "used_percent": 12,
    "reset_at": "2026-08-23T10:00:00Z",
    "remaining_seconds": 3600
  },
  "seven_day": {
    "used_percent": 27,
    "reset_at": "2026-08-28T10:00:00Z",
    "remaining_seconds": 432000
  },
  "fetched_at": "2026-08-23T09:00:00Z",
  "from_cache": false
}
```

### 获取日期范围统计

```bash
curl -u '用户名:密码' \
  'http://127.0.0.1:8123/api/usage/analytics?start_date=2026-08-01&end_date=2026-08-30&force=true'
```

`start_date` 和 `end_date` 可自定义统计范围，格式为 `YYYY-MM-DD`，最多 367 天，结束日期不能晚于当前日期。
两者都省略时兼容返回最近七个已完成日期（不包含当天）。每一天包含 token、额度、用户/线程/对话统计；`models` 包含当天各模型的 `usage_percent`，适合绘制折线图、柱状图和饼图。浏览器页面默认请求最近 30 个完整日期。

### 获取预测数据

```bash
curl -u '用户名:密码' \
  'http://127.0.0.1:8123/api/prediction'
```

`latest_reset` 为空表示暂无公告，`active_watch` 为空表示当前没有活跃观察窗口。这些是公共预测信息，不应直接当作个人账户已重置。

响应中的 `history` 来自 `https://codex-resets.com/api/resets`，按公告时间倒序提供 Reset 记录。每条记录包含重置类型、公告时间、公告内容和原文链接；该历史数据与当前账户额度相互独立，仅用于参考。前端预测页根据这些记录绘制最近 26 周的 UTC 日历热力图。

### 更新配置

配置更新是部分更新，未传字段不会改变：

```bash
curl -X PUT \
  -u '用户名:密码' \
  -H 'Content-Type: application/json' \
  -d '{
    "chatgpt_account_id": "你的 ChatGPT Account ID",
    "access_token": "你的 OAuth Bearer Token",
    "proxy_url": "http://127.0.0.1:7890",
    "cache_ttl": "10m"
  }' \
  'http://127.0.0.1:8123/api/config'
```

安全注意事项：

- `GET /api/config` 永远不会返回完整 OAuth Token；
- Token 会写入服务端配置文件，请限制配置文件权限；
- 不要把这个 PUT 接口直接暴露给不可信的浏览器端用户。

### 临时测试配置

设置页面的“测试连接”调用此接口。它会将请求体与当前服务端配置合并，临时请求一次 `wham/usage`，成功后返回 `ok: true`；不会写入配置文件，也不会返回 OAuth Token 或 Cookie。

```bash
curl -X POST \
  -u '用户名:密码' \
  -H 'Content-Type: application/json' \
  -d '{
    "access_token": "你的 OAuth Bearer Token",
    "cookie": "浏览器 Cookie（可选）",
    "chatgpt_account_id": "你的 ChatGPT Account ID",
    "client_build_number": "9758774",
    "client_version": "prod-...",
    "device_id": "设备 ID",
    "session_id": "会话 ID",
    "client_observation": "观察标识",
    "referer": "https://chatgpt.com/codex/cloud/settings/analytics",
    "proxy_url": "http://127.0.0.1:7890"
  }' \
  'http://127.0.0.1:8123/api/config/test'
```

### 先测试代理

设置页面会先调用这个接口测试代理，再保存代理配置并解锁账号凭证区域。请求体只需要提供代理地址；留空表示测试直连。支持 `http://`、`https://`、`socks5://`，也兼容 `socket5://`。

```bash
curl -X POST \
  -u '用户名:密码' \
  -H 'Content-Type: application/json' \
  -d '{"proxy_url":"http://127.0.0.1:7890"}' \
  'http://127.0.0.1:8123/api/config/test-proxy'
```

测试成功后，设置页面再通过 `PUT /api/config` 保存 `proxy_url`，之后才进入账号凭证配置步骤。该测试不会写入配置文件。

### 获取提示音

```bash
curl -u '用户名:密码' \
  'http://127.0.0.1:8123/audio?kind=warning' \
  -o warning.wav
```

`kind` 可选值为 `normal`、`warning`、`critical`。接口返回 `audio/wav`，不返回页面，也不会发生导航跳转。

## 7. 错误处理

错误响应统一为：

```json
{
  "error": "错误说明"
}
```

常见状态码：

- `400`：查询参数或配置格式错误；
- `401`：Basic Auth 或管理密钥认证失败；
- `404`：提示音类型不存在；
- `502`：上游额度、统计或预测接口请求失败。

上游错误时不要把 OAuth Token 或完整上游响应直接展示给最终用户；建议记录脱敏后的状态码和 request ID，并根据 `from_cache` 判断是否仍有可用缓存。

## 7. Basic Auth 与 Nginx

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

## 8. 安全注意事项

- 不要把 OAuth Token 写入 HTML、APK 或前端 JavaScript；
- 不要提交真实的 `config.json`，该文件已加入 `.gitignore`；
- 服务对外提供时请使用 HTTPS 和访问控制；
- `chatgpt.com/backend-api` 属于内部接口，返回结构可能随时变化；
- 请妥善保管 Basic Auth 密码、API Key 和 OAuth 凭证。
