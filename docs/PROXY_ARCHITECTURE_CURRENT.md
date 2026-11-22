# DeBrowser 代理模块技术现状（Stage 6C / PR #27）

本文档梳理 DeBrowser 代理模块**当前代码实际实现**的技术架构与运行逻辑，以仓库代码为唯一依据（基线：PR #27，head `2f0148c6e511c9dd5f81906621958ddbb13f001b`，main `989636eef076ddcafdbf179e29e190610db5f6c5`）。未实现的设计、历史方案与未来计划不在本文范围内；凡未实现的能力均明确标注「未实现」。

---

## 1. 当前架构概览

代理模块由四层组成，职责严格分开：

```text
┌────────────────────────────────────────────────────────────────┐
│ React 前端（Wails WebView）                                     │
│  代理配置页 ProxyConfigPage / 实例页 InstancesPage              │
│  只展示数据与收集用户意图，不解析配置、不管理进程               │
└───────────────────────────┬────────────────────────────────────┘
                            │ Wails bindings（ProxyService.*）
┌───────────────────────────▼────────────────────────────────────┐
│ Go 后端 ProxyService（internal/service/proxy_service.go）       │
│  业务数据：来源、实例绑定（SQLite）                             │
│  凭据：订阅 URL（Vault，DPAPI）                                 │
│  协调：生成核心配置、调用核心 API、核心生命周期、启动门禁       │
└──────┬──────────────────────┬──────────────────────┬───────────┘
       │ 子进程管理            │ REST API              │ SQL
       │ (Job Object)          │ (Bearer 密钥)         │
┌──────▼──────────┐   ┌────────▼─────────┐   ┌────────▼──────────┐
│ Mihomo 核心进程 │   │ Mihomo 控制器     │   │ SQLite            │
│ 官方 v1.19.34   │   │ 127.0.0.1:19097  │   │ proxy_sources     │
│ 独立进程        │   │                  │   │ instance_proxy_   │
│                 │   │                  │   │   bindings        │
└──────┬──────────┘   └──────────────────┘   └───────────────────┘
       │ 每实例一个 mixed Listener（127.0.0.1:21001+）
       ▼
  浏览器实例 A ── --proxy-server=http://127.0.0.1:21001 ──► Listener A
                                                              │ proxy:
                                                              ▼
                                                     实例专属 select 组
                                                     （REJECT + 绑定来源）
                                                              │
                                                              ▼
                                                         绑定节点出站
```

核心事实：

- **一个 Mihomo 核心进程**服务全部浏览器实例（`internal/mihomo/manager.go` 的 `Manager` 只持有一个子进程）。
- 每个已绑定实例在核心内有：一个 `mixed` Listener（独立回环端口）+ 一个专属 `select` 组。Listener 的 `proxy` 字段直接指向该组，流量不经过顶层规则（见第 7 节）。
- 节点全部经核心原生 **proxy-provider** 机制加载；生成的配置里**没有任何节点定义**（`internal/mihomo/config.go` 文件头注释即此约定）。
- 实例与节点的绑定关系是 DeBrowser 的业务数据，存 SQLite，**不依赖核心的 cache.db**（生成配置含 `profile.store-selected: false`）。
- 核心只由用户在代理页**手动开启**；DeBrowser 启动不自动开启，无任何后台自启路径。

关键业务流程（时序）：

```text
用户点「启动核心」
  └► ProxyService.StartCore
       ├► regenerate：SQLite（来源+绑定）+ Vault（订阅 URL）
       │     → mihomo.Generate → data/mihomo/config.yaml（原子替换）
       ├► Manager.Start：端口冲突探测 → 挂起创建子进程 → 分配 Job Object
       │     → 恢复进程 → 轮询 GET /version（≤15 秒）
       └► applySelections：逐绑定读 Provider 节点列表
             → PUT /proxies/{实例组}（节点存在选节点，不存在选 REJECT）

浏览器实例启动
  └► InstanceService 启动参数构建（instance_runtime.go）
       └► ProxyPortFor(= ProxyService.ProxyPortForInstance)
             ├─ 无绑定 → (0, nil)：不传代理参数，直连启动
             ├─ 核心未运行 / Listener 无响应 → 返回错误，拒绝启动
             └─ 正常 → (port, nil)：追加 --proxy-server=http://127.0.0.1:{port}
```

---

## 2. 技术选型与组件职责

只列与代理模块直接相关的选型：

