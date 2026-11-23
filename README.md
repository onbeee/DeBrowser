# DeBrowser

DeBrowser 是一款轻量、本地优先的 Windows 桌面应用，用于管理多个相互隔离的浏览器实例。每个实例拥有独立的 Profile（实例文件），可以独立启动、运行和关闭，彼此不共享数据。

DeBrowser 面向 **Windows 11 x64**，设计目标是稳定、简单、可靠、容易维护。应用按便携方式工作：程序与全部数据都位于同一个应用文件夹内，文件夹放在哪里，数据就跟到哪里。

## 功能介绍

应用界面由侧边栏的七个页面组成，启动后默认进入「浏览器实例」：

- **浏览器实例**：创建、编辑、删除浏览器实例；每个实例绑定一个独立的实例文件；支持批量创建、批量启动、批量关闭与批量删除；可为实例选择代理节点。
- **代理配置**：启动或关闭内置的 Mihomo 代理核心；导入订阅或本地 Clash/Mihomo YAML 配置并管理其中的代理节点；为每个实例绑定不同节点，并可对节点进行连通性测速。
- **浏览器内核**：手动登记本机已安装的浏览器程序（纯路径登记），供实例关联使用。DeBrowser 不自带浏览器，也不自动检测本机程序。
- **书签管理**：维护 DeBrowser 自己的书签资源库（文件夹树），并可把资源库书签分发到已停止的实例、跨实例搜索和批量编辑、删除实例的真实书签。
- **多开同步**：在多个运行中的实例之间同步标签页与页面操作（一个主控实例、多个跟随实例），并支持窗口排列与主窗口镜像。
- **数据管理**：查看应用版本与数据库状态；查看统一日志，并按类别与日期范围导出日志文件。
- **系统设置**：切换界面主题（浅色、暖色、深色），设置日志总容量上限；查看数据目录等应用信息。

其他贯穿全应用的能力：

- **进程身份核验**：实例启动的浏览器进程经 PID、进程创建时间、可执行路径与命令行四项核验；DeBrowser 重启后可重新识别仍在运行的实例，身份无法确认时实例进入受保护的「无法确认」状态，不执行可能危及数据的操作。
- **统一日志**：运行、操作、错误三类记录自动保存并按操作关联；日志总容量可由用户设置（默认 100 MB），超出自动清理最早记录。
- **全局通知**：各页面操作结果统一进入顶栏通知台（最近 80 条，最新在前）；其中的重点结果同时以浮窗提醒。通知只存在于内存中，与长期保存的日志相互独立。

## 使用说明

项目尚未发布正式版本，目前没有可供下载的安装包或便携包，现阶段只能从源码构建运行，构建方法见 [docs/DEVELOPMENT.md](docs/DEVELOPMENT.md)。正式发布渠道确定后，本节会更新为真实的获取方式。

系统要求：

- Windows 11 x64。
- WebView2 运行时（Windows 11 已预装）。
- 至少一个由用户自行安装的浏览器程序（DeBrowser 不自带浏览器，只登记和使用本机已有程序）。

基本使用流程：

1. 运行 `debrowser.exe` 直接启动，没有欢迎页或首次启动选择页，应用文件夹旁会自动创建便携数据目录。
2. 在「浏览器内核」页面用文件选择器登记一个本机浏览器程序（只保存其路径与显示名）。
3. 在「浏览器实例」页面新建实例并关联已登记的浏览器，即可启动、关闭和管理该实例的浏览器窗口。
4. 如需代理，在「代理配置」页面启动代理核心、导入订阅或本地配置，再为实例选择节点；已绑定实例经其专属本地监听端口使用所选节点，未绑定实例直连。核心未启动时，已绑定实例会被明确拒绝启动，而不是悄悄直连。
5. 书签分发与编辑只作用于已确认停止的实例；运行中的实例不会被读写其书签文件。

## 技术栈

| 层 | 技术 | 版本 |
| --- | --- | --- |
| 桌面框架 | Wails | v3.0.0 |
| 后端 | Go | 1.26 |
| 前端 | React + TypeScript | 18.3.1 / 5.9.3 |
| 构建工具 | Vite | 8.3.7 |
| 样式 | Tailwind CSS | 4.3.5 |
| 状态管理 | Zustand | 5.1.0 |
| 图标 | lucide-react | 0.469.0 |
| 数据库 | SQLite（modernc.org/sqlite） | 1.62.0 |
| 代理核心 | Mihomo | v1.19.34（运行时下载，不随应用打包） |

依赖版本全部锁定（`go.mod`/`go.sum`、`package.json`/`package-lock.json`）。

## 隐私与数据存储

DeBrowser 是便携优先、本地优先的：所有数据都位于可执行文件旁边，不写入 `%AppData%` 或 `%LocalAppData%`；文件夹不可写时应用明确报错，不把数据悄悄存到别处；也不从任何其他位置扫描、检测或迁移数据。

```text
<应用文件夹>\
  debrowser.exe
  data\            debrowser.db、backups\、logs\、profiles\、mihomo\、secrets.vault
  webview2-data\   应用窗口的 WebView2 用户数据
  config\          settings.json（语言、主题、日志容量）
```

- **Profile 隔离**：每个实例独占一个 `data/profiles/` 下的文件夹作为浏览器用户数据目录，运行中的实例之间永不共享；DeBrowser 不宣称指纹隔离。
- **敏感数据保护**：订阅地址等凭据只保存在凭据 Vault（`data/secrets.vault`，Windows DPAPI CurrentUser 保护）中，数据库只保存不透明引用；日志、数据库与设置中永不出现凭据明文。DPAPI 绑定的凭据不能保证在另一台 Windows 电脑或另一个 Windows 用户下仍可解密，受影响的条目会被保留但标记为不可用，需要重新录入。
- **删除保护**：删除实例默认只删除记录、保留实例文件；只有用户明确选择时才删除实例文件，且删除目标必须能证明位于受管目录内。
- 用户数据库、浏览器 Profile、Cookie 与凭据永不提交到本仓库。

系统结构见 [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)，数据库设计见 [docs/DATABASE.md](docs/DATABASE.md)。

## 文档导航

- [CONTRIBUTING.md](CONTRIBUTING.md)——如何参与贡献
- [CHANGELOG.md](CHANGELOG.md)——产品更新日志（尚无正式版本）
- [docs/ROADMAP.md](docs/ROADMAP.md)——功能清单与交付状态
- [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)——系统架构与运行机制
- [docs/DATABASE.md](docs/DATABASE.md)——数据库结构、迁移与数据保护
- [docs/DEVELOPMENT.md](docs/DEVELOPMENT.md)——开发环境、命令、测试与构建
- [AGENTS.md](AGENTS.md)——面向 AI Agent 与自动化执行者的工程规则

## 致谢

DeBrowser 的实现离不开以下开源项目：

- [Wails](https://wails.io)——Go + Web 桌面应用框架。
- [Mihomo](https://github.com/MetaCubeX/mihomo)——代理核心，DeBrowser 在运行时从其官方发布下载并校验后使用。
- [modernc.org/sqlite](https://modernc.org/sqlite)——纯 Go 的 SQLite 驱动。
- [React](https://react.dev)、[Vite](https://vite.dev)、[Tailwind CSS](https://tailwindcss.com)、[Zustand](https://zustand.docs.pmnd.rs)、[Lucide](https://lucide.dev)——前端框架、构建与界面组件。

## 许可证

尚未选定许可证。在项目负责人选定之前，保留全部权利。
