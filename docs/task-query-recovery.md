# 任务查询、清理与启动核对

`QueryService` 只依赖任务存储接口，不依赖 HTTP、Docker SDK 或配置包。创建时由上层注入 `Retention`、`MaxRecords` 和时钟；空值使用七天和一千条默认值。`List` 接受类型、状态、容器 ID、容器名称、页码和每页数量过滤，容器标识使用精确匹配。默认页为 1、每页 50 条，最大每页 100 条；页码小于 0、数量小于 0 或大于 100、以及溢出页偏移均返回明确错误。结果按创建时间倒序排列，时间相同则按 taskID 倒序，分页结果可稳定复现。`GetProgress` 返回显式 `PublicRecord`，保留旧进度字段并排除恢复快照引用和 pending-install 私有交接数据。

`Cleanup` 只删除终态、恢复状态为 `not-needed` 或 `restored`、无人工处理标记且无受保护名称的记录。终态保留时间优先使用 `FinishedAt`，旧版本缺少该字段时回退到 `UpdatedAt`，两者缺失时才使用 `CreatedAt`；年龄判断及数量排序都使用同一时间，避免长任务刚结束后被当作旧记录。它先删除超过保留期的普通记录，再将剩余普通历史数量限制在配置上限内；活动、受保护或恢复状态未解决的记录不参与此数量计算。快照和记录由存储层执行安全删除。若任何文件删除失败，返回部分删除报告和可检查错误，不能把未完成清理伪装为成功。清理调用由服务组合层安排，不在查询接口中隐式触发。

启动顺序应在 `Store.Open` 成功后、任务 runner 开始接受新修改前调用 `StartupReconciler.Run`。它把普通 `queued` / `running` 记录改为 `interrupted`，保留原阶段、容器 ID、名称保护和快照；不会调用任务回调，也不提供 Docker 写操作接口。`queued` 记录标记 `recovery.status=not-needed`；`running` 记录标记 `unknown` 和 `requiresAttention=true`，因为单凭只读 inspect 结果不能证明替换或配置恢复可以安全释放。

容器核对依赖 `ContainerInspector.Inspect(ctx, id)`，仅能返回名称、状态和运行标记。容器任务（`container` 或 `container-` 前缀类型）的批量 `Items` 也可按精确 ID/名称过滤，并将 item ID 作为容器引用进行只读核对；`ImageIDs` 以及非容器任务的 item ID 不会送入容器 inspector。检查结果以白名单 `ContainerCheck` 保存，不保存或序列化 Docker inspect 原始对象。inspect 失败会记录为 `unavailable`（适配器明确返回 `ErrContainerNotFound` 时记录为 `not-found`）并把不含原始错误文本的警告保存在任务记录；不会解释成可安全恢复、清除受保护名称或减少人工处理标记。运维人员应先检查任务记录中的原 stage、旧/新容器 ID 和只读状态，再通过容器页面确认目标实例及配置稳定。确认前保留原/新容器和受保护名称；如果无法确认归属，停止清理并人工核对 Docker。只有完成核对后，授权的后续恢复流程才可用 `Reconcile` 更新恢复状态、保护标记和受保护名称。任务服务本身不执行停止、启动、删除、改名或重放操作。

自身升级任务只有同时满足 `type=program-update`、`running` 状态、完整 typed `PendingInstallHandoff` 和外部 `PendingInstallValidator` 返回完全匹配的 verified proof 时，才会从普通中断处理中跳过，并把 taskID 返回给 updater。marker 必须含规范 UUID handoffID、目标版本、SHA256 和准备时间；proof 还必须逐一匹配 taskID、handoffID、目标版本和摘要。仅有任务类型、缺少 marker、validator 缺失、验证错误或任一字段不匹配，都按普通任务记为 `interrupted` 并保留警告。task 包只保留交接并返回待确认任务；它不会判定安装成功/失败。updater 必须在 HTTP 服务实际就绪后核对运行版本与目标版本，再确认终态。

`StartupReconciler.Run` 的错误表示无法读取任务列表或持久化中断状态，组合层应让服务启动失败并报告原因，避免新任务在旧记录未核对时开始执行。单条 pending-install 验证不通过属于可恢复状态：记录按普通中断保存，warning 保留具体 taskID，不把 validator 的原始错误字符串写入公共任务字段。