| 技术/组件 | 使用位置 | 用途 | 核心运行依赖 |
|---|---|---|---|
| Go 1.26 | 全部后端 | 业务逻辑、配置生成、进程管理 | 是 |
| Wails v3.0.0 | `internal/app/app.go`、`main.go` | 桌面外壳与前后端 bindings；ProxyService 是注册的服务之一 | 是 |
| React 18 + TypeScript | `frontend/src/pages/ProxyConfigPage.tsx`、`InstancesPage.tsx`、`components/NodeCapsule.tsx` | 代理页、实例表单的代理选择、节点胶囊 | 是 |
| **Mihomo v1.19.34**（官方 MetaCubeX 发布，版本与哈希在 `internal/mihomo/manager.go` 中以 `CoreVersion` 及资产表锁定） | 独立子进程 | 全部实际代理工作：节点解析、协议连接、DNS、健康检查、测速、流量转发 | 是 |
| SQLite（modernc.org/sqlite，WAL） | `internal/store/proxy_sources.go`、migration 0015 | 保存来源与绑定两张业务表 | 是 |
| Mihomo REST API（HTTP + Bearer 密钥） | `internal/mihomo/client.go` | 版本/就绪探测、Provider 读取与刷新、组测速、组选择、配置热重载 | 是 |
| 配置生成（手写字符串渲染，无模板引擎） | `internal/mihomo/config.go` 的 `Generate` | 由业务数据确定性生成核心 YAML；字符串用 JSON 双引号转义作 YAML 标量 | 是 |
| Windows Job Object（`JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE`）+ `CreateProcess`（挂起创建）+ `STARTUPINFOEX` 句柄列表 + `DuplicateHandle` | `internal/mihomo/child_windows.go` | 子进程生命周期：DeBrowser 退出/崩溃/被强杀时核心被系统终止；限定子进程句柄继承范围 | 是（Windows） |
| `os/exec` 普通子进程 | `internal/mihomo/child_unix.go` | 非 Windows 平台的等价子进程启动（仅编译兼容与测试环境） | 否（目标平台为 Windows） |
| DPAPI（Windows CryptProtectData） | `internal/secrets/`（protect_windows.go） | Vault 中订阅 URL 的落盘保护；SQLite 只存不透明引用 | 是 |
| 统一自动记录（`internal/logging` 三类 JSONL） | `internal/service/audit.go` 的 `beginOp/finishOp` | 代理操作的操作/错误记录 | 否（记录失败不拖垮业务） |

没有引入：模板引擎、YAML 库（生成靠手写渲染，解析全在核心内）、进程管理框架、watchdog、消息总线。

---

## 3. Mihomo 核心管理

实现位置：`internal/mihomo/manager.go`（`Manager`）、`child.go` / `child_unix.go` / `child_windows.go`（子进程）、`internal/service/proxy_service.go`（`StartCore`/`StopCore`/`GetCoreStatus`/`ServiceShutdown`）。

### 3.1 文件与目录

便携数据目录内（`paths.Layout.Mihomo` = `<程序目录>/data/mihomo/`）：

| 路径 | 内容 |
|---|---|
| `mihomo.exe` | 官方核心二进制（`BinaryPath`）；缺失时从 GitHub 官方 Release 下载，资产 SHA-256 与 exe SHA-256 **双重校验**（`EnsureBinary`）；已存在的文件也校验 exe 哈希，不符拒绝使用 |
| `config.yaml` | DeBrowser 生成的运行配置（权限 0640），含订阅 URL 与控制器密钥，文件头注明禁止分享/导出 |
| `controller-secret.txt` | 控制器随机密钥（24 字节随机数 hex，权限 0600），首次使用时生成（`loadControllerSecret`，`sync.Once` 保证并发首调只初始化一次） |
| `sources/<来源ID>.yaml` | 本地文件来源的内容副本（0640）；用户原文件从不修改 |
| `providers/<provider>.yaml` | http provider 的核心侧缓存（核心自行写入） |
| `core.log` | 核心进程的 stdout/stderr（0640，追加） |
| `cache.db` | 核心自身缓存（核心管理；DeBrowser 不读不写，绑定不依赖它） |
| `validate-*/` | 文件导入验证用的一次性临时目录（用后删除） |

### 3.2 启动（唯一入口：用户在代理页点「启动核心」）

`ProxyService.StartCore`（持有服务锁 `s.mu`，整体超时 90 秒）：

1. `regenerate`：按当前 DB 状态重新生成 `config.yaml`；
2. `Manager.Start`：
   - 已有本 Manager 启动的核心在运行 → 直接返回成功（不重复启动）；
   - `EnsureBinary` 准备二进制；
   - 探测控制器端口 19097：已被占用 → 报错（说明可能是外部 Clash/Mihomo），**不接管、不停止、不改配置**；
   - Windows：`CreateProcess` 以 `CREATE_SUSPENDED` 创建 → 创建 Job Object 并设 `KILL_ON_JOB_CLOSE` → 分配进程 → `ResumeThread`；子进程 std 句柄为日志文件/NUL 的可继承**副本**（DuplicateHandle），且以 `STARTUPINFOEX` 句柄列表限定只继承这三个；
   - 轮询 `GET /version` 直至就绪（`readyTimeout` 15 秒）；
   - 任一就绪失败路径走 `abortStart`：终止子进程并有界等待退出；无法确认退出时**保留进程跟踪**（不清空句柄，Stop 仍可重试）；
3. `applySelections`：按 DB 绑定逐组下达选择；失败则停掉刚启动的核心并报错，不留运行中的核心。

并发保护：`Manager.opMu` 把 Start/Stop 整体串行化；`ProxyService.s.mu` 串行化全部配置变更与生命周期操作。

### 3.3 运行检查

- `Manager.Running()`：本 Manager 是否持有未退出的子进程；
- `ProxyService.coreRunning(ctx)`：`Running()` 且 `GET /version` 以我方密钥应答；
- `GetCoreStatus` 的状态机（stopped / running / error）：能区分「未运行」「运行中」「进程在但控制器无响应」「有外部核心以相同密钥应答（不接管）」「控制端口被其他程序占用」以及核心异常退出后的等待错误（`WaitErr`）。

### 3.4 停止与退出

- 用户点「停止核心」→ `Manager.Stop`：终止子进程并等待（`stopTimeout` 5 秒）；Kill 失败或超时返回真实错误并保留句柄，**不假报停止成功**；
- DeBrowser 正常退出 → Wails 服务关闭钩子 `ServiceShutdown` 调用同一 `Stop`；
- DeBrowser 异常退出/崩溃/被强制结束 → Windows Job Object 随进程句柄关闭而终止核心（`KILL_ON_JOB_CLOSE`）。**该行为已实现但尚未在 Windows 11 实机验证**（见第 10 节）；
- 从不按进程名查杀、从不停止外部 Clash/Mihomo。

