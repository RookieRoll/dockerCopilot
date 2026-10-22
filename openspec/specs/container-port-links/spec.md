# container-port-links Specification

## Purpose

让用户在容器管理页面直接看到 Docker 容器的端口映射，并安全、明确地打开已发布的网页服务，同时保留对未发布端口和非网页协议端口的准确表达。

## Requirements

### Requirement: 容器列表返回完整端口映射
容器列表接口 MUST 为每个容器返回 ports 数组。数组中的每一项 MUST 表示一条可展示的逻辑端口映射，并包含宿主机地址、宿主机端口、容器端口和协议；没有宿主机映射时，宿主机端口 MUST 表示为未发布状态而不是伪造一个可访问端口。对于 hostPort、containerPort 和 protocol 完全相同、仅宿主机绑定地址不同的重复项，接口 MUST 只保留一条，避免页面重复展示；其他不同端口、不同协议或不同容器端口的映射 MUST 保留。已有容器字段和接口鉴权行为 MUST 保持兼容。

#### Scenario: 返回已发布端口
- **WHEN** Docker 返回一条宿主机 8080 映射到容器 80 的 TCP 端口
- **THEN** 对应容器的 ports 数组包含 hostPort=8080、containerPort=80 和 protocol=tcp，并保留该条映射的 hostIp

#### Scenario: 返回仅暴露未发布端口
- **WHEN** Docker 返回容器端口 80/tcp 但没有宿主机端口
- **THEN** ports 数组仍包含该端口，且其 hostPort 表示未发布，不能被接口或页面当作宿主机 80 端口

#### Scenario: 保留多条不同绑定
- **WHEN** 一个容器存在多个宿主机端口、不同协议或不同绑定地址的端口映射
- **THEN** 每一条映射都出现在 ports 数组中，不能因端口号相同而错误合并

#### Scenario: 容器没有端口信息
- **WHEN** Docker 返回的容器没有任何端口项
- **THEN** 接口返回空的 ports 数组，其他容器字段仍正常返回

### Requirement: 容器页面展示端口语义
容器页面 MUST 展示每个容器返回的端口项，并同时表达宿主机端口（若存在）、容器端口和协议。未发布端口和 UDP 端口 MUST 可被用户识别为不可通过网页链接打开的端口。

#### Scenario: 展示已发布 TCP 端口
- **WHEN** 容器包含 0.0.0.0:8080 -> 80/tcp 映射
- **THEN** 页面展示包含 8080、80 和 tcp 的端口信息，并将该项呈现为可访问链接

#### Scenario: 展示未发布或 UDP 端口
- **WHEN** 容器包含 80/tcp 未发布端口或 5353:53/udp 映射
- **THEN** 页面展示端口信息，但该项不是网页链接，不向用户承诺可以通过浏览器访问

### Requirement: 已发布 TCP 端口生成当前主机链接
对于 hostPort 大于 0 且 protocol 为 tcp 的端口项，页面 MUST 根据浏览器当前页面 URL 生成目标地址：保留当前协议和 hostname，将端口替换为 hostPort，并将路径、查询参数和片段设置为根路径 /。页面 MUST 在新的浏览上下文打开目标地址，当前管理页面不得被导航替换。

#### Scenario: 从带管理端口的 HTTP 地址打开容器服务
- **WHEN** 用户当前访问 http://192.0.2.10:12712/manager，并点击 hostPort=8080 的 TCP 端口
- **THEN** 新标签页打开 http://192.0.2.10:8080/，原标签页仍保持容器管理页面

#### Scenario: 从 HTTPS 地址打开容器服务
- **WHEN** 用户当前访问 https://example.test/manager，并点击 hostPort=8443 的 TCP 端口
- **THEN** 新标签页使用 https 协议和 example.test 主机打开 8443 端口，原管理页面不发生导航

#### Scenario: 当前地址包含查询和片段
- **WHEN** 当前管理页面 URL 包含查询参数或片段，并点击可访问端口
- **THEN** 目标 URL 的路径为 / 且不继承管理页面的查询参数和片段

