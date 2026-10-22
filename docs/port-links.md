# 容器端口展示与快捷访问

端口列表按宿主机端口、容器端口和协议规范化排序，并折叠重复映射。bridge 等网络模式只为已发布的 TCP 端口提供浏览器链接；UDP 和未发布端口仍显示为普通文本。链接使用当前管理页面的协议和主机名，替换为目标宿主机端口并在新标签页打开，不触发容器卡片的点击操作。

host 网络模式没有 Docker 发布映射，因此从容器卡片进入端口编辑器可保存或清空手工访问端口。保存成功后会刷新容器列表；清空后卡片回到“配置端口”入口。

本次浏览器验收使用本地 Vite 页面和同源 mock HTTP 服务，没有连接 Docker。截图记录了 bridge 列表、host 编辑表单、空表单和清空后的卡片：

- `docs/screenshots/ports-list.png`
- `docs/screenshots/ports-host-edit.png`
- `docs/screenshots/ports-host-empty.png`
- `docs/screenshots/ports-host-cleared.png`

Service Worker 继续缓存静态资源与离线导航页面；所有 `/api/*`（包含应用 base path 下的 API 路径）及带 `Authorization` 的 GET 请求均直接走网络，不读取或写入缓存，也不使用 HTML 离线页面兜底。激活新版本时只清理由本应用命名空间创建的旧缓存。最终浏览器验收使用已注册并控制页面的 Service Worker，以及同源本地 mock HTTP 服务，确认编辑和清空后列表均发起真实网络读取并展示最新结果。
