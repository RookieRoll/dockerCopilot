# 前端共享任务状态

`taskAPI.getTask(taskID)` 调用 `GET /api/progress/:taskid`，`taskAPI.listTasks(params)` 调用 `GET /api/tasks`。旧的 `progressAPI.getProgress` 保留并接受可选 Axios 配置。提交仍由具体业务 API 发起；`useTask().submit(() => apiCall())` 只调用一次提交函数，提取接受响应中的 `taskID` 后开始轮询。提交成功代表“已接受”，hook 不把接受本身报告为任务成功，也不会因提交响应丢失自动重放 POST。

`useTask` 接受可选 `taskId`、`containerId` / `containerName`、`storageKey`、相关 `invalidateKeys` 与回调。它将当前 taskID 保存在浏览器本地存储，刷新后优先续查该 ID；没有已保存 ID 时，若提供容器标识，则分别按 `queued` 和 `running` 查询第一页并选最新的活动任务。默认基础存储键为 `docker_copilot_active_task`，容器 ID 或名称会形成独立 scope 键；自定义 `storageKey` 也会按容器 scope 隔离。显式 `taskId` 变化会切换轮询会话，旧会话会取消请求并忽略迟到响应。只有任务达到 `succeeded`、`failed` 或 `interrupted` 时才停止轮询并对每个相关 query key 调用一次失效刷新。

轮询没有固定次数上限。HTTP/网络查询失败与服务端任务结果分开：网络错误进入 `connectionError` 并按指数退避重试，等待时间封顶 30 秒；401 停止轮询并设置 `unauthorized`。不完整任务记录或未知 status 作为结构化查询错误呈现并重试，不会伪装成空的成功结果。任务 `failed` / `interrupted` 仍是查询成功，保留服务端 `detailMsg`、`error`、`recovery` 和 `warnings` 供调用页展示。hook 不依赖进度文字或本地化字符串解释状态。页面应将断线描述为结果待确认，不要显示为操作失败；失败诊断和恢复字段来自任务本身。

API 客户端把带有非成功业务 `code` 的合法 HTTP 响应转为 `ApiError`，HTTP 错误统一保留 `status`、稳定 `errorCode`、`requestId` 和原始 `cause`。业务成功码兼容 `0`、`200`（并保留旧接口使用的 `204`）；无业务 envelope 的成功响应仍原样返回。这里统一传输错误，不改变业务页面对 payload 的展示策略。

容器更新、批量更新、备份导出/恢复、镜像清理和程序升级保留内部任务跟踪，但不增加任务列表或状态面板。业务 API 负责提交操作，共享 hook 负责接受后的查询、持久化活动任务 ID、终态处理和断线重试；页面复用原有行内进度及成功/失败反馈，不将“已接受”或进度阶段显示为成功。详细逐项结果、恢复字段与关联标识从 API 和日志查询，见[页面兼容边界](ui-compatibility.md)。

普通进度是服务进程内状态。收到 HTTP 410 且 `errorCode=OPERATION_RESULT_EXPIRED` 时，共享轮询器合成 `interrupted` / `recovery.status=unknown`，显示结果未知，停止旧进度查询并刷新相关数据；不重复提交修改。浏览器本地保存 ID 不代表服务端跨重启保存操作。
