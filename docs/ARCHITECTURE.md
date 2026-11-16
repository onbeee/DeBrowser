# DeBrowser 架构

## 概览

DeBrowser 是模块化单体：一个 Go 二进制内嵌一个 React 前端，通过 Wails v3 以原生 Windows 桌面应用形式交付。没有微服务，没有外部服务器——全部状态都保存在用户自己的机器上。

```text
┌────────────────────────────────────────────────────────┐
│ React 前端 (frontend/)                                 │
│  页面 · 布局组件 · Zustand 界面 store                  │
└──────────────┬──────────────────────┬──────────────────┘
               │ 生成的 bindings      │ 类型化 events
               │ (frontend/bindings)  │ (app:status, …)
┌──────────────▼──────────────────────▼──────────────────┐
│ Wails v3 应用 (internal/app)                           │
│  注册在应用对象上的 services                           │
├────────────────────────────────────────────────────────┤
│ 服务层 (internal/service)                              │
│  AppService · InstanceService · DiagnosticsService     │
│  BrowserService（Stage 9A；书签接入在 Stage 9C）       │
├────────────────────────────────────────────────────────┤
│ 基础层 (internal/)                                     │
│  paths · config · settings · logging                   │
│  database (SQLite + migrations + 备份) · version       │
│  model · store (repositories, Stage 2)                 │
├────────────────────────────────────────────────────────┤
│ 本地资源                                               │
│  <root>\data\debrowser.db（便携布局）                  │
│  <root>\data\profiles\<instance-id>\（Stage 3 路径规划；│
│        目录在 Stage 9B 随实例创建实际建立）            │
│  <root>\webview2-data\（应用窗口的 WebView2 数据）      │
│  经 CDP 控制的 Chromium 进程（计划中，Stage 9C）       │
└────────────────────────────────────────────────────────┘
```

## 后端（Go）

`main.go` 是入口。启动顺序固定：

