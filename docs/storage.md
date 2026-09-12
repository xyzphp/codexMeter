# 存储与工程结构

## SQLite 数据库

运行时额度采样存储在 `data/codex-meter.db`，使用纯 Go 的 `modernc.org/sqlite` 驱动，因此继续保持 `CGO_ENABLED=0` 的 Docker 构建方式。

核心表为 `quota_history`：

| 字段 | 说明 |
| --- | --- |
| `id` | SQLite 自增主键 |
| `sampled_at` | 定时采样时间，唯一；使用 UTC RFC3339 文本保存 |
| `used_percent` | 周额度使用百分比 |
| `five_hour_used_percent` | 5 小时额度使用百分比，可为空 |
| `stale` | 上游失败时沿用旧值的降级采样标记 |

每次后台定时采样只写入一条数据库记录，不因为页面的 48 点展示限制删除原始数据。读取接口先从完整记录计算周额度和 5 小时额度两条独立时间线，再分别按自己的百分比去重，优先选正常采样，最后截取最近最多 48 个不同值。

SQLite 开启 WAL、`busy_timeout` 和受控连接数，数据库文件、`-wal` 文件和 `-shm` 文件必须一起放在宿主机 `data/` 挂载目录中。项目已忽略 `data/*.db*`，不会把本地数据库提交到 Git。

## 旧数据迁移

如果数据库表为空，启动时会读取旧版本留下的 JSONL 主文件、`.bak` 文件以及原始归档，按采样时间合并后通过事务导入 SQLite。导入成功后旧 JSONL 不会删除，作为人工回查和安全回退副本保留；后续运行只写 SQLite。

旧版本已经截断的采样无法恢复，迁移只会导入文件中仍然存在的记录。之后所有定时采样都会进入 `quota_history`，因此可以持续累积到 48 个不同的周额度值。

## Go 代码职责

项目仍然编译为一个可执行程序，但按职责拆分为多个同包模块，避免包之间形成循环依赖：

- `main.go`：进程启动、信号处理和 HTTP Server 装配；
- `config.go`、`config_service.go`：配置读取、校验、保存和配置 API；
- `models.go`：API、额度、分析、预测和上游响应模型；
- `usage_service.go`、`usage_runtime.go`、`usage_analytics.go`：额度查询、缓存、定时采集和统计；
- `usage_history.go`、`usage_history_store.go`：历史值去重、JSONL 迁移兼容和 SQLite 持久化；
- `prediction.go`：公共重置状态、社区投票和历史预测；
- `server.go`、`assets.go`、`build_metadata.go`：HTTP 路由、中间件、嵌入资源和版本标识。

新增功能优先放入对应职责文件；只有跨模块共享的模型和常量才放在公共文件中。提交前运行 `go test ./...`、`go vet ./...` 和 `git diff --check`。
