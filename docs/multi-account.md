# 多账号监控与切换

服务支持最多 32 个账号。浏览器首页显示所有账号的五小时与本周剩余额度，选择框和卡片上的“切换”按钮可以设置页面展示账号。LX04 保持横屏 800×480 布局，额度页顶部提供账号选择框，底部“账号”页显示所有账号状态。

页面展示账号保存到 `active_account_id`，重启后继续使用。切换只改变页面展示，旧应用 API 始终使用主账号。后台始终按每 5 分钟的采样周期监控所有凭证完整的账号，未选中的账号也会积累历史。

## 账号管理

打开设置页的“账号管理”：

1. 点击“新增账号”，填写一个便于区分的名称。
2. 在该账号的 ChatGPT 浏览器会话中获取 `wham/usage` 的 Fetch / cURL 请求并粘贴。
3. 点击“识别并保存账号”，服务端验证连接后保存独立凭证。
4. 点击“设为展示账号”，或回首页选择该账号。

已有账号可单独更新凭证或名称。空白名称会被拒绝；未提交的凭证字段保持原值，显式空 Cookie 可以清除旧 Cookie。抓包中提供的 `chatgpt-account-id` 优先于 Token 内的账号声明。更新上游 Account ID 时请新增账号，避免两套额度数据混在同一份历史中。

删除当前展示账号会自动选择第一个剩余账号，主账号必须保留。删除只移除配置中的凭证和运行时缓存，保留数据库历史归档。

## 配置格式与旧版本兼容

完整默认字段见 `config.example.json`。新增的配置结构如下，示例中的凭证占位符必须替换：

```json
{
  "active_account_id": "work",
  "accounts": [
    {"id": "work", "name": "工作账号", "proxy": {"url": "http://proxy.example:8080"}, "openai": {"access_token": "<oauth-access-token>", "chatgpt_account_id": "<chatgpt-account-id>"}},
    {"id": "personal", "name": "个人账号", "proxy": {"url": ""}, "openai": {"access_token": "<oauth-access-token>", "chatgpt_account_id": "<chatgpt-account-id>"}}
  ]
}
```

`id` 是本项目稳定的账号标识，使用 1–64 位字母、数字、下划线或短横线，以字母或数字开头。`active` 和 `usage` 保留给 API 路由。账号名称最多 80 个字符，`active_account_id` 必须指向列表中的账号。API 新增账号时自动生成 id。

旧的顶层 `openai` 结构仍然可以读取，自动映射为 `id=default`、名称“主账号”。通过设置页保存后写入 `accounts` 结构，其他账号配置完整保留。若同时存在 `accounts` 和旧 `openai`，使用 `accounts`。

`OPENAI_*`、`CHATGPT_ACCOUNT_ID` 和 `UPSTREAM_PROXY` 环境变量固定覆盖 `default` 账号，不会随着默认展示账号的切换而覆盖另一套凭证。不存在 `default` 时会增加“环境变量账号”。这些变量在启动和配置文件整体重新加载时应用；缓存时间、Basic Auth、App API Key 和监听参数继续作为全局配置。

原有 `/api/usage` 与 `/api/usage/analytics` 省略或传空 `account_id` 时固定读取主账号，保持旧响应字段、认证、日期范围与 `force` 行为。只有显式指定账号的响应包含 `account_id` 与 `account_name`。主账号优先为迁移旧配置的 `id=default`；自定义配置没有该 id 时使用账号列表第一项。新增、重命名和页面切换不会重新选择主账号。

旧 SQLite 或 JSONL 历史只在首次升级时归属到 `default` 账号（没有该账号时使用账号列表第一项）。迁移标记记录在数据库中，切换或重启不会将旧历史复制给其他账号。历史隔离键同时包含本地 id 和上游 Account ID，离线修改配置或环境变量后也不会混用另一账号的数据。各账号可以在同一时间戳采样而互不覆盖。

主账号不能通过账号删除接口移除，避免旧应用意外读取其他账号；可更新其名称、凭证和代理。账号列表返回 `primary_account_id` 和每项的 `primary` 标记，页面选中项仍使用 `active_account_id`。旧配置读取、更新和测试接口在省略账号时也使用主账号，公共预测保留主账号的网络配置与缓存。

## 每个账号的代理与认证

