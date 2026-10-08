# 前端页面、构建方式与 UI 适配规范

## 前端页面与构建方式

前端页面使用原生 HTML、CSS 和 JavaScript，源文件位于：

```text
web/index.html       LX04 Android WebView 设备页面
web/browser.html     桌面/普通浏览器整页工作台
web/settings.html    配置页面
web/api-docs.html    接口文档与在线调试页面
web/setup.html       首次启动配置引导页面
```

Go 服务通过 `//go:embed` 将这些 HTML 文件嵌入可执行文件。访问 `/` 时，服务根据 User-Agent 分流：识别为 LX04、Android WebView 或 AndroidStream 的请求返回 `web/index.html`；桌面浏览器和普通 Android Chrome 返回 `web/browser.html`。`/settings` 始终返回配置页面，设置页使用与浏览器工作台一致的品牌、卡片、按钮、主题和响应式布局。因此，Release 压缩包不需要额外携带 `web` 目录，解压后直接运行对应平台的可执行文件即可访问页面。

所有内置页面顶部只显示简洁版本号（`vMAJOR.MINOR.PATCH` 或 `dev`），不附加平台、环境、构建后缀或短提交。悬停可查看完整提交和 UTC 构建时间；页面 meta 标签保存版本标签和完整提交，HTTP 响应头及 `/healthz` 保留完整构建信息，便于核对实际运行实例。

页面修改后需要重新执行 Go 构建，修改内容才会进入新的可执行文件。运行时的 `config.json` 不会嵌入程序，OAuth Token、账号 ID、代理和 Basic Auth 等配置需要在服务器上单独创建或通过环境变量提供。

## UI 适配规范

### 目标设备与视口

LX04 设备页面（`web/index.html`）主要适配 LX04 Android 8.1 横屏设备：

- 物理分辨率和系统逻辑分辨率：`800 × 480`
- 屏幕密度：`240 dpi`，约为 Android `1.5` density
- 页面布局依据 WebView 实际 CSS viewport，而不是直接依据物理像素排版
- 如果 Android 原生层叠加了按钮、标题栏或其他控件，实际可用高度可能小于 `480px`

浏览器页面（`web/browser.html`）是独立的整页工作台，不显示“额度 / 详情 / 预测”底部切换导航，而是按页面顺序展示额度、使用分析和重置预测。它使用浏览器宽屏布局，并在较窄窗口中自动改为单列或双列布局。使用分析中的每日 Token 日历固定显示最近 365 个完整日期；起止日期支持 7 / 30 / 60 / 90 天快捷查询和自定义范围，但只影响额度使用、总对话轮次和总计使用量三项汇总指标。页面保留每日 Token 日历和模型使用占比图表，不显示每日对话轮次图和模型每日使用图。点击固定年度日历中的日期时，下方模型占比区域会同步显示当天 Token 数、模型占比和对话轮次；额度历史图分别从完整定时采样归档计算 `weekly_history` 与 `five_hour_history`，按各自额度百分比独立去重，优先保留每个值最近的正常采样，然后绘制最近最多 48 个不同百分比。LX04 设备页仍使用最近 7 个完整日期。重置预测页面同时显示 `active_watch.reset_chance_percent` 模型预测概率和 `community_poll.yes_percent` 社区投票率；页面使用 `api/prediction.history` 绘制最近 26 周的 UTC 日历，按普通重置、赠送重置卡和无重置区分颜色，并通过悬停显示日期和公告内容；LX04 设备页也保留该日历，但不使用额外历史卡片容器，并采用适配 800 × 480 视口的紧凑尺寸。两套页面共用 Go API 和认证机制，但不共用视觉布局。

重置预测同时兼容已排期公告（`scheduled_reset`）和概率观察窗口（`active_watch`）。已排期公告优先显示，包含预计执行的本地时间、每秒更新的倒计时、公告内容和原文链接，并在浏览器端置顶；概率和投票区域在排期模式下隐藏。“距离执行”同时展示倒计时、剩余时间百分比和进度条，按公告时间至预计执行时间的整个窗口计算剩余比例，随时间逐秒缩短，刷新页面不会重新开始。未公布执行时间或缺少有效公告时间时，不显示百分比和进度条；计划时间已过时进度归零并显示“等待确认”，直到上游确认执行。不自动标记重置成功，也不将排期公告绘入已执行重置日历。上游概率为空时显示“暂无概率”。LX04 设备页的自动刷新也会重新请求预测接口，以持续更新信号状态。

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
- 详情页和预测页允许内容区域纵向滚动；额度首页保持在可视区域内，避免整页滚动造成 WebView 操作不稳定。
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

## 多账号入口

浏览器顶部的账号区显示全部账号的 5 小时与本周剩余额度，可通过选择框或卡片按钮切换展示账号。手机浏览器隐藏原生下拉框，点击账号名称展开可滚动的账号卡片，点击卡片上的“切换”按钮即可切换。所有选择项只显示账号名称，不追加主账号或当前展示说明。设置页的账号管理采用独立卡片，与标签栏保留间距，支持新增、选择、改名、更新凭证、切换和删除。更新后的凭证不会回显，切换编辑对象会清空未保存的抓包输入。

LX04 额度页顶部的标题、版本号和紧凑账号按钮保持在同一行，适配 800×480 和设备密度换算后的较窄 CSS 视口；账号名称过长时省略显示。点击账号按钮后弹出可滚动的触控账号列表，展示完整账号名称和剩余额度。点选账号即可切换；关闭按钮、点击背景或 Escape 可收起列表，键盘焦点限制在弹层内并在关闭后返回入口。底部“账号”页保留独立总览；列表可在固定 800×480 视口内滚动。切换时清空旧账号额度与统计，并在当前请求全部完成后启用切换控件，避免不同账号的数据交错显示。请求均使用相对路径，支持反向代理前缀。

失败账号显示自己的错误；有缓存时注明“最近快照”，其他账号继续显示正常数据。详见 [多账号监控说明](multi-account.md)。

设置页的账号选择器在各标签页之间保持可见，网络代理与凭证均属于所选账号。代理认证提供独立用户名、密码输入，密码不回显；切换账号清空密码与新增草稿。代理测试和保存期间禁用账号切换，避免把测试结果保存到另一个账号。

旧应用 API 的主账号与页面展示账号独立：浏览器与 LX04 显式传入所选 account_id，切换页面只更新 active_account_id。未传参数的旧客户端固定读取主账号。页面不展示该兼容标记，设置页隐藏受保护账号的删除入口，服务端仍保留删除保护。
