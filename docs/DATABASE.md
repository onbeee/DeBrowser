# DeBrowser Database

## Engine

- SQLite, accessed through the pure-Go driver `modernc.org/sqlite` (no CGO required, which keeps Windows x64 builds simple).
- The database file is `debrowser.db` inside the portable data directory (`<application root>\data\debrowser.db`; the root follows the executable — see `internal/paths`). Stage 1 databases left in `%AppData%\DeBrowser` are migrated automatically on first launch (see ARCHITECTURE.md).
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

Current state, as landed by Stage 2 (migrations 0001-0003, all applied at startup): the tables `app_settings`, `proxies`, `browser_instances` and `bookmarks` **exist** and are in use by the repository layer. There is **no `kernels` table yet** — kernel records and the instance→kernel foreign key are Stage 5 work; until then, instances only carry kernel identity columns (see `browser_instances` below). Anything not listed in this section does not exist in the database.

IDs are application-generated UUIDs (TEXT). Timestamps are UTC text (`datetime('now')` defaults; the repository refreshes `updated_at` on update).

### `proxies`

Proxy nodes. `protocol` is constrained to `http, https, socks5, vmess, vless, trojan, shadowsocks, hysteria2, tuic, wireguard, direct`; `port` must be 0-65535 when present; `enabled` is 0/1.

**Credential policy**: passwords, keys and tokens are never stored in this table (or in logs). Non-secret connection parameters may live in `config_json`; `secret_ref` is only an opaque reference to a future secure credential store — the mechanism is decided in Stage 4.

### `browser_instances`

One row per browser instance (isolated Chromium profile).

| Column | Notes |
| --- | --- |
| `name` | UNIQUE — instances are identified by name in the UI. |
| `kernel_id`, `kernel_name`, `kernel_version` | Kernel identity recorded as data. The `kernels` table and its foreign key arrive in Stage 5. |
| `user_data_dir` | UNIQUE. Profile path **relative** to the portable data directory (e.g. `profiles\<id>`), so the whole application folder stays movable. Profile contents are browser data and are never stored in SQLite. |
| `proxy_id` | FK → `proxies(id)`, `ON DELETE SET NULL`: deleting a proxy detaches instances instead of breaking them. |
| `status` | `stopped` / `running` / `error` (CHECK). The persisted lifecycle record; live runtime state is owned by the Stage 3 instance manager. |
| `start_url`, `sort_order` | Optional start page; display ordering. |

Indexes: `proxy_id`, `status`.

### `bookmarks`

`instance_id` FK → `browser_instances(id)`, `ON DELETE CASCADE`: deleting an instance deletes its bookmarks. A NULL `instance_id` means a **global** bookmark that applies to every instance. Uniqueness is one URL per scope, enforced by a unique index on `(IFNULL(instance_id, ''), url)` — a plain `UNIQUE(instance_id, url)` would not work because SQLite treats NULLs as distinct. Index on `(instance_id, sort_order)` for ordered listing.

### `app_settings`

Application-level key/value settings (Stage 1, migration 0001). The user-facing `config/settings.json` file (language/theme) is separate and unrelated.

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

## Backup before migration

Schema changes are the riskiest moment for a local database, so upgrades protect the data first:

- When pending migrations exist **and** the database already has applied migrations, startup takes a backup into `data\backups\` before the first pending migration runs: `debrowser-pre-migration-<UTC>-from-v<current>-to-v<target>.db`.
- The backup uses **`VACUUM INTO`**, which produces a transactionally consistent snapshot including data that still lives in the WAL. Copying the `.db` file directly is never used — it would silently miss uncheckpointed transactions.
- The snapshot is verified with `PRAGMA integrity_check` before migration proceeds. **If the backup fails, the migration is aborted** with an explicit error; the existing database is left untouched.
- A brand-new database (nothing applied yet) skips the backup — there is nothing to protect.
- `DB.BackupNow` exposes the same verified snapshot mechanism for manual backups (used by Stage 7's full backup/restore feature).
- The Stage 1.5 roaming→portable data migration (`internal/migration`) is a separate, file-level one-time migration that runs before the database is opened; it is unchanged and composes with this mechanism (a migrated Stage 1 database arrives at schema v1 and is then backed up and upgraded by the normal path).

Full backup/restore UX ships in Stage 7.
