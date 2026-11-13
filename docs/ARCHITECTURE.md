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

Stage 3 also adds `InstanceService` (bindings: `ListInstances`, `GetInstance`, `CreateInstance`, `UpdateInstance`, `DeleteInstance`, `DeleteInstanceAndProfile`, `ListProxies`) over the Stage 2 store: validated instance metadata CRUD with Chinese, user-facing constraint messages. `DeleteInstance` removes metadata only and never touches profile files; `DeleteInstanceAndProfile` is the explicit destructive path and refuses persisted-running instances, the profiles root itself, and anything that cannot be PROVEN to stay inside `data/profiles` by real-path resolution: the target itself must not be a symlink/reparse point, and every path component must resolve (intermediate symlinks and Windows junctions included; `GetFinalPathNameByHandle` on Windows, `EvalSymlinks` on Unix) to a location still strictly inside the resolved profiles directory. Deletion is then failure-atomic and OBJECT-anchored rather than a bare recursive remove (`internal/pfiles`): the move into a quarantine slot under `profiles/` acts on the verified filesystem object, never on the verified path string — on Linux the descent from the profiles root is anchored to directory file descriptors (`openat` with `O_NOFOLLOW` at every component, the root itself opened `O_NOFOLLOW`) and the move is `renameat2(RENAME_NOREPLACE)` between the two pinned fds, with a post-move `(dev, ino)` identity check; on Windows the target directory is opened with `FILE_FLAG_OPEN_REPARSE_POINT`, its identity is verified through the handle (reparse attributes plus `GetFinalPathNameByHandle` against the verified parent), and the rename itself is performed through that handle (`SetFileInformationByHandle`/`FileRenameInfo`, no-replace), so swapping any path component after verification cannot redirect the move. The database record is deleted next; on failure the object is moved back through the same anchor (on Windows the restore first re-verifies the original parent's identity and refuses, leaving the data in quarantine, if the parent now resolves elsewhere), and only with the record gone is the quarantine tree wiped by `internal/safedelete`, with the object identity chain unbroken from verification to deletion: the verified handle is NOT closed before the wipe — on Linux the wipe runs as `RemoveVerifiedAt` relative to the pinned profiles-root fd (the quarantine slot must still hold the exact `(dev, ino)` object verified at move time, and every entry's identity is re-verified immediately before it is unlinked), and on Windows the very handle that was verified and renamed is handed to the wipe engine, which enumerates children FROM directory handles (`GetFileInformationByHandleEx` with the extended directory classes `FileIdExtdDirectoryRestartInfo`/`FileIdExtdDirectoryInfo` — the documented Win32 `FILE_INFO_BY_HANDLE_CLASS` family — yielding per-child 128-bit file IDs), opens each child RELATIVE to its verified parent handle (`NtCreateFile` `RootDirectory`, deliberately WITHOUT `FILE_SHARE_DELETE`, so an opened object cannot be renamed out of the tree by another process while the wipe holds it), requires the opened child's 128-bit identity (volume serial plus `FileId` from `FileIdInfo`) to equal its enumerated identity (a swap between enumeration and open aborts the wipe; a filesystem that cannot provide the extended classes or a 128-bit identity fails the wipe closed rather than degrading the check; the identity guarantee is contemporaneous rather than a permanent object credential — Microsoft documents that file IDs may be reused after an object is deleted, while on NTFS an object keeps its ID until deletion — so the comparison binds only the enumerate→open window, which the share modes then close), and deletes through the verified handles (`FileDispositionInfo`), reparse points as link objects only. Linux guarantee boundary: descriptor-anchored deletion follows verified inodes and re-verifies identity before every unlink, so an object that was never verified inside the boundary can never be deleted — but Linux has no share modes, so a same-UID process that can rename entries inside the tree can move a verified object out mid-wipe and deletion follows that object; this residual requires a hostile same-UID process with write access inside the profile tree and is outside the single-user desktop threat model (Windows closes it via share modes). No path string is re-resolved at any depth on either platform, so replacing the profiles root or any intermediate path during the wipe cannot redirect the deletion. On Darwin the standard library offers no descriptor-relative or handle-based rename, so the destructive operation fails closed (refused, data and record intact) rather than falling back to a path-based move. There is no process manager yet, so at startup any instance still persisted as `running` is reconciled to `stopped` (`Store.ReconcileInstanceStatuses`) — with the single-instance lock, such a row can only be a leftover of a dead process. No schema migration was needed for Stage 3.

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

## Credential storage (Stage 4A, internal/secrets)

All proxy credentials and subscription URLs live in one encrypted vault file, `data/secrets.vault` (`paths.Layout.SecretsFile`). SQLite holds only the opaque `secret_ref`; plaintext never reaches the database, logs, settings files or frontend persistent state.

