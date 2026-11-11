# DeBrowser Roadmap

Development follows AGENTS.md section 9: small, testable stages, one Pull Request per stage, merged only after project-owner approval.

| Stage | Scope | Status |
| --- | --- | --- |
| 1 | Repository foundation and application shell | **In progress** (this Pull Request) |
| 2 | SQLite database and migrations | Foundation started in Stage 1; full schema lands in Stage 2 |
| 3 | Browser kernel detection and instance lifecycle | Planned |
| 4 | Proxy and Mihomo integration | Planned |
| 5 | Bookmarks and kernel management | Planned |
| 6 | Multi-window synchronization | Planned |
| 7 | Backup, recovery, performance and release testing | Planned |

## Stage 1 — Repository foundation and application shell

- Wails v3.0.0 + Go 1.26 + React 18 + TypeScript + Vite project, Windows 11 x64 target
- Modular monolith layout (`internal/` backend modules, `frontend/` UI)
- Configuration management and data-directory layout (portable layout next to the executable, adopted after Stage 1; Stage 1 originally used `%AppData%\DeBrowser` and existing data is migrated automatically)
- Structured JSON logging infrastructure
- SQLite infrastructure: WAL, foreign keys, transactional migrations (foundation schema only)
- `AppService` with live status over bindings and the typed `app:status` event
- Base desktop UI: sidebar navigation, top toolbar, main area, bottom status bar, console panel
- Engineering docs and GitHub Actions CI on a Windows x64 runner
- `.gitignore` rules keeping databases, profiles, cookies, secrets and build output out of git

## Stage 2 — SQLite database and migrations

- Core schema (proxies, browser instances, bookmarks) as versioned migrations 0002/0003 on top of the Stage 1 foundation; the kernels table lands with kernel management in Stage 5
- Repository layer (`internal/store`) over shared `internal/model` types, a `WithTx` transaction entry for cross-repository atomic work, and `synchronous=FULL`
- Verified pre-migration backups (`VACUUM INTO`) that abort the upgrade when they fail

## Stage 3 — Browser kernel detection and instance lifecycle

- Detect locally installed Chromium kernels
- Instance CRUD, start/stop, per-instance user data directories and CDP wiring (localhost only)

## Stage 4 — Proxy and Mihomo integration

- Mihomo process management, per-instance proxy assignment, subscription import (kept local, never committed)

## Stage 5 — Bookmarks and kernel management

- Bookmark import/export and management UI; kernel download/management

## Stage 6 — Multi-window synchronization

- Synchronized actions across multiple running instances

## Stage 7 — Backup, recovery, performance and release testing

- Backup and restore of database and configuration, recovery flows, performance pass, release testing on Windows 11 x64
