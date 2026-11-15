# DeBrowser 数据库

## 引擎

- SQLite，通过纯 Go 驱动 `modernc.org/sqlite` 访问（不需要 CGO，使 Windows x64 构建保持简单）。
- 数据库文件是便携数据目录内的 `debrowser.db`（`<应用根目录>\data\debrowser.db`；根目录跟随可执行文件——见 `internal/paths`）。文件缺失时在正常启动中创建（首次启动），之后按原样打开；永不从 `%AppData%` 或任何其他外部位置自动导入数据（见 ARCHITECTURE.md）。
- 路径总是来自 `internal/paths`；该包之外的任何代码都不构造存储位置，也没有 AppData 回退。

## 打开时 pragma

在 `internal/database/database.go` 中为每个连接设置：

| Pragma | 值 | 原因 |
| --- | --- | --- |
| `journal_mode` | `WAL` | 读者不阻塞写者；崩溃安全的日志。 |
| `synchronous` | `FULL` | 在 WAL 下，`NORMAL` 能扛住应用崩溃，但扛不住操作系统崩溃或断电。DeBrowser 是本地优先、写入率低的元数据存储，因此 FULL 的小额提交成本换来每个已提交事务的持久性——本项目把数据完整性置于边际写入延迟之上。 |
| `foreign_keys` | `1` | SQLite 默认禁用外键执行；本 schema 的引用完整性依赖它。 |
| `busy_timeout` | `5000` | 短暂的锁竞争（例如一次备份快照）会等待，而不是以 `SQLITE_BUSY` 失败。 |

连接池被限制为**一个**打开的连接：SQLite 只允许单个写者，单连接保证逐连接的 pragma 适用于每条语句。并发通过连接池串行化加 `busy_timeout` 处理；repository 对并发使用是安全的。

## 分层

- `internal/model`——纯数据类型（`Instance`、`Bookmark`，以及历史的 `Proxy` / `Subscription` / `MihomoConfig` 数据模型），无行为、无依赖。
- `internal/database`——打开、pragma、迁移、备份、`WithTx` 事务入口、健康检查。
- `internal/store`——仓储层。核心模型的全部 SQL 都在这里；每个 repository 针对一个小的 `querier` 接口工作，使同一份代码既能在连接池上运行，也能在事务内运行。`Store.WithTx` 原子地运行多个 repository 调用（嵌套事务被拒绝）。无 ORM。对于历史代理/订阅/配置表，store 有意**不**暴露任何 create/update/delete 或 list API——只有 Vault 引用查询 `SecretRefInUse` / `AllSecretRefs` 仍读取它们（见下方 schema 注记）。

## Schema（当前状态）

当前状态（migration 0001–0012，全部在启动时应用）：`app_settings`、`proxies`、`subscriptions`、`browser_instances`、`bookmarks`、`mihomo_configs`、`mihomo_proxy_groups` 与 `mihomo_proxy_group_members` 等表**存在**。在 PR #9 范围清理之后，代理/订阅/导入相关的表**只是历史数据结构——不是当前产品功能**。没有任何页面、服务或绑定为这些历史表暴露业务 CRUD 或列表操作。唯一剩余的读取是 Vault 引用保护查询 `SecretRefInUse` / `AllSecretRefs`，它们扫描这些表的 `secret_ref` 列，使历史凭据在仍被引用时永不被垃圾回收。这些表及其全部行出于兼容性保留（既有数据库保持数据与外键完好）。Stage 9A 增加了 `kernels` 表（migration 0010 创建的**历史表**，原为 DeBrowser 托管的 Chromium 构建；独立内核下载管理功能已取消，该表当前不被任何产品功能使用，见下方 `kernels` 一节）与 `external_browsers` 表（migration 0011/0012，用户手动添加程序的纯路径登记）；`browser_instances.kernel_id` 当前从未被写入，实例目前只携带内核身份的记录列（见下方 `browser_instances`）。本节未列出的任何东西都不存在于数据库中。

ID 是应用生成的 UUID（TEXT）。时间戳是 UTC 文本（`datetime('now')` 默认值；repository 在更新时刷新 `updated_at`）。

### `proxies`（历史）

由已移除的 Stage 4A–6 功能写入的代理节点。该表及其行仅作为历史数据保留；当前没有任何代码创建、列出或修改它们。`protocol` 约束为 `http, https, socks5, vmess, vless, trojan, shadowsocks, hysteria2, tuic, wireguard, direct`；`port` 在存在时必须为 0–65535；`enabled` 为 0/1。