### 3.5 运行期间的配置更新

不重启核心：`regenerateState` 生成新配置 → 原子替换文件 → `PUT /configs?force=true` 热重载 → `applySelections` 重新下达选择。核心拒绝重载时恢复旧文件字节并报错，运行配置不变（详见第 7 节）。

### 3.6 DeBrowser 重启后的状态恢复

- 核心**不恢复运行**：重启后核心默认关闭，与上次退出前状态无关（无状态恢复机制，这是产品规则）；
- 来源与绑定在 SQLite 中，重启后完整可见；节点列表/状态需待核心再次手动开启后从核心读取；
- 上一进程的核心不可能遗留（Job Object）；`GetCoreStatus` 若发现控制端口有以相同密钥应答的未知核心，只报错不接管。

---

## 4. 订阅来源管理

实现位置：`internal/service/proxy_service.go` 的来源部分；存储 `internal/store/proxy_sources.go`；表 `proxy_sources`（migration 0015）：`id / name / kind('url'|'file') / secret_ref（仅 URL 来源，Vault 引用）/ file_name（仅文件来源）/ provider_name（唯一，形如 src-<id 前 8 位>）/ 时间戳`。

### 4.1 添加 URL 订阅（`addURLSource`）

1. 前端弹窗收集名称与地址 → `ProxyService.AddURLSource`；
2. 校验：名称非空 ≤50 字；地址必须是 http/https 完整 URL；
3. 地址写入 Vault（`vault.Put`，DPAPI 保护），DB 只存引用；
4. 写 DB 行（provider 名由来源 ID 派生）；
5. 若核心正在运行：`regenerate` 把新 provider 热重载进核心 → `refreshProvider`（`PUT /providers/proxies/{name}`）要求核心立即抓取；**任一步失败整单回滚**（删行、回收 Vault 条目、重新生成不含该来源的运行配置）并报错；
6. 核心未运行时只存档，不抓取、不启动核心；节点在核心下次启动时由核心加载。

### 4.2 添加本地文件（`addFileSource`）

1. 前端先填名称，再经原生文件选择器选文件（`SourceFilePicker` 由 app 层注入）；
2. 浅检查（`readSourceFile`）：非空、≤32 MiB、文本包含 `proxies:`，否则明确拒绝；
3. 核心真验证（`checkFileData`/`countFileNodes`，核心二进制已存在时执行）：用同一子进程管理启动一个**一次性验证核心**（独立临时目录、控制器端口 19098、无 Listener），把候选文件作为 file provider 加载并计数节点；0 节点或无法加载 → 拒绝导入。二进制尚不存在时跳过验证，内容由生产核心首次加载时判定；
4. 内容复制到 `data/mihomo/sources/<来源ID>.yaml`（原文件不动），写 DB 行；
5. 核心运行时 `regenerate` 热重载；失败回滚（删行、删副本）。

### 4.3 Provider 的生成与加载

见第 7 节配置生成。URL 来源 → `type: http` provider（`interval: 86400` 即核心每 24 小时自行刷新一次、`proxy: DIRECT`、缓存路径 `./providers/<name>.yaml`）；文件来源 → `type: file` provider（路径指向 DeBrowser 的副本）。节点解析完全在核心内完成。

### 4.4 节点读取

`ListNodes`：核心运行时逐来源 `GET /providers/proxies/{name}`，把核心返回的节点（名称、类型、alive、最近一次健康检查 delay、是否有检查记录）映射为 `NodeView` 并标注来源。核心未运行时返回空列表。**DeBrowser 不保存任何节点数据的副本**（无节点表、无节点缓存文件）。

### 4.5 更新订阅（`RefreshSource`）

- 仅 URL 来源；要求核心正在运行（不隐式启动）；
- `refreshProvider` 要求核心重新抓取；核心抓取失败时**保留旧节点**（核心原生行为），服务返回「订阅更新失败，已保留原有节点」；
- 成功后 `applySelections` 重新下达全部绑定选择（消失的节点 → REJECT，回归的节点 → 恢复原绑定）；选择应用失败会作为错误返回；
- 本地文件来源的对应操作是「重新导入」（`reimportFileSource`）：先验证新文件 → 保留旧内容 → 替换副本 → 刷新 provider；刷新失败时把旧文件**逐字节写回**并再次刷新恢复，恢复失败返回包含双错误的明确提示。

### 4.6 删除来源（`DeleteSource`）

1. 有实例绑定该来源的任何节点 → 拒绝删除并提示先解绑（服务层检查 + 外键兜底）；
2. 先把「不含该来源」的提议配置推入核心（`regenerateState`），核心接受后才删 DB 行；删行失败则重新生成恢复运行配置，恢复失败明确报错；
3. 删行后：删除文件副本（文件来源）、删除 provider 缓存文件、经 `DeleteIfUnreferenced` 回收 Vault 凭据。

### 4.7 格式支持的准确边界（按代码判定）

- 订阅与本地文件**都只作为 proxy-provider 的载荷**交给核心。核心对 provider 载荷只提取顶层 `proxies:` 节点列表。
- 因此：含 `proxies:` 的节点文件可导入；**完整 Clash/Mihomo 配置文件也可导入，但只有其中的节点被使用**，其自带的 proxy-groups、rules、rule-providers、DNS、TUN 等一律不进入 DeBrowser 的运行配置（生成的配置是 DeBrowser 自己渲染的，不合并用户配置）。
- 节点不在顶层 `proxies:`（如订阅内部再嵌套 provider）时核心提取到 0 个节点：文件来源在验证阶段被拒绝；URL 来源在添加后显示为「empty」状态并给出明确提示，不报成功。
- 非 Clash 格式（Base64/URI 串等）核心无法解析，同样按 0 节点处理。
- DeBrowser 没有自研任何代理协议/订阅格式解析器；唯一的自研检查是导入前的浅检查（文件含 `proxies:` 字样）。

