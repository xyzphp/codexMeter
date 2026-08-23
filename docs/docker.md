# Docker 运行说明

本项目提供多阶段 Dockerfile 和 Docker Compose 配置，适合在 Linux 服务器、Docker Desktop 或其他支持 Docker Compose 的环境中运行。

## 前置条件

- 已安装 Docker Engine 或 Docker Desktop。
- 已启用 Docker Compose v2，可使用 `docker compose` 命令。
- 已准备 ChatGPT OAuth Access Token 和 ChatGPT Account ID。

## 使用 Docker Compose 启动

先在项目根目录复制配置模板：

```bash
cp config.example.json config.json
```

编辑 `config.json`，填写 `openai.access_token` 和 `openai.chatgpt_account_id`。如需从设备或公网访问，将 `bind_addr` 保持为 `0.0.0.0:8123`，并按需启用 Basic Auth。

构建镜像并启动服务：

```bash
docker compose up -d --build
```

查看容器日志：

```bash
docker compose logs -f codex-meter
```

查看运行状态：

```bash
docker compose ps
```

停止服务：

```bash
docker compose down
```

启动后访问：

```text
额度页面：http://127.0.0.1:8123/
配置页面：http://127.0.0.1:8123/settings
健康检查：http://127.0.0.1:8123/healthz
```

## 使用 docker run 启动

也可以手动构建并启动容器：

```bash
docker build -t codex-meter:local .
```

PowerShell：

```powershell
docker run -d --name codex-meter --restart unless-stopped -p 8123:8123 -v "${PWD}/config.json:/app/config.json" -e BIND_ADDR=0.0.0.0:8123 codex-meter:local
```

Linux 或 macOS：

```bash
docker run -d --name codex-meter --restart unless-stopped -p 8123:8123 -v "$(pwd)/config.json:/app/config.json" -e BIND_ADDR=0.0.0.0:8123 codex-meter:local
```

## 配置和数据持久化

Compose 会将宿主机的 `config.json` 挂载到容器的 `/app/config.json`，挂载默认可写，因此设置页面保存的配置可以保留在宿主机文件中。停止或重新创建容器不会删除该文件。

`config.json` 包含 OAuth 凭证，已被 `.gitignore` 和 `.dockerignore` 排除，不要提交到 Git，也不要在 Dockerfile 中复制真实配置。配置项、认证和代理的详细说明见[配置接入文档](openai-config.md)。

## 反向代理部署

如果通过 Nginx 使用 `/codex/` 等路径转发，容器内部仍监听 `8123`，宿主机端口映射保持 `8123:8123`。反向代理需要把对应前缀转发到容器，并根据部署路径设置 `base_path`；完整的路由和代理说明见[接口文档](openapi.md)。

## 镜像构建方式

Dockerfile 使用两阶段构建：第一阶段按 Docker 的目标平台编译无 CGO 二进制，第二阶段只保留运行所需的二进制、CA 证书、时区数据和配置模板。HTML 页面通过 Go 的 `//go:embed` 编译进二进制，不需要额外挂载 `web/` 目录。

默认构建当前主机平台，也可以使用 Buildx 构建 amd64 和 arm64 镜像：

```bash
docker buildx build --platform linux/amd64,linux/arm64 --tag codex-meter:local .
```
