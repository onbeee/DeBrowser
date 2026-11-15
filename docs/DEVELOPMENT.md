# DeBrowser 开发指南

## 前置条件

目标平台：**Windows 11 x64**。

| 工具 | 版本 | 说明 |
| --- | --- | --- |
| Go | 1.26.x | Wails v3.0.0 要求 Go 1.26 或更新 |
| Node.js | 22 LTS 或更新 | 包含 npm；用于前端与 Vite |
| Wails CLI | v3.0.0 | `go install github.com/wailsapp/wails/v3/cmd/wails3@v3.0.0` |
| Task（可选） | 近期任意版本 | 脚手架工作流是 Taskfile：https://taskfile.dev |
| WebView2 运行时 | — | Windows 11 已预装 |
| Git | 近期任意版本 | |

用 `wails3 doctor` 检查本地环境。

## 开始开发

```powershell
git clone https://github.com/onbeee/DeBrowser.git
cd DeBrowser
git checkout feature/<your-feature>   # 永不直接在 main 上工作
cd frontend; npm install; cd ..
wails3 dev                            # 开发模式：前端热重载，后端自动重启
```

## 环境变量

| 变量 | 作用 |
| --- | --- |
| `DEBROWSER_APP_ROOT` | 覆盖应用根目录（仅开发/测试用；默认是可执行文件所在目录）。全部数据路径都由它派生。 |
| `DEBROWSER_LOG_LEVEL` | `debug` / `info` / `warn` / `error`（默认 `info`） |

## 常用任务

```powershell
task build            # 通过 build/windows/Taskfile.yml 做 Windows x64 生产构建
task package          # NSIS 安装包（需要 makensis）
wails3 generate bindings -clean=true -ts -i   # 修改 Go 服务后重新生成 frontend/bindings（若提交的绑定与生成结果不一致，CI 会失败）
```

完整的 `task build` 还会生成 Windows 图标与 `.syso` 资源，这需要 `build/appicon.png`（一张 1024×1024 的 PNG 应用图标）。该二进制资源**尚未进入仓库**——见下方「已知限制」。在它补齐之前，生成 Wails 的示例图标作为占位：在 `build/` 目录内运行 `wails3 generate icons -example`，这正是 CI 的做法。

## 数据层（Stage 2）

- 结构变更以新文件形式进入 `internal/database/migrations/`（`NNNN_name.sql`，只追加——永不编辑已合并的 migration）。
- 核心模型的全部 SQL 属于 `internal/store`；业务代码使用 repository 与 `Store.WithTx` 做多行原子操作，不得把裸 SQL 散落到各个包中。
- 测试必须使用 `t.TempDir()` 数据库；永不让测试（或用生产代码路径做的人工实验）指向真实用户数据库。

## 检查（打开 PR 之前运行）

```powershell
gofmt -l .                       # 必须无任何输出
go vet ./...
go test ./...
cd frontend
npm run typecheck                # tsc --noEmit
npm run build                    # 前端生产构建
cd ..
```

## CI

GitHub Actions（`.github/workflows/ci.yml`）在 Windows x64 runner（`windows-latest`）上运行，触发条件为向 `main`/`develop` 的推送以及每一个 Pull Request：

1. Go 格式检查（`gofmt`）
2. 前端安装、绑定生成、类型检查与生产构建
3. `go vet ./...` 与 `go test ./...`
4. 安装 Wails CLI v3.0.0，生成占位图标（见下），并运行官方完整的 `wails3 build` 做 Windows x64 构建（绑定 → 前端 → 图标 → `.syso` → Go 构建）
5. 把构建出的可执行文件作为 workflow artifact 上传以供评审

任何内容都不会自动发布；发布是人工批准的步骤（AGENTS.md 第 8 节）。

版本兼容性：Go 1.26 + Wails v3.0.0（第一个要求 Go 1.26 的 Wails 版本）、Node.js 22 LTS 配 Vite 8 / React 18——与 `go.mod` 和 `frontend/package-lock.json` 中锁定的版本相同。

## Git 工作流（AGENTS.md 第 7 节）

- `main` 是稳定的生产分支；`develop` 是集成分支；功能在 `feature/*` 分支上开发。
- 每个功能通过 Pull Request 交付，且自动化检查必须全绿。
- 未经批准不得强推或删除分支；不得直接向 `main` 提交；未经项目负责人明确批准不得合并 PR。

## 已知限制

- **应用图标**：`build/appicon.png` 与 `build/windows/icon.ico` 是二进制资源，尚未提交（两者都作为生成文件被 git 忽略）。CI 与本地构建生成 Wails 的示例图标作为占位，使完整构建（包括 `.syso` 版本资源）能端到端跑通；占位图标不是 DeBrowser 的正式品牌图标。正式图标就绪后，用 `git add -f build/appicon.png` 添加，并从 `.gitignore` 中移除两行图标规则。
- NSIS 安装包任务（`task package`）已定义，但尚未在 CI 中实际运行。
- 业务模块现状：实例元数据管理与浏览器内核管理已交付（Stage 3、Stage 9A）；统一自动记录系统已交付（随 PR #10：统一事件模型与 operation_id 关联、用户可配置的日志总容量与最旧优先自动清理、数据管理页四类记录导出，见 `docs/ARCHITECTURE.md` 的日志与诊断一节）；书签前端、多开同步、备份界面属于后续阶段——见 `docs/ROADMAP.md`。代理相关功能已在 PR #9 范围清理中移除，不再是后续阶段内容。

## 构建产物内容

Windows 构建产物只包含应用可执行文件（`bin/*.exe`）。没有任何代理核心或其他 sidecar 二进制被打包、下载或由 CI 验证：Mihomo 核心集成已在 PR #9 范围清理中移除，应用在运行时既不安装也不启动任何核心。