---

## 5. 节点管理与测速

### 5.1 节点身份

- 节点没有 DeBrowser 侧的 ID。身份 = **（来源，来源内原始节点名）**二元组：来源由 provider 隔离（每实例组只 `use` 自己来源的 provider），同名节点在不同来源互不冲突；
- 绑定表只存 `source_id + node_name`（`instance_proxy_bindings`），不存节点其他参数；节点参数（服务器、端口、密码、协议字段）全在核心内，DeBrowser 不读取、不保存。

### 5.2 三种「测活」机制的区别（不可混为一谈）

| 机制 | 触发 | 实现 | 产出 | 用途 |
|---|---|---|---|---|
| Provider 健康检查 | 核心自动（节点被使用时）+ 测速/流量间接触发 | 核心内建：provider 配置 `health-check{enable:true, lazy:true, url: https://www.gstatic.com/generate_204, interval: 300}` | 每节点的 alive 与 delay 历史（`GET /providers/proxies/{name}` 的 history） | 节点列表的延迟显示、绑定状态四态（ok/down/missing/unknown）的数据来源 |
| 节点延迟测速（手动） | 用户点「节点测速」 | `ProxyService.TestDelays` → 对每个来源调用一次 `GET /group/delay-{provider}/delay?url=https://www.gstatic.com/generate_204&timeout=5000` | 每节点 delay（毫秒）；失败/超时的节点不在核心返回的 map 中，服务记为 **-1** | 测速结果显示（前端会话内保存，不落盘） |
| 实际代理连接测试 | **未实现**（无此功能） | — | — | 真实连通性只能通过启动浏览器实例实际使用来验证；DeBrowser 没有「经 Listener 发起一次真实请求」式的节点测试 |

测速细节（`testDelays`，`internal/service/proxy_service.go`）：

- 要求核心运行，否则明确报错，不隐式启动；
- 来源**逐个串行**处理：先读该来源节点列表，再对该来源的测速组发一次 group delay 调用；组内全部节点由核心并发测试（并发度是核心内部行为，DeBrowser 不控制）；
- 单次调用的测速超时为 **5000 ms**（`GroupDelay` 的 timeout 参数）；REST 客户端本身超时 10 秒（`client.go`）；整个操作上下文上限 120 秒；
- 测速组 `delay-<provider>` 是每来源一个隐藏 select 组，存在的理由（已实测核实）：provider 内节点没有单节点 delay 端点（`GET /proxies/{节点}/delay` 返回 404），provider healthcheck 端点是异步触发（204 立即返回、只能用配置内 URL、无逐节点结果），组 delay 是核心唯一同步返回逐节点延迟的 API；
- 某个来源测速失败（组调用出错）时，该来源全部节点记 -1，其余来源继续；
- 结果不持久化：前端 `ProxyConfigPage` 以 Map 保存在页面会话内，刷新页面后回到健康检查历史的 delay 显示；
- **没有自动测速、没有定时测速**；健康检查的 `lazy: true` 意味着未被使用/未被测速的节点可能长期没有检查记录，UI 显示为「未检查」（中性），不冒充健康。

### 5.3 节点状态展示

绑定状态由 `nodeState` 纯派生（不落库）：核心未运行或读不到 provider → `unknown`；节点不在来源当前列表 → `missing`（黄色）；在列表但无检查记录 → `unknown`（中性）；有记录且 alive → `ok`（绿色）；有记录且不 alive → `down`（红色）。三处 UI（实例列表、实例表单、代理页绑定总览）共用 `NodeCapsule` 组件以颜色呈现，胶囊内只有节点名。

---

## 6. 浏览器实例与代理绑定

数据：`instance_proxy_bindings`（migration 0015）：`instance_id`（主键，外键 → browser_instances，`ON DELETE CASCADE`）、`source_id`（外键 → proxy_sources，无级联）、`node_name`、`port`（唯一）、时间戳。

基本规则（按代码）：

- 一个实例**最多一个**绑定（一行）；一个节点可被**多个**实例同时绑定（各实例组独立选择，互不影响）；
- 端口分配（`allocatePort`）：21001–21999 内取最低的「未被其他绑定占用且当前回环探测无应答」的端口；实例一旦分配端口，换节点/换来源时**保留原端口**；
- 实例的 Profile 与代理绑定无数据关系：绑定只存在于上表，Profile 目录不写任何代理信息。

### 流程 1：实例创建

前端 `InstancesPage` 新建表单（实例本体由 `InstanceService` 创建，与代理无关）→ 表单内可同时选择代理节点（来源分组的节点下拉，数据来自 `ListNodes`）→ 保存实例后，前端单独调用 `ProxyService.SetBinding` 写入绑定；绑定失败与实例保存成功是两个结果，界面分别报告（绑定失败显示警告，不回滚实例）。

### 流程 2：实例绑定节点（`setBinding`）

