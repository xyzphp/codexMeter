# Docker 运行说明

本项目提供多阶段 Dockerfile 和 Docker Compose 配置，适合在 Linux 服务器、Docker Desktop 或其他支持 Docker Compose 的环境中运行。GHCR 发布镜像包含 `linux/amd64` 和 `linux/arm64` 两种架构，Docker 会根据宿主机自动选择对应镜像。

## 前置条件

- 已安装 Docker Engine 或 Docker Desktop。
- 已启用 Docker Compose v2，可使用 `docker compose` 命令。
- 已准备 ChatGPT OAuth Access Token 和 ChatGPT Account ID。

## 使用 Docker Compose 启动

`docker-compose.yml` 是部署配置，只使用 GHCR 中的已发布镜像，不包含本地 `build` 配置。

从 GitHub Container Registry 拉取 Compose 文件中锁定的版本镜像：

```bash
docker compose pull
```

启动服务：

```bash
docker compose up -d
```

首次启动不需要创建 `config.json`。Compose 会自动创建宿主机 `config/` 目录，并把它挂载到容器 `/app/config`；后端发现目录为空时会立即生成一份不含凭证的 `config/config.json`。此时访问首页会显示配置引导；进入后台配置页面后，先保存直连或代理设置，再粘贴账号请求即可。

如果从旧版升级，且项目根目录已有文件形式的 `config.json`，请在启动新版 Compose 前复制到新目录：

```powershell
New-Item -ItemType Directory -Force config
Copy-Item config.json config/config.json
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

配置目录为空时，后端会先自动创建默认配置文件；账号凭证尚未配置完整时，访问额度页面会显示配置引导。后台支持分步保存，账号凭证测试成功后重新打开首页即可进入额度面板。

## 从当前源码本地构建

`docker-compose.local.yml` 用于本地源码构建，包含 `build` 配置：

```bash
docker compose -f docker-compose.local.yml up -d --build
```

查看本地构建容器日志：

```bash
docker compose -f docker-compose.local.yml logs -f codex-meter
```

停止本地构建容器：

```bash
docker compose -f docker-compose.local.yml down
```

## 使用 docker run 启动

版本标签发布后，可以直接拉取 GHCR 镜像：

```bash
docker pull ghcr.io/xyzphp/codexmeter:latest
```

PowerShell：

```powershell
docker run -d --name codex-meter --restart unless-stopped -p 8123:8123 -v "${PWD}/config:/app/config" -v "${PWD}/data:/app/data" -e BIND_ADDR=0.0.0.0:8123 -e CONFIG_FILE=/app/config/config.json ghcr.io/xyzphp/codexmeter:latest
```

Linux 或 macOS：

```bash
docker run -d --name codex-meter --restart unless-stopped -p 8123:8123 -v "$(pwd)/config:/app/config" -v "$(pwd)/data:/app/data" -e BIND_ADDR=0.0.0.0:8123 -e CONFIG_FILE=/app/config/config.json ghcr.io/xyzphp/codexmeter:latest
```

如果不使用本地 Compose，也可以手动从当前源码构建：

```bash
docker build -t codex-meter:local .
```

PowerShell：

```powershell
docker run -d --name codex-meter --restart unless-stopped -p 8123:8123 -v "${PWD}/config:/app/config" -v "${PWD}/data:/app/data" -e BIND_ADDR=0.0.0.0:8123 -e CONFIG_FILE=/app/config/config.json codex-meter:local
```

Linux 或 macOS：

```bash
docker run -d --name codex-meter --restart unless-stopped -p 8123:8123 -v "$(pwd)/config:/app/config" -v "$(pwd)/data:/app/data" -e BIND_ADDR=0.0.0.0:8123 -e CONFIG_FILE=/app/config/config.json codex-meter:local
```

## 配置和数据持久化

Compose 会将宿主机的 `config/` 挂载到容器的 `/app/config`，并将宿主机的 `data/` 挂载到容器的 `/app/data`。后端首次启动时自动创建 `config/config.json`，配置页面会继续更新该文件；设置和额度采样历史都会保留在宿主机，停止或重新创建容器不会删除这些文件。

额度采样历史保存在：

```text
data/usage-history.jsonl
```

文件中的每行是一条去重后的采样记录；连续相同的额度值只保留该区间的第一条记录。服务启动时会读取完整历史，先去重再保留最新 48 个变化点，因此重复采样不会占用窗口。记录包含采样时间、5 小时已使用百分比和本周已使用百分比，不包含 OAuth Token、Cookie、Basic Auth 密码或 App API Key。使用 `docker compose down -v` 也不会删除该目录，因为它是宿主机 bind mount。

历史文件采用临时文件完整写入后原子替换，并在 `data/usage-history.jsonl.bak` 中保留上一份有效快照。若主文件为空或包含损坏记录，服务启动时会自动回退读取备份，避免容器重建或进程中断发生在写入期间时丢失整段历史。

额度历史由后端独立定时采集，后台每 5 分钟按时钟整点采集一次，例如 `00、05、10、15` 分钟，并将记录时间标记为对应的 5 分钟时点；服务启动后不会立即补采集。前端手动刷新和自动刷新不会额外写入采样记录。手动刷新仍可请求最新上游数据，但历史记录只由后台采集任务写入。上游异常时，后台会按最近一次成功采集的值记录对应时点的降级状态，并标记 `stale: true`；连续相同的成功或降级采样会合并，不重复占用变化点。即使前端在 00:00–08:00 暂停自动刷新，后端采集任务仍会继续运行。

`config/config.json` 包含 OAuth 凭证，整个 `config/` 目录已被 `.gitignore` 和 `.dockerignore` 排除，不要提交到 Git，也不要在 Dockerfile 中复制真实配置。配置项、认证和代理的详细说明见[配置接入文档](openai-config.md)。

## 反向代理部署

如果通过 Nginx 使用 `/codex/` 等路径转发，容器内部仍监听 `8123`，宿主机端口映射保持 `8123:8123`。反向代理需要把对应前缀转发到容器，并根据部署路径设置 `base_path`；完整的路由和代理说明见[接口文档](openapi.md)。

## 镜像地址和发布规则

GitHub Actions 在 `Release` 工作流中输入 `v*` 版本号后，自动将镜像发布到：

```text
ghcr.io/xyzphp/codexmeter:<版本标签>
ghcr.io/xyzphp/codexmeter:latest
```

例如：

```bash
docker pull ghcr.io/xyzphp/codexmeter:v1.0.0
```

如果 GHCR 包是私有的，需要先登录：

```bash
docker login ghcr.io
```

`docker-compose.yml` 使用具体版本号而不是 `latest`。每次发布前，GitHub Actions 会先将 Compose 文件更新为对应版本并提交到 `main`，然后再构建发布包、创建 GitHub Release 和发布 Docker 镜像。

## 镜像构建方式

Dockerfile 使用两阶段构建：第一阶段按 Docker 的目标平台编译无 CGO 二进制，第二阶段只保留运行所需的二进制、CA 证书、时区数据和配置模板。HTML 页面通过 Go 的 `//go:embed` 编译进二进制，不需要额外挂载 `web/` 目录。

默认构建当前主机平台，也可以使用 Buildx 验证多架构构建：

```bash
docker buildx build --platform linux/amd64,linux/arm64 --tag codex-meter:local .
```

多架构镜像需要推送到镜像仓库后，才能被不同架构的 Docker 主机按需拉取；GitHub Actions 发布流程会自动完成这一步。
