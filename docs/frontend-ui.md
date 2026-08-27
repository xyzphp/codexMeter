# 前端页面、构建方式与 UI 适配规范

## 前端页面与构建方式

前端页面使用原生 HTML、CSS 和 JavaScript，源文件位于：

```text
web/index.html       LX04 Android WebView 设备页面
web/browser.html     桌面/普通浏览器整页工作台
web/settings.html    配置页面
```

Go 服务通过 `//go:embed` 将这三个 HTML 文件嵌入可执行文件。访问 `/` 时，服务根据 User-Agent 分流：识别为 LX04、Android WebView 或 AndroidStream 的请求返回 `web/index.html`；桌面浏览器和普通 Android Chrome 返回 `web/browser.html`。`/settings` 始终返回配置页面。因此，Release 压缩包不需要额外携带 `web` 目录，解压后直接运行对应平台的可执行文件即可访问页面。

页面修改后需要重新执行 Go 构建，修改内容才会进入新的可执行文件。运行时的 `config.json` 不会嵌入程序，OAuth Token、账号 ID、代理和 Basic Auth 等配置需要在服务器上单独创建或通过环境变量提供。

## UI 适配规范

### 目标设备与视口

LX04 设备页面（`web/index.html`）主要适配 LX04 Android 8.1 横屏设备：

- 物理分辨率和系统逻辑分辨率：`800 × 480`
- 屏幕密度：`240 dpi`，约为 Android `1.5` density
- 页面布局依据 WebView 实际 CSS viewport，而不是直接依据物理像素排版
- 如果 Android 原生层叠加了按钮、标题栏或其他控件，实际可用高度可能小于 `480px`

浏览器页面（`web/browser.html`）是独立的整页工作台，不显示“额度 / 详情 / 预测”底部切换导航，而是按页面顺序展示额度、使用分析和重置预测。它使用浏览器宽屏布局，并在较窄窗口中自动改为单列或双列布局。两套页面共用 Go API 和认证机制，但不共用视觉布局。

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