1. 校验实例存在、来源存在、节点名非空；
2. 核心运行时：先读该来源当前节点列表，**节点不存在直接拒绝**（核心未运行时无法校验，绑定先按用户选择存档，待核心启动后由 `applySelections` 与状态派生处理，见第 10 节限制）；
3. 分配或复用端口；
4. 一致性顺序：先把含提议绑定的配置生成并热重载入核心（`regenerateState`），核心接受后才 `UpsertBinding` 写 DB；写 DB 失败则重新生成恢复旧配置，恢复失败明确报错；
5. 返回 `BindingView`（含派生状态与 Listener 探测结果）。

### 流程 3：实例启动

`InstanceService` 构建启动参数（`internal/service/instance_runtime.go`）时调用注入的 `ProxyPortFor`（`main.go` 中接线为 `ProxyService.ProxyPortForInstance`）：

1. 查绑定：无绑定 → `(0, nil)`，不追加代理参数，与无代理时完全一致（直连）；
2. 有绑定：`coreRunning` 为假 → 返回错误「已绑定代理但核心尚未开启」，**拒绝启动**，不自动开启核心；
3. 核心在运行：`ProbePort(绑定端口)` 探测 Listener 是否应答，无应答 → 返回错误并拒绝启动（提示检查核心状态）；
4. 正常：返回端口，启动参数追加 `--proxy-server=http://127.0.0.1:{port}`，浏览器流量进入该实例的 Listener → 实例组 → 当前选择节点（或 REJECT）。

### 流程 4：实例更换节点

同流程 2（`SetBinding` 覆盖同一行；端口不变）。换绑会立即把新配置重载入核心并由 `applySelections` 更新该实例组的选择。已启动的浏览器仍连在同一个 Listener 上，而 Listener 的下一跳就是该实例组，因此**换绑后已启动浏览器的出站节点随组选择即时改变**（新连接起）；端口与 Listener 本身不变。未启动的实例则在下次启动时按新绑定工作。

### 流程 5：实例清除绑定（`ClearBinding`）

与 SetBinding 对称：先把不含该绑定的配置推入核心，成功后删 DB 行；失败恢复并报错。清除后该实例的 Listener 与组从运行配置中消失（下一次重载起），实例恢复无代理启动。

### 流程 6：实例删除

`InstanceService.DeleteInstance` 只删实例记录（Profile 目录保留；`DeleteInstanceAndProfile` 才删 Profile）。绑定行由外键 `ON DELETE CASCADE` 随实例行一并删除，**但实例服务不通知代理服务**：运行中的核心配置不会因此立即重载，被删实例的 Listener 与组会残留在运行配置中，直到下一次任何来源/绑定操作触发 `regenerate` 或核心重启（见第 10 节）。端口因 DB 行已删可被新绑定重新分配，但 `allocatePort` 的实时探测会跳过仍被残留 Listener 占用的端口。

### 节点失效与消失时的实例行为

- 节点连接失败（节点仍在）：绑定与组选择不变，流量继续走该节点（失败由节点本身呈现），状态显示红色；
- 节点从来源消失：组的选择在核心内回落到首成员 **REJECT**（实测行为），流量被拒绝，不会改走其他节点；DeBrowser 在每次 `applySelections` 时对缺失节点显式再选 REJECT 并保持 DB 绑定原样；
- 节点恢复出现：下一次 `applySelections`（核心启动、手动刷新来源、重新导入、任一绑定/来源变更触发）时自动重新选回原节点，无需用户重选。核心自身的 provider 定时刷新（86400 秒）**不触发** DeBrowser 的重新选择（见第 10 节）。

---

## 7. Mihomo 配置生成逻辑

实现：`internal/mihomo/config.go` 的 `Generate(in ConfigInput)`，纯函数、确定性输出（来源按 provider 名、绑定按端口排序）。输入由 `buildConfigInput` 从 DB 快照组装，订阅 URL 在此处且仅此处从 Vault 读出。

### 7.1 生成的完整结构（脱敏示例）

```yaml
# DeBrowser 生成的 Mihomo 运行配置，请勿手动编辑。
# 订阅地址等敏感信息只出现在本文件中，不要分享或导出本文件。
log-level: warning
mode: rule
ipv6: false
external-controller: "127.0.0.1:19097"
secret: "<控制器随机密钥>"
profile:
  store-selected: false
dns:
  enable: true
  ipv6: false
  enhanced-mode: redir-host
  nameserver:
    - system
proxy-providers:
  src-xxxxxxxx:                      # URL 来源
    type: http
    url: "<订阅地址，仅存在于本文件>"
    interval: 86400
    proxy: DIRECT
    path: "./providers/src-xxxxxxxx.yaml"
    health-check:
      enable: true
      lazy: true
      url: "https://www.gstatic.com/generate_204"
      interval: 300
  src-yyyyyyyy:                      # 文件来源
    type: file
    path: "./sources/<来源ID>.yaml"
    health-check: { 同上 }
proxy-groups:
  - name: "delay-src-xxxxxxxx"       # 每来源一个隐藏测速组
    type: select
    hidden: true
    use: [src-xxxxxxxx]
  - name: "inst-<实例ID前12位>"       # 每绑定一个实例组
    type: select
    proxies: [REJECT]
    use: [src-xxxxxxxx]              # 只含该实例绑定来源
listeners:
  - name: "inst-<实例ID前12位>"
    type: mixed
    listen: "127.0.0.1"
    port: 21001
    proxy: "inst-<实例ID前12位>"
rules:
  - "MATCH,DIRECT"
```

### 7.2 字段来源与用途

