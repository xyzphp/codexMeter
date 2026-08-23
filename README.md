# ChatGPT & Codex 使用额度面板

这是一个使用 Go 编写的单文件后端应用，内嵌 HTML 前端页面，适用于 LX04 Android WebView 额度面板。

默认显示 ChatGPT 的只读额度和统计数据。

数据查询不会提交模型提示词，也不会发起模型探测请求，因此额度查询不会通过发送对话请求来获取数据。

## 效果演示

### 设备实机效果

![LX04 设备实机效果](docs/media/codex-meter-device.png)

### 操作演示动画

![操作演示动画](docs/media/codex-meter-demo.gif)

## 主要功能

- 查询 ChatGPT OAuth 账号额度、Token 使用量和重置时间
- 显示本周剩余额度、近七天使用量和各模型占比
- 查询最近七个已完成日期的 Token、对话轮次和模型使用情况
- 对接公开的 Codex Reset 预测接口
- 支持自动刷新、额度区间提示音、HTTP Basic Auth、API Key 和代理
- 前端页面内嵌到 Go 二进制文件中

## 运行

复制配置模板：

```bash
cp config.example.json config.json
```

填写 OAuth Access Token 和 ChatGPT Account ID 后，启动服务：

```bash
go run .
```

当前部署页面地址：

```text
额度页面：http://127.0.0.1:8123/
配置页面：http://127.0.0.1:8123/settings
```

默认只监听本机地址。启用管理密钥或 Basic Auth 后，请按照[配置接入文档](docs/openai-config.md)完成配置。

## 前端与 UI 文档

前端页面结构、Go 内嵌构建方式、Android WebView 设置和 UI 适配验收规范见 [docs/frontend-ui.md](docs/frontend-ui.md)。

## 发布文档

GitHub Actions 自动发布、触发方式、构建平台和发布包内容见 [docs/github-actions.md](docs/github-actions.md)。

## 配置文档

配置文件、环境变量、认证、代理和 OpenAI/ChatGPT 凭证的完整说明见 [OpenAI 配置接入说明](docs/openai-config.md)。

## 接口文档

接口路径、OpenAPI 3.0 定义、认证、反向代理、调用示例和错误处理见：

- [openapi.yaml](openapi.yaml)：可导入 Swagger UI、Postman、Apifox；
- [docs/openapi.md](docs/openapi.md)：中文接口接入和部署说明。
