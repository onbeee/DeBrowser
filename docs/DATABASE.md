# DeBrowser 数据库

本文档是 SQLite 数据结构、迁移与数据库保护机制的唯一记录。系统架构见 [ARCHITECTURE.md](ARCHITECTURE.md)，阶段状态见 [ROADMAP.md](ROADMAP.md)。

## 概览

- 引擎：SQLite，经纯 Go 驱动 `modernc.org/sqlite` 访问（不需要 CGO）。
- 文件：便携数据目录内的 `data/debrowser.db`（路径只来自 `internal/paths`，没有 AppData 回退）。文件缺失时在正常启动中创建；永不从任何外部位置自动导入数据。
- 用途：保存实例、浏览器登记、书签、运行身份等元数据。浏览器 Profile 内容永不进入数据库（见「数据约束」）。

## 运行配置

在 `internal/database/database.go` 中为每个连接设置：

| Pragma | 值 | 原因 |
| --- | --- | --- |
| `journal_mode` | `WAL` | 读者不阻塞写者；崩溃安全 |
| `synchronous` | `FULL` | WAL 下 `NORMAL` 扛不住操作系统崩溃或断电；本项目是低写入率的本地元数据存储，以小额提交成本换取每个已提交事务的持久性 |
| `foreign_keys` | `1` | SQLite 默认不执行外键，本 schema 的引用完整性依赖它 |
| `busy_timeout` | `5000` | 短暂锁竞争等待而非直接失败 |

连接池限制为一个打开连接：SQLite 只有一个写者，单连接保证逐连接 pragma 对每条语句生效。ID 是应用生成的 UUID（TEXT）；时间戳是 UTC 文本，更新时由仓储层刷新 `updated_at`。

## 数据访问分层

- `internal/model`：纯数据类型，无行为、无依赖。
- `internal/database`：打开、pragma、迁移、迁移前备份、健康检查、事务入口。
- `internal/store`：仓储层，核心模型的全部 SQL 都在这里；每个 repository 针对一个小的 `querier` 接口工作，同一份代码可在连接池或事务内运行；`Store.WithTx` 原子地运行多个 repository 调用（嵌套事务被拒绝）。无 ORM。

## 当前有效表

当前产品实际使用的数据表共五张：`browser_instances`、`bookmark_nodes`、`external_browsers`、`instance_runtime`、`profile_counter`。

### `browser_instances`

每个浏览器实例一行。

| 列 | 说明 |
| --- | --- |
| `name` | UNIQUE，实例在界面中以名称标识 |
| `kernel_id`、`kernel_name`、`kernel_version` | 关联的已登记浏览器：`kernel_id` 存 `external_browsers` 登记的稳定 ID（按 ID 关联，不按名称；无关联时为空），`kernel_name` 是关联时的名称快照，`kernel_version` 仅保留历史自由文本。可执行路径不落库，服务按 `kernel_id` 实时解析 |
| `user_data_dir` | UNIQUE。实例文件的相对路径（如 `profiles/P-1`），与 `name` 的唯一约束及服务检查共同保证实例与实例文件一对一。文件夹在创建实例时实际建立；其内容是浏览器数据，永不存入 SQLite |
| `status` | CHECK：`stopped` / `running` / `error`，由运行时管理器按已核验的进程事实写入。`starting` / `stopping` 只是内存过渡状态，不落库；`unknown`（无法确认）同样不落库，由「`running` 行 + 未能通过身份核验的 `instance_runtime` 记录」推导（见 `instance_runtime`） |
| `start_url`、`sort_order` | 可选起始页；显示顺序 |
| `proxy_id`、`proxy_listen_port` | 历史兼容字段，见「历史兼容结构」 |

索引：`proxy_id`、`status`。

实例与实例文件是一对一关系（见上表 `user_data_dir`）。删除实例的默认行为是只删除本表中的记录、**保留**实例文件目录及其全部浏览器数据；只有用户在删除时明确选择「删除实例及实例文件」，才删除对应目录（删除的保护规则见 [ARCHITECTURE.md](ARCHITECTURE.md) 的「安全与数据保护」）。

### `bookmark_nodes`

DeBrowser 书签资源库（migration 0014）：文件夹与书签组成的树。列：`id`、`kind`（CHECK：`bookmark` / `folder`）、`name`、`url`（文件夹为空串）、`parent_id`（外键 → `bookmark_nodes(id)`；NULL 表示根级）、`sort_order`、时间戳。`(parent_id, sort_order, created_at)` 索引供逐层有序读取。文件夹循环引用由服务层校验阻止（不得移入自身或其后代）；删除文件夹由服务层在事务内自底向上删除整个子树，外键保证不留孤儿。

资源库与各实例 Profile 中的真实书签（Chromium `Bookmarks` 文件）完全独立：资源库的增删改不自动影响任何实例，实例书签的修改也不回写资源库；分发只通过同步操作显式进行。实例真实书签不进数据库，见 [ARCHITECTURE.md](ARCHITECTURE.md) 的书签机制说明。