| 字段 | 来源 | 用途 |
|---|---|---|
| `log-level: warning` | 固定 | 核心日志级别；核心日志写 `core.log` |
| `mode: rule` | 固定 | 核心运行模式（实例流量实际不经规则，见 7.3） |
| `ipv6: false` | 固定 | 关闭 IPv6 |
| `external-controller` / `secret` | 固定端口 19097 + Vault 外的本地密钥文件 | REST API 入口与 Bearer 认证，仅回环 |
| `profile.store-selected: false` | 固定 | 禁止核心持久化组选择；选择真源是 DeBrowser 的 DB |
| `dns` | 固定 | 核心 DNS：启用、redir-host 模式、上游为系统解析器。DeBrowser 只下发这组最小配置，解析行为全在核心 |
| `proxy-providers.*` | 每来源一条 | http：订阅地址（Vault 解出）、每日自动刷新、抓取固定直连、缓存路径；file：指向 DeBrowser 的副本文件 |
| `health-check` | 固定参数 | 见第 5 节 |
| `proxy-groups`（delay-*） | 每来源一条 | 测速专用隐藏组 |
| `proxy-groups`（inst-*） | 每绑定一条 | 实例专属选择组：`proxies: [REJECT]` + `use: [绑定来源]` |
| `listeners` | 每绑定一条 | mixed 监听（HTTP/SOCKS 同端口）、仅 127.0.0.1、`proxy` 指向本实例组 |
| `rules` | 固定一条 `MATCH,DIRECT` | 仅作用于核心自身经规则的流量（如未被 provider 固定的抓取）；实例流量不经此表 |

### 7.3 关键问答（按代码）

- **一个实例几个 Listener、几个组？** 恰好各一个。Listener 名与组名相同（`inst-<实例ID前12位十六进制>`）。
- **Listener 用 `proxy` 还是 `rule`？** 只用 `proxy`（直指本实例组）。没有生成任何 listener `rule`（sub-rules）。
- **实例流量是否经过顶层 rules？** 否。Listener `proxy` 直绑在核心语义上绕过规则引擎；顶层 `rules` 只有一条 `MATCH,DIRECT`，与实例流量无关。
- **是否支持每实例独立规则？** **未实现。** 每实例的全部路由决策就是其组的选择（绑定节点或 REJECT）。
- **节点绑定如何影响出站？** DB 绑定 → 生成组与 Listener → 核心启动/重载后 `applySelections` 用 `PUT /proxies/{组名}` 把组选择设为绑定节点；浏览器流量经 Listener 到组，按组当前选择出站。
- **更新配置是否需要重启核心？** 不需要。`PUT /configs?force=true` 热重载（`regenerateState`）；仅当核心未运行时，配置只写文件，待下次手动启动加载。
- 组选择的易失性：`store-selected: false` + 热重载会重置组选择，因此**每次**启动与重载后都必须 `applySelections`——代码中所有触发重载的路径（StartCore、regenerateState 内部）都串联了它。

---

## 8. 规则系统现状

逐项明确（按代码）：

| 问题 | 现状 |
|---|---|
| 是否读取订阅原始 rules | **未实现**（不读取；订阅只作 provider 载荷，核心只提取节点） |
| 是否读取 rule-providers | **未实现** |
| 是否支持订阅中的 proxy-groups | **未实现**（不合并；运行配置中的组全部由 DeBrowser 按来源/绑定生成） |
| 是否支持完整 Mihomo 配置导入并生效 | **未实现**（完整配置可被导入，但只有其 `proxies:` 节点被使用，其余内容被忽略，不报错也不生效） |
| 实例流量是否经过规则匹配 | **否**（Listener 直绑实例组，绕过规则引擎） |
| DIRECT 如何处理 | 仅出现在两处：provider 抓取固定 `proxy: DIRECT`；顶层兜底规则 `MATCH,DIRECT`（只作用于核心自身流量） |
| REJECT 如何处理 | 作为每实例组的首成员与安全回落目标：节点未选/消失/选择失败时流量被拒绝；它是运行时安全目标，**从不写入 DB 冒充绑定** |
| MATCH 如何处理 | 只有顶层一条 `MATCH,DIRECT` |
| 规则策略页面是否有真实执行能力 | **没有规则页面**。代理页顶部曾有过的规则集 UI 已在开发期删除；当前 UI 无任何规则开关，代码中无 DeBrowser 规则集、规则启停或规则编辑逻辑 |
| 核心的 `/rules/disable` 等 API | 不使用（客户端 `client.go` 未实现这些端点） |

一句话：**DeBrowser 当前没有规则系统**；路由 = 每实例组的选择（绑定节点 / REJECT），这是全部。

---

## 9. 日志与错误处理

### 9.1 记录机制

- 统一自动记录（`internal/logging`）：operation / error / runtime 三类 JSONL，位于 `data/logs/`；每个用户操作一个 operation_id 贯穿。代理模块经 `beginOp/finishOp`（`internal/service/audit.go`）记录以下操作的结果（成功/失败与错误文本）：`proxy.core_start`、`proxy.core_stop`、`proxy.source_add_url`、`proxy.source_add_file`、`proxy.source_refresh`、`proxy.source_reimport`、`proxy.source_delete`、`proxy.delay_test`、`proxy.binding_set`、`proxy.binding_clear`。实例启动被代理门禁拒绝时，错误进入该实例的启动操作记录。
- 核心自身日志：`data/mihomo/core.log`（核心 stdout/stderr，warning 级别）。这是核心内部事件（provider 加载细节、Listener 冲突、连接错误）的**唯一**出处，不进入 JSONL。
- 脱敏：操作记录只带动作名与目标 ID；订阅 URL、控制器密钥从不进入任何记录（代码检索确认）；错误消息由服务层构造，不拼接 URL/凭据。数据管理页的日志导出只含三类 JSONL 并经敏感扫描与路径脱敏；`config.yaml`、Vault 内容、核心目录**不在任何导出范围内**。

