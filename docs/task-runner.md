# 任务执行器

`internal/task.NewRunner` 接收窄化的记录存储接口（`Create`、`Save`、`Get`）及 `RunnerOptions`。默认并发数为 2，等待队列容量为 100，任务期限为 30 分钟，恢复期限为 2 分钟。`Submit` 会先在 admission 锁内预留总容量，再创建 `queued` 记录，成功后才发布到 worker 队列；队列已满或初始持久化失败时不会调用任务函数。任务上下文从 `context.Background()` 创建，不继承 HTTP 请求的取消。

任务回调通过 `TaskSpec.Run` 接收有期限的 context 和 `*Execution`。回调必须配合检查 context，且在每个可能造成破坏的步骤前调用 `Execution.Critical(stage, mutate)` 持久化步骤和所需对象标识。只有 checkpoint 成功且期限仍有效时才可开始该步骤。关键写入失败会把观察到的 checkpoint 事实留在内存供终态/恢复使用，记录任务 ID 与错误并立即取消任务 context；回调必须停止后续修改并返回。`Progress` 更新旧进度字段，但它不是破坏性步骤的持久化门槛。

回调发生 panic 时，runner 捕获 panic 并统一写入 `failed` 终态。普通业务错误在没有恢复 callback 时记录为 `failed` / `not-needed`，不因此新增人工处理标记；已经存在的保护标记会保留。迟到的 `Progress` 或 `Critical` 会收到 `ErrExecutionClosed`，不能覆盖已完成记录。终态写入失败会向 stdout 报告任务 ID 和错误，记录不能被视为可靠保存。`WaitIdle` 只在已接受的 callback 都退出后返回；`Close` 停止新提交、排空队列并等待 callback 退出。

Go 无法安全强杀一个忽略 context 的函数。任务期限到达时，runner 会持久化失败终态并禁止该任务继续通过 `Execution` 更新记录，但仍保留其 worker 并发额度，直到原 callback 返回。这样不会因超时不断释放容量而累积失控 goroutine。任务失败、panic、超时或关键持久化失败时，提供的恢复 callback 会在原执行函数退出后运行，并使用新的、独立的有期限 context。恢复 callback 超时会立即把失败/未知/需人工处理写入终态；worker 仍被占用，直到 callback 真正退出。随后若存储实现支持 `ReconciliationStore`，runner 只更新恢复状态、保护标记和受保护名称，不改写任务终态或最终结果。对于迟迟不返回的 callback，不会并行启动 Docker 恢复操作。

批量任务通过 `SubmitBatch` 作为单个排队任务接受，在一个 worker 内顺序处理项目，不会从 worker 再提交子任务到同一队列。各项目保留成功、失败、跳过状态，并写入 `successCount`、`failedCount`、`skippedCount`；只要有失败项目，总任务就为 `failed`。批量回调使用相同的 `Critical` 约束。

恢复函数签名为 `RecoveryFunc(context.Context, error)`，由 runner 用独立的 `RecoveryTimeout` 包装。任务函数或恢复函数都应遵守收到的 context，且不得遗留在 callback 返回后仍会修改 Docker 状态的后台 goroutine；runner 不会为不合作函数反复创建新的 goroutine 来逃避超时。
