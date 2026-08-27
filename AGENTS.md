# Repository Guidelines（仓库贡献指南）

## 项目结构与模块组织

本项目是一个 Go 1.22 单体服务。`main.go` 包含配置、HTTP 路由、上游请求、缓存、认证和嵌入式 Web 服务；`main_test.go` 存放 Go 测试。浏览器页面位于 `web/`，包括 `index.html`、`browser.html`、`settings.html` 和 `api-docs.html`；静态媒体与项目文档位于 `docs/`。`openapi.yaml` 是版本化的接口契约，并会嵌入最终二进制文件。Docker 与发布相关文件包括 `Dockerfile`、`docker-compose*.yml` 和 `.github/workflows/release.yml`。

## 构建、测试与本地开发命令

请在仓库根目录使用 PowerShell：

```powershell
go test ./...
go vet ./...
go run .
docker compose -f docker-compose.local.yml up -d --build
```

前两个命令用于验证 Go 代码；`go run .` 使用本地配置启动服务；Compose 命令会构建并启动监听 `8123` 端口的本地容器。不要提交 `config.json` 或任何凭证文件。

## 代码风格与命名约定

修改 Go 文件后运行 `gofmt`。遵循 Go 惯用命名：导出标识符使用 `PascalCase`，内部标识符使用 `camelCase`，请求处理函数使用 `handle<Resource>`。HTTP 路径使用小写且表达清晰。前端继续使用当前的原生 HTML、CSS 和 JavaScript 风格，保留响应式布局；除非确有必要，不要新增前端构建依赖。

## 测试规范

涉及路由、认证、配置或上游数据解析时，在 `main_test.go` 中新增或更新表驱动测试。提交前运行 `go test ./...`、`go vet ./...` 和 `git diff --check`。修改页面时，还应检查 JavaScript 语法，并通过本地服务或 Docker 容器验证相关路由。

## API 文档与页面同步

每次新增或修改 API，都必须同步更新 `openapi.yaml` 以及 `web/api-docs.html` 中的接口列表和在线调试器，包括请求方法、路径、认证方式、请求示例和响应行为。公开配置或接口契约发生变化时，同时更新对应的 `docs/*.md` 文档。

## 提交与合并请求规范

提交信息使用简洁的 Conventional Commits 风格，例如 `feat: add ...`、`fix: correct ...` 或 `ui: adjust ...`。每次提交应保持主题单一。合并请求需要说明行为变化、列出验证命令、注明配置或部署影响；涉及浏览器或设备页面变化时，应附上截图。

## 安全与配置注意事项

OAuth Token、Cookie、Basic Auth 密码、App API Key 和代理凭证都属于敏感信息。请将它们保存在被忽略的本地配置或部署密钥中，禁止写入源代码、日志、截图、URL 或文档示例。配置接口和在线调试接口必须继续保留认证保护。
