# DeBrowser Database

## Engine

- SQLite, accessed through the pure-Go driver `modernc.org/sqlite` (no CGO required, which keeps Windows x64 builds simple).
- The database file is `debrowser.db` inside the portable data directory (`<application root>\data\debrowser.db`; the root follows the executable — see `internal/paths`). It is created by the normal startup when missing (first launch) and opened as-is afterwards; no data is ever imported automatically from `%AppData%` or any other external location (see ARCHITECTURE.md).
- The path always comes from `internal/paths`; no code outside that package constructs storage locations, and there is no AppData fallback.

## Open-time pragmas

Set in `internal/database/database.go` for every connection:

| Pragma | Value | Why |
| --- | --- | --- |
| `journal_mode` | `WAL` | Readers do not block the writer; crash-safe journaling. |
| `synchronous` | `FULL` | With WAL, `NORMAL` survives application crashes but not OS crashes or power loss. DeBrowser is a local-first metadata store with a low write rate, so FULL's small commit cost buys durability of every committed transaction — the project prioritises data integrity over marginal write latency. |
| `foreign_keys` | `1` | SQLite disables FK enforcement by default; the schema's referential integrity depends on it. |
| `busy_timeout` | `5000` | Brief lock contention (e.g. a backup snapshot) waits instead of failing with `SQLITE_BUSY`. |

The connection pool is limited to **one** open connection: SQLite permits a single writer, and a single connection guarantees the per-connection pragmas apply to every statement. Concurrency is handled by serialising through the pool plus `busy_timeout`; repositories are safe for concurrent use.

## Layers

- `internal/model` — plain data types (`Proxy`, `Instance`, `Bookmark`), no behaviour, no dependencies.
- `internal/database` — opening, pragmas, migrations, backups, `WithTx` transaction entry, health.
- `internal/store` — the repository layer. All SQL for the core models lives here; each repository works against a small `querier` interface so the same code runs on the pool or inside a transaction. `Store.WithTx` runs several repository calls atomically (nested transactions are rejected). No ORM.

## Schema (current state)

Current state (migrations 0001-0006, all applied at startup): the tables `app_settings`, `proxies`, `subscriptions`, `browser_instances`, `bookmarks`, `mihomo_configs`, `mihomo_proxy_groups` and `mihomo_proxy_group_members` **exist** and are in use by the repository layer. There is **no `kernels` table yet** — kernel records and the instance→kernel foreign key are Stage 9 work; until then, instances only carry kernel identity columns (see `browser_instances` below). Anything not listed in this section does not exist in the database.

IDs are application-generated UUIDs (TEXT). Timestamps are UTC text (`datetime('now')` defaults; the repository refreshes `updated_at` on update).

### `proxies`

Proxy nodes. `protocol` is constrained to `http, https, socks5, vmess, vless, trojan, shadowsocks, hysteria2, tuic, wireguard, direct`; `port` must be 0-65535 when present; `enabled` is 0/1.

**Credential policy**: passwords, keys and tokens are never stored in this table (or in logs). Non-secret connection parameters may live in `config_json`; `secret_ref` is an opaque reference into the credential vault (`internal/secrets`, Stage 4A).

Stage 4A (migration 0004) adds: `sort_order`, `source` (`manual` / `file_import` / `subscription` — renamed to `custom` / `file` / `subscription` by migration 0007, see below), `subscription_id` (FK → `subscriptions`, `ON DELETE CASCADE`), `dedup_key` (import fingerprint; partial UNIQUE index `WHERE dedup_key <> ''`, so empty keys never collide), and the connection-test record `last_latency_ms` / `last_test_ok` / `last_tested_at`. (Migration 0004 also added a `group_id` column here and to `subscriptions`, plus the `proxy_groups` / `proxy_tags` tables; the Stage 4A correction removed all of them again — see migration 0005 below.)

