# 单主机管理架构验收记录

本次验收在 Windows x64 隔离工作树进行，采用本地假 Docker HTTP daemon、httptest、临时目录和浏览器 mock API。测试没有操作真实容器、生产管理服务或真实升级安装。

工具链：Go 1.27.2、Node 26.10.0、npm 11.19.1；race 使用 CGO 与便携 LLVM-MinGW Clang 23.1.4；启动脚本使用 Git Bash 5.3.15。生成的 `frontend/dist` 内容同步为根目录 `front/` 后验证嵌入式页面。`-p 2` 限制并行测试包，避免同时多轮编译和同步写盘使短期限故障夹具超时。

## 验收结果

| 范围 | 命令/方式 | 结果 |
| --- | --- | --- |
| 后端服务包 | `go test -p 2 ./internal/... -count=1 -timeout=180s` | 通过 |
| 完整 Go 测试及嵌入资源 | `go test -p 2 ./... -count=1 -timeout=180s` | 通过 |
| 前端测试 | `npm.cmd test` | 54/54 通过 |
| 前端生产构建 | `npm.cmd run build` | 通过 |
| 相关业务包 race | 下述 11 包及根包、HTTP 日志回归 | 全部通过 |
| 启动脚本 | `sh -n start.sh`、`sh -n scripts/test-start.sh`、`sh scripts/test-start.sh` | 通过；MINGW 跳过 POSIX SIGKILL 用例 |
| Linux amd64 / arm64 | `CGO_ENABLED=0` 交叉构建、ELF 架构与本地包校验 | 构建、ELF 架构、唯一根条目和 SHA256 均通过 |
| 格式、关系图和规格 | 改动文件 gofmt、`git diff --check`、`graphify update .`、OpenSpec strict | 全部通过 |

race 覆盖：`internal/task`、`container`、`image`、`settings`、`operation`、`svc`、`updater`、`logic/container`、`logic/image`、`logic/version`、`observability`。分别运行：

```powershell
go test -race -p 2 ./internal/task ./internal/container ./internal/image ./internal/settings ./internal/operation ./internal/svc ./internal/updater ./internal/logic/container ./internal/logic/image ./internal/logic/version ./internal/observability -count=1 -timeout=180s
go test -race -p 2 . ./internal/observability -count=1 -timeout=180s
```

## 故障与兼容覆盖

| 场景 | 验证证据 |
| --- | --- |
| 拉取失败、创建/启动失败、外部重名、响应丢失及受限恢复 | `internal/container/update_test.go`、`internal/logic/container/update_adapter_test.go`；不删除外部容器或数据卷 |
| 批量部分冲突/失败、提交检查点写失败、恢复期限及分项计数 | `batch_adapter_test.go`、`batch_update_test.go`、`internal/task/runner_test.go` |
| 服务重启与批量分项结算 | `internal/task/query_recovery_test.go`；已提交项保留成功，运行项标记未知并保护，排队项跳过，不重放 Docker 修改 |
| 原子写、目录同步与回退失败、损坏记录 | `internal/task/store_test.go` 及提交不确定性回归；不把缺失事实当成完整保护列表 |
| 端口旧快照清理与改名迁移写盘失败 | settings generation、container port list、rename partial 测试；按同一容器 ID 对账 |
| 镜像删除、活动任务保护与 TLS 检查失败 | image service、svc image protection、module registry 测试；force 不绕过任务保护 |
| 旧文件和接口 | [兼容矩阵](compatibility-baseline.md)；登录、列表 8 路 inspect、端口、旧 JSON/Compose、图标和版本字段 |
| 查询断网、刷新续查、接受与完成、部分失败 | 前端 Node 测试和本地浏览器 mock；[容器页面记录](container-ui-smoke.md)、[镜像页面](image-management.md)、[程序升级](program-update.md) |
| 日志与凭据 | observability 及根包真实 HTTP 测试；关联请求/任务/恢复，认证和 URL 凭据脱敏，成功轮询不产生日志噪声 |

