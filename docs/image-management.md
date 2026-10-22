# 镜像更新检查

服务启动时和之后每小时会检查本地滚动 tag 的 registry manifest digest。成功结果保留最后检查和最后成功时间；失败会保留上一次成功的“有更新”结论，并标记为陈旧、附上本次失败原因。首次检查失败保持未知，不会当成无需更新。超过两小时没有成功检查的结果在读取时也会标记为陈旧。

应用侧 digest 检查默认验证系统根证书。使用私有 registry 或企业 TLS 检查代理时，可将受信任的 PEM CA 证书路径配置到 `Updates.RegistryCAFile`，也可通过 `DOCKERCOPILOT_REGISTRY_CA_FILE` 设置。附加 CA 与系统根证书共同信任；配置文件无法读取或不含有效证书时服务启动会报告配置错误，不会关闭证书校验。

这个设置只控制应用发出的 `/v2/`、认证 token 和 manifest digest 请求。应用侧请求遵循进程的 `HTTP_PROXY`/`HTTPS_PROXY` 环境；它不改变 Docker daemon 的镜像拉取、registry 凭据或加速器设置。摘要检查成功仅说明读取到了 registry 中的 manifest，不表示 Docker daemon 能成功拉取该镜像。更新任务会单独报告 daemon 拉取失败。

配置示例：

```yaml
Updates:
  RegistryCAFile: /etc/dockerCopilot/certs/registry-ca.pem
```

## 镜像清理任务

“无 Tag”和“未使用”清理会提交一个后台任务。页面收到 `queued` 只表示服务已接收；可在任务面板查看逐镜像结果和成功、失败、跳过数量。服务端在任务执行时重新读取镜像和容器使用状态，清理过程使用普通删除，不会强制删除正在使用的镜像；活动更新或恢复任务保护的镜像也会跳过，并注明关联任务。部分删除失败时任务显示失败，同时保留已完成项目和错误原因，不能将其理解为全部成功。

检查结果也会显示在镜像卡片上：未知、陈旧或本次检查失败会明确展示，不会被当成“无需更新”。页面验收使用本地 mock API 与固定镜像数据，不连接 Docker daemon。

![镜像检查陈旧状态与清理部分失败任务](screenshots/image-cleanup-partial.png)
