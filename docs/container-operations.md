# 容器修改占用

`internal/container.Service` 持有一个 `internal/operation.Coordinator`。生产装配应创建唯一的 `Service` 实例，并让单项、详情和批量入口复用它；协调器按 Docker 完整 ID 和规范化名称建立同一任务的别名。只读列表和 inspect 不获取占用。

同步启停、重启和改名按“inspect → 同时占用 ID/当前名称 → 调用 Docker → 释放”执行。改名会在调用 Docker 前把新旧名称都加入同一占用，目标名冲突时不执行重命名。占用冲突返回 `*operation.ConflictError`，提供 HTTP 409 分类、冲突任务 ID 和错误码 `CONTAINER_OPERATION_CONFLICT`。

异步更新必须在提交任务前生成 UUID，并调用 `Service.ReserveUpdate` 获取占用。等价更新的判定键应由稳定的、类型化的字段生成，包含规范容器名、镜像引用、有效的 `deleteOldContainer` 及所有其他会改变操作行为的选项。Docker 名称区分大小写，规范化只去除空白及前导 `/`；容器 ID 则按小写比较。若返回 `existingTaskID`，调用方直接复用该任务，不再向 Runner 提交。新任务通过 `TaskSpec.OnAccepted` 在 queued 记录成功持久化后、任务对 worker 可见前调用 `Lease.Confirm()`；这个回调在 Runner admission 锁内同步执行，只应做快速、本地且不重入 Runner 的接收确认。提交拒绝或 `OnAccepted` 出错时，调用方负责 `Lease.Release()`；此时 Runner 不调用 `OnSettled`。Runner 会把 OnAccepted 失败的已创建记录终结为失败且不执行任务。确认之前的重复提交会等待接受结果：接受时复用已持久化任务，拒绝时自行重新尝试占用。

Runner 接受任务后，`TaskSpec.OnSettled` 在执行和恢复回调都真实返回后调用一次。更新逻辑应将 `lease.Release` 作为该 hook，不能在 Run 回调返回时提前释放；Runner 超时后仍在执行的非协作回调继续持有 worker 与容器占用。更新服务在 Docker 返回新容器 ID 后立即为该任务加入新 ID 别名，直至 `OnSettled` 释放全部身份。

活动操作占用只用于防止本服务内并发修改，不替代端口配置的恢复保护，也不能锁住外部 Docker 客户端。

容器镜像更新通过 `Service.UpdateWithOptions` 执行。任务在任何停止或改名之前保存私有 inspect 快照并持久化快照引用；先拉取镜像，再 inspect 当前引用并把返回的不可变 image ID 用于创建。更新会复制显式 hostname、HostConfig 和网络声明，清除旧实例的网络运行时 ID，并根据 Docker 的 Mounts 检查结果复用原匿名卷。停止时传递原容器的 StopSignal/StopTimeout；字段未设置时将空值交给 Docker 使用其默认值。

若快照或其关键任务检查点保存失败，更新不会拉取镜像或修改容器。备份名称在停止前加入协调器并在改名请求前持久化为计划值；改名成功后再写入 `old-renamed` 阶段。新容器 ID 在启动或清理前写入任务记录并加入占用别名。`deleteOldContainer` 使用请求显式值，缺省时使用全局默认；为保持既有入口语义，`true` 会在新容器启动且（原容器运行时）验证通过后移除旧容器，`false` 会保留重命名后的旧实例。原容器停止时新容器保持停止并跳过运行验证，之后按 `deleteOldContainer` 清理旧实例。

原容器运行时，新实例启动后若配置了 healthcheck，会等待其变为 `healthy`，默认期限为 `Tasks.HealthcheckWait`（120 秒）；未配置 healthcheck 时，会按 `Tasks.RunningObservation`（10 秒）持续 inspect，并且只报告容器持续运行，不代表应用功能正常。健康检查期限不延长；持续运行窗口结束后额外允许固定 1 秒完成最终 inspect，该宽限只覆盖最终读取。每次 inspect 都有截止时间，超期返回的健康结果不接受；无 healthcheck 时，状态暂停、退出、重启中，或窗口内重启计数/启动时间变化都会使验证失败。有 healthcheck 的任务若 inspect 未返回健康状态，也会失败而不会降级成持续运行检查。原容器停止时新实例也保持停止，任务说明未执行运行验证。两种时限都受任务总期限约束。

更新失败后，恢复回调根据阶段和私有快照检查原容器及新实例。新实例只有在任务标签、固定 image ID、目标名称和唯一性都匹配时才会删除；随后尝试恢复原名称与运行状态。任何身份冲突、旧实例缺失、Docker 检查失败或关键恢复检查点写入失败都会保留为需人工核对的未知结果，不按名称猜测删除容器。该恢复不会回滚数据卷内容，也不保证外部 Docker 修改可逆。若旧容器已删除，任务保留证据并要求人工处理。

创建配置会保留原有用户 Labels，并写入本项目保留键 `io.dockercopilot.update-task=<task UUID>`。该标记只用于在 Docker 创建响应丢失时查找并核实唯一候选；恢复前先持久化发现的容器 ID并加入任务占用。恢复移除新实例时不请求删除卷。成功替换后的旧容器清理失败仍会使更新任务失败；后续清理阶段会调整为成功替换附带警告。快照与恢复只覆盖容器配置和身份，不包含数据卷内容。