1. `internal/config` 与 `internal/paths` 从可执行文件所在目录（永不从工作目录）解析便携布局，创建全部目录并探测应用根目录是否可写——只读位置会以明确错误中止启动；不回退到 `%APPDATA%`。启动是直接的：首次启动创建并初始化的目录与文件，与已有安装完全相同——没有欢迎页或选择页。
2. `internal/logging` 在 `data/logs` 下打开分类结构化日志（Stage 5B，见下方「日志」）——在数据库与服务之前打开，在关闭时最后关闭。
2b. `internal/singleinstance` 取得按安装划分的锁 `data/app.lock`（Stage 3），它是操作系统级文件锁——Unix 上是 `flock`，Windows 上是 `LockFileEx`。操作系统是唯一的仲裁者：只有一个进程能持有它，竞争者永不删除或重建对方的锁文件（没有「先检查再删除」的竞态），且锁由操作系统在持有者以任何方式退出（正常退出、崩溃、强制结束、断电）时自动释放，因此不存在陈旧锁。该文件仅出于信息目的记录持有者的 PID。对同一便携文件夹的第二次启动得到 AlreadyRunning，写入一个激活信号（`data/activate.request`）请已运行的实例聚焦其窗口，然后在不触碰数据库的情况下退出。其他便携文件夹使用其他锁文件，互不影响。
3. 没有外部数据迁移步骤：DeBrowser 永不从 `%APPDATA%`、`%LOCALAPPDATA%` 或任何其他位置扫描、检测或导入数据。Stage 1 的 Roaming 自动迁移（`internal/migration`）已在 Stage 5A 移除；唯一存在的迁移是第 4 步的数据库内 SQLite schema 迁移。
4. `internal/database` 打开 SQLite（WAL 模式、`synchronous=FULL`、外键开启）并以事务方式应用待执行的迁移；升级已有数据库时，先把经验证的 `VACUUM INTO` 备份写入 `data\backups\`。核心数据访问经 `internal/store` 的 repositories 操作 `internal/model` 类型（Stage 2）。
5. `internal/service` 构建 Wails 服务（包括由 `internal/settings` 管理、来自 `config/settings.json` 的持久化用户设置）。
6. `internal/app` 组装 Wails 应用（服务、主窗口，并把 `Windows.WebviewUserDataPath` 设为便携的 WebView2 目录），`main` 运行它。后台循环（10 秒一次的状态事件、500 毫秒一次的激活监视）经 `internal/lifecycle` 在应用 context 下运行；关闭时取消 context，`App.Run` 等待循环排空后才返回——该等待按设计不设上限（每 5 秒记录一条警告），因为取消 context 无法打断已经在运行的回调，而提前返回会让 `main` 在仍有活动读取者时关闭数据库。只有当每个循环都被证明已经退出后，`main` 才依次关闭数据库、释放实例锁、关闭日志——顺序固定。`main` 是 `run() error` 的一层薄包装；启动中途没有任何地方调用 `log.Fatalf`，因此延迟清理总会运行。

Stage 3 还增加了 `InstanceService`（bindings：`ListInstances`、`GetInstance`、`CreateInstance`、`UpdateInstance`、`DeleteInstance`、`DeleteInstanceAndProfile`，Stage 9B 增加 `StartInstance`、`StopInstance`、`ListAvailableProfiles`——Stage 4A 曾增加的 `ListProxies` 绑定已在 PR #9 清理中再次移除），它建立在 Stage 2 store 之上：带校验的实例元数据 CRUD，约束错误信息为面向用户的中文。`DeleteInstance` 只删除元数据，永不触碰 Profile 文件；`DeleteInstanceAndProfile` 是显式的破坏性路径，它拒绝删除持久化状态为 running 的实例、拒绝 profiles 根目录本身，并拒绝任何无法通过真实路径解析**证明**严格位于 `data/profiles` 之内的目标：目标本身不得是符号链接/重解析点，每个路径组件都必须解析（包括中间的符号链接与 Windows junction；Windows 上用 `GetFinalPathNameByHandle`，Unix 上用 `EvalSymlinks`）到仍严格位于已解析 profiles 目录之内的位置。删除随后是失败原子的、且以**对象**为锚而非裸的递归删除（`internal/pfiles`）：移入 `profiles/` 下隔离槽的操作作用于经验证的文件系统对象，永不作用于经验证的路径字符串——在 Linux 上，从 profiles 根开始的下降以目录文件描述符为锚（每个组件用 `openat` 加 `O_NOFOLLOW` 打开，根目录本身也以 `O_NOFOLLOW` 打开），移动是两个固定 fd 之间的 `renameat2(RENAME_NOREPLACE)`，并在移动后做 `(dev, ino)` 身份核对；在 Windows 上，目标目录以 `FILE_FLAG_OPEN_REPARSE_POINT` 打开，其身份通过句柄验证（重解析属性加上相对已验证父目录的 `GetFinalPathNameByHandle`），重命名本身通过该句柄完成（`SetFileInformationByHandle`/`FileRenameInfo`，不替换），因此在验证后替换任何路径组件都无法重定向这次移动。接着删除数据库记录；失败时对象经同一锚点移回（在 Windows 上，恢复前会先重新验证原父目录的身份，若父目录现在解析到别处则拒绝恢复、把数据留在隔离区），只有当记录确实删除后，隔离树才由 `internal/safedelete` 擦除，且从验证到删除的对象身份链不断裂：经验证的句柄在擦除前**不**关闭——在 Linux 上，擦除以 `RemoveVerifiedAt` 相对固定的 profiles 根 fd 执行（隔离槽必须仍持有移动时验证过的那个确切的 `(dev, ino)` 对象，且每个条目的身份在被 unlink 前立即重新验证）；在 Windows 上，被验证并重命名的正是那个句柄被交给擦除引擎，引擎从目录句柄枚举子项（`GetFileInformationByHandleEx` 配合扩展目录类 `FileIdExtdDirectoryRestartInfo`/`FileIdExtdDirectoryInfo`——即有文档记载的 Win32 `FILE_INFO_BY_HANDLE_CLASS` 家族——得到每个子项的 128 位文件 ID），相对其经验证的父句柄打开每个子项（`NtCreateFile` 的 `RootDirectory`，刻意**不带** `FILE_SHARE_DELETE`，使擦除持有期间已打开的对象无法被其他进程重命名出这棵树），要求打开的子项的 128 位身份（卷序列号加 `FileIdInfo` 的 `FileId`）与枚举到的身份相等（枚举与打开之间发生替换会中止擦除；无法提供扩展类或 128 位身份的文件系统会让擦除失败关闭，而不是降低检查强度；身份保证是即时性的而非永久的对象凭据——Microsoft 文档说明文件 ID 在对象被删除后可能被复用，而在 NTFS 上对象在删除前保持其 ID——因此该比较只约束枚举→打开这个窗口，共享模式随后把这个窗口关闭），并通过经验证的句柄删除（`FileDispositionInfo`），重解析点只作为链接对象本身处理。Linux 的保证边界：以描述符为锚的删除跟随经验证的 inode 并在每次 unlink 前重新验证身份，因此从未在边界内验证过的对象永远不会被删除——但 Linux 没有共享模式，能在树内重命名条目的同 UID 进程可以在擦除中途把一个已验证对象移出，删除会跟随那个对象；这一残余风险需要一个对 Profile 树有写权限的敌对同 UID 进程，超出单用户桌面威胁模型（Windows 用共享模式关闭了它）。两个平台在任何深度都不重新解析路径字符串，因此在擦除期间替换 profiles 根或任何中间路径都无法重定向删除。在 Darwin 上，标准库不提供描述符相对或句柄式的重命名，因此破坏性操作失败关闭（拒绝，数据与记录完好），而不是回退到基于路径的移动。Stage 9B 起，进程管理由 `internal/runner` 与 `InstanceService` 的运行时管理器承担（见下方 Stage 9B 一节）：启动对账不再无条件把 `running` 对账为 `stopped`——无 `instance_runtime` 记录的 `running` 行仍按已死进程残留对账为 `stopped`（`Store.ReconcileInstanceStatuses`），有记录的行则逐项核验进程身份后重新接管、清除或置于无法确认状态。Stage 3 不需要 schema 迁移。

AGENTS.md 的职责划分：Go 负责浏览器进程管理、实例生命周期、Profile 隔离、内核管理、书签、同步、数据库操作与备份/恢复。React 只负责渲染、交互、表单、表格、前端状态与通知。代理管理**不是**当前职责：它已在 PR #9 清理中移出产品范围（见下方「已移除的代理 / 订阅 / Mihomo 功能」）；历史代理数据库结构与 Vault 引用查询仅作为兼容设施保留。

### 前端 ↔ 后端通信

- **Bindings**：注册的 Go 服务上导出的方法，通过静态生成的 TypeScript bindings 暴露在 `frontend/bindings/`（用 `wails3 generate bindings` 重新生成；永不手工编辑）。
- **Events**：在 `main.go` 中注册的类型化自定义事件（`application.RegisterEvent`）。目前只有一个：`app:status`（携带 `service.Status`，每 10 秒发出一次，也可通过 `AppService.GetStatus()` 按需获取）。

## 前端（React）

- `src/components/layout/`——侧边栏导航与顶部栏。顶部栏是页面标题的唯一所有者（各页面内部不再重复主标题）；应用没有底部固定状态栏。
- `src/components/`——共享布局组件（侧边栏、顶栏）。没有控制台面板。
- `src/pages/`——每个导航入口一个页面。尚未交付的模块渲染明确的「计划在 Stage N 交付」状态，而不是假数据。
- `src/stores/`——Zustand store，保存界面状态（当前页面、后端状态、主题、会话日志行）。
- 样式经 `@tailwindcss/vite` 插件使用 Tailwind CSS v4（`src/index.css`）。

## 浏览器手动登记（Stage 9A）

Stage 9A 增加 `BrowserService`（bindings：`AddBrowserManually`、`UpdateBrowserPath`、`ListBrowsers`、`GetBrowserDetails`、`RemoveBrowserRegistration`），支撑「浏览器内核」页面的唯一功能区域「自定内核」。

- 手动登记是**纯路径登记**，完全不涉及任何验证包：用户在文件选择器中任选一个程序可执行文件，`BrowserService` 只把所选的绝对路径和一个从文件名派生的显示名存入 `external_browsers`（migration 0011，经 migration 0012 重塑）。DeBrowser 对用户所选文件**不做**任何身份、产品、版本、签名或发布者检查，不要求它必须是 Chrome 或 Brave，在本阶段永不启动它，也永不复制、修改、卸载或删除它、永不读取它的 Profile；重新选择路径会更新该行，移除登记只删除数据库行。
- **实例与登记的关联**：创建/编辑实例时，用户可从已登记浏览器中选择一个关联（也可不选）。关联按登记的稳定 ID 建立（`browser_instances.kernel_id` 存 `external_browsers` 的 ID，`kernel_name` 存名称快照），不按名称关联——同名登记不会关联错误。实例的生效可执行路径不落库，由 `InstanceService` 按 ID 从登记记录实时解析为 `kernelExecutablePath` 返回，因此登记重新选择路径后，关联实例立即使用新路径。编辑实例时选择空项会清除已有关联；而关联功能之前以自由文本记录的内核名称/版本（`kernel_id` 为空的历史实例）在编辑时原样保留，不会被无关编辑静默清除。被实例引用的登记禁止移除（服务先计数引用并明确报错，不级联删除实例）。关联不引入任何文件检查：登记与关联全程不查存在性、身份、版本、签名或发布者，也不启动任何程序；实例真实启动由 Stage 9B 交付（见下）。
- **没有独立内核下载管理功能**：DeBrowser 主动下载、安装、验证、管理独立 Chromium 的功能已按产品决定取消（PR #11）。曾经支撑该功能的 `internal/kernels`（下载/解压/安装）与 `internal/browserprobe`（Authenticode 签名与发布者验证）两个包、BrowserService 的托管内核接口以及 `kernel:install-progress` 事件均已删除。数据库中的 `kernels` 表（migration 0010）作为历史结构保留，既有数据库照常升级，但当前没有任何用户可操作的内核管理功能，store 也不再提供 kernels 的增删改查。

Stage 9A 本身不启动任何浏览器进程；实例真实运行由 Stage 9B 交付（见下），CDP 接入在 Stage 9C 交付。

### Stage 9B：实例真实运行

- **进程层（`internal/runner`）**：只管理 DeBrowser 自己启动的浏览器主进程。Windows 实现以进程句柄等待退出、以 `GetProcessTimes` 取创建时间、以 `QueryFullProcessImageName` 取可执行路径、经 `NtQueryInformationProcess` 读取目标进程命令行（只读核验），优雅关闭是向该进程的顶层窗口投递 `WM_CLOSE`——等同于用户点击窗口关闭按钮；Unix 实现语义相同（/proc 身份信息、SIGTERM 优雅关闭）。从不按进程名全局结束进程，从不强制结束进程树：关闭在限定时间内未完成即如实报告「关闭未完成」，实例保持运行中。
- **启动**：`InstanceService.StartInstance` 校验实例、内核登记（按关联 ID 实时取登记的当前路径）与实例文件（必须是 `data/profiles/` 下的受管文件夹；缺失时按首次启动补建空目录；符号链接等无法证明位于受管根内的目标拒绝启动），以 `--user-data-dir=<实例文件绝对路径>` 启动进程；进程对象不等于启动成功——进程须存活过稳定窗口，且身份核验全部通过（可执行路径一致、命令行经严格参数解析后 `--user-data-dir` 的值与实例文件完整路径精确相等——子串或前缀相同不算一致，参数缺失/重复/为空一律不通过，命令行或可执行路径不可读同样视为核验失败），才写入 `instance_runtime` 身份记录、置 `running` 并开始监控。任一核验失败都会让本次启动如实失败（本次操作刚启动的未核验进程会被请求关闭，不留幻影进程）。若进程已启动但进程跟踪建立失败（句柄打开或创建时间读取失败），runner 会通过启动时持有的进程对象结束**本次启动的准确子进程**并确认其退出，绝不按进程名或 PID 匹配误伤其他进程；退出无法确认时，实例写入无创建时间的身份记录并进入生效状态 `unknown` 冻结危险操作，留待启动对账处理。
- **退出感知与重启识别**：每个被跟踪进程都有监控协程；进程自行退出（用户直接关闭浏览器窗口或浏览器崩溃）会被立即感知，身份记录随之删除、状态按退出码更新为 `stopped`/`error` 并写入运行记录。DeBrowser 退出不关闭浏览器（子进程是独立操作系统进程）；下次启动时按身份记录逐项重新核验（见 `docs/DATABASE.md` 的 `instance_runtime` 一节），核验通过的重新接管监控，证明失效的清除并置 `stopped`，无法确认的置于生效状态 `unknown` 并冻结一切可能危及数据的操作。
- **实例文件**：实例文件是 `data/profiles/` 下与实例一对一绑定的文件夹（数据库 UNIQUE 约束 + 服务检查）。新建可选默认（持久计数器顺序编号 P-1、P-2……，永不复用）、自定（新建）、选择（绑定保留的未绑定文件夹，不复制内容）；编辑时的「自定」是重命名当前文件夹本身（数据随文件夹移动，不复制不合并），「选择」是更换绑定（原文件夹解除绑定并保留在磁盘上）。可选列表只包含经证明存在、未绑定、未被运行记录引用且名称合法的文件夹——状态不明者一律不出现。运行中（或状态无法确认）的实例禁止更换内核、重命名/更换实例文件与删除。

## 浏览器隔离（基础在 Stage 3，运行时在 Stage 9）

Stage 3 只交付了基础：实例元数据管理、`profiles/` 下每实例一个目录的独立 Profile 目录**布局规划**（路径记录在实例元数据中；目录本身在 Stage 3 时不会在创建实例时实际生成——Stage 9B 起随实例创建实际建立），以及上文所述的 Profile 删除安全机制。Stage 3 **不**实现 Chromium 进程管理，也不实际启动浏览器——进程管理由 Stage 9B 的 `internal/runner` 与运行时管理器交付（见上文 Stage 9B 一节）。

Stage 9 增加运行时部分：浏览器手动登记（9A，已交付）、浏览器进程与实例启动/停止（9B）、CDP 接入（9C）、独立的浏览器配置与运行时隔离。在最终形态下，每个浏览器实例拥有唯一 ID、位于 `profiles/` 下的独立用户数据目录、独立配置，以及它自己的浏览器进程。用户数据目录永不在运行中的实例之间共享，活动 Profile 永不在 Chromium 使用期间被复制。DeBrowser 不宣称指纹隔离，除非它被明确实现并通过测试。

## 凭据存储（Stage 4A，internal/secrets）

凭据 Vault 保存由已移除的代理/订阅功能写入的凭据（代理凭据与订阅 URL），存放在一个加密的 Vault 文件 `data/secrets.vault`（`paths.Layout.SecretsFile`）中。SQLite 只保存不透明的 `secret_ref`；明文永不进入数据库、日志、设置文件或前端持久状态。当前没有任何功能创建新凭据，但 Vault、它的条目及其引用保护被完整保留——历史凭据必须保持可解析，且只要还有历史行引用它们就永不被回收——`main` 仍在启动时按原样打开 Vault（见下方范围注记）。

- **加密**：生产环境使用 Windows DPAPI（CurrentUser）。DPAPI 把解密绑定到同一台机器上的同一 Windows 用户：便携文件夹在同一台机器内移动仍可工作；换机器或换用户会使条目无法解密，存储层会逐条报告（`ErrCorrupt`），以便界面要求重新输入——条目被保留，永不静默丢弃，也永不降级为「无凭据」。Unix 构建使用 AES-256-GCM 密钥文件保护器，仅作开发替身。
- **与 SQLite 的一致性**：blob 不可变。创建 = 先持久化密钥，再提交数据库引用（数据库失败时补偿删除）。更新 = 持久化新条目，在一个事务中换引用，再删除旧条目。删除 = 先删数据库行，再删 Vault 条目。`Put` 只在新的 Vault 文件已持久落盘后才报告成功，因此数据库永不引用未持久化的密钥；清理失败会留下可识别的孤儿，由垃圾回收清扫，只回收未被引用且超过 24 小时的条目（年龄门槛覆盖「持久 Put 完成与其数据库提交之间」的崩溃窗口；被多行共享的引用是活引用，不可触碰）。
- **共享引用删除契约（对任何未来调用方有约束力）**：Vault API **没有**无条件删除。补偿使用 `DeleteCompensating`，它只适用于仍处于待定状态的引用（由本进程创建、从未提交），因此不可能被引用或共享。全部业务清理使用 `DeleteIfUnreferenced`，其活引用检查在决策时于 Vault 锁下运行。任何未来创建或丢弃引用的服务，必须把这些数据库变更与这些清理调用在同一服务级互斥锁下串行化，使「检查并删除」相对引用变更是原子的；单实例锁排除其他进程。
- **范围注记（PR #9 清理）**：Stage 4A 的 `ProxyService` 及其界面已按项目负责人决定移除。Vault 本身、它的文件格式以及全部既有条目与引用作为数据兼容层原样保留：`main` 仍在启动时打开 Vault（保留下方的恢复行为），但当前没有任何产品功能通过面向用户的服务读写凭据，也没有 Vault 侧边栏入口。

- **原子性与恢复（Windows）**：每次写入都走 临时文件 → fsync → 限制权限 → 把上一个完好文件快照到 `secrets.vault.bak` → `MoveFileExW` 带 `MOVEFILE_REPLACE_EXISTING|WRITE_THROUGH`（在 Windows 上不假设普通 `os.Rename` 的替换语义）→ 写入后验证读取。主文件损坏时回退到备份；两者都损坏时以重命名方式隔离（永不删除），存储从空开始，每个引用都解析为「凭据不可用」。
- **文件权限**：Vault 及其备份带有一个受保护的 DACL，恰好授予当前用户、Local SYSTEM 与 Administrators 完全访问——通过 SDDL（`D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;<user SID>)`）应用到文件本身，切断来自便携文件夹的继承，并在每次打开时回读描述符验证。若权限无法收紧，存储层拒绝打开或写入，而不是悄悄继续。任何未来的凭据文件（例如代理能力若被重新引入时代理核心的控制器密钥）都必须接受同样的 `secrets.RestrictFile` 处理。

## 代理能力（已移出产品范围）

代理管理、订阅导入、统一代理配置以及任何代理核心运行时已在 PR #9 清理中移出产品范围（项目负责人决定），不是当前或已排期的功能。「代理配置」侧边栏入口只是占位页面。代理能力是否重新引入将在未来单独评估；它不在路线图上。（为这种评估记录一条设计约束，以免丢失：经代理的实例只有在代理核心本身通过其控制器确认后才可视为「就绪」——成功与监听端口建立 TCP 连接不是充分证据——且未就绪的经代理实例必须失败关闭，永不回退到直连。当前代码中这些都不存在。）

## 日志（Stage 5B 基础 + 统一自动记录系统，internal/logging）

三条分类的 JSON Lines 流位于便携目录的 `data/logs/`，各自存放在按日期命名的文件中（`<category>-YYYY-MM-DD.log`，在 5 MB 时滚动到 `.002`、`.003`…… 分卷）：

- **run**——运行记录（统一类别名 `runtime`）：应用生命周期、页面打开/离开、模块与后台循环运行事实，以及既有的 `*slog.Logger` 输出（经一个消毒守卫 handler）；保留 30 天。
- **operation**——操作记录：用户发起的业务操作及其后台步骤与结果（实例创建/更新/删除、浏览器登记、主题与容量设置、各类导出）；保留 90 天。
- **error**——错误记录：由同一守卫 handler 从 run 流分流的错误级 slog 记录，以及每次失败操作的完整错误事件（错误代码、类别、详情、用户实际看到的提示、耗时）；保留 90 天。

**统一事件模型**：结构化记录一律是统一事件（`internal/logging` 的 `Event`）：`event_id`、`timestamp`、`event_type`、`category`、`module`、`action`、`target_type` / `target_id`、`operation_id`、`parent_event_id`、`trigger`、`status`、`duration_ms`、`error_code` / `error_category` / `error_detail`、`user_message`、`metadata`。每次用户操作由前端生成唯一的 `operation_id`，作为业务绑定的第一个参数传入后端；后端以同一 ID 写下根事件（operation_start）、步骤事件（operation_step，经 `parent_event_id` 链接根事件或上一步）与终局事件（operation_result），前端再以同一 ID 补记用户实际看到的反馈（client_result），因此「用户操作 → 前端请求 → 后端执行 → 后台步骤 → 执行结果 → 用户提示」可以完整还原，不同操作永不共用 ID。失败时另在 error 流写一条完整错误事件，与操作事件是不同事件而非副本；「全部记录」是三条流的汇总视图，不额外复制数据。既有 slog 行与 5B 时期的审计行仍是各自流的合法成员，范围统计与导出按行内时间戳处理它们。

**容量管理**：三条流共用**一个**用户可配置的总容量上限（不再有每类 MB 上限；天数保留期不变），默认 100 MB，保存在 `config/settings.json` 的 `logCapacityBytes`，数据管理页可修改（预设 30/50/100/200/500 MB、1 GB 或自定义）。已用/剩余容量一律按日志目录内文件的实际字节统计（不含导出的 ZIP）。达到上限时自动清理最早记录：先整文件删除已轮转文件（跨类别按日期最旧优先，永不删正在写的文件）；仍超出时再按行修剪最旧的当前文件——同一 `operation_id` 的记录成组删除，组内部分删除时保留该组最后一条（结果），且文件中最新的一条记录永不删除。修改容量后立即重新统计并在超出时立即清理；因文件锁定等原因达不到上限时明确报错，绝不假装成功，清理失败同时记录在案并在数据管理页提示。

**可靠性与隐私**：记录写入与业务操作隔离——写入失败只累计计数（在数据管理页可见），永不使业务失败，也不会递归记录自身错误。事件落盘前统一清洗：凭据形 metadata 键直接丢弃，自由文本经与守卫 handler 相同的清洗（URL userinfo、key=value 密钥、长 token 串），密码、Token、Cookie、Vault 明文与 Profile 内容永不进入记录。守卫 handler 同样丢弃凭据命名的 slog 属性。

**数据管理页与导出**：数据管理页的「日志」区域固定五行——全部记录 / 操作记录 / 错误记录 / 运行记录各一行（实际日志文件名摘要——多文件时显示首个文件名加「另有 N 个文件」，悬停显示完整清单，无文件显示「暂无文件」—— + 实际日志存储目录 + 导出按钮），第五行「存储容量」显示「上限 / 剩余」并提供修改入口（对话框内同时显示上限、已用、剩余与使用进度）；页面不提供记录查看、实时流或日志中心。四个导出各自打开导出设置：后端按当前实际保留的记录动态返回最早/最新日期，用户选择日期范围后生成 ZIP——`records/<类别>.jsonl`（主要记录）、`manifest.json`（导出类别、请求范围、实际最早/最新事件时间、主要与上下文的数量区分、逐文件 SHA-256）、`README.md`；导出错误（或操作）记录时，对方流中同 `operation_id` 的关联记录作为 `context/` 下的上下文文件一并导出并在 manifest 中明确区分，绝不冒充主要记录。日志是唯一的记录系统：不存在独立的操作审计模块，曾经的诊断导出与故障报告导出已随数据管理页「诊断与分析」区域一并删除（PR #11）。所有导出在归档前做路径脱敏与敏感性扫描、命中即中止（失败关闭），目标位置通过原生保存对话框选择（接线在 `internal/app`，使服务层保持无 GUI），归档先写临时文件再原子替换，导出元数据中不记录任何机器绝对路径。

## 已移除的代理 / 订阅 / Mihomo 功能（PR #9 范围清理）

以下 Stage 4A / 6 / 6C 产品功能已按项目负责人决定移除，不再存在于代码中：代理服务器 CRUD（`ProxyService`）及其页面；订阅管理、带 SSRF 防护的订阅抓取器（包括 Fake-IP 系统代理回退）与导入管线（`SubscriptionService`、`internal/mihomoimport`）；统一的「代理配置」页面（已被一个只说明功能暂未开放、不调用任何服务的占位页取代）；以及 Mihomo 核心运行时（`internal/mihomocore`、`CoreService`、设置中的「Mihomo 内核」小节，以及任何核心二进制的打包——构建产物只有应用可执行文件，应用在启动时不下载、不安装、不启动任何核心，退出时也不停止任何核心）。

数据兼容性被有意保留：历史表（`proxies`、`subscriptions`、`mihomo_configs`、`mihomo_proxy_groups`、`mihomo_proxy_group_members`）及其全部行保留在既有数据库中，migration 0001–0009 未改动，这些表的 `internal/model` 类型作为历史数据模型保留。store 层在最终评审轮收敛到最小兼容面：不再有任何代理/订阅/配置的 create、update 或 delete 方法——只有 Vault 引用查询（`SecretRefInUse`、`AllSecretRefs`）仍读取这些表，使受守卫的 Vault 清理继续识别历史凭据引用。实例记录把已存的 `proxy_id` 值作为数据保留，但它不再是绑定：实例页面不显示代理列或选择器，`InstanceService` 不暴露代理列表，创建总是写入 `proxy_id` NULL，更新逐字保留已存值。凭据 Vault 文件及其条目同样未被触碰。本次清理不删除任何用户数据。

诊断故障报告曾在同一轮做过字段清理；该导出本身连同「诊断与分析」区域已在 PR #11 中整体删除，当前唯一的导出是上文的分类日志导出。

## 安全注记

- CDP 调试端口永不暴露到公网；它们只绑定 localhost（Stage 9C 接入，当前尚未实现）。
- 前端不能执行任意命令；它只能调用明确注册的服务方法。
- 密钥（凭据、订阅 URL、私有代理配置）永不以明文存入 SQLite、永不进入本仓库——见 `docs/DATABASE.md` 与 `.gitignore`。

## 明确排除

Electron、Docker、Redis、MongoDB、GraphQL 与微服务不在范围内（AGENTS.md 第 3 节）。脚手架的移动端与 Docker 资源不属于本项目；唯一支持的目标是 Windows 11 x64。