### Requirement: 端口链接不扩大容器卡片行为
端口链接点击 MUST 阻止事件冒泡，不得同时触发容器卡片的详情、选择或其他父级点击行为；链接 MUST 使用浏览器提供的安全新窗口关系属性，避免新页面反向控制管理页面。

#### Scenario: 点击端口不打开容器详情
- **WHEN** 用户点击容器卡片中的可访问端口
- **THEN** 浏览器打开目标服务页面，且容器卡片的详情点击处理不被触发

#### Scenario: 未发布端口不可点击
- **WHEN** 用户点击只包含容器端口而没有 hostPort 的端口文本
- **THEN** 页面不打开新的服务地址，也不将该端口解释为当前管理页面的端口

#### Scenario: UDP 端口不可作为网页链接
- **WHEN** 用户点击 UDP 端口的展示文本（若该文本可获得焦点）
- **THEN** 页面不创建 HTTP 或 HTTPS 网页目标，不触发端口导航


### Requirement: host 网络容器的手工端口配置
系统 MUST 仅对 Docker host 网络模式容器提供可保存的手工端口配置。配置 MUST 保存在 /data/config/container-port-overrides.json，按容器名称关联，并在 GET /api/containers 的 configuredPorts 字段中返回。

#### Scenario: 保存 host 网络端口
- **WHEN** 用户为 host 网络容器提交 TCP 12712 和 UDP 5353 端口
- **THEN** 系统将它们写入数据卷配置文件，且后续容器列表返回相同配置

#### Scenario: 拒绝非 host 网络容器
- **WHEN** 用户尝试为 bridge 或其他网络模式容器保存手工端口
- **THEN** 系统拒绝请求，且不修改已有配置

### Requirement: 容器卡片端口区域布局稳定
容器卡片 MUST 始终渲染端口区域，并保留统一的最小高度。无端口数据时 MUST 显示空状态；host 网络无手工配置时 MUST 显示配置入口。可点击 TCP 端口 MUST 仍在新标签页中打开。

#### Scenario: 端口数据缺失
- **WHEN** 普通网络容器没有已发布或声明的端口
- **THEN** 卡片显示端口空状态，操作按钮与同行其他卡片对齐

### Requirement: 手工端口配置生命周期与编辑
手工端口配置 MUST 以 Docker 成功返回的完整容器列表作为生命周期校验依据。容器仍存在但处于停止状态时，配置 MUST 保留；容器已经从 Docker 列表中删除后，系统 MUST 在下一次成功的容器列表刷新中删除对应的孤儿配置。列表请求失败时 MUST 保留已有配置。已有配置 MUST 支持编辑、替换和清空删除。

#### Scenario: 容器删除后在列表刷新时清理配置
- **WHEN** Docker 容器已经被删除，且下一次 GET /api/containers 成功返回的容器列表不再包含该容器名称
- **THEN** 系统删除该容器在 /data/config/container-port-overrides.json 中的配置项，后续列表响应不再返回该容器的 configuredPorts

#### Scenario: 停止容器不触发配置清理
- **WHEN** host 网络容器被停止但仍存在于 Docker 返回的全部容器列表中
- **THEN** 系统保留该容器的手工端口配置，容器重新启动后仍可继续使用

#### Scenario: Docker 列表失败时保留配置
- **WHEN** Docker 容器列表请求失败，系统无法确认容器是否仍存在
- **THEN** 系统不执行孤儿配置清理，并在下次成功刷新前保留已有配置文件内容

#### Scenario: 编辑已有端口配置
- **WHEN** 用户打开已经存在手工端口配置的 host 网络容器编辑入口
- **THEN** 弹窗预填已有的名称、端口和协议，用户修改后保存，系统以提交的新端口集合替换旧集合

#### Scenario: 删除全部端口配置
- **WHEN** 用户在编辑弹窗中删除全部端口并保存
- **THEN** 系统删除该容器的持久化配置项，容器卡片显示未配置状态和配置入口
