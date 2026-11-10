# DeBrowser Database

## Engine

- SQLite, accessed through the pure-Go driver `modernc.org/sqlite` (no CGO required, which keeps Windows x64 builds simple).
- The database file is `debrowser.db` inside the application data directory (`%AppData%\DeBrowser` by default; override with `DEBROWSER_DATA_DIR`).
- Open-time pragmas (see `internal/database/database.go`): `journal_mode=WAL`, `foreign_keys=1`, `busy_timeout=5000`. The connection pool is limited to one open connection because SQLite permits a single writer.

## Rules (AGENTS.md section 5)

- WAL mode, foreign key constraints, migrations, transactional writes, appropriate indexes, backup/restore support.
- **Never** store browser cookies or browser profile contents in SQLite. Profiles live as files under `profiles\<instance-id>\`.
- Never commit user databases (`*.db`, `*.db-wal`, `*.db-shm` are git-ignored), credentials, subscription URLs or private proxy configurations.

## Migrations

- SQL files live in `internal/database/migrations/`, named `NNNN_description.sql` with a monotonically increasing version.
- Migrations are embedded in the binary and applied at startup in version order. Each migration runs in its own transaction together with its row in `schema_migrations (version, name, applied_at)`, so a failure cannot leave a half-applied schema.
- Migrations are append-only: once merged, a migration file is never edited; corrections ship as a new migration.

### Applied migrations

| Version | File | Contents |
| --- | --- | --- |
| 1 | `0001_foundation.sql` | `app_settings (key, value, updated_at)` — application-level key/value settings |

### Planned schema (later stages, not yet created)

- Stage 2: business tables for browser instances, proxies, kernels and bookmarks with foreign keys between them (instance → kernel, instance → proxy) and indexes on foreign keys and names.
- The exact tables are designed in Stage 2 and documented here when they land. Until then, no business tables exist and the UI shows no placeholder data.

## Backup and restore

Full backup/recovery ships in Stage 7. The foundation already reserves the `backups/` directory in the data directory, and WAL mode keeps the database file safe to copy while the application is closed. A live database must be backed up through SQLite's backup mechanisms, never by copying files while writes are in flight.
