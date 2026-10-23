# 程序版本检查与在线升级

## 远端版本检查

版本查询与程序升级共用 `Updates.Repository` 和 `Updates.GitHubProxy`。未设置仓库时使用 `RookieRoll/dockerCopilot`；代理按前缀拼接 GitHub 原始文件地址。版本文件必须返回 HTTP 200，响应正文最多 4 KiB，并且内容必须是三段数字版本号（例如 `v2.2.2` 或 `2026.10.22`），可附带合法的 prerelease/build 后缀。HTML 错误页、空内容、非法版本、非 200 状态、网络错误、超时和取消均作为查询失败返回，不缓存失败结果。

成功结果按完整版本文件 URL 缓存 5 分钟，不同仓库或代理之间不共享缓存。请求使用有限超时并响应调用方取消。FNOS 本地版本不请求远端，直接报告当前版本。

## 在线升级前提

远端版本检查只验证版本元数据，不代表安装成功。升级包使用同一更新源的对应 release：`dockerCopilot-<arch>.tar.gz` 及其 `dockerCopilot-<arch>.tar.gz.sha256` 校验文件。缺少校验文件（旧 release）、状态码异常、校验不匹配或资产超过限制时，准备阶段失败，不修改当前程序。归档必须只包含根目录普通文件 `dockerCopilot-new`，文件必须是与目标相符的 Linux amd64 或 arm64 ELF；路径穿越、符号/硬链接、额外条目及非零尾随数据都会被拒绝。压缩资产上限为 128 MiB，单个解包二进制上限为 256 MiB，展开后的归档流也受限。SHA256 校验用于确认下载内容与同一 release 提供的校验文件一致，不构成独立来源的签名验证。

准备产物写入配置的 `Tasks.Directory` 下 `program-update-stage` 子目录中的专属暂存目录，目录权限为 `0700`，下载包与验证后的暂存二进制权限为 `0600`。暂存目录按任务 ID 的 SHA256 派生，重复任务 ID 会被拒绝，准备失败只清理本次创建的目录。准备成功不会自动替换运行程序或触发退出。准备结果分别提供归档和二进制的 SHA256；交接任务持久化待安装二进制摘要及架构，并在放置前重新核验实际二进制。交接副本仅以 `dockerCopilot-new` 名称原子放置到程序工作目录，权限为 `0755`；运行中的 `dockerCopilot` 不会被覆盖，已存在候选文件也不会被替换。

自身升级准备完成后，系统先暂停新的修改请求并有界等待已有同步修改和后台任务结束；排空失败时恢复接收并保持当前程序运行。pending-install 记录写入成功后才放置交接副本并请求受控重启；交接失败应删除仍与本任务摘要匹配的候选文件并清除pending记录，不能把未知结果报告为成功。新进程核对运行版本、架构和当前可执行文件摘要与pending记录一致，并且HTTP服务实际就绪后，先原子写入launcher就绪结果，再持久确认任务成功。launcher结果仅证明该新进程已运行并就绪，不代表任务存储已确认成功；确认写入失败时服务继续运行并保留pending事实，后续尝试确认或人工恢复，不回滚已就绪程序。版本页面通过任务 ID 恢复查询；若提交响应丢失，页面以提交前生成并持久化的 request ID 查询 `program-update` 任务，只接受 `RequestID` 完全匹配的记录，不按时间猜测，也不重放 PUT。下载/解包完成不表示安装成功。

容器启动脚本只读取程序工作目录下的 `.dockerCopilot-handoff` 和 `.dockerCopilot-handoff-result`，按固定 `key=value` 白名单解析，不执行文件内容。schema 1 的身份字段为 `task_id`、`handoff_id`、`target_version`、`goarch`、`binary_sha256`；结果文件另含 `result=ready|failed-start` 与 `error_code`。两个 UUID 和二进制 SHA256 必须合法，架构仅允许 amd64/arm64；重复、缺失、未知字段及不匹配结果都会被拒绝。结果还必须与本次 marker 的 task/handoff ID、版本、架构和摘要完全相同，旧进程留下的 ready 文件不能确认新交接。ready 仅用于启动脚本判断候选进程已就绪；task 成功只由任务存储确认决定，两者不是跨文件原子事务。

启动脚本先验证候选程序摘要和主机架构，再将旧程序保留为 `.dockerCopilot-previous-<handoff_id>` 并启动候选文件。它最多等待 60 秒的新进程就绪结果；候选提前退出、报告匹配身份的失败结果或超时会恢复旧程序并写入 `failed-start`，同一 handoff 后续启动会保留该失败结果且不重复安装已失败候选。身份字段不匹配的陈旧结果会被忽略，但仍受就绪期限约束。ready 结果出现后，脚本保持新服务为前台子进程并转发 TERM/INT。旧程序备份保留用于人工恢复。脚本夹具可通过 `sh scripts/test-start.sh` 在 Linux 或 Windows Git Bash 环境执行；该测试仅用临时目录和假 shell 程序，不代表 Linux 容器运行验收。