**凭据政策**：口令、密钥与 token 永不存入此表（或日志）。非密钥的连接参数可以位于 `config_json`；`secret_ref` 是指向凭据 Vault 的不透明引用（`internal/secrets`，Stage 4A）。

Stage 4A（migration 0004）增加：`sort_order`、`source`（`manual` / `file_import` / `subscription`——由 migration 0007 改名为 `custom` / `file` / `subscription`，见下）、`subscription_id`（外键 → `subscriptions`，`ON DELETE CASCADE`）、`dedup_key`（导入指纹；部分 UNIQUE 索引 `WHERE dedup_key <> ''`，使空键永不冲突），以及连接测试记录 `last_latency_ms` / `last_test_ok` / `last_tested_at`。（Migration 0004 还给此表和 `subscriptions` 增加了 `group_id` 列，以及 `proxy_groups` / `proxy_tags` 表；Stage 4A 更正把它们全部再次移除——见下方 migration 0005。）

Stage 6（migration 0006）增加：`config_id`（外键 → `mihomo_configs`，`ON DELETE SET NULL`）——节点来源的导入配置——与 `source_node_name`（节点在其来源文档中的名称；原生 proxy-group 成员以此名称寻址节点，因此它与显示用的 `name` 分开保留）。导入的节点带有非空的 `dedup_key`，形如 `<scope>:<owner-id>:<sha256>`，指纹覆盖所有者 ID + 名称 + 协议 + 服务器 + 端口（永不含密钥）。这些列由 Stage 6 的导入/刷新管线写入与匹配，该管线已被移除；今天它们只是按原样保留的历史存储值。

### `subscriptions`（历史）

由已移除的 Stage 6 订阅功能写入的订阅元数据。该表及其行仅作为历史数据保留；当前产品中没有订阅刷新管线、抓取器或界面。**完整的订阅 URL 从未存入此表**——它可能携带凭据，只存在于 Vault 中（`secret_ref`）；此表只保留 `url_hint`（scheme+host）、当时记录的抓取策略值（`allow_lan` 默认 0、`auto_update_minutes` 默认 0 = 关闭）与一份脱敏的刷新记录（`last_updated_at`、`last_status`、`last_error`——只有状态类别，永不含 URL）。

### `mihomo_configs`（Stage 6，migration 0006——历史）

由已移除的 Stage 6 导入功能导入的每个 Mihomo 配置文档一行。该表及其行仅作为历史数据保留；当前没有任何代码导入、解析或读取这些配置（Vault 引用查询仍扫描它的 `secret_ref` 列）。`source_type` 为 `file`（本地导入；`subscription_id` 为 NULL）或 `subscription`（由订阅拥有；部分 UNIQUE 索引保证每个订阅至多一份当前配置）。脱敏后的文档本身曾作为文件存放在 `data/mihomo/configs/<uuid>.yaml`；此表只保存相对的 `file_path`（相对 `data/mihomo/`，永不为绝对路径）、它的 `sha256`，以及 `secret_ref`——脱敏时提取的配置级凭据包的不透明 Vault 引用（文档不含凭据时为空）。`security_report_json` 是 Stage 6 的发现数组（`allow_lan`、`external_bind`、`external_controller`、`controller_no_secret`、`tun_enabled`、`dns_external_listen`）；它只记录风险。`format_version` 是工件格式的版本（当前为 1）。

### `mihomo_proxy_groups`（Stage 6，migration 0006——历史）

一份曾经导入的配置的**原生 Mihomo `proxy-groups`**，按结构保留（名称、类型、非密钥的 `options_json`、文档顺序在 `sort_order` 中）。这些是配置作者的分组——不是 DeBrowser 的分组功能（后者已在 migration 0005 移除）。这些行仅作为历史数据保留；当前没有任何代码读取或显示 proxy groups。`(config_id, name)` 是 UNIQUE。删除配置会级联到它的分组（schema 行为；当前没有任何代码删除配置）。

### `mihomo_proxy_group_members`（Stage 6，migration 0006——历史）

每个分组成员一行，按文档顺序（`PRIMARY KEY (group_id, sort_order)`）。`member_kind` 为 `proxy`、`group` 或 `builtin`，CHECK 约束强制恰好只有匹配的目标列被设置：`proxy` 用 `proxy_id`（**同一**配置的一个节点——解析从不跨配置边界）、`group` 用 `target_group_id`、`builtin` 两者都不用（DIRECT、REJECT、GLOBAL……）。分组引用在导入时由已移除的导入管线验证为无环；这些行仅作为历史数据保留。

