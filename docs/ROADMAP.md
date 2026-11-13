# DeBrowser Roadmap

Development follows AGENTS.md section 9: small, testable stages, one Pull Request per stage, merged only after project-owner approval.

Stage numbering was revised on 2026-11-13 after the product-requirements review: the former Stage 4B/4C work moved to Stages 6/7, the former Stage 5 split and moved later, and first-launch / backup / logging foundations were pulled forward as Stages 5A/5B/5C. Historical stage numbers 1–3 and 4A are unchanged.

Revised again later on 2026-11-13 by product-owner decision: the Stage 5A first-launch welcome page (新用户 / 导入数据 choice) was cancelled — first launch is a direct startup with automatic initialization — and backup & restore moved from 5B to Stage 7, shifting the former Stages 7–11 to 8–12. Logging and diagnostics became Stage 5B.

| Stage | Scope | Status |
| --- | --- | --- |
| 1 | Repository foundation and application shell | Delivered (PR #1, #2) |
| 2 | SQLite database and migrations | Delivered (PR #3) |
| 3 | App lifecycle and instance management foundation | Delivered (PR #4) |
| 4A | Proxy data foundation: credential vault, proxy schema, ProxyService, proxy UI | Delivered (PR #5, merged 2026-11-13) |
| 5A | Direct startup: automatic initialization of the portable data directory; removal of the Stage 1 Roaming auto-migration | Implemented in the Stage 5A pull request; pending project-owner review |
| 5B | Logging and diagnostics (run/operation/error logs, retention, diagnostics export) | Planned |
| 6 | Subscription and full Mihomo config import (incl. native proxy-groups) | Planned |
| 7 | Backup and restore (portable backup package, independent backup password) | Planned |
| 8 | Mihomo core integration and run modes (Rule / Global / Direct) | Planned |
| 9 | Rule management (airport / custom / mixed rule sources) | Planned |
| 10 | Browser kernels, instance launch and per-instance proxy wiring; bookmarks | Planned |
| 11 | Multi-window synchronization | Planned |
| 12 | In-app update (after a code-signing / trusted-release solution) and release | Planned |

## Stage 1 — Repository foundation and application shell

- Wails v3.0.0 + Go 1.26 + React 18 + TypeScript + Vite project, Windows 11 x64 target
- Modular monolith layout (`internal/` backend modules, `frontend/` UI)
- Configuration management and data-directory layout (portable layout next to the executable, adopted after Stage 1). Note: Stage 1 originally used `%AppData%\DeBrowser` with an automatic migration of existing data; that automatic migration conflicts with the first-launch rule decided on 2026-11-13 and is removed in Stage 5A.
- Structured JSON logging infrastructure
- SQLite infrastructure: WAL, foreign keys, transactional migrations (foundation schema only)
- `AppService` with live status over bindings and the typed `app:status` event
- Base desktop UI: sidebar navigation, top toolbar, main area, bottom status bar, console panel
- Engineering docs and GitHub Actions CI on a Windows x64 runner
- `.gitignore` rules keeping databases, profiles, cookies, secrets and build output out of git

## Stage 2 — SQLite database and migrations

- Core schema (proxies, browser instances, bookmarks) as versioned migrations 0002/0003 on top of the Stage 1 foundation; the kernels table lands with kernel management in Stage 10
- Repository layer (`internal/store`) over shared `internal/model` types, a `WithTx` transaction entry for cross-repository atomic work, and `synchronous=FULL`
- Verified pre-migration backups (`VACUUM INTO`) that abort the upgrade when they fail

## Stage 3 — Application lifecycle and instance management foundation (delivered)

- App-level single-instance protection per portable installation, with stale-lock recovery and focus-the-running-window activation
- Application lifecycle governance: context-owned background loops that drain on shutdown, `run() error` instead of mid-startup `log.Fatalf`, ordered resource release (loops → database → lock → log)
- Instance management foundation: validated metadata CRUD over the Stage 2 store, startup status reconciliation, and a profile-deletion safety model that separates metadata deletion from profile data deletion
- Still to come in later stages: kernel detection, instance start/stop with managed Chromium processes and CDP wiring (localhost only)

## Stage 4A — Proxy data foundation (PR #5)

- Credential vault (`internal/secrets`): DPAPI-protected file store in the portable directory, opaque references in SQLite, vault-first write protocol with guarded cleanup and orphan GC
- Migration 0004: `subscriptions` metadata (URL only in the vault), proxy source/dedup/test fields, `browser_instances.proxy_listen_port`
- `ProxyService` and the proxy management page: manual node CRUD, credential handling that never round-trips plaintext to the UI, TCP reachability test recorded per node
- **Correction (2026-11-13)**: DeBrowser's own proxy groups and tags were removed again by migration 0005 (product decision — the feature is out of scope). This is unrelated to Mihomo's native `proxy-groups`, which are part of imported airport configurations and are kept for Stage 6.

## Stage 5A — Direct startup and stable data directory

- Direct startup with automatic initialization: double-clicking `DeBrowser.exe` resolves the portable layout, creates any missing directories, opens (creating when needed) the SQLite database and applies pending schema migrations, opens the credential vault, loads settings and services, and opens the main window. There is NO first-launch welcome or choice page — no 「新用户 / 导入数据」 selection; backup import is not a startup feature (it belongs to the backup & restore stage, Stage 7)
- The formal portable layout is unchanged (same `data/`, `config/`, `webview2-data/` paths); existing Stage 4A installations start exactly as before, with their data untouched, and replacing the executable never touches user data
- No scanning, detection or automatic migration of legacy data anywhere (`internal/migration` and the automatic Roaming migration are removed); the SQLite schema migration mechanism is retained unchanged

## Stage 5B — Logging and diagnostics

- Run / operation / error log separation with retention (30 / 90 / 90 days) and size caps
- Operation audit for instance/proxy changes, subscription imports, mode and rule changes, backup/restore and updates; no secrets in logs
- In-app diagnostics view and one-click diagnostics/acceptance record export

## Stage 6 — Subscription and full configuration import

- Four import formats through one pipeline: full Clash/Mihomo YAML, proxies-only YAML, V2Ray/Base64 URI lists, local config files; unsupported content errors explicitly, never silently dropped
- Native Mihomo `proxy-groups` parsed into structured storage; members resolve by stable node identity (rendered names unique per node), never silently rebound to a same-named node from another subscription
- Full-config management: original artifacts stored with credentials stripped into the vault; security defaults enforced on render (localhost-only listeners/DNS/controller, controller secret, TUN off, allow-lan off) with every adjustment reported to the user

## Stage 7 — Backup and restore

- User-created `.dbak` backup packages (manifest + per-file SHA-256), saveable outside the application folder
- Independent backup password (PBKDF2-HMAC-SHA256 from the Go standard library + AES-256-GCM) so backups restore on other machines/users — DPAPI alone is not the cross-machine mechanism
- Restore validates fully in a staging area before touching current data; a failed restore leaves current data untouched
- Backup export, import and restore live here as a standalone feature — they are not part of the startup flow

## Stage 8 — Mihomo core integration and run modes

- Pinned, SHA-256-verified Mihomo core download/install; single core process with per-instance listeners on 127.0.0.1
- Render → `mihomo -t` validate → atomic swap → controller hot-reload; failures keep the previous working config
- Core-level run modes Rule / Global / Direct; per-instance readiness verification (`VerifyInstanceReady`) and fail-closed behaviour

## Stage 9 — Rule management

- Rule sources for Rule mode: airport rules / custom rules / mixed (user rules first, airport base preserved, subscription updates never overwrite user rules)
- Original config, user rules and rendered runtime config kept as three separate artifacts

## Stage 10 — Browser kernels, instance launch and proxy wiring

- Kernel management (verified downloads, `kernels` table), instance start/stop with isolated user-data dirs and localhost CDP
- Per-instance proxy binding (fixed node / native group / follow run-mode); changing one instance never affects others; proxy failure is surfaced and fails closed — never a silent direct leak
- Bookmarks delivered with this stage

## Stage 11 — Multi-window synchronization

- Synchronized actions across multiple running instances

## Stage 12 — In-app update and release

- In-app update via GitHub Releases only after a code-signing / trusted release-signing solution exists (until then, manual downloads from GitHub Releases); updates verify downloads, preserve all portable data directories, and roll back on failure
