# 前端共享任务状态

`taskAPI.getTask(taskID)` 调用 `GET /api/progress/:taskid`，`taskAPI.listTasks(params)` 调用 `GET /api/tasks`。旧的 `progressAPI.getProgress` 保留并接受可选 Axios 配置。提交仍由具体业务 API 发起；`useTask().submit(() => apiCall())` 只调用一次提交函数，提取接受响应中的 `taskID` 后开始轮询。提交成功代表“已接受”，hook 不把接受本身报告为任务成功，也不会因提交响应丢失自动重放 POST。

`useTask` 接受可选 `taskId`、`containerId` / `containerName`、`storageKey`、相关 `invalidateKeys` 与回调。它将当前 taskID 保存在浏览器本地存储，刷新后优先续查该 ID；没有已保存 ID 时，若提供容器标识，则分别按 `queued` 和 `running` 查询第一页并选最新的活动任务。默认基础存储键为 `docker_copilot_active_task`，容器 ID 或名称会形成独立 scope 键；自定义 `storageKey` 也会按容器 scope 隔离。显式 `taskId` 变化会切换轮询会话，旧会话会取消请求并忽略迟到响应。只有任务达到 `succeeded`、`failed` 或 `interrupted` 时才停止轮询并对每个相关 query key 调用一次失效刷新。

轮询没有固定次数上限。HTTP/网络查询失败与服务端任务结果分开：网络错误进入 `connectionError` 并按指数退避重试，等待时间封顶 30 秒；401 停止轮询并设置 `unauthorized`。不完整任务记录或未知 status 作为结构化查询错误呈现并重试，不会伪装成空的成功结果。任务 `failed` / `interrupted` 仍是查询成功，保留服务端 `detailMsg`、`error`、`recovery` 和 `warnings` 供调用页展示。hook 不依赖进度文字或本地化字符串解释状态。页面应将断线描述为结果待确认，不要显示为操作失败；失败诊断和恢复字段来自任务本身。

API 客户端把带有非成功业务 `code` 的合法 HTTP 响应转为 `ApiError`，HTTP 错误统一保留 `status`、稳定 `errorCode`、`requestId` 和原始 `cause`。业务成功码兼容 `0`、`200`（并保留旧接口使用的 `204`）；无业务 envelope 的成功响应仍原样返回。这里统一传输错误，不改变业务页面对 payload 的展示策略。

当前提交任务 hook 与任务 API 已提供给后续页面接入。容器、备份、版本等现有页面仍保留自己的旧 polling 和展示逻辑；它们将在后续页面接入任务中迁移，本次不宣称全站 UI 已替换。