部署还需确保任务状态目录及程序工作目录可写、服务可由现有容器重启策略重新启动，并保留旧二进制供人工恢复。升级不能打断正在执行的容器修改操作。

## 版本页面与任务确认

程序升级保留原有版本入口和更新弹窗，不在“关于”页新增版本、任务或人工恢复区块。点击更新后，内部只在 PUT 被接受时开始跟踪任务；下载或暂存进度不会显示为升级成功。页面刷新或服务重启后，已接受的任务 ID 会恢复查询。若提交响应在服务重启时丢失，页面按提交前保存的 UUID request ID 精确查找 `program-update` 任务，不再次发送升级请求，也不把最近的旧任务误认为本次升级。断线时以退避间隔继续查询，只有任务存储返回 `succeeded` 才确认成功。详细恢复结果、警告和任务/请求 ID 从 API 与日志查询，见[页面兼容边界](ui-compatibility.md)。

## 人工恢复

发生安装失败、旧版本重新启动或任务长期处于待确认时：

1. 查看管理容器日志（例如 `docker logs <容器名>`），按操作时间取得任务 ID 和请求 ID，再通过任务 API 核对错误、恢复结果和当前运行版本。不要再次提交升级，也不要删除任务状态目录。
2. 停止管理容器后，先把当前程序、交接标记、交接结果及旧程序备份复制到安全位置。标记和结果只作为普通文本检查，不能用 `source` 或 `eval` 执行。
3. 交接标记给出 `handoff_id`。在程序工作目录中核对 `.dockerCopilot-previous-<handoff_id>` 是否存在，并确认它是本次升级前保留的旧程序；保留当前 `dockerCopilot-new`、`.dockerCopilot-handoff` 和 `.dockerCopilot-handoff-result` 供诊断。
4. 只有确认旧程序备份可用且版本符合预期后，才将该备份恢复为工作目录中的 `dockerCopilot` 并保留可执行权限。对 Docker 部署可在容器停止后使用 `docker cp` 将备份复制到容器的 `/app/dockerCopilot`；如果工作目录使用宿主机挂载，应直接操作对应挂载目录。不要触碰 `/data` 中的任务、配置或用户数据。
5. 启动容器并登录管理页面，核对 `/api/version?type=local` 显示的运行版本、任务终态和容器健康状态。若没有可验证的旧程序备份、任务仍有未决交接事实或恢复后状态不符，保持现状并交由管理员检查，不删除保护文件或任务记录。

### Linux 发布资产验收记录

Windows 本地已交叉构建 Linux `amd64`、`arm64` ELF，并通过本地包布局和 SHA256 校验检查。该结果验证了本地构建产物，不代表目标二进制已在 Linux 主机运行，也不代表真实 CI 发布资产已验收。实际命令、产物大小和校验值见[架构验收记录](architecture-validation.md)；每次真实发布仍应在 Linux CI 对发布资产执行下述校验并保留输出。

| 架构 | 资产 | 期望包内路径 | 本地验收状态 |
| --- | --- | --- | --- |
| amd64 | `dockerCopilot-amd64.tar.gz` + `.sha256` | 仅根目录 `dockerCopilot-new`，Linux ELF amd64 | Windows 本地交叉构建及包布局/校验通过；Linux 主机运行和真实 CI 资产未验证 |
| arm64 | `dockerCopilot-arm64.tar.gz` + `.sha256` | 仅根目录 `dockerCopilot-new`，Linux ELF arm64 | Windows 本地交叉构建及包布局/校验通过；Linux 主机运行和真实 CI 资产未验证 |

Linux 验收至少核对 `sha256sum -c dockerCopilot-<arch>.tar.gz.sha256`、`tar -tzf dockerCopilot-<arch>.tar.gz` 的唯一根条目，以及解包后二进制的 ELF 架构；不得在开发机上替换运行中的程序。

## 验证

`go test ./internal/updater` 使用 `httptest` 验证版本检查与升级准备：状态码、HTML/空/超限/非法版本、代理路径、缓存隔离和期限、超时与取消；以及缺失/错误校验、压缩/展开上限、错误 ELF 架构、越界/链接/额外归档条目、唯一暂存和失败清理。sidecar 测试验证字段白名单、重复/异常字段拒绝、原子替换和权限。`sh scripts/test-start.sh` 验证候选 ready、提前退出回滚、失败结果保留和摘要不匹配拒绝。所有测试使用本地假数据，不下载真实 release，不执行或替换服务二进制。
