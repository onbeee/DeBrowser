# DeBrowser 开发指南

本文档是开发环境、命令、测试、构建与 Git 流程的唯一记录。工程规则以 [AGENTS.md](../AGENTS.md) 为准；系统架构见 [ARCHITECTURE.md](ARCHITECTURE.md)，数据库规则见 [DATABASE.md](DATABASE.md)，阶段状态见 [ROADMAP.md](ROADMAP.md)。

## 开发环境要求

目标平台：**Windows 11 x64**。

| 工具 | 版本 | 说明 |
| --- | --- | --- |
| Go | 1.26.x | Wails v3.0.0 要求 Go 1.26 或更新 |
| Node.js | 22 LTS 或更新 | 含 npm；用于前端与 Vite（CI 使用 Node 22） |
| Wails CLI | v3.0.0 | `go install github.com/wailsapp/wails/v3/cmd/wails3@v3.0.0` |
| Git | 近期任意版本 | |
| WebView2 运行时 | — | Windows 11 已预装 |
| Task（可选） | 近期任意版本 | Taskfile 工作流：https://taskfile.dev |

用 `wails3 doctor` 检查本地环境。

## 获取项目

```powershell
git clone https://github.com/onbeee/DeBrowser.git
cd DeBrowser
git checkout -b feature/<your-feature>   # 从最新 main 创建功能分支，永不直接在 main 上工作
cd frontend; npm install; cd ..
```

## 启动开发环境

```powershell
wails3 dev   # 开发模式：前端热重载，后端自动重启
```

## 环境变量

| 变量 | 作用 |
| --- | --- |
| `DEBROWSER_APP_ROOT` | 覆盖应用根目录（仅开发/测试用；默认是可执行文件所在目录）。全部数据路径都由它派生 |
| `DEBROWSER_LOG_LEVEL` | `debug` / `info` / `warn` / `error`（默认 `info`） |

## 常用开发命令

修改 Go 服务后，在仓库根目录重新生成 bindings（生成物提交进仓库，CI 会校验一致性）：

```powershell
wails3 generate bindings -clean=true -ts -i
```

前端命令（在 `frontend/` 目录）：

```powershell
npm run dev          # 仅前端的 Vite 开发服务器（常规开发请用 wails3 dev）
npm test             # 前端单元测试（node --test）
npm run typecheck    # tsc --noEmit
npm run build        # 前端生产构建（tsc + vite build）
```

## 测试与质量检查

打开 PR 之前运行：

```powershell
gofmt -l .            # 必须无任何输出
go vet ./...
go test ./...
cd frontend
npm test
npm run typecheck
npm run build
cd ..
```

测试约定：

- Go 测试必须使用 `t.TempDir()` 数据库；永不让测试（或用生产代码路径做的人工实验）指向真实用户数据库。
- bindings 生成后不要再修改导出服务方法的注释，否则生成结果会与已提交的 bindings 不一致。

### CI

GitHub Actions（`.github/workflows/ci.yml`）在 Windows x64 runner（`windows-latest`）上运行，触发条件为向 `main`/`develop` 的推送以及每一个 Pull Request。步骤依次为：Go 格式检查、安装 Wails CLI、前端依赖安装、生成 bindings 并校验与已提交版本一致、界面语言检查（仅简体中文）、前端类型检查、前端单元测试、前端生产构建、`go vet`、Go 全部测试、生成占位图标（见「已知限制」）、`wails3 build` 完整 Windows x64 构建、上传构建产物（artifact 名 `DeBrowser-windows-amd64`）。

任何内容都不会自动发布；发布是人工批准的步骤。

## 构建与产物

```powershell
task build      # Windows x64 生产构建（经 build/windows/Taskfile.yml）
task package    # NSIS 安装包（需要 makensis；尚未在 CI 中运行）
```

Windows 构建产物只包含应用可执行文件（`bin/*.exe`）。没有任何代理核心或其他 sidecar 二进制被打包或下载：应用在运行时不安装、不启动任何外部核心。

## Git 与 PR 工作流

- 从最新 `main` 创建 `feature/*` 分支；一个阶段一个 PR；不直接向 `main` 提交。
- PR 的自动化检查必须全绿；未经项目负责人明确批准不得合并 PR。
- 未经批准不得强推、不得删除分支。

## 开发注意事项

- 结构变更以新文件进入 `internal/database/migrations/`（`NNNN_name.sql`，只追加，永不编辑已合并的 migration）；迁移规则与数据保护见 [DATABASE.md](DATABASE.md)。
- 核心模型的全部 SQL 属于 `internal/store`；业务代码使用 repository 与 `Store.WithTx` 做多行原子操作，不把裸 SQL 散落到各个包。
- 用户数据库、浏览器 Profile、凭据与订阅 URL 永不提交到仓库（`.gitignore` 已覆盖数据库与构建产物）。
- 界面只使用简体中文（CI 有专门检查步骤）；主题颜色一律使用设计 token（CSS 变量），不硬编码颜色。

## 已知限制

- **应用图标**：`build/appicon.png`（1024×1024 PNG）是二进制品牌资源，尚未进入仓库（与 `build/windows/icon.ico` 一起作为生成文件被 git 忽略）。CI 与本地构建在 `build/` 目录运行 `wails3 generate icons -example` 生成 Wails 示例图标作为占位，使完整构建（含 `.syso` 版本资源）能端到端跑通；占位图标不是正式品牌图标。正式图标就绪后用 `git add -f build/appicon.png` 添加，并移除 `.gitignore` 中两行图标规则。
- **业务模块现状**：以 [ROADMAP.md](ROADMAP.md) 为准。当前边界简述：书签只有数据层（前端在 Stage 9C 接入）；代理相关功能已在 PR #9 范围清理中移除，不是后续阶段内容。