### `external_browsers`

用户手动添加的程序的纯路径登记：`id`、`name`（从所选文件名派生的显示名）、`executable_path`（用户所选的**绝对**路径，该程序位于 DeBrowser 文件夹之外，没有便携相对形式）、时间戳。`executable_path` 有唯一索引，重复登记同一路径返回既有记录。

DeBrowser 对所选文件不做任何身份、版本、签名、发布者或存在性检查。移除登记只删除这一行；被实例引用的登记由服务层先行检查并拒绝移除（不级联删除实例、不留悬空关联）。本表由 migration 0011 创建、0012 重建为纯路径形态（去掉早期验证列，既有登记保留）。

### `instance_runtime`

每个已启动且尚未观察到退出的实例一行浏览器主进程身份记录：`instance_id`（主键，外键 → `browser_instances(id)`，随实例删除级联）、`pid`、`process_created_at`（进程创建时间；PID 会被操作系统复用，只有 PID + 创建时间才能区分前后两个进程）、`platform`、`executable_path`、`user_data_dir`、`started_at`。启动成功时写入，观察到进程退出或启动对账证明记录失效时删除。

DeBrowser 重启后的重新识别逐项核验记录（PID 存在、创建时间一致、可执行路径一致、命令行中 `--user-data-dir` 与实例文件完整路径精确相等），全部通过才重新接管。例外：进程跟踪建立失败且退出无法确认时写入的记录 `process_created_at` 为 0，永远无法完整核验，只用于让实例保持「无法确认」冻结状态，进程消失后由对账清除。进程存在但身份无法确认时记录与 `running` 状态都保留，生效状态推导为 `unknown`。表中永不保存浏览器 Profile 内容。运行机制细节见 [ARCHITECTURE.md](ARCHITECTURE.md)。

无法确认状态的保护以身份记录成功写入为成立前提，不只依赖会话内存标记；写入失败时本次操作以「严重错误」明确报告。不设第二张回退表：真实的数据库写入失败是整库性的，同库回退表同样失效。

### `profile_counter`

默认实例文件编号的持久计数器（单行，`next_seq` 只增不减）。删除实例或实例文件都不会让编号被重新分配；分配时服务还会跳过已被绑定或已存在于磁盘的候选名。

## 遗留表（当前未使用）

### `bookmarks`

migration 0003 的历史扁平书签表：`instance_id` 外键 → `browser_instances(id)`，`ON DELETE CASCADE`；`instance_id` 为 NULL 表示全局书签；`(IFNULL(instance_id, ''), url)` 唯一索引与 `(instance_id, sort_order)` 索引保留原样。Stage 9C 的书签资源库改用 `bookmark_nodes` 树模型后，本表不再被当前产品写入；表与既有行按原样保留，不删除、不迁移数据。

### `app_settings`

应用级键值表（migration 0001）。**当前没有任何生产代码读写此表**（其 store 访问器已在 PR #9 清理中移除），因此不计入当前实际使用的数据表。表中可能存在的历史键（如已取消 Stage 7 的 `proxy.port_range`）只是既有数据库中的历史数据。面向用户的设置实际存放在 `config/settings.json`（语言、主题、日志总容量 `logCapacityBytes`），与此表无关。

## 历史兼容结构

以下结构来自已取消的功能（代理/订阅/Mihomo、独立内核管理）。它们及其全部行保留在既有数据库中，使旧数据库照常升级、数据与外键保持完好；已发布 migration 不删除、不改写。当前没有任何页面、服务或绑定为它们提供业务读写；唯一的读取是凭据 Vault 的引用查询 `SecretRefInUse` / `AllSecretRefs` 扫描 `secret_ref` 列，使历史凭据在仍被引用时永不被回收。

| 表 | 来源 | 说明 |
| --- | --- | --- |
| `proxies` | Stage 4A–6 | 代理节点历史数据；`secret_ref` 指向 Vault，凭据明文从未入表 |
| `subscriptions` | Stage 6 | 订阅元数据；完整订阅 URL 从未入表（只存 `url_hint` 与 `secret_ref`） |
| `mihomo_configs` | Stage 6 | 导入的 Mihomo 配置文档元数据（文件本体曾存于 `data/mihomo/configs/`，只记相对路径与 SHA-256） |
| `mihomo_proxy_groups` | Stage 6 | 导入配置的原生 proxy-groups 结构，与 `(config_id, name)` 唯一；不是 DeBrowser 自有的分组功能（自有分组/标签已由 migration 0005 删除，表已不存在） |
| `mihomo_proxy_group_members` | Stage 6 | 原生分组的成员行，主键 `(group_id, sort_order)` |
| `kernels` | Stage 9A（migration 0010） | 原为 DeBrowser 托管的独立 Chromium 构建；该功能已取消，当前无任何代码使用此表。用户手动登记在 `external_browsers`，永不写入此表 |