Stage 6 (migration 0006) adds: `config_id` (FK → `mihomo_configs`, `ON DELETE SET NULL`) — the imported config a node came from — and `source_node_name` (the node's name inside its source document; native proxy-group members address nodes by this name, so it is kept separately from the editable display `name`). Imported nodes carry a non-empty `dedup_key` of the form `<scope>:<owner-id>:<sha256>` where the fingerprint covers owner ID + name + protocol + server + port (never secrets): a subscription refresh matches its own previous nodes by this key and preserves their row IDs, and different subscriptions can hold same-named nodes without colliding.

### `subscriptions`

Subscription metadata; the refresh pipeline is Stage 6 work (implemented). The **full subscription URL is never stored here** — it may carry credentials and lives exclusively in the vault under `secret_ref`; this table keeps only `url_hint` (scheme+host), the fetch policy (`allow_lan` default 0, `auto_update_minutes` default 0 = off) and a sanitised refresh record (`last_updated_at`, `last_status`, `last_error` — status class only, never a URL).

### `mihomo_configs` (Stage 6, migration 0006)

One row per imported Mihomo configuration document. `source_type` is `file` (local import; `subscription_id` is NULL) or `subscription` (owned by a subscription; a partial UNIQUE index guarantees at most one current config per subscription). The sanitized document itself is a file under `data/mihomo/configs/<uuid>.yaml`; this table stores only the relative `file_path` (relative to `data/mihomo/`, never absolute), its `sha256`, and `secret_ref` — the opaque vault reference of the config-level credential bundle extracted during sanitisation (empty when the document carried none). `security_report_json` is the Stage 6 findings array (`allow_lan`, `external_bind`, `external_controller`, `controller_no_secret`, `tun_enabled`, `dns_external_listen`); it records risks only — Stage 7 enforces the safe values at render time. `format_version` versions the artifact format (currently 1).

### `mihomo_proxy_groups` (Stage 6, migration 0006)

The **native Mihomo `proxy-groups`** of one imported config, preserved structurally (name, type, non-secret `options_json`, document order in `sort_order`). These are the config author's groups — not a DeBrowser grouping feature (that was removed in migration 0005). `(config_id, name)` is UNIQUE. Deleting the config cascades to its groups.

### `mihomo_proxy_group_members` (Stage 6, migration 0006)

One row per group member, in document order (`PRIMARY KEY (group_id, sort_order)`). `member_kind` is `proxy`, `group` or `builtin`, and CHECK constraints enforce that exactly the matching target column is set: `proxy_id` (a node of the SAME config — resolution never crosses config boundaries) for `proxy`, `target_group_id` for `group`, neither for `builtin` (DIRECT, REJECT, GLOBAL, …). Group references were validated acyclic at import time.

### `browser_instances`

One row per browser instance (isolated Chromium profile).

| Column | Notes |
| --- | --- |
| `name` | UNIQUE — instances are identified by name in the UI. |
| `kernel_id`, `kernel_name`, `kernel_version` | Kernel identity recorded as data. The `kernels` table and its foreign key arrive in Stage 9. |
| `user_data_dir` | UNIQUE. Profile path **relative** to the portable data directory (e.g. `profiles\<id>`), so the whole application folder stays movable. Profile contents are browser data and are never stored in SQLite. |
| `proxy_id` | FK → `proxies(id)`, `ON DELETE SET NULL`: deleting a proxy detaches instances instead of breaking them. |
| `proxy_listen_port` | Stage 4A (migration 0004): the instance's Mihomo listener port, allocated in Stage 7 from the `app_settings` range `proxy.port_range` (default `19000-19499`). Unique via a unique index (SQLite cannot add a UNIQUE column by ALTER); NULL ports never collide. |
| `status` | `stopped` / `running` / `error` (CHECK). The persisted lifecycle record; live runtime state is owned by the Stage 3 instance manager. |
| `start_url`, `sort_order` | Optional start page; display ordering. |

Indexes: `proxy_id`, `status`.

### `bookmarks`

`instance_id` FK → `browser_instances(id)`, `ON DELETE CASCADE`: deleting an instance deletes its bookmarks. A NULL `instance_id` means a **global** bookmark that applies to every instance. Uniqueness is one URL per scope, enforced by a unique index on `(IFNULL(instance_id, ''), url)` — a plain `UNIQUE(instance_id, url)` would not work because SQLite treats NULLs as distinct. Index on `(instance_id, sort_order)` for ordered listing.

### `app_settings`

Application-level key/value settings (Stage 1, migration 0001), accessed through the repository's `GetSetting`/`SetSetting`/`DeleteSetting`. One reviewed key: `proxy.port_range` (for example `19000-19499`), the instance listener range consumed in Stage 7; an unset or malformed value falls back to the default in code. The user-facing `config/settings.json` file (language/theme) is separate and unrelated.

## Rules (AGENTS.md section 5)

- WAL mode, foreign key constraints, migrations, transactional writes, appropriate indexes, backup/restore support.
- **Never** store browser cookies or browser profile contents in SQLite. Profiles live as files under `profiles\<instance-id>\`.
- Never commit user databases (`*.db`, `*.db-wal`, `*.db-shm` are git-ignored), credentials, subscription URLs or private proxy configurations.

## Migrations

- SQL files live in `internal/database/migrations/`, named `NNNN_description.sql` with a monotonically increasing version.
- Migrations are embedded in the binary and applied at startup in version order. Each migration runs in its own transaction together with its row in `schema_migrations (version, name, applied_at)`, so a failure cannot leave a half-applied schema and restarts never re-apply a recorded migration.
- Applied versions are queryable via `DB.AppliedVersion`.
- Migrations are append-only: once merged, a migration file is never edited; corrections ship as a new migration.

### Applied migrations

| Version | File | Contents |
| --- | --- | --- |
| 1 | `0001_foundation.sql` | `app_settings` |
| 2 | `0002_proxies_and_instances.sql` | `proxies`, `browser_instances` (+ indexes) |
| 3 | `0003_bookmarks.sql` | `bookmarks` (+ scoped-URL unique index) |
| 4 | `0004_proxy_foundation.sql` | `proxy_groups`, `proxy_tags`, `subscriptions`; `proxies` group/source/dedup/test columns; `browser_instances.proxy_listen_port` |
| 5 | `0005_remove_proxy_groups_tags.sql` | Stage 4A correction: removes DeBrowser's own proxy groups/tags — rebuilds `proxies` and `subscriptions` without `group_id` and drops `proxy_tags` and `proxy_groups` (see below) |
| 6 | `0006_mihomo_config_import.sql` | Stage 6: `mihomo_configs`, `mihomo_proxy_groups`, `mihomo_proxy_group_members`; `proxies.config_id` / `proxies.source_node_name` (see below) |
| 7 | `0007_proxy_source_vocabulary.sql` | Stage 6C: rebuilds `proxies` with the unified source vocabulary `custom` / `file` / `subscription` (values mapped `manual` → `custom`, `file_import` → `file`); link rescue preserves instance bindings and group members (see below) |
| 8 | `0008_proxy_group_custom_members.sql` | Stage 6C review fix: user custom-member overlay for native proxy-groups (superseded the same day — see 0009) |
| 9 | `0009_drop_custom_members_overlay.sql` | Stage 6C redo: drops the 0008 overlay table; the custom-member feature was withdrawn before release (see below) |

### Migration 0005: removing groups and tags (Stage 4A correction)

Product decision (2026-11-13): DeBrowser does not offer its own proxy grouping/tagging. Migration 0005 removes the structures migration 0004 had added for it. This is **not** related to Mihomo's native `proxy-groups`, which are part of imported airport configurations and arrive with subscription/config import in Stage 6.

Because both `group_id` columns carry a foreign key to `proxy_groups` (and `proxies.group_id` an index), 0005 uses a table rebuild instead of `DROP COLUMN`. The rebuild works inside the migration runner's per-migration transaction with `foreign_keys=1`: on this SQLite build, foreign-key actions (`ON DELETE SET NULL` / `CASCADE`) fire immediately when a parent table is dropped, so the migration first rescues the incoming links (`browser_instances.proxy_id`, then `proxies.subscription_id`) into scratch tables and nulls them, swaps in the rebuilt tables under the same names, and then restores the links (the restores are themselves FK-checked). `proxies.sort_order` is kept — it is plain list ordering, not a group feature. The v4 → v5 upgrade test (`internal/database/stage5_test.go`) verifies data preservation, `PRAGMA foreign_key_check` and `PRAGMA integrity_check` over a populated v4 fixture.

### Migration 0006: Mihomo config import (Stage 6)

Purely additive: three new tables plus two new `proxies` columns with compatibility defaults (`config_id` NULL, `source_node_name` `''`), so existing Stage 4A/5A/5B rows, values and relationships are untouched. The v5 → v6 upgrade test (`internal/database/stage6_test.go`) verifies data preservation over a populated v5 fixture, the member CHECK constraints, `PRAGMA foreign_key_check` and `PRAGMA integrity_check`. The vault reference checks (`SecretRefInUse`, `AllSecretRefs`) cover `mihomo_configs.secret_ref` as well, so config credential bundles take part in guarded cleanup and garbage collection exactly like proxy and subscription references.

### Migration 0007: unified proxy source vocabulary (Stage 6C)

The unified 「代理配置」 module manages three sources — subscription, local file and custom (manual) nodes. Migration 0007 renames the Stage 4A `proxies.source` values to that vocabulary (`manual` → `custom`, `file_import` → `file`, `subscription` unchanged) and changes the column CHECK/default accordingly, so the values match `mihomo_configs.source_type` and the UI. Changing a CHECK constraint takes a table rebuild; the rebuild reuses migration 0005's link-rescue pattern inside the runner's transaction (FK actions fire immediately with `foreign_keys=1`): `browser_instances.proxy_id` links are copied aside and restored, and proxy-kind `mihomo_proxy_group_members` rows (whose CHECK forbids detaching them) are copied aside in full, deleted, and re-inserted against the rebuilt table with group, order and names intact. No row, ID, `secret_ref`, config/subscription link or member relation changes. The v6 → v7 upgrade test (`internal/database/stage7_test.go`) verifies the mapping and preservation over a populated v6 fixture (manual node bound to an instance, subscription + file configs with groups and members), plus `PRAGMA foreign_key_check` / `integrity_check`, rejection of the retired vocabulary, and the partial unique dedup index.

### Migration 0008: custom proxy-group members overlay (Stage 6C review fix)

> Superseded by migration 0009 (Stage 6C redo, 2026-11-14): the custom-node / custom-member feature was withdrawn by product decision before PR #9 merged. The 0008 file remains in the migration history untouched — databases from the PR #9 acceptance builds already applied it — and 0009 drops the table. The overlay never owned user data (it only referenced configs and custom nodes), so the drop loses nothing; existing `source='custom'` node records and their vault credentials are unaffected.

`mihomo_proxy_group_custom_members` stores **user-added** memberships: a `source = 'custom'` node attached to a native proxy-group of an imported config. It is strictly separate from the imported members (`mihomo_proxy_group_members`, which mirror the source document):

- Primary key `(config_id, group_name, proxy_id)`; groups are addressed by **name**, because a subscription refresh deletes and recreates the group rows themselves — the name is the identity that survives a refresh.
- `config_id` and `proxy_id` are `ON DELETE CASCADE` foreign keys: deleting a config or a custom node removes only its overlay rows; nothing ever cascades into nodes, configs or imported members.
- That the node is really `source = 'custom'` and that `(config_id, group_name)` names a group the config actually has is validated by the service layer inside the write transaction (a CHECK cannot express cross-table conditions).
- Refresh/re-import rebuilds only the imported structures (`ReplaceProxyGroups`) and never touches this table: after a refresh the attachment reappears in the same-named group; if the group vanished upstream the row is kept and reported as orphaned, and it takes effect again when the group returns.

## Backup before migration

Schema changes are the riskiest moment for a local database, so upgrades protect the data first:

- When pending migrations exist **and** the database already has applied migrations, startup takes a backup into `data\backups\` before the first pending migration runs: `debrowser-pre-migration-<UTC>-from-v<current>-to-v<target>.db`.
- The backup uses **`VACUUM INTO`**, which produces a transactionally consistent snapshot including data that still lives in the WAL. Copying the `.db` file directly is never used — it would silently miss uncheckpointed transactions.
- The snapshot is verified with `PRAGMA integrity_check` before migration proceeds. **If the backup fails, the migration is aborted** with an explicit error; the existing database is left untouched.
- A brand-new database (nothing applied yet) skips the backup — there is nothing to protect.
- `DB.BackupNow` exposes the same verified snapshot mechanism for manual backups (a low-level safety facility; the full user backup/restore feature is Stage 12 work).
- The external Stage 1 Roaming data migration that once ran before the database was opened was removed in Stage 5A (no automatic legacy-data migration exists); this in-database schema migration mechanism is unaffected and remains the long-term compatibility path.

Full backup/restore UX ships in Stage 12.

## Stage 3 note

Stage 3 (application lifecycle and instance management foundation) adds **no schema migration**: it works entirely on the existing tables. One behavioural addition lives in the repository layer, not the schema — at startup the instance service reconciles any `browser_instances` row still marked `running` back to `stopped` (see `Store.ReconcileInstanceStatuses`), because with app-level single-instance protection such a row can only be a leftover from a process that no longer exists.
