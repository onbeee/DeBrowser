# DeBrowser 开发指南

本文档是开发环境、命令、测试、构建的操作指南。工程规则以 [AGENTS.md](../AGENTS.md) 为准；系统架构见 [ARCHITECTURE.md](ARCHITECTURE.md)，数据库规则见 [DATABASE.md](DATABASE.md)，功能与交付状态见 [ROADMAP.md](ROADMAP.md)。

## 开发环境要求

目标平台：**Windows 11 x64**。

| 工具 | 版本 | 说明 |
| --- | --- | --- |
| Go | 1.26.x | Wails v3.0.0 要求 Go 1.26 或更新（CI 使用 Go 1.26.10） |
| Node.js | 22 LTS 或更新 | 含 npm；用于前端与 Vite（CI 使用 Node 22） |
| Wails CLI | v3.0.0 | `go install github.com/wailsapp/wails/v3/cmd/wails3@v3.0.0` |
| Git | 近期任意版本 | |
| WebView2 运行时 | — | Windows 11 已预装 |
| Task（可选） | 近期任意版本 | Taskfile 工作流：https://taskfile.dev |

用 `wails3 doctor` 检查本地环境。

## 获取项目

```powershell
git clone <仓库地址>
cd DeBrowser
git checkout -b feature/my-change   # 从最新 main 创建功能分支（示例名），永不直接在 main 上工作
cd frontend; npm install; cd ..
```

## 安装依赖

- Go 依赖由 `go.mod`/`go.sum` 锁定，首次构建或测试时自动下载。
- 前端依赖在 `frontend/` 目录执行 `npm install`（版本由 `package.json`/`package-lock.json` 锁定）。

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

前端命令（在 `frontend/` 目录）：

```powershell
npm run dev          # 仅前端的 Vite 开发服务器（常规开发请用 wails3 dev）
npm test             # 前端单元测试（node --test，文件清单在 package.json 的 test 脚本中）
npm run typecheck    # tsc --noEmit
npm run build        # 前端生产构建（tsc + vite build）
```

## Wails bindings 生成

修改导出的 Go 服务方法或类型后，在仓库根目录重新生成 bindings，生成物提交进仓库，CI 会校验一致性：

```powershell
wails3 generate bindings -clean=true -ts -i
```

注意：bindings 生成后不要再修改导出服务方法与类型的注释——注释会内嵌进生成的 TypeScript，改动会使生成结果与已提交的 bindings 不一致。`frontend/bindings` 是生成物，永不手工编辑。

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

- Go 测试必须使用 `t.TempDir()` 的数据库与目录；永不让测试（或用生产代码路径做的人工实验）指向真实用户数据库。
- 新增前端测试文件时，同步加入 `frontend/package.json` 的 `test` 脚本文件清单，否则测试不会被执行。
- 在非 Windows 环境下，根 `main` 包与 `internal/app` 依赖桌面 GUI 库，可能无法构建或测试；可改用 `GOOS=windows go vet ./...` 与 `GOOS=windows go build ./...` 做交叉校验（Windows 构建需要先完成前端构建生成 `frontend/dist`），完整 Windows 构建以 CI 为准。

## CI 检查

GitHub Actions（`.github/workflows/ci.yml`）在 Windows x64 runner（`windows-latest`）上运行，触发条件为向 `main` 的推送以及每一个 Pull Request（工作流文件中另列有 `develop` 分支的推送触发，但仓库未启用该分支，无实际作用）。共 22 个步骤，依次为：Go 格式检查、安装 Wails CLI、前端依赖安装、生成 bindings 并校验与已提交版本一致、界面语言检查（仅简体中文）、前端类型检查、前端单元测试、前端生产构建、`go vet`、Go 全部测试、生成占位图标（见「已知限制」）、`wails3 build` 完整 Windows x64 构建、上传构建产物（artifact 名 `DeBrowser-windows-amd64`）。

任何内容都不会自动发布；发布是人工批准的步骤。CI 全绿只说明自动化检查通过，不等于 Windows 11 实机验收通过。

## Windows 构建与打包

```powershell
task build      # Windows x64 生产构建（经 build/windows/Taskfile.yml）
task package    # NSIS 安装包（需要 makensis；尚未在 CI 中运行）
```

Windows 构建产物只包含应用可执行文件（`bin/*.exe`）与前端内嵌资源。

关于 Mihomo 代理核心：**Mihomo 不随应用构建产物打包**。代理核心在运行时首次启动时，由代理模块从 Mihomo 官方 GitHub 发布下载对应版本（v1.19.34）的发行包，经压缩包与可执行文件双重 SHA-256 校验后安装到便携目录的 `data/mihomo/`，并由代理模块负责其启动与关闭。开发与构建环境不需要、也不会预置该核心；机制细节见 [ARCHITECTURE.md](ARCHITECTURE.md) 的「代理运行机制」。

## Git 与 PR 工作流

- 从最新 `main` 创建功能分支（如 `feature/*`、`fix/*`、`chore/*`）；一个 PR 只做一件事；不直接向 `main` 提交。
- PR 的自动化检查必须全绿；未经项目负责人明确批准不得合并 PR。
- 未经批准不得强推、改写已有提交历史或删除分支。

## 开发注意事项

- 结构变更以新文件进入 `internal/database/migrations/`（`NNNN_name.sql`，只追加，永不编辑已合并的 migration）；迁移规则与数据保护见 [DATABASE.md](DATABASE.md)。
- 核心模型的全部 SQL 属于 `internal/store`；业务代码使用 repository 与 `Store.WithTx` 做多行原子操作，不把裸 SQL 散落到各个包。
- 用户数据库、浏览器 Profile、凭据与订阅 URL 永不提交到仓库（`.gitignore` 已覆盖数据库与构建产物）。
- 界面只使用简体中文（CI 有专门检查步骤）；主题颜色一律使用设计 token（CSS 变量），不硬编码颜色。
- 代码与日志中不得出现 Cookie、密码、Token、凭据明文或订阅 URL；拼接外部错误文本时注意其中可能包含的敏感内容。
- 主窗口默认 1280×800，最小 1024×640；页面布局需在该范围内保持可用（列表内部滚动、工具栏按功能组换行）。

## 已知限制

- **应用图标**：`build/appicon.png`（1024×1024 PNG）是二进制品牌资源，尚未进入仓库（与 `build/windows/icon.ico` 一起作为生成文件被 git 忽略）。CI 与本地构建在 `build/` 目录运行 `wails3 generate icons -example` 生成 Wails 示例图标作为占位，使完整构建（含 `.syso` 版本资源）能端到端跑通；占位图标不是正式品牌图标。正式图标就绪后用 `git add -f build/appicon.png` 添加，并移除 `.gitignore` 中两行图标规则。
- **非 Windows 开发环境**：应用面向 Windows 构建与运行；在 Linux/macOS 上只能做交叉编译校验与非 GUI 包的测试，完整运行与实机行为必须在 Windows 11 上确认。
- **尚未发布**：项目尚未发布正式版本，没有安装包或便携包产物；`task package` 的 NSIS 打包尚未纳入 CI。
