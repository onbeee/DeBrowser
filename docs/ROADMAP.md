# DeBrowser Roadmap

Development follows AGENTS.md section 9: small, testable stages, one Pull Request per stage, merged only after project-owner approval.

Stage numbering evolved during 2026-11-13 as the product definition was revised several times (delivered Stages 1–3 and 4A were never renumbered). The FINAL order, fixed by product-owner decision on 2026-11-13, is the table below: the first-launch welcome page was cancelled (Stage 5A is direct startup), and full backup & restore is the LAST functional stage (Stage 12), implemented only after every other data-producing feature exists. Earlier intermediate numberings mentioned in dated historical notes are superseded by this table.

| Stage | Scope | Status |
| --- | --- | --- |
| 1 | Repository foundation and application shell | Delivered (PR #1, #2) |
| 2 | SQLite database and migrations | Delivered (PR #3) |
| 3 | App lifecycle and instance management foundation | Delivered (PR #4) |
| 4A | Proxy data foundation: credential vault, proxy schema, ProxyService, proxy UI | Delivered (PR #5, merged 2026-11-13) |
| 5A | Direct startup and portable data directory stability | Delivered (PR #6, merged 2026-11-13) |
| 5B | Logging and diagnostics (run/operation/error logs, retention, diagnostics export) | Delivered (PR #7, merged 2026-11-13) |
| 6 | Subscription management and full Mihomo config import (incl. native proxy-groups) | Delivered (PR #8, squash-merged; main `44e96e42`) |
| 6C | Unified proxy configuration — **superseded by the PR #9 scope cleanup (2026-11-15)**: the proxy/subscription/Mihomo features built in this PR were removed again by project-owner decision before merge; see the cleanup note below | Closed as a feature; PR #9 repurposed as the scope-cleanup PR |
| 7 | Mihomo core integration and run modes — **cancelled with the proxy scope**; whether proxy capability returns is a future re-evaluation (see below) | Cancelled |
| 8 | Rule management (airport / custom / mixed rule sources) — **cancelled with the proxy scope**; rules only made sense on top of a proxy runtime, which is no longer in the product plan | Cancelled |
| 9 | Browser kernels, instance launch (per-instance profiles, CDP ports, lifecycle); bookmarks | Planned — current development focus |
| 10 | Multi-window synchronization | Planned |
| 11 | In-app update (after a code-signing / trusted-release solution) and release | Planned |
| 12 | Full backup and restore (portable backup package, independent backup password) — final stage | Planned |

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

- Core schema (proxies, browser instances, bookmarks) as versioned migrations 0002/0003 on top of the Stage 1 foundation; the kernels table lands with kernel management in Stage 9
- Repository layer (`internal/store`) over shared `internal/model` types, a `WithTx` transaction entry for cross-repository atomic work, and `synchronous=FULL`
- Verified pre-migration backups (`VACUUM INTO`) that abort the upgrade when they fail

## Stage 3 — Application lifecycle and instance management foundation (delivered)

- App-level single-instance protection per portable installation, with stale-lock recovery and focus-the-running-window activation
- Application lifecycle governance: context-owned background loops that drain on shutdown, `run() error` instead of mid-startup `log.Fatalf`, ordered resource release (loops → database → lock → log)
- Instance management foundation: validated metadata CRUD over the Stage 2 store, startup status reconciliation, and a profile-deletion safety model that separates metadata deletion from profile data deletion
- Still to come in later stages: kernel detection, instance start/stop with managed Chromium processes and CDP wiring (localhost only)

## Stage 4A — Proxy data foundation (PR #5)

> **Superseded in part (2026-11-15)**: `ProxyService` and the proxy page were removed by the PR #9 scope cleanup. The credential vault, migration 0004 and the stored data remain as a compatibility layer.

- Credential vault (`internal/secrets`): DPAPI-protected file store in the portable directory, opaque references in SQLite, vault-first write protocol with guarded cleanup and orphan GC
- Migration 0004: `subscriptions` metadata (URL only in the vault), proxy source/dedup/test fields, `browser_instances.proxy_listen_port`
- `ProxyService` and the proxy management page: manual node CRUD, credential handling that never round-trips plaintext to the UI, TCP reachability test recorded per node
- **Correction (2026-11-13)**: DeBrowser's own proxy groups and tags were removed again by migration 0005 (product decision — the feature is out of scope). This is unrelated to Mihomo's native `proxy-groups`, which are part of imported airport configurations and are kept for Stage 6.

## Stage 5A — Direct startup and stable data directory

- Direct startup with automatic initialization: double-clicking `DeBrowser.exe` resolves the portable layout, creates any missing directories, opens (creating when needed) the SQLite database and applies pending schema migrations, opens the credential vault, loads settings and services, and opens the main window. There is NO first-launch welcome or choice page — no 「新用户 / 导入数据」 selection; backup import is not a startup feature (it belongs to the backup & restore stage, Stage 12)
- The formal portable layout is unchanged (same `data/`, `config/`, `webview2-data/` paths); existing Stage 4A installations start exactly as before, with their data untouched, and replacing the executable never touches user data
- No scanning, detection or automatic migration of legacy data anywhere (`internal/migration` and the automatic Roaming migration are removed); the SQLite schema migration mechanism is retained unchanged

## Stage 5B — Logging and diagnostics (delivered, PR #7)

- Run / operation / error log separation with retention (30 / 90 / 90 days) and size caps
- Operation audit for instance changes, theme changes and diagnostic exports; no secrets in logs
- In-app diagnostics view and one-click diagnostics/acceptance record export

## Stage 6 — Subscription and full configuration import (delivered, PR #8)

> **Superseded (2026-11-15)**: the features described in this section were removed again by the PR #9 scope cleanup (project-owner decision) — see the cleanup section below. The historical record of what PR #8 delivered is kept unchanged.

- Four import formats through one pipeline: full Clash/Mihomo YAML, proxies-only YAML, V2Ray/Base64 URI lists, local config files; unsupported content errors explicitly, never silently dropped
- Native Mihomo `proxy-groups` parsed into structured storage; members resolve by stable node identity (rendered names unique per node), never silently rebound to a same-named node from another subscription
- Full-config management: original artifacts stored with credentials stripped into the vault; Stage 6 records a security report of risky settings (allow-lan, external listeners/controller, missing controller secret, TUN, external DNS listen) — the safe defaults themselves (localhost-only listeners/DNS/controller, controller secret, TUN off, allow-lan off) were planned to be enforced when Stage 7 rendered a runnable config; Stage 7 has since been cancelled, so that enforcement is no longer part of the product plan

## Stage 6C / PR #9 — Product scope cleanup (project-owner decision, 2026-11-15)

PR #9 no longer delivers a proxy feature. By the project owner's decision it now removes one:

- **Removed**: basic proxy server management (page, `ProxyService`, bindings); subscription management and config import (subscription UI, `SubscriptionService`, the SSRF-guarded fetcher including the Fake-IP fallback, `internal/mihomoimport`, local YAML import); the unified 「代理配置」 page (cards, node list, group views); and the Mihomo core runtime (`internal/mihomocore`, `CoreService`, settings kernel section, CI bundling — the build artifact is the executable only; the app does not download, install or start a core).
- **Sidebar fixed at eight entries**: 总览、浏览器实例、代理配置、浏览器内核、书签管理、多开同步、数据管理、系统设置. 「代理配置」 opens a placeholder page that only states the feature is not yet open.
- **Data management**: logs, operation audit and diagnostics (formerly the standalone 「诊断」 entry) are unified under 「数据管理」, which also hosts the data overview and the (not yet open) backup & restore area. The fault report no longer contains any core, subscription or proxy fields.
- **Data protection**: migrations 0001–0009 are untouched; no table, column or row was deleted; the vault file and entries are preserved and still opened at startup; existing instances and profiles are unaffected. The historical proxy/subscription/config tables remain in the schema and in `internal/model`; the `internal/store` layer over them was converged to read-only vault reference queries (no create/update/delete methods remain).
- **Final review round**: the instance page's proxy column, form selector and `InstanceService.ListProxies` were removed as well — instance creation always writes `proxy_id` NULL and updates preserve a historical row's stored `proxy_id` verbatim, so no code path can create or change an instance proxy binding, while historical bindings stay readable as data.
- **Current focus** (owner's phased route): (1) Chromium kernel download/install/version management, real instance launch/stop with isolated profiles and per-instance CDP ports, 3–5 instances verified on Windows 11; (2) bookmarks, browser basics, multi-window synchronization; (3) future re-evaluation of proxy capability; (4) in-app update, then full backup & restore (Stage 12, last).

## Stage 7 — (cancelled)

Mihomo core integration and run modes were cancelled together with the proxy product scope in the PR #9 cleanup. Proxy capability may be re-evaluated in a future phase; nothing in the current code starts, bundles or manages a proxy core.

## Stage 8 — Rule management (cancelled)

Cancelled in the PR #9 scope cleanup, together with the proxy scope it depended on: rule sources only have meaning on top of a proxy runtime. Whether proxy capability — and with it any rule management — is ever reintroduced will be evaluated separately in the future; it is not part of the current plan.

## Stage 9 — Browser kernels and real instance launch

- Chromium kernel download, installation and version management (verified downloads, `kernels` table)
- Real browser instance start and stop, with process lifecycle management
- Independent per-instance profiles: Cookie, Local Storage and cache isolation; a user data directory is never shared between running instances
- An independent localhost CDP port per instance
- Changing one instance never affects others
- Bookmark management delivered with this stage
- No proxy wiring: instances no longer expose any proxy binding in the UI or service API (a historical `proxy_id` value stored on old rows is preserved as data only)

Stage 9 is delivered in parts:

- **Stage 9A — browser discovery and kernel management** (delivered in this stage's first PR): the app first detects and uses the user's already-installed Google Chrome and Brave (registry App Paths and uninstall records plus well-known install directories; version-resource identity plus Authenticode publisher verification; results are live-only and never persisted, and system browsers are never copied, modified or deleted). Downloading a DeBrowser-managed Chromium is optional, never a first-run prerequisite: managed builds come only from the official Chrome for Testing Stable metadata (HTTPS + host whitelist, per-hop redirect validation), are verified (ZIP structure, version match, Authenticode/Google LLC) and installed atomically under `kernels/<version>` (migration 0010 `kernels` table, managed kernels only), with guarded deletion. No browser is launched in 9A.
- **Stage 9B — real instance launch** (next): real browser start/stop and process lifecycle, per-instance profile creation and isolation, per-instance CDP ports.
- **Stage 9C — bookmarks** (after 9B): bookmark frontend wiring on the existing store CRUD.

## Stage 10 — Multi-window synchronization

- Synchronized actions across multiple running instances, with an explicit synchronization scope, failure handling and instance-isolation boundaries

## Stage 11 — In-app update and release

- In-app update via GitHub Releases only after a code-signing / trusted release-signing solution exists (until then, manual downloads from GitHub Releases); updates verify downloads, preserve all portable data directories, and roll back on failure

## Stage 12 — Full backup and restore (final stage)

Backup & restore is deliberately the last functional stage: it must cover the complete user-data system in its final form (instances, kernels, bookmarks, multi-window state, update state). Fixing a full backup format before those exist would force constant format extensions, so this stage starts with an inventory of everything the finished application persists.

- User-created `.dbak` backup packages (manifest + per-file SHA-256), saveable outside the application folder; the backup format carries its OWN format version, independent of the application version, with compatibility/conversion for historical backup formats
- Independent backup password (PBKDF2-HMAC-SHA256 from the Go standard library + AES-256-GCM) so backups restore across machines, Windows users and application versions — DPAPI alone is not the cross-machine mechanism
- An explicit inventory decides what is backed up versus what is re-downloaded or rebuilt — never a blind copy of the whole program folder
- Restore validates the complete backup in a staging area before any current data is replaced; a failed restore leaves current data untouched
- Backup export, import and restore live here as a standalone feature — they are not part of the startup flow, and no backup/restore user feature is added in Stages 5A–11. (The pre-migration `VACUUM INTO` snapshots, integrity checks and vault mechanisms in the data layer are low-level safety facilities, not this feature.)