- **Encryption**: Windows DPAPI (CurrentUser) in production. DPAPI binds decryption to the same Windows user on the same machine: moving the portable folder on one machine keeps working; another machine or user makes entries undecryptable, which the store reports per entry (`ErrCorrupt`) so the UI can ask for re-entry — entries are retained, never silently dropped or downgraded to "no credential". Unix builds use an AES-256-GCM keyfile protector as a development stand-in only.
- **Consistency with SQLite**: blobs are immutable. Create = persist the secret first, then commit the DB reference (compensating delete on DB failure). Update = persist a new entry, swap the reference in one transaction, then delete the old entry. Delete = DB row first, vault entry second. `Put` reports success only after the new vault file is durably in place, so the database can never reference an unpersisted secret; cleanup failures leave identifiable orphans, collected by a garbage sweep that only removes unreferenced entries older than 24 hours (the age gate covers the crash window between a durable Put and its DB commit; references shared by several rows are live and untouchable).
- **Shared-reference deletion contract (binding on ProxyService)**: the vault API has NO unconditional delete. Compensation uses `DeleteCompensating`, which only ever applies to a reference that is still pending (created by this process, never committed) and therefore cannot be referenced or shared. All business cleanup uses `DeleteIfUnreferenced`, whose live-reference check runs under the vault lock at decision time. ProxyService must serialize its reference-creating/dropping DB mutations together with these cleanup calls under one service-level mutex, making check-and-delete atomic with respect to reference changes; the single-instance lock excludes other processes.
- **ProxyService (Stage 4A, implemented to that contract)**: `internal/service.ProxyService` serializes every ref-creating/dropping mutation and cleanup under its service mutex; create/update/delete follow the vault-first order (Put → DB transaction → MarkCommitted / guarded cleanup), transaction failures after a Put are compensated with `DeleteCompensating`, and cleanup uses `DeleteIfUnreferenced` with an in-use check spanning both `proxies` and `subscriptions`. Startup runs one garbage-collection pass with the database-wide live set. Read paths return credential-free views (`ProxyView`): only `hasSecrets`, the secret field names and a status — a ref that cannot be loaded reports `unavailable`, never a silent downgrade to credential-less. The Stage 4A connection test is a TCP reachability check recorded on the proxy row; protocol-level delay/exit-IP testing arrives with the Mihomo core in Stage 4C. Every persistence also verifies, after the atomic replace, that the file on disk equals the intended state entry-for-entry (version, reference set, per-entry timestamp and blob — a matching count is not accepted as proof), and verifies the ACL of every credential-bearing file (temp files before replace, final main and backup files after replace, and both files again on open); any verification failure fails the operation closed.
- **Atomicity and recovery (Windows)**: every write goes temp file → fsync → permission restriction → snapshot of the previous good file to `secrets.vault.bak` → `MoveFileExW` with `MOVEFILE_REPLACE_EXISTING|WRITE_THROUGH` (plain `os.Rename` replace semantics are not assumed on Windows) → post-write verification read. A corrupt main file falls back to the backup; if both are corrupt they are quarantined by rename (never deleted) and the store starts empty with every reference resolving to "credential unavailable".
- **File permissions**: the vault and its backup carry a protected DACL granting full access to exactly the current user, Local SYSTEM and Administrators — applied to the file itself via SDDL (`D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;<user SID>)`), severing inheritance from the portable folder, and verified by reading the descriptor back on every open. If permissions cannot be tightened, the store refuses to open or write rather than continue quietly. The same `secrets.RestrictFile` treatment is REQUIRED in Stage 4C for the Mihomo controller secret and the generated runtime config (which contains plaintext credentials by Mihomo's input format).

## Instance readiness verification (Stage 4C constraint, recorded in 4A)

A proxied instance is "ready" only when the Mihomo core itself confirms it through the external controller — a successful TCP connection to the listener port is NOT sufficient evidence, because any other process could be occupying that port. Readiness (Stage 4C's `VerifyInstanceReady`) must establish, via the controller: (1) the running core is the expected version and healthy; (2) the instance's listener exists in the core's applied configuration; (3) its bind address, port and bound outbound match the intended assignment; (4) the applied configuration corresponds to the current database state. Any mismatch means not ready, and a not-ready proxied instance must fail closed (Stage 5 must not launch it, and must never fall back to a direct connection).

## Security notes

- CDP debugging ports are never exposed publicly; they bind to localhost only (Stage 3).
- The frontend cannot execute arbitrary commands; it can only call the explicitly registered service methods.
- Secrets (credentials, subscription URLs, private proxy configuration) are never stored in SQLite in plain committed files and never enter this repository — see `docs/DATABASE.md` and `.gitignore`.

## Deliberately excluded

Electron, Docker, Redis, MongoDB, GraphQL and microservices are out of scope (AGENTS.md section 3). The scaffold's mobile and Docker assets are not part of this project; the only supported target is Windows 11 x64.