### `browser_instances`

每个浏览器实例一行。

| 列 | 说明 |
| --- | --- |
| `name` | UNIQUE——实例在界面中以名称标识。 |
| `kernel_id`、`kernel_name`、`kernel_version` | 作为数据记录的内核身份。`kernels` 表是 migration 0010 的历史结构；当前 Service 从不写入 `kernel_id`，实例只携带 `kernel_name`/`kernel_version` 字符串。 |
| `user_data_dir` | UNIQUE。相对便携数据目录的 Profile 路径（例如 `profiles\<id>`），使整个应用文件夹可整体移动。它是实例元数据中记录的**路径规划**：创建实例时该目录目前并不会被实际创建，浏览器运行目录在 Stage 9B 随实例启动建立。Profile 内容是浏览器数据，永不存入 SQLite。 |
| `proxy_id` | **历史兼容字段。** 外键 → `proxies(id)`，`ON DELETE SET NULL`（schema 行为：删除一行代理会使实例脱钩，而不是破坏实例）。当前产品中没有代理绑定界面或操作：实例创建总是写入 NULL，实例更新逐字保留某行已有的历史值。 |
| `proxy_listen_port` | **历史兼容字段。** Stage 4A（migration 0004）增加，原为实例的 Mihomo 监听端口，本应由已取消的 Stage 7 从 `app_settings` 的 `proxy.port_range` 范围（默认 `19000-19499`）分配。它不再被任何当前业务逻辑分配或使用；已存值按原样保留。通过唯一索引实现唯一（SQLite 不能用 ALTER 增加 UNIQUE 列）；NULL 端口永不冲突。 |
| `status` | `stopped` / `running` / `error`（CHECK）。持久化的生命周期记录。目前没有运行时进程管理器：该状态由 Stage 3 的启动对账维护（见下方 Stage 3 注记），真实的运行时状态管理属于 Stage 9B。由于启动功能尚不存在，现有实例的状态实际恒为 `stopped`。 |
| `start_url`、`sort_order` | 可选的起始页；显示顺序。 |

索引：`proxy_id`、`status`。

### `bookmarks`

`instance_id` 外键 → `browser_instances(id)`，`ON DELETE CASCADE`：删除实例会删除它的书签。`instance_id` 为 NULL 表示适用于每个实例的**全局**书签。唯一性是每个作用域一个 URL，由 `(IFNULL(instance_id, ''), url)` 上的唯一索引强制——普通的 `UNIQUE(instance_id, url)` 不行，因为 SQLite 把 NULL 视为彼此不同。`(instance_id, sort_order)` 上有索引，供有序列出。

注：书签目前只有数据层（本表与 store CRUD）；没有 Service、没有 Wails 绑定，前端是占位页面。书签前端接入是 Stage 9C 的工作。

### `kernels`（历史结构，migration 0010）

migration 0010 创建的历史表结构仍然存在：`id`、`version`、`channel`（默认 `stable`）、`platform`（默认 `win64`）、`install_path` 与 `executable_path`（相对便携应用根目录存储）、`status`（`ready` / `verification_failed` / `missing`，由 CHECK 强制）、时间戳，`(version, platform)` 上有唯一索引。它原为 DeBrowser 托管的独立 Chromium 构建而设。**当前产品不再提供独立内核的下载、安装、验证或删除功能**（该功能已按产品决定取消，PR #11）：当前服务不再通过 store 提供 kernels 的增删改查（对应的 store 代码已删除），界面上也没有任何内核管理入口。本表仅作为历史数据库结构保留，使既有数据库照常升级；已发布 migration 不删除、不改写。用户手动登记的程序**永不**写入此表：手动登记位于 `external_browsers`（见下）。

### `external_browsers`（Stage 9A，migration 0011 + 0012）

