# GitHub Actions 自动发布

仓库使用 GitHub Actions 自动完成测试、静态检查、跨平台构建和 GitHub Release 发布，工作流文件位于 [.github/workflows/release.yml](../.github/workflows/release.yml)。

## 自动触发

推送版本标签后会触发完整的发布流程：

```bash
git tag v1.0.0
git push origin v1.0.0
```

标签名称需要以 `v` 开头，例如 `v1.0.0`、`v1.2.3`。

也可以在 GitHub 仓库的 **Actions** 页面手动运行 `Release` 工作流。手动运行会执行测试和构建，但不会创建 GitHub Release，因为 Release 任务只对版本标签触发。

## 流程内容

发布工作流包含以下阶段：

1. 使用 Go `1.22.x` 环境执行 `go test ./...`。
2. 执行 `go vet ./...` 进行静态检查。
3. 构建以下平台的无 CGO 可执行文件：
   - Windows amd64
   - Linux amd64
   - Linux arm64
4. 将各平台文件打包为 ZIP 或 tar.gz。
5. 版本标签触发时，自动创建 GitHub Release 并上传全部构建包。

## 发布包内容

每个平台的压缩包包含：

- 对应平台的 `codex-meter` 可执行文件；
- `config.example.json`；
- `openapi.yaml`；
- `README.md`；
- `docs/` 中文文档目录和效果演示素材。

HTML 页面通过 Go 的 `//go:embed` 嵌入可执行文件，因此发布包不需要额外携带 `web/` 目录。运行时使用的 `config.json` 不会进入发布包，需要在部署服务器上单独创建，也不应提交到 Git 仓库。

## 发布前检查

发布标签前建议在本地执行：

```bash
go test ./...
go vet ./...
git diff --check
```

确认配置、OAuth 凭证和真实 `config.json` 没有被加入提交后，再创建并推送版本标签。
