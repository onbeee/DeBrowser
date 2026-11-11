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
2b. `internal/singleinstance` takes the per-installation lock `data/app.lock` (Stage 3). A second launch of the same portable folder finds the lock held by a live PID, writes an activation signal (`data/activate.request`) asking the running instance to focus its window, and exits without touching the database. A lock left by a dead process (crash, kill) is stale and taken over. Other portable folders are unaffected.
3. `internal/migration` performs the one-time, idempotent migration of Stage 1 roaming data (`%AppData%\DeBrowser`): the database is snapshotted with `VACUUM INTO` from a read-only connection (WAL-consistent) and verified; auxiliary files are copied with hash verification; existing files are never overwritten, the legacy folder is kept as a backup, and a failure aborts startup instead of creating an empty replacement database. Legacy WebView2 data is never migrated - the portable build re-initializes it under `<root>\webview2-data`.
4. `internal/database` opens SQLite (WAL mode, `synchronous=FULL`, foreign keys on) and applies pending migrations transactionally, taking a verified `VACUUM INTO` backup into `data\backups\` first when upgrading an existing database. Core data access goes through `internal/store` repositories over `internal/model` types (Stage 2).
5. `internal/service` constructs the Wails services (including persisted user settings from `config/settings.json`, managed by `internal/settings`).
6. `internal/app` assembles the Wails application (services, main window with `Windows.WebviewUserDataPath` set to the portable WebView2 directory) and `main` runs it. Background loops (10s status events, 500ms activation watcher) run under an application context via `internal/lifecycle`; shutdown cancels the context and `App.Run` waits for the loops to drain before returning, after which `main` closes the database, releases the instance lock and closes the log — in that order. `main` is a thin wrapper over `run() error`; nothing calls `log.Fatalf` mid-startup, so deferred cleanup always runs.

Stage 3 also adds `InstanceService` (bindings: `ListInstances`, `GetInstance`, `CreateInstance`, `UpdateInstance`, `DeleteInstance`, `DeleteInstanceAndProfile`, `ListProxies`) over the Stage 2 store: validated instance metadata CRUD with Chinese, user-facing constraint messages. `DeleteInstance` removes metadata only and never touches profile files; `DeleteInstanceAndProfile` is the explicit destructive path and refuses targets outside `data/profiles`, the profiles root itself, and persisted-running instances. There is no process manager yet, so at startup any instance still persisted as `running` is reconciled to `stopped` (`Store.ReconcileInstanceStatuses`) — with the single-instance lock, such a row can only be a leftover of a dead process. No schema migration was needed for Stage 3.

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