用户**手动**添加的程序的**纯路径登记**：`id`、`name`（从所选文件名派生的显示名）、`executable_path`（用户所选的**绝对**路径——该程序位于 DeBrowser 的文件夹之外，因此不存在便携相对形式）、时间戳。`executable_path` 上的唯一索引使每个路径只有一份登记；重复添加同一路径会返回既有登记。DeBrowser 对用户所选文件不做任何身份、版本、签名、发布者或存在性检查，也不要求路径对应的文件实际存在：migration 0011 最初为手动添加的 Chrome/Brave 存储验证结果，migration 0012 在去掉验证列的情况下重建了该表（既有登记作为路径记录保留）。移除登记只删除这一行——永不删除程序本身。store 暴露 `CreateExternalBrowser`/`GetExternalBrowser`/`GetExternalBrowserByPath`/`ListExternalBrowsers`/`UpdateExternalBrowserPath`/`DeleteExternalBrowser`。

### `app_settings`

应用级键值设置（Stage 1，migration 0001）。一个历史键：`proxy.port_range`（例如 `19000-19499`）——已取消的 Stage 7 本应消费的实例监听范围。当前没有任何代码读写此表（它的 store 访问器已在 PR #9 清理中移除）；提及该键只是为了让既有数据库中存储的值被理解为历史数据。面向用户的 `config/settings.json` 文件（语言/主题，以及统一记录系统的日志总容量 `logCapacityBytes`——缺失或为 0 表示未配置、采用默认 100 MB）是独立的、无关的。日志记录本身不进数据库：三类记录以 JSON Lines 文件存放在 `data/logs/`（见 `docs/ARCHITECTURE.md`）。

## 规则（AGENTS.md 第 5 节）

