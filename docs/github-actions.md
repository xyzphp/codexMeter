# GitHub Actions 自动发布

仓库使用 GitHub Actions 自动完成测试、静态检查、跨平台构建、Docker 镜像构建、GHCR 发布和 GitHub Release 发布，工作流文件位于 [.github/workflows/release.yml](../.github/workflows/release.yml)。

## 创建发布版本

在 GitHub 仓库的 **Actions** 页面运行 `Release` 工作流，并填写版本号，例如 `v1.0.2`。版本号必须符合 `vMAJOR.MINOR.PATCH` 格式。

工作流会按以下顺序执行：

1. 从 `main` 分支更新 `docker-compose.yml` 为本次版本号；
2. 自动提交并推送更新到 `main`；
3. 基于更新后的提交创建版本标签；
4. 执行测试、跨平台构建并创建 GitHub Release；
5. 构建并发布 GHCR Docker 镜像。

## 流程内容

发布工作流包含以下阶段：

1. 更新并提交本次版本对应的 `docker-compose.yml`。
2. 使用 Go `1.22.x` 环境执行 `go test ./...`。
3. 执行 `go vet ./...` 进行静态检查。
4. 构建以下平台的无 CGO 可执行文件：
   - Windows amd64
   - Linux amd64
   - Linux arm64
5. 将各平台文件打包为 ZIP 或 tar.gz。
6. 构建 `codex-meter:ci` Docker 镜像，验证 Dockerfile 和容器构建流程。
7. 创建 GitHub Release 并上传全部构建包。
8. Release 创建成功后，将镜像推送到 `ghcr.io/xyzphp/codexmeter`，同时生成版本标签和 `latest` 标签。

## 发布包内容

每个平台的压缩包包含：

- 对应平台的 `codex-meter` 可执行文件；
- `config.example.json`；
- `openapi.yaml`；
- `README.md`；
- 与本次版本对应的 `docker-compose.yml`；
- `docker-compose.local.yml`；
- `docs/` 中文文档目录和效果演示素材，其中包含 Docker 运行说明。

HTML 页面通过 Go 的 `//go:embed` 嵌入可执行文件，因此发布包不需要额外携带 `web/` 目录。运行时使用的 `config.json` 不会进入发布包，需要在部署服务器上单独创建，也不应提交到 Git 仓库。

## 发布前检查

发布标签前建议在本地执行：

```bash
go test ./...
go vet ./...
docker build --pull --tag codex-meter:ci .
git diff --check
```

确认配置、OAuth 凭证和真实 `config.json` 没有被加入提交后，在 Actions 页面输入版本号启动发布。发布完成后可以拉取镜像：

```bash
docker pull ghcr.io/xyzphp/codexmeter:v1.0.2
```
