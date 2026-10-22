# 诊断日志

服务使用 go-zero `logx` 生成 JSON 日志，并将同一条记录写入标准输出和配置目录中的持久文件。日志初始化由 `SetupLogWithOptions` 调用 `internal/observability.Setup` 完成；重复调用不会重复挂接 writer。应用继续使用 `logx` 记录日志。

默认配置为 `/data/logs`、INFO 级别、保留 7 天、单个文件最多 20 MiB、目录总量最多 200 MiB。文件按本地日期切分，单文件超过限制后追加带序号的文件；每次写入会清除过期日志，并在容量不足时优先删除最旧的已关闭日志。当前活动文件也计入总量上限。日志文件格式为一行一条 JSON，文件名格式为 `dockercopilot-YYYY-MM-DD[.序号].jsonl`。

超过单文件上限的单条记录仍写标准输出，但不写文件，并向标准错误发出降级警告。日志目录不可创建、清理失败或磁盘写入失败时，也只停用本次文件写入并保留 stdout；后续记录会重试文件端，成功后自动恢复。警告直接写标准错误，限频且不通过应用 logger 回写，避免故障递归。由于容量限制优先于保存文件，当旧日志已无法清理且活动日志会超过总量上限时，新记录只保留在 stdout。

go-zero 的默认 HTTP 日志中间件会在错误请求时转储完整请求，在慢请求时记录原始 URI，因此启动时会关闭该中间件和详细模式；由后续诊断中间件提供白名单请求摘要。go-zero 的 panic recover 仍负责返回 500，但没有默认的请求日志收集器时不会转储请求/堆栈。

所有 `logx` 输出在分发到 stdout 和文件前经过统一脱敏：结构化对象会递归处理敏感键，并隐藏环境变量、inspect、备份和请求体字段；普通错误文本会清理 Bearer/Basic 凭据、认证与 Cookie header、敏感键值、URL userinfo 和敏感 query。脱敏保留 URL 主机、路径以及 DNS、TLS、连接和 daemon 错误类别。日志只应使用白名单摘要，不应主动传入完整业务对象；请求/任务关联标识仍由后续诊断日志任务补充。

## 验证

在仓库根目录运行：

```powershell
go test ./internal/...
go test .
```

包内测试使用临时目录和可注入时钟验证按日/按大小轮转、每次写入清理过期文件、总容量（含活动文件）限制、过大记录、文件故障降级和目录恢复；子进程集成测试验证 JSON 输出双写一致及凭据/env/inspect/备份/panic 文本脱敏。根包的 HTTP 集成测试使用本地假服务，验证 go-zero 不转储 500、慢请求及 panic 请求的 header、query 或 body。测试不访问 Docker daemon 或固定运行时数据目录。

## 事件与级别

任务接受、执行开始、阶段变化、恢复尝试和恢复结果、终态均以结构化事件关联同一 taskId 和提交 requestId。适用字段包含 operation、stage、containerId/name、imageRef/imageId、durationMs、result、errorCode 和经脱敏的原始错误。短容器修改也记录请求标识、目标、结果与耗时；错误展开有界的 cause 链后脱敏，页面仍显示简明错误。

默认 INFO 仅记录阶段及结果。镜像拉取细节使用 DEBUG，每个任务最多每秒一条；启用配置 Logs.Level=debug 后可查看节流后的进度，仍不记录请求体、环境变量或完整 inspect。成功的任务轮询不产生重复错误日志。失败和恢复失败使用 ERROR；清理警告、状态未知和中断使用 WARN。go-zero 没有独立 WARN 输出接口，WARN 事件的传输 level 为 info，结构化 severity 为 WARN；检索时以 severity 字段为准。