- WAL 模式、外键约束、迁移、事务性写入、适当的索引、备份/恢复支持。
- **永不**把浏览器 Cookie 或浏览器 Profile 内容存入 SQLite。Profile 以文件形式位于 `profiles\<instance-id>\` 下（目录在 Stage 9B 随实例启动实际创建；当前只是路径规划）。
- 永不提交用户数据库（`*.db`、`*.db-wal`、`*.db-shm` 被 git 忽略）、凭据、订阅 URL 或私有代理配置。

## 迁移

- SQL 文件位于 `internal/database/migrations/`，命名为 `NNNN_description.sql`，版本号单调递增。
- 迁移被嵌入二进制，在启动时按版本顺序应用。每个迁移在自己的事务中运行，并与它在 `schema_migrations (version, name, applied_at)` 中的行同事务，因此失败不会留下半应用的 schema，重启永不重复应用已记录的迁移。
- 已应用版本可通过 `DB.AppliedVersion` 查询。
- 迁移只追加：一旦合并，迁移文件永不编辑；更正以新迁移的形式交付。

### 已应用的迁移

| 版本 | 文件 | 内容 |
| --- | --- | --- |
| 1 | `0001_foundation.sql` | `app_settings` |
| 2 | `0002_proxies_and_instances.sql` | `proxies`、`browser_instances`（+ 索引） |
| 3 | `0003_bookmarks.sql` | `bookmarks`（+ 作用域 URL 唯一索引） |
| 4 | `0004_proxy_foundation.sql` | `proxy_groups`、`proxy_tags`、`subscriptions`；`proxies` 的分组/来源/去重/测试列；`browser_instances.proxy_listen_port` |
| 5 | `0005_remove_proxy_groups_tags.sql` | Stage 4A 更正：移除 DeBrowser 自有的代理分组/标签——重建 `proxies` 与 `subscriptions`（去掉 `group_id`），删除 `proxy_tags` 与 `proxy_groups`（见下） |
| 6 | `0006_mihomo_config_import.sql` | Stage 6：`mihomo_configs`、`mihomo_proxy_groups`、`mihomo_proxy_group_members`；`proxies.config_id` / `proxies.source_node_name`（见下） |
| 7 | `0007_proxy_source_vocabulary.sql` | Stage 6C：以统一来源词汇 `custom` / `file` / `subscription` 重建 `proxies`（值映射 `manual` → `custom`、`file_import` → `file`）；链接抢救保留实例绑定与分组成员（见下） |
| 8 | `0008_proxy_group_custom_members.sql` | Stage 6C 评审修复：原生 proxy-groups 的用户自定义成员 overlay（同日被取代——见 0009） |
| 9 | `0009_drop_custom_members_overlay.sql` | Stage 6C 重做：删除 0008 的 overlay 表；自定义成员功能在发布前被撤回（见下） |
| 10 | `0010_kernels.sql` | Stage 9A：`kernels`——原为 DeBrowser 托管的 Chromium 构建（便携相对路径、status CHECK、`(version, platform)` 唯一）；独立内核功能已取消，本表作为历史兼容结构保留 |
| 11 | `0011_external_browsers.sql` | Stage 9A（当时的设计，已被 0012 取代）：`external_browsers`——用户在完整验证后手动登记的浏览器（绝对可执行路径、按路径唯一、`browser_type` CHECK chrome/brave、status `verified`/`invalid`）；DeBrowser 不做任何自动浏览器检测 |
| 12 | `0012_external_browsers_path_only.sql` | Stage 9A（产品调整）：手动登记变为纯路径登记——重建 `external_browsers`，去掉验证列/CHECK（既有登记的 id、名称、路径与时间戳保留）；不再对用户所选可执行文件做任何身份或签名检查 |

### Migration 0005：移除分组与标签（Stage 4A 更正）

产品决定（2026-11-13）：DeBrowser 不提供自有的代理分组/标签。Migration 0005 移除 migration 0004 为此增加的结构。这与 Mihomo 原生的 `proxy-groups` **无关**；后者是导入的机场配置的一部分，随 Stage 6 订阅/配置导入到达（其本身已在 PR #9 清理中移除；Stage 6 的 schema 与数据作为历史结构保留）。

由于两个 `group_id` 列都带有指向 `proxy_groups` 的外键（且 `proxies.group_id` 带索引），0005 使用表重建而不是 `DROP COLUMN`。重建在迁移运行器的逐迁移事务中以 `foreign_keys=1` 工作：在这个 SQLite 构建上，外键动作（`ON DELETE SET NULL` / `CASCADE`）在父表被删除时立即触发，因此迁移先把进入的链接（`browser_instances.proxy_id`，然后 `proxies.subscription_id`）抢救到临时表并置空，把重建后的表以同名换入，再恢复链接（恢复本身也经过外键检查）。`proxies.sort_order` 被保留——它只是普通的列表排序，不是分组功能。v4 → v5 升级测试（`internal/database/stage5_test.go`）在填充好的 v4 夹具上验证数据保留、`PRAGMA foreign_key_check` 与 `PRAGMA integrity_check`。

### Migration 0006：Mihomo 配置导入（Stage 6）

纯增量：三张新表加两个带兼容默认值的 `proxies` 新列（`config_id` NULL、`source_node_name` `''`），因此既有的 Stage 4A/5A/5B 行、值与关系不受影响。v5 → v6 升级测试（`internal/database/stage6_test.go`）在填充好的 v5 夹具上验证数据保留、成员 CHECK 约束、`PRAGMA foreign_key_check` 与 `PRAGMA integrity_check`。Vault 引用检查（`SecretRefInUse`、`AllSecretRefs`）同样覆盖 `mihomo_configs.secret_ref`，使配置凭据包与代理、订阅引用一样参与受守卫的清理与垃圾回收。（写入这些表的 Stage 6 导入功能已在 PR #9 清理中移除；迁移、schema 与数据作为历史结构保留，上述 Vault 引用检查仍是现行代码。）

### Migration 0007：统一代理来源词汇（Stage 6C）

Migration 0007 是为 Stage 6C 统一代理模块编写的（该模块此后已在 PR #9 范围清理中移除——迁移及其产生的全部数据保留在原地）。它把 Stage 4A 的 `proxies.source` 值改名为该词汇（`manual` → `custom`、`file_import` → `file`、`subscription` 不变），并相应修改该列的 CHECK/默认值，使这些值与 `mihomo_configs.source_type` 及界面一致。修改 CHECK 约束需要表重建；重建在运行器事务内复用 migration 0005 的链接抢救模式（`foreign_keys=1` 时外键动作立即触发）：`browser_instances.proxy_id` 链接被复制到一旁再恢复，proxy 类型的 `mihomo_proxy_group_members` 行（其 CHECK 禁止将其脱钩）被完整复制到一旁、删除，再对重建后的表重新插入，分组、顺序与名称完好。没有任何行、ID、`secret_ref`、配置/订阅链接或成员关系发生变化。v6 → v7 升级测试（`internal/database/stage7_test.go`）在填充好的 v6 夹具上验证映射与保留（手动节点绑定到实例、带分组与成员的订阅 + 文件配置），外加 `PRAGMA foreign_key_check` / `integrity_check`、对退役词汇的拒绝，以及部分唯一去重索引。

### Migration 0008：自定义 proxy-group 成员 overlay（Stage 6C 评审修复）

> 已被 migration 0009 取代（Stage 6C 重做，2026-11-14）：自定义节点 / 自定义成员功能在 PR #9 合并前按产品决定撤回。0008 文件在迁移历史中原样保留——PR #9 验收构建的数据库已经应用过它——0009 删除该表。该 overlay 从未拥有用户数据（它只引用配置与自定义节点），因此删除不丢失任何东西；既有的 `source='custom'` 节点记录及其 Vault 凭据不受影响。

`mihomo_proxy_group_custom_members` 曾存储**用户添加的**成员关系：一个 `source = 'custom'` 的节点被挂到一份导入配置的原生 proxy-group 上。它与导入的成员（`mihomo_proxy_group_members`，镜像来源文档）严格分开：

- 主键 `(config_id, group_name, proxy_id)`；分组按**名称**寻址，因为一次订阅刷新会删除并重建分组行本身——名称是能在刷新后存活的身份。
- `config_id` 与 `proxy_id` 都是 `ON DELETE CASCADE` 外键：删除一份配置或一个自定义节点只移除它的 overlay 行；任何级联都不会进入节点、配置或导入成员。
- 「该节点确实是 `source = 'custom'`」以及「`(config_id, group_name)` 确实命名了该配置拥有的一个分组」由服务层在写事务内验证（CHECK 无法表达跨表条件）。
- 刷新/重新导入只重建导入的结构，从不触碰此表：刷新后，挂接会重新出现在同名分组中；若该分组在上游消失，该行被保留并报告为孤儿，分组回归时重新生效。

以上全部只是迁移历史：migration 0009 已删除此表，所述机制在当前代码中均不存在。

### 范围清理注记（PR #9，2026-11-15）

PR #9 范围清理只在代码层移除了代理/订阅/Mihomo 产品功能。**没有增加、删除、重命名或编辑任何迁移**：migration 0001–0009 逐字节未改，没有删除任何表或列，没有删除任何行。既有数据库——包括存有代理、订阅、导入配置与 Vault 引用数据的数据库——照常升级与启动；每个阶段（0004–0009）的填充夹具升级测试继续原样运行。

PR #9 的最终评审轮进一步收敛了数据层，仍不触碰已存数据：store 的代理/订阅/配置 repository 失去了全部 create/update/delete 方法，现在只在这些表上暴露 Vault 引用查询（`SecretRefInUse`、`AllSecretRefs`）；实例 create/update 不再能设置或改变 `proxy_id`（创建写入 NULL，更新逐字保留已存值——由 `TestInstanceUpdatePreservesHistoricalProxyID` 固定），实例界面不带代理列或选择器。

## 迁移前备份

Schema 变更是本地数据库风险最高的时刻，因此升级先保护数据：

- 当存在待执行的迁移**且**数据库已有已应用的迁移时，启动会在第一个待执行迁移运行之前，先把备份写入 `data\backups\`：`debrowser-pre-migration-<UTC>-from-v<current>-to-v<target>.db`。
- 备份使用 **`VACUUM INTO`**，它产生事务一致的快照，包括仍在 WAL 中的数据。从不直接复制 `.db` 文件——那会静默丢失未 checkpoint 的事务。
- 快照在迁移继续之前经 `PRAGMA integrity_check` 验证。**若备份失败，迁移被中止**并给出明确错误；既有数据库不受影响。
- 全新数据库（尚无任何已应用迁移）跳过备份——没有需要保护的东西。
- 这套经验证的快照机制是迁移流程内部的底层安全设施，没有公开的手动备份 API 暴露给任何功能层。

必须区分：本节的迁移前保护快照只是数据层的底层安全设施，**不是**面向用户的备份与恢复功能。完整的备份导出、导入与恢复用户功能属于 Stage 12（见下方），尚未开放。曾经在数据库打开之前运行的外部 Stage 1 Roaming 数据迁移已在 Stage 5A 移除（不存在任何自动旧数据迁移）；本数据库内 schema 迁移机制不受影响，仍是长期的兼容路径。

完整的备份/恢复用户体验在 Stage 12 交付。

## Stage 3 注记

Stage 3（应用生命周期与实例管理基础）**不增加任何 schema 迁移**：它完全在既有表上工作。有一项行为增量位于 repository 层而非 schema——启动时，实例服务把任何仍标记为 `running` 的 `browser_instances` 行对账回 `stopped`（见 `Store.ReconcileInstanceStatuses`），因为在应用级单实例保护之下，这样的一行只可能是已不存在的进程的残留。
