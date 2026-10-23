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

