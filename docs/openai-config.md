# OpenAI 配置获取与项目接入

本文说明如何准备本项目需要的 OpenAI/ChatGPT 相关配置，以及如何安全地写入 `config.json`。

## 1. 先区分两类凭证

本项目当前只支持 `usage_mode: wham`，请求的是 ChatGPT/Codex Web 后端的只读额度和统计接口。因此它需要的是：

| 配置 | 用途 | 能否替代 |
| --- | --- | --- |
| ChatGPT OAuth Access Token | 请求 `chatgpt.com/backend-api/wham/*` | 不能用普通 OpenAI API Key 替代 |
| ChatGPT Account ID | 通过 `chatgpt-account-id` 识别账户 | 不是 OpenAI Organization ID 或 Project ID |
| OpenAI API Key | `api.openai.com/v1/*` 的官方 API 请求 | 不能直接用于本项目的 `wham` 接口 |

官方 OpenAI API 文档说明，官方 API 使用 API Key 或短期 workload identity token，并通过 `Authorization: Bearer ...` 发送：[Authentication](https://developers.openai.com/api/reference/overview#authentication)。这不等于本项目访问 ChatGPT Web 后端所需的 ChatGPT OAuth 凭证。

## 2. 如何获取 OAuth 凭证

本项目没有实现 OAuth 登录、回调和 Token 刷新流程，也不会为账户生成凭证。应使用你有权访问的 ChatGPT/Codex 客户端提供的登录和授权流程，并通过该客户端支持的凭证交接方式取得 Access Token。

如果授权客户端没有提供 Token 导出或交接功能，请不要从浏览器 Cookie、Local Storage、网页抓包或第三方 Token 解码网站中复制凭证。OAuth Token 等同于账户会话凭证，泄露后可能导致账户被他人使用；本项目也不能替你判断 Token 是否仍然有效。

取得 Token 后，只在服务端配置一次，不要放到：

- HTML、JavaScript、Android WebView 或 Git 仓库；
- URL 查询参数、截图、工单和聊天记录；
- 前端日志、Nginx access log 或 CI 构建日志。

如果你只有官方 OpenAI API Key，而没有 ChatGPT OAuth Token，本项目的额度页面不能使用该 API Key 查询 `wham` 额度；需要改用与 API Key 对应的官方 API 用量方案，而不是把 API Key 填入 `access_token`。

## 3. 如何确认 Account ID

项目需要的是 ChatGPT Account ID，通常是 UUID 格式，并且必须与 OAuth Token 属于同一个账户。

推荐顺序：

1. 从授权客户端或组织提供的账户信息中复制 ChatGPT Account ID；
2. 如果授权客户端明确提供了 OAuth Token 的本地解析方式，可以仅在本机离线读取 Token payload 中的 `chatgpt_account_id` claim；
3. 不要把完整 Token 上传到在线 JWT 解码网站；如果 Token 中没有该 claim，应向凭证提供方确认 Account ID。

不要把以下值误填为 Account ID：

- ChatGPT 用户 ID；
- OpenAI Organization ID；
- OpenAI Project ID；
- 邮箱地址。

## 4. 配置文件方式

先复制示例配置。Windows PowerShell：

```powershell
Copy-Item config.example.json config.json
notepad config.json
```

Linux/macOS：

```bash
cp config.example.json config.json
chmod 600 config.json
```

只修改 `config.json`，不要修改 `config.example.json`。示例结构如下，值必须替换为你自己的凭证：

```json
{
  "bind_addr": "0.0.0.0:8123",
  "base_path": "/codex",
  "app_api_key": "",
  "basic_auth": {
    "enabled": true,
    "username": "管理用户名",
    "password": "管理密码"
  },
  "cache_ttl": "10m",
  "usage_mode": "wham",
  "openai": {
    "access_token": "你的 ChatGPT OAuth Access Token",
    "chatgpt_account_id": "你的 ChatGPT Account ID",
    "user_agent": "codex-tui/0.146.0",
    "fedramp": false
  },
  "proxy": {
    "url": "http://127.0.0.1:7890"
  }
}
```

上面使用 `0.0.0.0:8123` 是为了演示服务器部署场景；如果不填写 `bind_addr`，项目默认监听 `127.0.0.1:8080`。端口和监听地址请按实际 Nginx、容器或防火墙配置调整。

字段说明：

| 字段 | 必填 | 说明 |
| --- | --- | --- |
| `openai.access_token` | 是 | ChatGPT OAuth Access Token；服务端保存，前端不读取原文 |
| `openai.chatgpt_account_id` | 是 | 与 Token 对应的 ChatGPT Account ID |
| `usage_mode` | 是 | 固定为 `wham` |
| `openai.user_agent` | 否 | 上游请求 User-Agent；留空时使用项目默认值 |
| `openai.fedramp` | 否 | 仅在明确需要 FedRAMP 请求头时设为 `true` |
| `proxy.url` | 否 | 代理 URL，建议写完整的 `http://host:port` 或 `https://host:port` |
| `bind_addr` | 否 | 服务监听地址，例如 `0.0.0.0:8123` |
| `base_path` | 否 | 反向代理前缀，例如 `/codex` |
| `cache_ttl` | 否 | 缓存时长，例如 `10m`、`30s` 或 `0` |

代理地址必须包含协议和端口，例如：

```text
http://192.168.0.21:7890
```

环境变量可以覆盖 JSON 配置，适合容器或服务器部署：

```text
OPENAI_ACCESS_TOKEN
CHATGPT_ACCOUNT_ID
OPENAI_USER_AGENT
OPENAI_FEDRAMP
UPSTREAM_PROXY
USAGE_MODE=wham
CONFIG_FILE=/path/to/config.json
```

## 5. 通过设置页面配置

启动服务后打开：

```text
http://127.0.0.1:8123/settings
```

如果配置了 `base_path=/codex`，则打开：

```text
https://你的域名/codex/settings
```

在设置页面填写 Token、Account ID、代理和缓存时长后保存。保存接口只返回脱敏后的 `token_hint`，不会回显完整 Token。

## 6. 配置后验证

先检查服务：

```bash
curl -u '管理用户名:管理密码' \
  'http://127.0.0.1:8123/healthz'
```

检查配置是否生效：

```bash
curl -u '管理用户名:管理密码' \
  'http://127.0.0.1:8123/api/config'
```

确认返回：

```json
{
  "token_configured": true,
  "token_hint": "****末四位"
}
```

最后强制刷新一次额度：

```bash
curl -u '管理用户名:管理密码' \
  'http://127.0.0.1:8123/api/usage?force=true'
```

正常时返回 `source: "wham_usage"`。如果使用了 `app_api_key`，也可以改用 `X-App-API-Key` 请求头。

## 7. 常见问题

### 返回 401 或 403

通常是 OAuth Token 过期、被撤销、Account ID 不匹配，或者误填了普通 OpenAI API Key。请重新通过授权客户端取得凭证，并核对 Account ID。

### 返回 502

通常是代理、DNS、TLS 或 ChatGPT 上游接口请求失败。先检查代理 URL 是否包含协议，再检查服务器是否能够访问 `chatgpt.com`。

### `token_configured` 为 false

检查实际加载的配置文件。默认是工作目录下的 `config.json`；如果设置了 `CONFIG_FILE`，项目会读取该环境变量指定的文件。

### Token 已配置但页面仍无数据

先执行 `/api/usage?force=true`，再查看服务端日志中的 HTTP 状态码。不要在日志中打印 `Authorization` 请求头或完整 Token。
