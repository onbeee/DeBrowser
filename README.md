# DeBrowser

一款轻量、本地优先的桌面应用，用于管理多个相互隔离的浏览器 Profile。

DeBrowser 面向 **Windows 11 x64**。当前已交付实例元数据管理与（Stage 9B，待 Windows 11 实机验收）实例真实运行：每个实例绑定一个独立的实例文件（`data/profiles/` 下的文件夹），创建实例时实际建立（默认顺序编号 P-1、P-2…… 永不复用，或自定新建，或绑定保留的未绑定文件夹），编辑时可重命名或更换实例文件；创建实例必须从「浏览器内核」页面已登记的浏览器中选择一个关联（按登记 ID 关联，实例使用的程序路径取自该登记记录）。启动时以实例文件作为 `--user-data-dir` 启动真实浏览器进程，每个实例独立启动、独立运行、独立关闭；进程身份经 PID + 创建时间 + 可执行路径 + 命令行核验，DeBrowser 重启后可重新识别仍在运行的实例。需要特别说明：**真实浏览器（Brave/Chrome）在 Windows 11 上的多开隔离效果尚待实机验收**——自动化测试已用受控测试程序验证启动参数、进程隔离、优雅关闭、退出感知与重启识别（见 [docs/ROADMAP.md](docs/ROADMAP.md)）。运行中的实例之间永不共享实例文件。

> **状态：Stage 1–6 与 PR #9 产品范围清理已交付并合并；Stage 9A（浏览器手动登记（纯路径））已合并；Stage 9B（实例真实运行）已实施，待评审与 Windows 11 实机验收。** 本仓库包含应用外壳、SQLite 数据层、实例管理基础、作为数据兼容层保留的凭据 Vault，以及统一自动记录系统（运行 / 操作 / 错误三类记录自动保存、按操作关联、可在「数据管理」页按类别与日期范围导出，日志总容量可由用户设置、默认 100 MB、超出自动清理最早记录；日志是唯一的记录系统，没有独立的操作审计模块或诊断导出）。根据项目负责人的决定，代理服务器管理、订阅管理与配置导入、统一代理配置界面以及 Mihomo 核心运行时**不属于**当前产品范围：它们已在 PR #9 中移除，「代理配置」侧边栏入口是仅说明功能暂未开放的占位页面。侧边栏固定为八项：一、总览；二、浏览器实例；三、代理配置；四、浏览器内核；五、书签管理；六、多开同步；七、数据管理；八、系统设置。Stage 9A 不自动检测浏览器——不扫描注册表、不扫描磁盘、没有任何自动发现。用户只能通过一种方式添加浏览器：手动登记是**纯路径登记**——用户在文件选择器中任选一个程序可执行文件，DeBrowser 只保存其路径和显示名（跨重启持久化），**不进行任何身份、版本、签名、发布者或存在性验证**；被登记的程序不会被 DeBrowser 启动、复制、移动、修改或删除，移除登记只删除数据库记录。DeBrowser 不提供主动下载、安装或管理独立 Chromium 的功能（该功能已按产品决定取消；数据库中的 kernels 历史表仅为兼容既有数据保留，不代表当前产品功能）。实例真实多开启动已在 Stage 9B 实施（待实机验收）；CDP 与书签接入在 Stage 9C；多开同步、程序内更新以及（最后的）完整备份与恢复在后续阶段交付——见 [docs/ROADMAP.md](docs/ROADMAP.md)。代理能力是否回归将在未来阶段另行评估。未交付的功能不使用任何占位数据冒充。

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
  webview2-data\   应用窗口的 WebView2 用户数据
  config\          settings.json（语言、主题）
```

任何内容都不会写入 `%AppData%` 或 `%LocalAppData%`；应用程序、SQLite 数据库与普通文件都采用便携目录布局，把应用文件夹移动到同一台设备的其他位置或其他驱动器即可继续使用（需要系统 WebView2 运行时）。但便携布局不等于全部数据都能跨设备迁移：凭据 Vault 使用 Windows DPAPI（CurrentUser）保护，DPAPI 绑定的凭据**不能保证**在复制到另一台 Windows 电脑或另一个 Windows 用户后仍可直接解密——受影响的条目会被保留但标记为不可用，需要重新录入。跨机器的完整数据迁移与恢复目前尚不具备，它属于 Stage 12，将使用独立备份口令与正式的备份恢复机制（DPAPI 本身不是跨机器机制）。如果文件夹不可写，应用会明确报错，而不是把数据悄悄存到别处。首次启动时，应用直接创建便携数据目录并自动初始化数据库与凭据 Vault——不需要任何设置选择，也不会从 `%AppData%` 或任何其他位置扫描、检测或迁移数据。界面仅提供简体中文，并内置浅色、暖色与深色三套主题，首次启动默认浅色，用户选择的主题会保存并在重启后恢复。用户数据库、浏览器 Profile、Cookie 与凭据永不提交到本仓库。

## 许可证

尚未选定许可证。在项目负责人选定之前，保留全部权利。
