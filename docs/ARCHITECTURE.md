# DeBrowser Architecture

## Overview

DeBrowser is a modular monolith: one Go binary embedding a React frontend, shipped as a native Windows desktop application through Wails v3. There are no microservices and no external servers — all state lives on the user's machine.

```text
┌────────────────────────────────────────────────────────┐
│ React frontend (frontend/)                             │
│  pages · layout components · Zustand UI store          │
└──────────────┬──────────────────────┬──────────────────┘
               │ generated bindings   │ typed events
               │ (frontend/bindings)  │ (app:status, …)
┌──────────────▼──────────────────────▼──────────────────┐
│ Wails v3 application (internal/app)                    │
│  services registered with the application object       │
├────────────────────────────────────────────────────────┤
│ Services (internal/service)                            │
│  AppService — application status & lifecycle           │
│  (instance / proxy / bookmark services in later stages)│
├────────────────────────────────────────────────────────┤
│ Foundation (internal/)                                 │
│  paths · config · settings · migration · logging       │
│  database (SQLite + migrations + backups) · version    │
│  model · store (repositories, Stage 2)                 │
│  version                                               │
├────────────────────────────────────────────────────────┤
│ Local resources                                        │
│  <root>\data\debrowser.db (portable layout)      │
│  <root>\data\profiles\<instance-id>\  (Stage 3)│
│  <root>\webview2-data\  (app window WebView2 data)    │
│  Chromium processes over CDP                  (Stage 3)│
│  Mihomo proxy engine                          (Stage 4)│
└────────────────────────────────────────────────────────┘
```

## Backend (Go)

`main.go` is the entry point. Startup order is fixed:

