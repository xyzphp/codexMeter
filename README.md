# ChatGPT & Codex 使用额度面板

这是一个使用 Go 编写的单文件后端应用，内嵌 HTML 前端页面，适用于 LX04 Android WebView 额度面板。

默认使用 ChatGPT 的只读接口：`GET https://chatgpt.com/backend-api/wham/usage`。

当前只使用 `wham` 只读模式，不提交模型提示词，也不会发起模型探测请求，因此额度查询不会通过发送对话请求来获取数据。

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

默认只监听本机地址。启用管理密钥或 Basic Auth 后，请按照[配置接入文档](docs/openai-config.md)完成配置。

## 前端页面与构建方式

前端页面使用原生 HTML、CSS 和 JavaScript，源文件位于：

```text
web/index.html       额度首页
web/settings.html    配置页面
```

Go 服务通过 `//go:embed` 将这两个 HTML 文件嵌入可执行文件中，并分别提供 `/` 和 `/settings` 页面。因此，Release 压缩包不需要额外携带 `web` 目录，解压后直接运行对应平台的可执行文件即可访问页面。

页面修改后需要重新执行 Go 构建，修改内容才会进入新的可执行文件。运行时的 `config.json` 不会嵌入程序，OAuth Token、账号 ID、代理和 Basic Auth 等配置需要在服务器上单独创建或通过环境变量提供。

## UI 适配规范

### 目标设备与视口

当前页面主要适配 LX04 Android 8.1 横屏设备：

- 物理分辨率和系统逻辑分辨率：`800 × 480`
- 屏幕密度：`240 dpi`，约为 Android `1.5` density
- 页面布局依据 WebView 实际 CSS viewport，而不是直接依据物理像素排版
- 如果 Android 原生层叠加了按钮、标题栏或其他控件，实际可用高度可能小于 `480px`

调试适配问题时，应优先查看以下值：

```javascript
console.log({
  innerWidth: window.innerWidth,
  innerHeight: window.innerHeight,
  devicePixelRatio: window.devicePixelRatio,
  visualWidth: window.visualViewport?.width,
  visualHeight: window.visualViewport?.height
});
```

### HTML 和 CSS 基准

页面必须使用设备宽度 viewport，并禁止浏览器自动放大文字：

```html
<meta name="viewport" content="width=device-width, initial-scale=1, maximum-scale=1, user-scalable=no">
```

```css
html,
body {
  width: 100%;
  height: 100%;
  margin: 0;
  overflow: hidden;
  -webkit-text-size-adjust: 100%;
  text-size-adjust: 100%;
}

.app {
  width: 100vw;
  height: 100vh;
}
```

禁止使用固定 `800px × 480px` 画布再配合 `transform: scale()` 的二次缩放方案。设备密度已经由 WebView 处理，再进行整体缩放会导致文字、进度条、卡片间距和按钮位置在真机上与桌面浏览器不一致。

### 布局与间距

- 页面使用 `flex` 布局，根容器采用 `box-sizing: border-box`。
- 卡片、进度条和图表必须放在明确的布局流中，避免使用未经计算的绝对定位。
- 卡片之间使用固定 `gap` 或 `margin`，不能依赖文字宽度撑开间距。
- 进度条必须设置 `overflow: hidden`，宽度使用父容器的 `100%`，不能超出卡片边界。
- 顶部操作栏、主体卡片和底部导航分别占用独立区域，不能互相覆盖。
- 详情页允许纵向滚动；额度首页和预测页保持在可视区域内，避免整页滚动造成 WebView 操作不稳定。
- 需要在小屏幕上完整显示的文字使用明确的 `font-size`、`line-height` 和 `white-space` 规则，必要时使用省略号，但模型名称和关键数值不能被截断。

### 字体和图表

- 优先使用系统字体：`-apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Arial, sans-serif`。
- 不依赖 Android 系统字体放大来决定布局尺寸，关键数值、模型名称和百分比需要显式设置字号。
- SVG 图表必须设置明确的宽高，并在父容器中预留图例、标签和数值空间。
- 图表、图例和进度条使用 `min-width: 0`、`flex-shrink` 等规则防止挤压相邻卡片。
- 额度剩余状态颜色统一为：`80%~100%` 绿色、`50%~80%` 黄绿色、`20%~50%` 黄色、`0%~20%` 红色。

### Android WebView 容器设置

承载页面的 Android WebView 建议使用以下设置；这些设置属于 Android 容器项目，本 Go 项目只负责提供页面：

```kotlin
webView.settings.apply {
    javaScriptEnabled = true
    domStorageEnabled = true
    useWideViewPort = true
    loadWithOverviewMode = false
    textZoom = 100
    setSupportZoom(false)
    builtInZoomControls = false
    displayZoomControls = false
}

webView.layoutParams.width = ViewGroup.LayoutParams.MATCH_PARENT
webView.layoutParams.height = ViewGroup.LayoutParams.MATCH_PARENT
```

同时建议关闭 WebView 的水平、垂直滚动条，由 HTML 自己控制详情区域的滚动。修改 Android WebView 容器时，不要再对整个 WebView 或网页根节点调用 `setInitialScale()`、`transform: scale()` 等整体缩放操作。

### 适配验收

每次调整 UI 后，应至少在桌面浏览器和真实 Android WebView 各检查一次：

1. Logo、标题和顶部按钮没有重叠。
2. 本周剩余进度条、卡片分割线和自动刷新进度条之间有明确间距。
3. 左右卡片边界完整，进度条没有溢出卡片或屏幕。
4. 额度、详情、预测三个导航按钮的整个按钮区域都可以点击，而不是只能点击文字。
5. 详情页内容可以纵向滚动，图表、模型名称和 Token 数值完整显示。
6. 在存在 Basic Auth、反向代理前缀（例如 `/codex/`）时，页面和相对 API 路径仍然可以正常访问。

## GitHub Actions 自动发布

仓库已配置 GitHub Actions 发布工作流。推送版本标签后，会自动运行测试、静态检查并构建发布包：

`git tag v1.0.0 && git push origin v1.0.0`

工作流会自动创建 GitHub Release，并上传 Windows amd64、Linux amd64 和 Linux arm64 三个平台的压缩包。每个发布包包含对应平台的可执行文件、内嵌的 HTML 页面、`config.example.json`、OpenAPI 文件和 `docs/` 中文文档，不包含独立的 `web` 目录和真实的 `config.json`。

## 配置文档

配置文件、环境变量、认证、代理和 OpenAI/ChatGPT 凭证的完整说明见 [OpenAI 配置接入说明](docs/openai-config.md)。

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

完整的 OpenAPI 3.0 接口定义和中文接入说明见：

- [openapi.yaml](openapi.yaml)：可导入 Swagger UI、Postman、Apifox；
- [docs/openapi.md](docs/openapi.md)：认证、反向代理、调用示例和安全说明。

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
GET /audio?kind=normal|warning|critical
```

`/audio` 返回内嵌的额度提示音，设置页面会直接预加载并播放，不会跳转到额度页面。

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
