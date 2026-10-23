# Docker 相关代理

应用查询镜像摘要、仓库元数据或程序更新源时使用标准 HTTP 代理环境变量。在现有 compose 服务的 `environment` 中加入：

```yaml
HTTP_PROXY: http://192.168.1.10:7890
HTTPS_PROXY: http://192.168.1.10:7890
NO_PROXY: localhost,127.0.0.1,registry.internal
```

地址须能从 dockerCopilot 容器访问；容器内 `127.0.0.1` 指容器自身。`NO_PROXY` 填需要直连的仓库或服务。修改后重新创建应用容器以加载环境变量。现有 `githubProxy` / `Updates.GitHubProxy` 是 GitHub 下载 URL 前缀，与这里的正向代理用途不同。

真正的镜像拉取由 Docker Engine 执行，需要另行配置宿主机 Docker daemon。Linux Engine 可将以下内容合并到已有 `/etc/docker/daemon.json`，保留原配置其他字段：

```json
{
  "proxies": {
    "http-proxy": "http://192.168.1.10:7890",
    "https-proxy": "http://192.168.1.10:7890",
    "no-proxy": "localhost,127.0.0.1,registry.internal"
  }
}
```

daemon 配置需重启 Docker 后生效，应选择允许中断的维护时间。Docker Desktop 应使用其代理设置，Desktop 不读取上述 daemon 代理配置。部署方式、systemd 环境变量方案及版本支持以 [Docker 官方文档](https://docs.docker.com/engine/daemon/proxy/) 为准。

排查时分别核对应用元数据请求和宿主机 `docker pull tailscale/tailscale:latest` 的结果；前者成功不能证明后者的网络已配置好。应用日志记录阶段和脱敏错误，不输出代理凭据。
