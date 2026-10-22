# 现有 API 与文件兼容基线

本文记录单主机管理重构前可观察到的旧行为。新增用例在 Docker Engine 假 HTTP 服务和临时目录中运行，不连接真实 Docker daemon，也不读写 `/data`。成功响应检查字段名和代表性值；时间戳和完整中文提示不作为兼容承诺。

| 功能 | 验证的兼容行为 | 测试或样本 |
|---|---|---|
| 登录 | `POST /api/auth` 无需 JWT，接受现有 `secretKey` 表单字段，成功 JSON 含 `code`、`msg`、`data.jwt`，且 JWT 可验证。 | `internal/handler/routes_test.go::TestAuthRouteDoesNotRequireJWT`；`internal/logic/auth/loginlogic_test.go::TestLoginSuccessResponseRetainsJWTField` |
| 容器基础操作 | 启动、停止、重启、改名调用 Docker API，成功响应保留 `code`、`msg`、`data`。 | `internal/logic/container/basic_operations_compat_test.go::TestBasicContainerOperationsKeepSuccessResponseAndDaemonCalls` |
| 容器列表与端口 | 列表 JSON 保留 lower camel case 端口字段，包含已发布和未发布端口，并显示手工 host 端口；已有用例继续覆盖 inspect 并发上限、缓存和端口清理。 | `internal/logic/container/containerslistlogic_test.go::TestInfoPortsUseLowerCamelCaseJSON`；`dockercopilot_test.go::TestContainersAPIIncludesPublishedAndUnpublishedPorts`、`TestConfiguredPortsAPIPersistsHostNetworkPorts`、`TestContainersAPIInspectsContainersInParallel` |
| 镜像管理 | 镜像列表保留 `id`、`name`、`tag`、`size`、`inUsed`；普通删除按非 force 调用 Docker，并保留成功响应字段。 | `internal/logic/image/compat_test.go::TestImageListAndDeleteCompatibilityWithFakeDaemon` |
| 备份与恢复 | JSON 备份与 Compose 导出从假 daemon 的 inspect 数据生成到临时目录，产物可解析且保留镜像、Env、挂载、网络模式、重启策略和网络别名；旧 Docker create-config JSON 可恢复并创建原名容器、报告完成进度且不启动容器。 | `internal/utiles/backup_compat_test.go::TestBackupWritersPreserveLegacyContainerConfiguration`；`internal/utiles/testdata/legacy-container-backup.json`；`internal/utiles/restore_compat_test.go::TestRestoreLegacyBackupCreatesContainerWithoutStarting` |
| 图标 | 上传新图标时保留旧 `imageLogos.js` 导出和静态 URL，并返回 `code`、`msg`、生成的文件名。 | `internal/handler/icons/testdata/legacy-imageLogos.js`；`internal/handler/icons/uploadhandler_test.go::TestUploadPreservesLegacyImageLogosAndResponseFields` |
| 端口文件 | 旧 schema version 1 JSON 可读取并规范化协议大小写、标签空白和排序；当前仍使用 version 1，新增的可选 `renameIntents` 不要求旧文件迁移。改名未完成时保留原映射和意图，后续只按同一容器 ID 对账；端口清理要求完整稳定的容器列表。 | `internal/utiles/testdata/legacy-container-port-overrides.json`；`internal/utiles/containerportoverrides_compat_test.go::TestLoadLegacyConfiguredPortsFileSample`；`dockercopilot_test.go::TestConfiguredPortsAPIPersistsHostNetworkPorts`；[端口改名恢复说明](container-port-rename-recovery.md) |
| 版本 | 本地版本保留 `version`、`buildDate`；成功远端查询保留 `remoteVersion`。远端响应由 httptest 提供，不访问外网。 | `internal/logic/version/versionlogic_test.go::TestVersionSuccessResponsesKeepLegacyFields` |
| 嵌入式页面 | `/manager` 返回嵌入式 HTML，HTML 引用的 JavaScript 和 CSS 资源均可访问。 | `dockercopilot_test.go::TestEmbeddedManagerAssets` |

## 验证命令

- `go test ./internal/...`：运行服务包测试。
- `go test ./...`：在生成的 `front/` 存在时运行全 Go 测试，包括嵌入式页面与容器/端口 API。
- 在 `frontend/` 下运行 `npm.cmd test`：执行 Node 前端测试。

在重构前提交 `609fe28` 的 detached worktree 中，以上三条命令均通过：internal Go 包测试通过；完整 Go 测试通过（包含嵌入式页面）；前端 10 项测试全部通过。

## 已知基线缺陷

旧版本查询失败分支曾把业务码 `50001` 传给 `WriteHeader`，该值不是合法 HTTP 状态码，可能触发 panic；这是历史基线缺陷，不属于兼容协议。当前错误响应使用合法 HTTP 状态码，并在 JSON 中保留业务码、错误类别和请求 ID。上表的版本兼容用例只断言成功响应字段；错误协议与具体失败场景由版本/HTTP 错误测试覆盖。

## 本次最终回归

2026-10-22 在隔离工作树完成前端 54/54 项测试与生产构建，将新产物同步到 front/ 后，执行 go test -p 2 ./internal/... -count=1 -timeout=180s 和 go test -p 2 ./... -count=1 -timeout=180s 均通过。上表的登录/JWT、容器列表并发、端口、旧备份、图标和嵌入式页面用例均包含在本轮中。前端同时保留主题、登录和任务状态测试；实际浏览器 mock 结果与验证边界见[架构验收记录](architecture-validation.md)。