在设置页上方先选择账号，再进入“网络代理”配置它的 HTTP、HTTPS 或 SOCKS5 代理。需要认证时勾选“代理需要用户名和密码”，分别填写用户名和密码；特殊字符无需自行 URL 编码。密码只保存在服务端，刷新或切换账号后输入框为空，已保存的密码可留空保留。修改用户名需要同时填写新密码；取消认证并测试保存可清除原有认证。关闭“使用代理”将该账号改为直连，直连不会采用进程的 HTTP_PROXY / HTTPS_PROXY。

保存的配置位于 `accounts[].proxy.url`，认证编码在该账号的代理 URL 中。旧的顶层 `proxy.url` 仍兼容读取：未声明 `proxy` 的已有账号继承旧代理，显式 `{"proxy":{"url":""}}` 始终使用直连。通过页面保存后写入独立账号代理，不再写入共享代理。新增账号默认直连，不继承当前账号的代理或密码。

`PUT /api/config` 的 `account_id` 或 `PUT /api/accounts/{account_id}` 可指定目标账号；`proxy_url` 设置地址，`proxy_username`、`proxy_password` 设置认证，`proxy_clear_auth=true` 清除认证。密码省略保持原值，空字符串明确设置空密码。更换地址且不提供认证字段时保留该账号的已有认证。读取配置或账号列表只返回脱敏 `proxy_url` 和 `proxy_password_configured`。

`POST /api/config/test-proxy` 也支持这些字段。省略 `proxy_url` 测试指定账号已保存的代理，传空字符串测试直连；`account_id` 显式为空测试独立新草稿，不使用其他账号的认证。测试不会保存配置，也不发送 OAuth Token 或 Cookie。ChatGPT 返回 403、429 或 5xx 时仍表示网络已连通，原始状态码返回在 `status_code` 中；代理认证失败、DNS、TLS 或连接失败返回 502。

新增账号时可先确认代理，再配置凭证，代理将随新账号一起保存。更改代理只更新目标账号的客户端和缓存；后台采样、额度和统计请求都使用各账号自己的代理。

## API

认证继续使用服务已有的 Basic Auth / App API Key，凭证通过请求体提交，禁止放到 URL 中。账号列表只返回配置状态与脱敏提示；原始配置文件接口继续要求管理认证。所有 API 响应禁止缓存。

| 方法 | 路径 | 行为 |
| --- | --- | --- |
| GET | `/api/accounts` | 读取脱敏账号列表、主账号与页面展示账号 |
| POST | `/api/accounts` | 新增账号，返回 201 和完整脱敏列表 |
| PUT | `/api/accounts/{account_id}` | 更新指定账号名称、凭证或代理 |
| DELETE | `/api/accounts/{account_id}` | 删除账号，保留历史归档 |
| PUT | `/api/accounts/active` | 保存页面展示账号 |
| GET | `/api/accounts/usage?force=true` | 查询全部账号，逐账号返回状态 |
| GET | `/api/usage?account_id=work` | 查询指定账号额度与历史 |
| GET | `/api/usage/analytics?account_id=work` | 查询指定账号统计，日期参数保持原有行为 |

切换请求体为 `{"account_id":"work"}`。新增请求体为 `{"name":"工作账号","access_token":"<oauth-access-token>","chatgpt_account_id":"<chatgpt-account-id>"}`；更新只发送需要改变的字段即可。保存账号不自动请求上游，可先通过 `POST /api/config/test` 验证。

`POST /api/config/test` 与 `PUT /api/config` 支持 `account_id` 指定已有账号。测试时显式传 `"account_id":""` 表示一个独立新账号草稿，必须同时提交 Token 和 ChatGPT Account ID，不继承主账号凭证、代理或代理认证。配置更新不改变页面展示账号；省略 account_id 固定更新或测试主账号。

未知账号返回 404，切换到凭证不完整的账号、删除主账号或最后一个账号返回 409。未指定账号的旧额度和统计接口保留原有 502 配置或上游失败状态。多账号额度总览即使部分账号失败也返回 200，失败条目带 `error`；仍有最近额度快照时同时返回 `usage`、`stale=true` 和 `usage.from_cache=true`，页面明确显示“最近快照”。无快照时显示空额度而不会套用其他账号的数据。

OpenAPI 契约及在线调试器同步覆盖这些接口，所有前端请求使用相对路径，兼容 `/codex` 代理前缀。