### 9.2 可诊断性矩阵（当前代码）

| 问题 | 能否从 DeBrowser 侧判断 | 依据 |
|---|---|---|
| Provider 加载失败 | **部分** | 添加/刷新操作记录报错；来源状态显示 error/empty 与提示；细节（核心为何拒绝）只在 `core.log` |
| 节点测速失败 | **部分** | 测速操作本身有记录；逐节点失败只是返回给 UI 的数据（-1），不逐条记录，事后无法从日志还原哪个节点失败 |
| DNS 解析失败 | **否** | 核心内部行为，DeBrowser 无观测点；至多见于 `core.log`（warning 级别下未必记录） |
| 节点连接失败 | **部分** | 绑定状态派生（down）与健康历史可在 UI 实时看到，但不落入日志；连接级错误只在 `core.log` |
| Listener 创建失败 | **部分** | 核心只在 `core.log` 记录（如端口冲突时核心不退出、仅丢弃该 Listener）；DeBrowser 通过 `ProbePort` 间接发现：绑定列表显示 Listener 状态 down、实例启动被拒绝并记录 |
| Mihomo 配置更新失败 | **能** | `regenerateState` 的重载失败/回滚都是操作的同步返回值，进入操作记录并直接提示用户（含恢复失败的双错误） |

---

## 10. 当前已知问题和技术限制

### 10.1 已实现但未完成 Windows 11 实机验证

- Job Object 在 DeBrowser 被强制结束/崩溃时带走核心的实际行为；
- STARTUPINFOEX 句柄列表与 DuplicateHandle 副本在实机上的继承范围与核心日志写入；
- 真实订阅、真实节点出口 IP、真实浏览器经 Listener 的联网行为；
- 与外部 Clash 共存、外部 Clash 开 TUN 时对 DeBrowser 核心出站的影响（IP 层可能被 TUN 接管，只能实机判定）；
- 首次下载核心的网络与防火墙提示、端口冲突时的用户体验。

以上均为「待验收」，自动化与 Linux 侧验证不能替代。

### 10.2 实机已报告现象与代码层面的对应（根因均标注状态）

1. **同一订阅在 Clash Verge 全部节点可用，DeBrowser 约 1/3 节点测速超时** — 根因**待验证**。代码可确认的我方参数：测速超时固定 **5000 ms**；测速 URL 为 `https://www.gstatic.com/generate_204`；每来源一次 group delay 调用、组内节点由核心并发测试；DNS 为 redir-host + 系统解析器。这些参数与常见 Clash 客户端的默认值并不完全相同（超时、DNS 模式、并发方式都可能影响慢节点的测速结果），但仅凭代码不能断定是哪一项造成差异，需实机对照实验（同一节点分别用不同 timeout/URL 复测）才能定位。
2. **曾出现 Listener 本地端口无响应** — 代码可确认的机制：核心在 Listener 端口冲突时**不退出**，只在 `core.log` 记录并丢弃该 Listener，进程存活与控制器应答都正常，因此「核心运行中」与「某 Listener 已死」可以并存。DeBrowser 的发现手段是探测：绑定列表的 Listener 状态、实例启动门禁都会 `ProbePort` 并在无响应时明确提示/拒绝。至于该次实机事件是否即此机制（或端口被其他程序占用、重载瞬间窗口），**待验证**（需当时的 `core.log`）。
3. **曾出现配置更新失败** — 代码路径明确：热重载被核心拒绝时 `regenerateState` 恢复旧文件并返回错误，操作不假报成功。何种配置内容会触发核心拒绝（生成配置为程序渲染，顶层字段固定），**待验证**（需当时的错误文本与 `core.log`）。

### 10.3 代码可确认的限制与缺陷

- **删除实例不立即清理运行配置**：绑定行被外键级联删除，但运行核心中的组与 Listener 残留到下一次配置重载或核心重启（第 6 节流程 6）。影响有界：无浏览器再使用该端口；端口再分配有实时探测兜底。
- **核心未运行时 SetBinding 不校验节点存在性**：离线绑定按用户所选存档；若节点名已不存在，核心启动后该实例进入 REJECT + 黄色「已不存在」状态，需用户改绑或等节点回归。
- **核心定时刷新后的选择恢复缺口**：http provider 每 86400 秒由核心自行刷新；若绑定节点在该次刷新中消失又回归，组选择停留在 REJECT，直到下一次 DeBrowser 侧触发 `applySelections`（启动核心、手动刷新、重新导入、任一来源/绑定变更）才恢复。手动刷新订阅无此问题（刷新后必重下达选择）。
- **applySelections 的读失败即拒绝**：某绑定的 provider 暂时读失败时，该实例被显式置 REJECT（fail-closed 设计），瞬时 API 故障会让实例暂时断流，直到下一次重下达；不会误走其他节点。
- **测速结果不可分辨失败原因**：超时、连接失败、DNS 失败在测速结果中同为 -1，无细分。
- **健康状态依赖检查历史**：`lazy: true` 下未被使用过的节点长期为「未检查」；绑定状态的颜色只有在节点被测速或被实际使用后才有意义。
- **GetCoreStatus 不检查 Listener 健康**：核心状态显示「运行中」不代表每个 Listener 都活着；Listener 健康只在绑定列表与启动门禁处探测。
- **来源级状态粒度**：`ListNodes` 对读取失败的来源静默跳过（该来源自身视图报 error），节点列表页本身不汇总报错。
- **验证核心端口固定**（19098）：被占用时文件导入验证直接报错，无备用端口机制。

