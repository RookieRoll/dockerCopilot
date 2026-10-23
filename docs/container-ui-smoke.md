# 容器任务页面本地 mock 验收

> 历史验收：下述任务面板和发现失败重试区已按 2026-10-23 的[页面兼容边界](ui-compatibility.md)撤回。旧截图保留为历史证据，不代表当前页面。

本次使用 Playwright 启动本地 Vite 前端和独立 mock HTTP API；所有浏览器 API 请求均指向本机 mock 服务，未连接真实 Docker daemon 或管理服务。端口链接的目标导航在浏览器中被拦截，不访问目标端口服务。任务状态由 mock 顺序返回 queued、running、succeeded，页面行为按真实接受响应与轮询流程验证。

| 验收项 | 结果 |
| --- | --- |
| 刷新后活动任务发现失败 | mock 首轮 queued/running 列表查询返回 503；页面显示错误和“立即重试”。点击后重新查询两个状态，未提交任何操作请求。 |
| 容器卡片端口链接 | 点击 `18080:80/tcp` 打开独立页面；未打开容器详情，也未新增 API 请求。 |
| 单容器基础操作 | `stop`、`start`、`restart` 分别向对应 API 路径发送一次 POST。 |
| 更换镜像 tag | 在详情中将镜像改为 `nginx:smoke`，仅发送一次 update 请求并携带新 tag；接受后展示任务状态，成功后容器列表刷新为新镜像。 |
| 任务面板 | 活动任务面板可折叠且没有终态关闭按钮；任务成功后出现关闭按钮，关闭后面板移除。 |
| 批量操作 | 选择两个容器后只发送一次 batch POST，内容为 `action=stop` 和两个容器 ID；任务终态展示 2 项成功、0 失败、0 跳过。 |

截图：

- [活动任务发现失败与重试](screenshots/container-task-discovery-retry.png)
- [单项更新执行中](screenshots/container-task-update-running.png)
- [单项更新成功及列表刷新](screenshots/container-task-update-succeeded.png)
- [批量任务成功及分项计数](screenshots/container-batch-succeeded.png)

临时 Playwright mock 脚本未加入仓库；本次运行通过本地 Vite 服务、固定假容器数据及 mock API 完成。截图为浏览器实际渲染页面，不是静态拼图。
