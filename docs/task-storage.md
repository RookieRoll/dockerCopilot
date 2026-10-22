# 任务记录存储

`internal/task` 在配置的任务目录中为每个 UUID 保存一个版本化 JSON 记录，当前格式为 `schemaVersion: 1`。记录包含 `queued`、`running`、`succeeded`、`failed`、`interrupted` 状态，阶段、时间、容器和镜像标识、受保护名称、类型化操作选项、请求标识、恢复信息、警告及批量项目。为兼容旧进度展示，也保留 `percentage`、`message`、`name`、`detailMsg`、`isDone` 字段。创建记录前由调用方生成规范格式的 UUID 并传入 `TaskID`；存储层不会生成一个调用者拿不到的 ID。

记录通过同目录临时文件写入，设置受限权限、写入并同步后再原子替换目标文件。替换或目录同步失败时，存储层会尽力恢复旧文件；新记录则移除未提交文件。若回退也失败，返回值会同时保留提交错误和回退错误，调用方不能据此假定旧记录已可靠恢复。Unix 上还会尝试同步目录。启动时，损坏或不支持的记录会在安全时移入 `corrupt/` 子目录，并通过默认 `logx` 警告或调用方提供的 `Warnf` 报告；告警不会包含记录正文。有效记录仍可读取。记录和快照拒绝符号链接及非普通文件。

存储目录在 Unix 上设为 `0700`，记录、快照和锁文件设为 `0600`。Go 在 Windows 上的 `Chmod` 不提供 POSIX 权限位语义，文件 ACL 继承目录 ACL；部署方需要通过 Windows ACL 仅向服务账户及指定管理员授权。进程锁使用 Unix `flock` 或 Windows 文件锁 API；进程退出时由操作系统释放锁。

配置快照独立保存为 `<任务 UUID>.snapshot`，不嵌入记录，也不进入 `PublicRecord`。快照只允许在任务处于 `queued` 或 `running` 时首次写入；已有快照不可覆盖，终态任务不可新增快照，以保留最初的恢复基线。公共查询应只序列化显式投影 `Public(record)`。存储层不会记录快照内容。错误和警告会出现在公共投影中，调用方保存前必须脱敏。

终态任务不能通过 `Save` 改写结果。`Reconcile` 只接受终态任务，并只更新核对所得的 `Recovery`、`RequiresAttention` 和 `ProtectedNames`，不会更改原任务结果。删除任务或快照时，必须是终态，且无 `requiresAttention`、受保护名称或未解决恢复状态；恢复状态为空、`not-needed` 或 `restored` 才视为已解决。调用方应先确认相关对象和配置稳定，清除保护信息，再清理快照和记录。

记录保留期限和数量上限由上层通过 `QueryOptions` 注入，默认值为终态普通记录保留七天、最多一千条。活动任务及仍有快照保护或未解决恢复工作的记录不参与数量预算，也不会按普通历史策略清理。`QueryService.Cleanup` 会显式返回删除列表和删除错误；启动失败或文件故障不能被报告成清理成功。具体分页、过滤、启动核对和人工处理流程见 [task-query-recovery.md](task-query-recovery.md)。
