# 程序版本检查与在线升级

## 远端版本检查

版本查询与程序升级共用 `Updates.Repository` 和 `Updates.GitHubProxy`。未设置仓库时使用 `RookieRoll/dockerCopilot`；代理按前缀拼接 GitHub 原始文件地址。版本文件必须返回 HTTP 200，响应正文最多 4 KiB，并且内容必须是三段数字版本号（例如 `v2.2.2` 或 `2026.10.22`），可附带合法的 prerelease/build 后缀。HTML 错误页、空内容、非法版本、非 200 状态、网络错误、超时和取消均作为查询失败返回，不缓存失败结果。

成功结果按完整版本文件 URL 缓存 5 分钟，不同仓库或代理之间不共享缓存。请求使用有限超时并响应调用方取消。FNOS 本地版本不请求远端，直接报告当前版本。

## 在线升级前提

远端版本检查只验证版本元数据，不代表安装成功。升级包使用同一更新源的对应 release：`dockerCopilot-<arch>.tar.gz` 及其 `dockerCopilot-<arch>.tar.gz.sha256` 校验文件。缺少校验文件（旧 release）、状态码异常、校验不匹配或资产超过限制时，准备阶段失败，不修改当前程序。归档必须只包含根目录普通文件 `dockerCopilot-new`，文件必须是与目标相符的 Linux amd64 或 arm64 ELF；路径穿越、符号/硬链接、额外条目及非零尾随数据都会被拒绝。压缩资产上限为 128 MiB，单个解包二进制上限为 256 MiB，展开后的归档流也受限。SHA256 校验用于确认下载内容与同一 release 提供的校验文件一致，不构成独立来源的签名验证。

准备产物写入程序工作目录下专用的 `.dockerCopilot-update-stage` 暂存区，不使用 `Tasks.Directory`。暂存根目录和任务专属目录权限为 `0700`，下载包和暂存二进制权限为 `0600`。任务记录只保存在当前进程内存中，不写入 task JSON；目录配置和旧任务记录不会被读取。暂存目录按任务 ID 派生，重复任务 ID 会被拒绝，准备失败只清理本次创建的目录。准备成功不会自动替换运行程序或触发退出。准备结果分别提供归档和二进制的 SHA256；放置交接副本前会重新核验实际二进制。交接副本仅以 `dockerCopilot-new` 名称原子放置到程序工作目录，权限为 `0755`；运行中的 `dockerCopilot` 不会被覆盖，已存在候选文件也不会被替换。

自身升级准备完成后，系统先暂停新的修改请求并有界等待已有同步修改和后台任务结束；排空失败时恢复接收并保持当前程序运行。系统把已验证的候选二进制放入工作目录后，才原子写入 `.dockerCopilot-handoff` 身份 sidecar 并请求受控重启。若 handoff 未被接受，恢复回调只清理仍与本任务摘要匹配的候选文件，并写入匹配该交接的失败结果；身份不匹配或结果未知时保留文件供人工核对。已接受交接的旧进程不会把任务提前标为成功或失败，也不会删除候选文件；它等待服务关闭时 Runner 取消执行上下文。新进程启动时检测 sidecar；HTTP 服务实际就绪后，再读取并核对 sidecar、运行版本、架构、当前可执行文件摘要及 launcher 结果，然后原子写入 `ready` 结果，并在内存任务查询中重建该任务的成功状态。launcher 的 `ready` 结果只证明新进程已运行且就绪；任务进度不是跨重启持久化的，除了该程序交接 sidecar 外没有 task JSON 或历史任务记录。若 ready 结果写入失败，服务继续运行并保留交接事实，不回滚已就绪程序。

任务 ID 使用有效 request ID 的规范 UUID；若没有有效的 UUID，服务生成 UUID 并同时用作任务 ID 和 request ID。sidecar 只包含 `task_id`，新进程依它重建记录，并将 request ID 设为同一个 task ID。前端以本地保存的任务 ID 续查；如果请求 ID 有效但提交响应丢失，页面可按该精确 ID 查找，不按时间猜测，也不重放 PUT。若没有可用 request ID，且接受响应在 marker 建立前丢失，重启后没有足够事实恢复该任务，系统不会猜测关联。下载/解包完成不表示安装成功。

容器启动脚本只读取程序工作目录下的 `.dockerCopilot-handoff` 和 `.dockerCopilot-handoff-result`，按固定 `key=value` 白名单解析，不执行文件内容。schema 1 的身份字段为 `task_id`、`handoff_id`、`target_version`、`goarch`、`binary_sha256`；结果文件另含 `result=ready|failed-start` 与 `error_code`。两个 UUID 和二进制 SHA256 必须合法，架构仅允许 amd64/arm64；重复、缺失、未知字段及不匹配结果都会被拒绝。结果还必须与本次 marker 的 task/handoff ID、版本、架构和摘要完全相同，旧交接留下的 ready 文件不能确认新交接。新进程写入匹配身份的 `ready` 后，应用在 HTTP 就绪检查通过的流程中重建内存任务结果；不是写入持久任务存储，也不是跨文件原子事务。

