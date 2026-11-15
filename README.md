# DeBrowser

一款轻量、本地优先的桌面应用，用于管理多个相互隔离的浏览器 Profile。

DeBrowser 面向 **Windows 11 x64**。当前已交付的是实例元数据管理：每个浏览器实例保存自己独立的 `user_data_dir` 路径、配置与状态，Profile 路径经过校验，Profile 数据有专门的安全删除机制。需要特别说明当前状态：**创建实例时尚未实际创建 Profile 目录，浏览器进程的启动与关闭尚未实现，Cookie、Local Storage、Cache 的运行时隔离尚未完成实机验证**——实例真实启动、独立进程与运行时隔离属于 Stage 9B 的交付范围（见 [docs/ROADMAP.md](docs/ROADMAP.md)）。在最终形态下，每个实例拥有独立的用户数据目录、独立配置与独立进程，运行中的实例之间永不共享 Profile。

> **状态：Stage 1–6 与 PR #9 产品范围清理已交付并合并；Stage 9A（浏览器手动登记与内核管理）正在评审中。** 本仓库包含应用外壳、SQLite 数据层、实例管理基础、作为数据兼容层保留的凭据 Vault，以及结构化日志 / 操作审计 / 诊断（统一归入「数据管理」侧边栏入口）。根据项目负责人的决定，代理服务器管理、订阅管理与配置导入、统一代理配置界面以及 Mihomo 核心运行时**不属于**当前产品范围：它们已在 PR #9 中移除，「代理配置」侧边栏入口是仅说明功能暂未开放的占位页面。侧边栏固定为八项：一、总览；二、浏览器实例；三、代理配置；四、浏览器内核；五、书签管理；六、多开同步；七、数据管理；八、系统设置。Stage 9A 不自动检测浏览器——不扫描注册表、不扫描磁盘、没有任何自动发现。用户通过以下两种明确分离的方式添加浏览器：（1）手动登记是**纯路径登记**——用户在文件选择器中任选一个程序可执行文件，DeBrowser 只保存其路径和显示名（跨重启持久化），**不进行任何身份、版本、签名或发布者验证**；被登记的程序不会被 DeBrowser 启动、复制、修改或删除，移除登记只删除数据库记录；或（2）用户主动下载由 DeBrowser 管理的独立 Chromium 构建，来源为官方 Chrome for Testing 渠道——只有这条托管下载/安装链路包含下载与安装验证（包括对托管构建的 Authenticode / Google 发布者检查）。下载永不是首次启动的前提。实例真实多开启动在 Stage 9B 交付；CDP 与书签接入在 Stage 9C；多开同步、程序内更新以及（最后的）完整备份与恢复在后续阶段交付——见 [docs/ROADMAP.md](docs/ROADMAP.md)。代理能力是否回归将在未来阶段另行评估。未交付的功能不使用任何占位数据冒充。

## 技术栈

| 层 | 技术 | 版本 |
| --- | --- | --- |
| 桌面框架 | Wails | v3.0.0 |
| 后端 | Go | 1.26（模块声明 `go 1.26.0`） |
| 前端 | React + TypeScript | React 18.3.1、TypeScript 5.9.3 |
| 构建工具 | Vite | 8.3.7 |
| 样式 | Tailwind CSS | 4.3.5 |
| 图标 | lucide-react | 0.469.0 |
| 状态管理 | Zustand | 5.1.0 |
| 数据库 | SQLite（经 modernc.org/sqlite） | 1.62.0 |
| 代理引擎 | — | 不在当前范围内（已在 PR #9 移除；未来另行评估） |
| 浏览器控制 | Chrome DevTools Protocol（CDP） | Stage 9C 接入（当前尚未实现） |

全部依赖版本均已锁定（Go：`go.mod`/`go.sum`；前端：`package.json`/`package-lock.json`）。参与开发前请先阅读 [AGENTS.md](AGENTS.md)——它定义了本项目的工程规则。

## 快速开始

前置条件（Windows 11 x64）：Go 1.26+、Node.js 22+（含 npm）、Git，以及 WebView2 运行时（Windows 11 已预装）。

```powershell
go install github.com/wailsapp/wails/v3/cmd/wails3@v3.0.0
git clone https://github.com/onbeee/DeBrowser.git
cd DeBrowser
wails3 dev          # 开发模式：前端热重载
```

生产构建与更多工作流见 [docs/DEVELOPMENT.md](docs/DEVELOPMENT.md)。

## 文档

- [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)——系统设计与模块地图
- [docs/ROADMAP.md](docs/ROADMAP.md)——分阶段交付计划
- [docs/DATABASE.md](docs/DATABASE.md)——SQLite 设计、迁移与备份规则
- [docs/DEVELOPMENT.md](docs/DEVELOPMENT.md)——环境搭建、检查、CI 与分支

## 数据与隐私

DeBrowser 是便携优先、本地优先的。所有数据都位于可执行文件旁边——应用文件夹放在哪里，数据就跟到哪里：

```
<应用文件夹>\
  debrowser.exe
  data\            debrowser.db、backups\、logs\、profiles\
  kernels\         DeBrowser 管理的 Chromium 构建（仅当用户主动下载时存在）
  webview2-data\   应用窗口的 WebView2 用户数据
  config\          settings.json（语言、主题）
```

任何内容都不会写入 `%AppData%` 或 `%LocalAppData%`；把文件夹移动或复制到其他驱动器或电脑上，一切照常工作（需要系统 WebView2 运行时）。如果文件夹不可写，应用会明确报错，而不是把数据悄悄存到别处。首次启动时，应用直接创建便携数据目录并自动初始化数据库与凭据 Vault——不需要任何设置选择，也不会从 `%AppData%` 或任何其他位置扫描、检测或迁移数据。界面仅提供简体中文，并内置深色、浅色与暖色三套主题。用户数据库、浏览器 Profile、Cookie 与凭据永不提交到本仓库。

## 许可证

尚未选定许可证。在项目负责人选定之前，保留全部权利。