1. `internal/config` + `internal/paths` resolve the portable layout from the executable's directory (never the working directory), create all directories and probe that the application root is writable - a read-only location aborts startup with a clear error; there is no fallback to `%APPDATA%`.
2. `internal/logging` opens the structured (JSON) log file under `data/logs`.
2b. `internal/singleinstance` takes the per-installation lock `data/app.lock` (Stage 3) as an operating-system file lock — `flock` on Unix, `LockFileEx` on Windows. The OS is the only arbiter: exactly one process can hold it, contenders never delete or recreate each other's lock file (no check-then-delete race), and the lock is released by the OS itself when the holder exits for any reason (clean exit, crash, force kill, power loss), so stale locks cannot exist. The file records the holder's PID for information only. A second launch of the same portable folder gets AlreadyRunning, writes an activation signal (`data/activate.request`) asking the running instance to focus its window, and exits without touching the database. Other portable folders use other lock files and are unaffected.
3. `internal/migration` performs the one-time, idempotent migration of Stage 1 roaming data (`%AppData%\DeBrowser`): the database is snapshotted with `VACUUM INTO` from a read-only connection (WAL-consistent) and verified; auxiliary files are copied with hash verification; existing files are never overwritten, the legacy folder is kept as a backup, and a failure aborts startup instead of creating an empty replacement database. Legacy WebView2 data is never migrated - the portable build re-initializes it under `<root>\webview2-data`.
4. `internal/database` opens SQLite (WAL mode, `synchronous=FULL`, foreign keys on) and applies pending migrations transactionally, taking a verified `VACUUM INTO` backup into `data\backups\` first when upgrading an existing database. Core data access goes through `internal/store` repositories over `internal/model` types (Stage 2).
5. `internal/service` constructs the Wails services (including persisted user settings from `config/settings.json`, managed by `internal/settings`).
6. `internal/app` assembles the Wails application (services, main window with `Windows.WebviewUserDataPath` set to the portable WebView2 directory) and `main` runs it. Background loops (10s status events, 500ms activation watcher) run under an application context via `internal/lifecycle`; shutdown cancels the context and `App.Run` waits for the loops to drain before returning — the wait is unbounded by design (a warning is logged every 5s), because cancelling a context cannot interrupt a callback already running, and returning early would let `main` close the database underneath a live reader. Only once every loop has provably exited does `main` close the database, release the instance lock and close the log — in that order. `main` is a thin wrapper over `run() error`; nothing calls `log.Fatalf` mid-startup, so deferred cleanup always runs.

Stage 3 also adds `InstanceService` (bindings: `ListInstances`, `GetInstance`, `CreateInstance`, `UpdateInstance`, `DeleteInstance`, `DeleteInstanceAndProfile`, `ListProxies`) over the Stage 2 store: validated instance metadata CRUD with Chinese, user-facing constraint messages. `DeleteInstance` removes metadata only and never touches profile files; `DeleteInstanceAndProfile` is the explicit destructive path and refuses persisted-running instances, the profiles root itself, and anything that cannot be PROVEN to stay inside `data/profiles` by real-path resolution: the target itself must not be a symlink/reparse point, and every path component must resolve (intermediate symlinks and Windows junctions included; `GetFinalPathNameByHandle` on Windows, `EvalSymlinks` on Unix) to a location still strictly inside the resolved profiles directory. Deletion is then failure-atomic and OBJECT-anchored rather than a bare recursive remove (`internal/pfiles`): the move into a quarantine slot under `profiles/` acts on the verified filesystem object, never on the verified path string — on Linux the descent from the profiles root is anchored to directory file descriptors (`openat` with `O_NOFOLLOW` at every component, the root itself opened `O_NOFOLLOW`) and the move is `renameat2(RENAME_NOREPLACE)` between the two pinned fds, with a post-move `(dev, ino)` identity check; on Windows the target directory is opened with `FILE_FLAG_OPEN_REPARSE_POINT`, its identity is verified through the handle (reparse attributes plus `GetFinalPathNameByHandle` against the verified parent), and the rename itself is performed through that handle (`SetFileInformationByHandle`/`FileRenameInfo`, no-replace), so swapping any path component after verification cannot redirect the move. The database record is deleted next; on failure the object is moved back through the same anchor (on Windows the restore first re-verifies the original parent's identity and refuses, leaving the data in quarantine, if the parent now resolves elsewhere), and only with the record gone is the quarantine tree wiped by `internal/safedelete` — descriptor-anchored traversal on Linux, and on Windows per-object handle verification with `FileDispositionInfo` deletion, where reparse points are removed as link objects only and no link is ever followed out of the tree. On Darwin the standard library offers no descriptor-relative or handle-based rename, so the destructive operation fails closed (refused, data and record intact) rather than falling back to a path-based move. There is no process manager yet, so at startup any instance still persisted as `running` is reconciled to `stopped` (`Store.ReconcileInstanceStatuses`) — with the single-instance lock, such a row can only be a leftover of a dead process. No schema migration was needed for Stage 3.

Ownership rules from AGENTS.md: Go owns browser process management, instance lifecycle, profile isolation, proxy management, kernel management, bookmarks, synchronization, database operations and backup/recovery. React owns rendering, interaction, forms, tables, frontend state and notifications only.

### Frontend ↔ backend communication

- **Bindings**: exported methods on registered Go services are exposed through statically generated TypeScript bindings in `frontend/bindings/` (regenerate with `wails3 generate bindings`; never edit by hand).
- **Events**: typed custom events registered in `main.go` (`application.RegisterEvent`). Stage 1 defines one event: `app:status` carrying `service.Status`, emitted every 10 seconds and on demand through `AppService.GetStatus()`.

## Frontend (React)

- `src/components/layout/` — sidebar navigation, top toolbar, bottom status bar.
- `src/components/` — shared components (console panel).
- `src/pages/` — one page per navigation entry. Modules not yet delivered render an explicit "planned for Stage N" state instead of fake data.
- `src/stores/` — Zustand store for UI state (current page, console visibility, backend status, session console lines).
- Styling is Tailwind CSS v4 through the `@tailwindcss/vite` plugin (`src/index.css`).

## Browser isolation (from Stage 3)

Every browser instance will have a unique ID, an independent user data directory under `profiles/`, an independent configuration, its own managed Chromium process and an optional independent proxy. A user data directory is never shared between running instances, and a live profile is never copied while Chromium is using it. DeBrowser does not claim fingerprint isolation unless it is explicitly implemented and tested.

## Security notes

- CDP debugging ports are never exposed publicly; they bind to localhost only (Stage 3).
- The frontend cannot execute arbitrary commands; it can only call the explicitly registered service methods.
- Secrets (credentials, subscription URLs, private proxy configuration) are never stored in SQLite in plain committed files and never enter this repository — see `docs/DATABASE.md` and `.gitignore`.

## Deliberately excluded

Electron, Docker, Redis, MongoDB, GraphQL and microservices are out of scope (AGENTS.md section 3). The scaffold's mobile and Docker assets are not part of this project; the only supported target is Windows 11 x64.