## 首轮本地 Linux 资产检查（2026-10-22）

使用 `GOOS=linux GOARCH=<arch> CGO_ENABLED=0 go build -p 2 -trimpath -ldflags="-w -s ..."` 构建。每份归档只有根目录普通文件 `dockerCopilot-new`，同时生成并核对对应 SHA256 文件；ELF machine 分别应为 62 和 183。这验证本地构建产物，不能代替真实 release 资产验收。

| 架构 | ELF machine | 二进制字节数 | 归档 SHA256 |
| --- | --- | --- | --- |
| amd64 | 62 | 22560928 | `d6a35cdabde4bcb1ebbcec3e06a4650f67f4986bf65bbdef550e3a8f6e55a09f` |
| arm64 | 183 | 21037216 | `793d6311b9bf1c718514d4c340aefd9cfd426d73866d99e908dfd0b5fa483400` |

## v2.3.0 发布前 CR（2026-10-23）

审查发现并修复以下问题，均有针对性回归测试：

| 级别 | 问题与修复 | 证据 |
| --- | --- | --- |
| P2 | 端口保存可能与更新、改名竞争，写入临时或过期名称；现在持有容器操作租约直至设置保存完成，并在取得租约后重新 inspect、保护最新名称 | `configuredportslogic_coordination_test.go`：活动更新冲突、inspect 与租约之间完成改名、缺失容器 HTTP/body 均为 404 |
| P2 | 单容器更新结算后仍保留名称保护，导致任务及私有快照无法按保留期删除；现在只在回调实际返回且恢复状态明确时释放保护 | `settled_retention_test.go`：成功并带 warning、修改前失败、恢复成功可过期；未知恢复与超时后仍运行的回调继续保护 |
| P2 | 错误登录密钥返回 HTTP 500；现在返回 HTTP 401、业务码 401 和 `UNAUTHORIZED`，保留请求标识 | `routes_test.go::TestAuthWrongSecretReturnsUnauthorizedEnvelope`，同时检查输入密钥不回显 |
| P3 | 关于页面的人工恢复完整文档链接指向不存在的 `main` 分支；改为仓库实际使用的 `latest` | `frontend/src/components/About.jsx`，前端生产构建通过 |

修复集成后，`go test -p 2 ./... -count=1 -timeout=180s` 全部通过，覆盖根包与全部内部包。新增改动相关的八个包通过 race：`task`、`observability`、`handler`、`logic/auth`、`container`、`logic/container`、`settings`、`operation`。前端使用 Git 跟踪的测试文件运行 `node --test`，54 个测试全部通过，生产构建结果重新同步至嵌入目录后验证。启动脚本隔离夹具复验通过；MINGW 仍跳过 POSIX SIGKILL。

使用 `config.Version=v2.3.0`、`config.BuildDate=2026-10-23` 重新交叉构建 Linux amd64/arm64；ELF machine 分别为 62/183，归档仅含根目录 `dockerCopilot-new`，SHA256 文件与归档校验一致。以上是本地构建验收，GitHub Actions 生成的实际发布资产需另行验收。改动已通过 `gofmt`、`git diff --check` 和 `graphify update .`。

## 验证边界

未连接真实 Docker daemon，未在 Linux amd64/arm64 主机运行目标程序，未对实际部署执行升级、回滚或 Docker socket 生命周期演练。Git Bash 的隔离脚本测试不证明真实 Linux 信号行为；POSIX SIGKILL 用例需在 Linux 执行。真实 CI 发布资产仍需按[升级文档](program-update.md#linux-发布资产验收记录)校验。

部署仍要求单实例独占状态目录，备份和恢复只覆盖容器配置，不包含数据卷内容。故障后保留任务、恢复标记和原始文件，先按[任务排障](task-troubleshooting.md)、[端口改名恢复](container-port-rename-recovery.md)及[日志说明](logging.md)核对，再进行人工处理。