### 10.4 未实现的功能（明确清单）

- 规则系统（规则集、规则启停、规则编辑、订阅规则/rule-providers 导入）：未实现；
- TUN、系统代理设置：未实现（且不在产品范围内）；
- 实际连接测试（经 Listener 的真实请求测试）：未实现；
- 节点手动添加/编辑、节点参数查看（服务器/密码等）：未实现（节点参数只在核心内）；
- 自动测速/定时测速：未实现；
- 核心自动启动/状态恢复/自动重启（watchdog）：未实现（产品规则禁止）；
- 流量统计、连接列表、核心日志查看页：未实现；
- 多核心/每实例独立核心：未实现（单核心设计）。

### 10.5 测试覆盖不到的地方

- Windows 子进程路径（Job Object、句柄继承）只有交叉编译与代码审查，无实机行为测试；
- 真实订阅形态（各机场的 YAML 变体、嵌套结构）只有样本验证，无法穷举；
- 核心与真实公网节点之间的协议兼容性（各协议/传输组合）依赖核心自身，DeBrowser 侧测试使用本地假节点，只证明路由与选择，不证明真实出口；
- 长时间运行（provider 定时刷新、健康检查累积、端口与句柄资源）无长稳测试。

---

## 11. 当前功能实现状态表

| 功能 | 状态 | 备注 |
|---|---|---|
| 核心下载与双哈希校验 | 已实现 | `EnsureBinary` |
| 核心手动启动/停止 | 已实现 | 唯一启动路径为代理页按钮 |
| 异常退出带走核心（Job Object） | 已实现 | **未实机验证** |
| 端口冲突不接管外部核心 | 已实现 | 控制器 19097 探测 + 明确报错 |
| URL 订阅添加/更新/删除 | 已实现 | Vault 存地址；失败全量回滚 |
| 本地 YAML 导入/重新导入/删除 | 已实现 | 一次性核心真验证；刷新失败逐字节恢复 |
| 节点列表（名称/类型/延迟/状态） | 已实现 | 全部读核心 API，无本地副本 |
| 节点测速（手动、逐来源） | 已实现 | group delay，5 秒超时 |
| Provider 健康检查 | 已实现（核心内建） | lazy + 300 秒间隔配置 |
| 实例绑定/换绑/解绑 | 已实现 | 先配置后落库的一致性顺序 |
| 每实例独立 Listener 与组 | 已实现 | 21001–21999 自动分配 |
| 浏览器启动代理门禁 | 已实现 | 核心未开/Listen 无响应均拒绝启动 |
| 节点消失防回退（REJECT） | 已实现 | 实测 + 集成测试覆盖 |
| 节点恢复自动续绑 | 已实现 | 限 DeBrowser 触发的重下达时机（见 10.3） |
| 节点状态胶囊（四态） | 已实现 | 三处 UI 共用组件 |
| 代理设置弹窗 | 已实现 | 只读信息，无可调参数 |
| 规则系统 | **未实现** | 见第 8 节 |
| TUN / 系统代理 | **未实现** | 不在产品范围 |
| 实际连接测试 | **未实现** | — |
| 自动测速 | **未实现** | — |
| 核心状态恢复（重启后自动运行） | **未实现** | 产品规则：重启后默认关闭 |

---

## 12. 关键代码文件索引

| 文件 | 内容 |
|---|---|
| `internal/mihomo/config.go` | 配置生成 `Generate`、组名/测速组名派生、YAML 标量转义 |
| `internal/mihomo/client.go` | 核心 REST 客户端（Version / ProviderProxies / RefreshProvider / GroupDelay / SelectNode / ReloadConfig） |
| `internal/mihomo/manager.go` | 核心进程管理（EnsureBinary 双哈希、Start/Stop/abortStart、opMu 串行化）、版本与资产锁定 |
| `internal/mihomo/child.go`、`child_unix.go`、`child_windows.go` | 子进程抽象与平台实现（Windows：Job Object + 挂起创建 + 句柄列表 + DuplicateHandle 副本；`StartProcess` 供一次性验证核心复用） |
| `internal/service/proxy_service.go` | 代理业务服务全部逻辑（来源、节点、测速、绑定、配置生成协调、启动门禁 `ProxyPortForInstance`） |
| `internal/store/proxy_sources.go` | 来源与绑定的 SQLite 仓储 |
| `internal/model/model.go` | `ProxySource`、`InstanceProxyBinding` 数据模型 |
| `internal/database/migrations/0015_proxy_management.sql` | 两张业务表定义（schema v15） |
| `internal/secrets/` | 凭据 Vault（Windows DPAPI） |
| `internal/service/instance_runtime.go` | 浏览器启动参数构建（`--proxy-server` 注入点） |
| `internal/service/instance_service.go` | `ProxyPortFor` 注入字段定义 |
| `main.go` | 服务组装与 `ProxyPortFor` 接线（第 196–198 行附近） |
| `frontend/src/pages/ProxyConfigPage.tsx` | 代理配置页（核心控制、来源、节点、绑定总览、设置弹窗） |
| `frontend/src/pages/InstancesPage.tsx` | 实例列表代理胶囊与表单节点选择 |
| `frontend/src/components/NodeCapsule.tsx` | 节点状态胶囊共享组件 |
| `internal/service/proxy_core_integration_test.go` | 真实核心集成测试（路由、防回退、健康状态、回滚、生命周期） |
| `internal/mihomo/manager_test.go`、`config_test.go` | 进程管理与配置生成测试 |
