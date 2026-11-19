# DeBrowser

一款轻量、本地优先的 Windows 桌面应用，用于管理多个相互隔离的浏览器实例。每个实例拥有独立的 Profile（实例文件），可以独立启动、运行和关闭，彼此不共享数据。

DeBrowser 面向 **Windows 11 x64**，设计目标是稳定、简单、可靠、容易维护。

## 当前状态

核心功能已经交付：浏览器手动登记、浏览器实例管理与真实运行均可正常使用。Windows 11 已完成基本实机验收，用户未报告问题。

书签管理已交付（Stage 9C 书签部分）：DeBrowser 书签资源库（文件夹树）与实例真实书签相互独立；实例书签通过其 Profile 的 Chromium 原生书签文件管理，仅限已确认停止的实例，支持分发同步与跨实例批量编辑、删除。多开同步已实现（Stage 10）：基于 CDP 页面事件的主控—被控同步，主实例的网页鼠标、键盘与文本输入同步到跟随实例，标签对应关系由 CDP 显式建立；实现完成，待 Windows 11 实机验收。尚未开放：代理配置（Stage 6C，待开发）、程序内更新、完整备份与恢复。各阶段状态见 [docs/ROADMAP.md](docs/ROADMAP.md)。

## 功能概览

- **浏览器实例管理**：创建、编辑、删除实例；名称自然排序；批量选择、批量启动/关闭/删除与批量创建。
- **独立 Profile**：每个实例绑定一个独立的实例文件（`data/profiles/` 下的文件夹），创建时实际建立（默认顺序编号 P-1、P-2……永不复用，或自定新建，或绑定保留的未绑定文件夹），编辑时可重命名或更换绑定。删除实例默认只删记录、保留实例文件。
- **浏览器登记**：用户在「浏览器内核」页面手动登记本机浏览器（纯路径登记，见下），实例按登记关联并启动真实浏览器进程；进程身份经严格核验，DeBrowser 重启后可重新识别仍在运行的实例。
- **统一日志**：运行、操作、错误三类记录自动保存并按操作关联，可在「数据管理」页按类别与日期范围导出；日志总容量可由用户设置（默认 100 MB），超出自动清理最早记录。日志是唯一的记录系统。
- **多开同步**：选择一个主实例与若干跟随实例，主实例的网页鼠标、滚动、键盘与文本输入经 CDP 同步到跟随实例，支持页面导航与标签创建、关闭、切换，主窗口的移动、调整大小、最大化与还原同步（最小化不同步），以及窗口自动排列与恢复；粘贴只经文本增量同步一次，iframe 内输入不支持；暂停会停止全部分发。单个跟随实例失败不影响其他实例。浏览器自身界面（菜单、扩展弹窗等）不在同步范围内。
- **界面**：侧边栏固定八项（总览、浏览器实例、代理配置、浏览器内核、书签管理、多开同步、数据管理、系统设置）；仅简体中文；浅色、暖色、深色三套主题，首次启动默认浅色，选择会持久化。

## 产品范围

- **纯路径登记**：DeBrowser 不自动检测浏览器（不扫描注册表、不扫描磁盘）。用户在文件选择器中任选一个程序可执行文件，DeBrowser 只保存其路径和显示名，不做任何身份、版本、签名、发布者或存在性检查；移除登记只删除数据库记录。
- **代理配置尚未开发**：过去的代理服务器管理、订阅管理与配置导入、Mihomo 核心运行时等历史实现已经移除；「代理配置」（Stage 6C）仍属于后续产品规划，尚未开发，当前入口只是说明功能暂未开放的占位页面。
- **不提供**独立 Chromium 的下载、安装和管理。
- 尚未交付的功能使用明确的占位页面说明，不使用假数据冒充。

系统结构见 [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)，数据库设计见 [docs/DATABASE.md](docs/DATABASE.md)。

## 技术栈

| 层 | 技术 | 版本 |
| --- | --- | --- |
| 桌面框架 | Wails | v3.0.0 |
| 后端 | Go | 1.26 |
| 前端 | React + TypeScript | 18.3.1 / 5.9.3 |
| 构建工具 | Vite | 8.3.7 |
| 样式 | Tailwind CSS | 4.3.5 |
| 状态管理 | Zustand | 5.1.0 |
| 数据库 | SQLite（modernc.org/sqlite） | 1.62.0 |

依赖版本全部锁定（`go.mod`/`go.sum`、`package.json`/`package-lock.json`）。参与开发前请先阅读 [AGENTS.md](AGENTS.md)，它定义了本项目的工程规则。

## 快速开始

前置条件（Windows 11 x64）：Go 1.26+、Node.js 22+（含 npm）、Git，以及 WebView2 运行时（Windows 11 已预装）。

```powershell
go install github.com/wailsapp/wails/v3/cmd/wails3@v3.0.0
git clone https://github.com/onbeee/DeBrowser.git
cd DeBrowser
wails3 dev          # 开发模式：前端热重载
```

生产构建与完整开发流程见 [docs/DEVELOPMENT.md](docs/DEVELOPMENT.md)。

## 文档导航

- [docs/ROADMAP.md](docs/ROADMAP.md)——阶段进度与后续计划
- [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)——系统架构与运行机制
- [docs/DATABASE.md](docs/DATABASE.md)——数据库结构、迁移与数据保护
- [docs/DEVELOPMENT.md](docs/DEVELOPMENT.md)——开发环境、命令、测试与构建

## 数据与隐私

DeBrowser 是便携优先、本地优先的：所有数据都位于可执行文件旁边，应用文件夹放在哪里，数据就跟到哪里。

```text
<应用文件夹>\
  debrowser.exe
  data\            debrowser.db、backups\、logs\、profiles\、secrets.vault
  webview2-data\   应用窗口的 WebView2 用户数据
  config\          settings.json（语言、主题、日志容量）
```

- 任何内容都不写入 `%AppData%` 或 `%LocalAppData%`；文件夹不可写时应用明确报错，不把数据悄悄存到别处。
- 首次启动直接创建便携数据目录并初始化，不提供欢迎页或选择页，也不从任何其他位置扫描、检测或迁移数据。
- 凭据 Vault 使用 Windows DPAPI（CurrentUser）保护。DPAPI 绑定的凭据不能保证在另一台 Windows 电脑或另一个 Windows 用户下仍可解密；受影响的条目会被保留但标记为不可用，需要重新录入。跨机器的完整迁移与恢复属于 Stage 12（使用独立备份口令），尚未交付。
- 用户数据库、浏览器 Profile、Cookie 与凭据永不提交到本仓库。

## 许可证

尚未选定许可证。在项目负责人选定之前，保留全部权利。
