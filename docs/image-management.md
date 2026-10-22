# 镜像更新检查

服务启动时和之后每小时会检查本地滚动 tag 的 registry manifest digest。成功结果保留最后检查和最后成功时间；失败会保留上一次成功的“有更新”结论，并标记为陈旧、附上本次失败原因。首次检查失败保持未知，不会当成无需更新。超过两小时没有成功检查的结果在读取时也会标记为陈旧。

应用侧 digest 检查默认验证系统根证书。使用私有 registry 或企业 TLS 检查代理时，可将受信任的 PEM CA 证书路径配置到 `Updates.RegistryCAFile`，也可通过 `DOCKERCOPILOT_REGISTRY_CA_FILE` 设置。附加 CA 与系统根证书共同信任；配置文件无法读取或不含有效证书时服务启动会报告配置错误，不会关闭证书校验。

这个设置只控制应用发出的 `/v2/`、认证 token 和 manifest digest 请求。应用侧请求遵循进程的 `HTTP_PROXY`/`HTTPS_PROXY` 环境；它不改变 Docker daemon 的镜像拉取、registry 凭据或加速器设置。摘要检查成功仅说明读取到了 registry 中的 manifest，不表示 Docker daemon 能成功拉取该镜像。更新任务会单独报告 daemon 拉取失败。

配置示例：

```yaml
Updates:
  RegistryCAFile: /etc/dockerCopilot/certs/registry-ca.pem
```
