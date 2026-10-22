# dockerCopilot
<a href="https://www.gnu.org/licenses/agpl-3.0.en.html">
    <img alt="License: AGPLv3" src="https://shields.io/badge/License-AGPL%20v3-blue.svg">
  </a>

# 介绍

一个主打便捷的docker容器管理工具，现在已经支持所有平台。
已经实现：
1. 一键更新容器
2. 指定镜像和tag更新
3. 启动、停止、重启容器
4. 重命名容器
5. 删除无TAG镜像
6. 删除未使用镜像
7. 更新进度查看
8. 备份容器设置
9. 恢复容器设置

## 使用

docker compose 安装

```
services:
  dockercopilot:
    container_name: dockercopilot
    restart: always
    privileged: true
    network_mode: bridge
    ports:
      - 12712:12712
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - ./data:/data
    environment:
      - TZ=Asia/Shanghai
      - DOCKER_HOST=unix:///var/run/docker.sock
      - secretKey=密码，不少于八位且非纯数字
    image: 2807645688/dockercopilot:latest
```

## 服务配置

服务默认读取 `etc/dockerCopilot.yaml`。集中配置预设任务记录目录为 `/data/tasks`、并发上限为 2、队列容量为 100、更新/恢复期限为 30 分钟、终态记录保留 7 天且最多 1000 条。健康检查等待、持续运行观察和恢复预算分别预设为 120 秒、10 秒和 2 分钟；任务执行器和这些行为由后续任务接入。

日志配置预设写入 `/data/logs`、级别为 `info`、保留 7 天，单文件与目录容量上限分别为 20 MiB 和 200 MiB；轮转和容量清理由后续日志任务接入。备份默认写入 `/data/backups`，端口覆盖配置默认写入 `/data/config/container-port-overrides.json`。在线更新源默认为 `RookieRoll/dockerCopilot`。旧的 `BACKUP_DIR`、`DelOldContainer`、`githubProxy` 和 `CONTAINER_PORT_OVERRIDES_PATH` 环境变量继续生效；对应环境变量优先于 YAML。`DelOldContainer` 接受 `true` 或 `false`，单次更新请求显式提供 `delOldContainer` 时优先使用请求值。

JSON 与 Compose 备份保存容器配置（包括环境变量、挂载定义和网络配置），不复制挂载卷中的文件或应用数据；恢复会按备份配置创建容器，但不会自动启动。备份文件使用限制性权限保存，请按敏感配置数据管理。

任务、日志和更新配置也可使用 `DOCKERCOPILOT_TASK_*`、`DOCKERCOPILOT_LOG_*`、`DOCKERCOPILOT_UPDATE_REPOSITORY` 与 `DOCKERCOPILOT_GITHUB_PROXY` 覆盖。`WORKDIR` 可设置在线程序更新目录；未设置时使用当前可执行文件所在目录。`DOCKERCOPILOT_SELF_CONTAINER` 可显式指定运行服务的容器名或 ID；未设置时仅当容器默认 hostname 是至少 12 位十六进制 ID 时尝试通过 Docker inspect 核验。任务超时、保留期和恢复/健康检查时间使用 Go duration 格式，例如 `30m`、`168h` 和 `120s`。更新仓库使用 `owner/repository` 格式；代理地址必须是没有凭据、查询参数或片段的 HTTP(S) URL。

在线程序更新只支持能通过 Docker inspect 确认自身容器的部署，并要求容器重启策略为 `always` 或 `unless-stopped`、root filesystem 可写、安装目录与运行程序目录一致且可写。无法确认这些条件时更新任务会拒绝 handoff；普通容器管理仍可使用。使用自定义 hostname 的部署请显式设置 `DOCKERCOPILOT_SELF_CONTAINER`，例如 compose 中的固定 `container_name`。`WORKDIR` 应与挂载并运行 `start.sh` 的安装目录一致。

关键默认值可在 YAML 中显式调整：

```yaml
Tasks:
  Directory: /data/tasks
  MaxConcurrent: 2
  QueueCapacity: 100
  Timeout: 30m
  Retention: 168h
  MaxRecords: 1000
  HealthcheckWait: 120s
  RunningObservation: 10s
  RecoveryTimeout: 2m
Logging:
  Directory: /data/logs
  Level: info
  RetentionDays: 7
  MaxFileSizeMiB: 20
  MaxTotalSizeMiB: 200
Updates:
  Repository: RookieRoll/dockerCopilot
  GitHubProxy: ""
Backup:
  Directory: /data/backups
Docker:
  DeleteOldContainer: true
  PortOverridesPath: /data/config/container-port-overrides.json
```

本任务集中提供配置加载、默认值和启动校验；任务运行器、日志轮转/容量限制以及健康检查等待会由后续功能接入。

## 开发环境

go版本：1.23+

前端源码位于 `frontend/`，生产构建产物输出到 `frontend/dist/`，发布流程会将其放入根目录的 `front/` 后由 Go 服务嵌入。

```powershell
cd frontend
npm ci
npm run dev
```

本地验证生产前端构建：

```powershell
cd frontend
npm ci
npm run build
```

构建后的 `frontend/dist/` 需要复制到根目录 `front/` 后，才能执行根项目的完整 Go 构建。
PowerShell 7 同步构建产物时请复制 `dist/` 内的内容，而不是把 `dist/` 目录嵌套到 `front/`：

```powershell
New-Item -ItemType Directory -Force .\front | Out-Null
Copy-Item -Path .\frontend\dist\* -Destination .\front\ -Recurse -Force
go test ./...
```