此外，`browser_instances` 的两个历史字段按原样保留：`proxy_id`（外键 → `proxies(id)`，`ON DELETE SET NULL`；当前创建实例总是写入 NULL，更新逐字保留已存值，没有任何代码路径能创建或改变它）与 `proxy_listen_port`（原为已取消 Stage 7 的监听端口，不再被分配或使用；经唯一索引保持唯一，NULL 永不冲突）。

## 迁移

- SQL 文件位于 `internal/database/migrations/`，命名 `NNNN_description.sql`，版本号单调递增，嵌入二进制，在启动时按版本顺序应用。
- 每个迁移在自己的事务中运行，并与它在 `schema_migrations (version, name, applied_at)` 中的记录同事务：失败不留半应用的 schema，重启不重复应用。已应用版本可经 `DB.AppliedVersion` 查询。
- 迁移只追加：一旦合并，迁移文件永不编辑；更正以新迁移的形式交付。

| 版本 | 文件 | 核心变更 |
| --- | --- | --- |
| 1 | `0001_foundation.sql` | `app_settings` |
| 2 | `0002_proxies_and_instances.sql` | `proxies`、`browser_instances`（+ 索引） |
| 3 | `0003_bookmarks.sql` | `bookmarks`（+ 作用域 URL 唯一索引） |
| 4 | `0004_proxy_foundation.sql` | `proxy_groups`、`proxy_tags`、`subscriptions`；`proxies` 来源/去重/测试列；`browser_instances.proxy_listen_port` |
| 5 | `0005_remove_proxy_groups_tags.sql` | 移除 DeBrowser 自有代理分组/标签：重建 `proxies` 与 `subscriptions`（去 `group_id`），删除 `proxy_groups` 与 `proxy_tags` |
| 6 | `0006_mihomo_config_import.sql` | `mihomo_configs`、`mihomo_proxy_groups`、`mihomo_proxy_group_members`；`proxies.config_id` / `source_node_name` |
| 7 | `0007_proxy_source_vocabulary.sql` | 以统一来源词汇（`custom` / `file` / `subscription`）重建 `proxies`，链接抢救保留实例绑定与分组成员 |
| 8 | `0008_proxy_group_custom_members.sql` | 原生 proxy-group 的用户自定义成员 overlay（同日被 0009 取代） |
| 9 | `0009_drop_custom_members_overlay.sql` | 删除 0008 的 overlay 表（该功能在发布前撤回，表从未有用户数据） |
| 10 | `0010_kernels.sql` | `kernels`（历史结构，见「历史兼容结构」） |
| 11 | `0011_external_browsers.sql` | `external_browsers`（当时的验证式登记设计，已被 0012 取代） |
| 12 | `0012_external_browsers_path_only.sql` | 重建 `external_browsers` 为纯路径登记（去验证列，既有登记保留） |
| 13 | `0013_instance_runtime.sql` | `instance_runtime` 与 `profile_counter` |
| 14 | `0014_bookmark_library.sql` | `bookmark_nodes`（书签资源库树） |

涉及表重建的迁移（0005、0007、0012 等）在迁移事务内先抢救进入的链接再恢复，并由填充夹具的升级测试验证数据保留、`PRAGMA foreign_key_check` 与 `PRAGMA integrity_check`。

## 数据保护

迁移是本地数据库风险最高的时刻，升级先保护数据：

- 当存在待执行迁移且数据库已有已应用迁移时，启动在第一个待执行迁移之前把备份写入 `data/backups/`：`debrowser-pre-migration-<UTC>-from-v<current>-to-v<target>.db`。
- 备份使用 `VACUUM INTO` 产生事务一致的快照（包含仍在 WAL 中的数据）；从不直接复制 `.db` 文件。
- 快照在迁移继续之前经 `PRAGMA integrity_check` 验证；**备份失败则中止迁移**并明确报错，既有数据库不受影响。全新数据库跳过备份。

本节的迁移前快照是数据层内部的底层安全设施，**不是**面向用户的备份与恢复功能；完整的备份导出、导入与恢复属于 Stage 12，尚未开放。

## 数据约束

- 永不把浏览器 Cookie 或 Profile 内容存入 SQLite；实例文件以文件夹形式位于 `data/profiles/`，数据库只记相对路径绑定。
- 凭据、订阅 URL 等敏感内容只存入凭据 Vault（见 [ARCHITECTURE.md](ARCHITECTURE.md)），数据库只存不透明 `secret_ref`。
- 用户数据库永不提交到 Git（`*.db`、`*.db-wal`、`*.db-shm` 已被 git 忽略）。
- 日志不进数据库：三类记录以 JSON Lines 文件存放在 `data/logs/`。