启动脚本先验证候选程序摘要和主机架构，再将旧程序保留为 `.dockerCopilot-previous-<handoff_id>` 并启动候选文件。它最多等待 60 秒的新进程就绪结果；候选提前退出、报告匹配身份的失败结果或超时会恢复旧程序并写入 `failed-start`，同一 handoff 后续启动会保留该失败结果且不重复安装已失败候选。身份字段不匹配的陈旧结果会被忽略，但仍受就绪期限约束。ready 结果出现后，脚本保持新服务为前台子进程并转发 TERM/INT。旧程序备份保留用于人工恢复。脚本夹具可通过 `sh scripts/test-start.sh` 在 Linux 或 Windows Git Bash 环境执行；该测试仅用临时目录和假 shell 程序，不代表 Linux 容器运行验收。

部署还需确保程序工作目录可写、服务可由现有容器重启策略重新启动，并保留旧二进制供人工恢复。任务进度仅在进程运行期间可查；程序 handoff sidecar 是跨重启确认升级所需的最小交接事实，不是一般任务日志或任务数据库。升级不能打断正在执行的容器修改操作。

## 版本页面与任务确认

程序升级保留原有版本入口和更新弹窗，不在“关于”页新增版本、任务或人工恢复区块。点击更新后，内部只在 PUT 被接受时开始跟踪任务；下载或暂存进度不会显示为升级成功。浏览器刷新可按本地保存的任务 ID 续查；服务重启会清空普通内存任务进度，只有建立了 handoff sidecar 的程序升级可由新进程按其 `task_id` 重建结果。若提交响应丢失，页面按有效的 UUID request ID 精确查找对应任务，不再次发送升级请求，也不把最近的旧任务误认为本次升级。断线时以退避间隔继续查询；任务只有在 Runner 或新进程确认后才呈现终态。详细恢复结果、警告和任务/请求 ID 从 API 与日志查询，见[页面兼容边界](ui-compatibility.md)。

## 人工恢复

发生安装失败、旧版本重新启动或任务长期处于待确认时：

1. 查看管理容器日志（例如 `docker logs <容器名>`），记录 sidecar 中的任务 ID 和交接 ID，并核对当前运行版本。普通任务进度不跨重启保存，不要根据相邻时间猜测任务归属；不要再次提交升级。
2. 停止管理容器后，先把当前程序、交接标记、交接结果及旧程序备份复制到安全位置。标记和结果只作为普通文本检查，不能用 `source` 或 `eval` 执行。
3. 交接标记给出 `handoff_id`。在程序工作目录中核对 `.dockerCopilot-previous-<handoff_id>` 是否存在，并确认它是本次升级前保留的旧程序；保留当前 `dockerCopilot-new`、`.dockerCopilot-handoff` 和 `.dockerCopilot-handoff-result` 供诊断。
4. 只有确认旧程序备份可用且版本符合预期后，才将该备份恢复为工作目录中的 `dockerCopilot` 并保留可执行权限。对 Docker 部署可在容器停止后使用 `docker cp` 将备份复制到容器的 `/app/dockerCopilot`；如果工作目录使用宿主机挂载，应直接操作对应挂载目录。不要触碰 `/data` 中的任务、配置或用户数据。
5. 启动容器并登录管理页面，核对 `/api/version?type=local` 显示的运行版本、升级任务结果和容器健康状态。当前内存进度仅在该进程生命周期内有效；若没有可验证的旧程序备份、marker 与运行二进制不匹配或状态不符，保持现状并交由管理员检查，不删除 sidecar 或备份。

### Linux 发布资产验收记录

Windows 本地已交叉构建 Linux `amd64`、`arm64` ELF，并通过本地包布局和 SHA256 校验检查。该结果验证了本地构建产物，不代表目标二进制已在 Linux 主机运行，也不代表真实 CI 发布资产已验收。实际命令、产物大小和校验值见[架构验收记录](architecture-validation.md)；每次真实发布仍应在 Linux CI 对发布资产执行下述校验并保留输出。

| 架构 | 资产 | 期望包内路径 | 本地验收状态 |
| --- | --- | --- | --- |
| amd64 | `dockerCopilot-amd64.tar.gz` + `.sha256` | 仅根目录 `dockerCopilot-new`，Linux ELF amd64 | Windows 本地交叉构建及包布局/校验通过；Linux 主机运行和真实 CI 资产未验证 |
| arm64 | `dockerCopilot-arm64.tar.gz` + `.sha256` | 仅根目录 `dockerCopilot-new`，Linux ELF arm64 | Windows 本地交叉构建及包布局/校验通过；Linux 主机运行和真实 CI 资产未验证 |

Linux 验收至少核对 `sha256sum -c dockerCopilot-<arch>.tar.gz.sha256`、`tar -tzf dockerCopilot-<arch>.tar.gz` 的唯一根条目，以及解包后二进制的 ELF 架构；不得在开发机上替换运行中的程序。

## 验证

`go test ./internal/updater` 使用 `httptest` 验证版本检查与升级准备：状态码、HTML/空/超限/非法版本、代理路径、缓存隔离和期限、超时与取消；以及缺失/错误校验、压缩/展开上限、错误 ELF 架构、越界/链接/额外归档条目、唯一暂存和失败清理。svc/version 测试验证普通 task directory 不读写、内存进度从空开始、启动 readiness 后按 sidecar 身份重建成功/失败进度、二进制核验失败时保留 marker，以及 handoff 等待旧进程关闭且不会在旧进程中提前成功。sidecar 测试验证字段白名单、重复/异常字段拒绝、原子替换和权限。`sh scripts/test-start.sh` 验证候选 ready、提前退出回滚、失败结果保留和摘要不匹配拒绝。所有测试使用本地假数据，不下载真实 release，不执行或替换服务二进制。
