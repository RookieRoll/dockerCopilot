# frontend-monorepo-build Specification

## Purpose

让 Docker Copilot 在同一仓库中同时维护 React 前端与 Go 后端，并通过统一且可重复的构建流程生成可由 Go 服务提供的管理页面，减少跨仓库接口和发布协作成本。

## Requirements

### Requirement: 前端源码属于主项目
项目 MUST 在 frontend/ 下包含管理页面所需的 React 源码、依赖清单和构建配置；发布构建不得依赖从外部 Docker-Copilot-React 仓库临时获取源码。

#### Scenario: 从主仓库构建前端
- **WHEN** 在主项目的干净检出上执行前端安装和构建流程
- **THEN** 构建流程使用 frontend/ 中的依赖清单和锁文件完成构建，不需要 checkout 外部前端仓库

#### Scenario: 前端源码与 Go 后端同时修改
- **WHEN** 一个变更同时调整容器 API 和容器页面
- **THEN** 两部分修改可以在同一个项目提交中审查、测试和发布

### Requirement: 构建产物可被 Go 管理服务提供
发布流程 MUST 将 frontend/ 的构建结果放入 Go 服务约定的 front/ 生成目录，并使 Go 二进制能够继续通过 /manager 提供管理页面及其静态资源。front/ 是生成产物，不得被要求作为前端源码提交。

#### Scenario: 前端构建产物传递到后端构建
- **WHEN** 前端构建成功并将构建产物传递给后端构建阶段
- **THEN** 后端构建阶段可以在 front/ 中找到 index.html 和静态资源，并使用它们完成 Go 二进制编译

#### Scenario: 访问嵌入式管理页面
- **WHEN** 运行包含前端构建产物的 Go 服务并请求 /manager 及其静态资源路径
- **THEN** 服务返回管理页面和对应的脚本、样式等资源，且页面不依赖独立的前端开发服务器

### Requirement: 构建路径与运行路径保持一致
前端构建输出、资源引用和路由基路径 MUST 与 /manager、/manager/assets/ 和现有 Go 嵌入式静态文件服务兼容；构建配置的变化不得导致管理页面出现资源 404。

#### Scenario: 使用 /manager 访问页面资源
- **WHEN** 用户通过 /manager 打开管理页面
- **THEN** 页面引用的 JavaScript、CSS 和其他静态资源使用服务实际提供的路径并成功加载

#### Scenario: 前端构建失败
- **WHEN** frontend/ 依赖安装或构建失败
- **THEN** 发布流程 MUST fail，并且不得将不完整或不可用的前端产物作为可发布后端构建输入
