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
| 6C | Unified proxy configuration, redone 2026-11-14: single-column 「代理配置」 page (quick subscription import, config cards, read-only node list, create dialog); forward-reference import fix; Fake-IP system-proxy fallback; custom nodes/members removed; official Mihomo core bundled with minimal lifecycle + manual kernel update in settings | Implemented in the Stage 6C pull request (redo); pending project-owner review |
| 7 | Mihomo core integration and run modes (Rule / Global / Direct) | Planned — starts only after Stage 6C is delivered and accepted |
| 8 | Rule management (airport / custom / mixed rule sources) | Planned |
| 9 | Browser kernels, instance launch and per-instance proxy wiring; bookmarks | Planned |
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
- Operation audit for instance/proxy changes, subscription imports, mode and rule changes, backup/restore and updates; no secrets in logs
- In-app diagnostics view and one-click diagnostics/acceptance record export

## Stage 6 — Subscription and full configuration import (delivered, PR #8)

- Four import formats through one pipeline: full Clash/Mihomo YAML, proxies-only YAML, V2Ray/Base64 URI lists, local config files; unsupported content errors explicitly, never silently dropped
- Native Mihomo `proxy-groups` parsed into structured storage; members resolve by stable node identity (rendered names unique per node), never silently rebound to a same-named node from another subscription
- Full-config management: original artifacts stored with credentials stripped into the vault; Stage 6 records a security report of risky settings (allow-lan, external listeners/controller, missing controller secret, TUN, external DNS listen) — the safe defaults themselves (localhost-only listeners/DNS/controller, controller secret, TUN off, allow-lan off) are enforced when Stage 7 renders a runnable config, with every adjustment reported to the user

## Stage 6C — Unified proxy configuration (redone 2026-11-14 in the same pull request, pending review)

- Page: one 「代理配置」 page in a single column — a top action bar (paste a subscription URL and import directly, or open the create dialog), a vertical list of configuration cards (name, 远程/本地 source, node and group counts, update time, last refresh state and failure reason; select / refresh / delete with confirmation), and below it the selected configuration's node list as a read-only table (name, protocol, address, port, status — never credentials), with the native proxy-groups shown read-only underneath. The create dialog makes remote (name + subscription URL) or local (name + native file picker, choose first then create) configurations. Custom nodes and custom group membership were REMOVED by product decision: nodes come only from subscriptions and local files. Stage 4A's stored custom-node records and credentials are untouched in the database; only the unshipped PR #9 feature surface was withdrawn
- Data model: migration 0007's unified source vocabulary (`custom` / `file` / `subscription`) is kept. Migration 0008 stays in the published history (the PR #9 acceptance builds already applied it); the overlay table it created is dropped by migration 0009, which harms no other data (populated-fixture tested)
- Import correctness: `ReplaceProxyGroups` now inserts all group rows before all member rows — real-world configs whose first group references a group defined later (a forward reference) previously died on the `target_group_id` foreign key and surfaced as 「关联数据不存在或已被删除」
- Subscription downloads in Fake-IP environments: when the whole DNS answer is rejected AND every rejected address is inside the Clash/Mihomo Fake-IP pool (198.18.0.0/15), the placeholder addresses are a local proxy core's mappings that no application can dial directly, so the fetch retries through the operating system's configured proxy (which receives the hostname), accepting only loopback/private proxy endpoints. The direct path's SSRF policy is unchanged, 198.18.0.0/15 stays refused for direct dials, mixed answers get no fallback, and environment proxy variables are never used. DNS rejection errors still name the offending IP and its category
- Mihomo core (pulled forward by the 2026-11-14 instruction; run modes remain Stage 7): the official MetaCubeX binary (pinned v1.19.34, SHA-256 in `internal/mihomocore/release.go`) ships unmodified beside DeBrowser.exe and is copied into `data/mihomo/` on first run. The app reads its real version, can start/stop it with a minimal loopback-only configuration (no proxy listeners, no TUN, no system-proxy changes), shows its log, and stops it on exit. System settings host the kernel section: current version, latest version, check time, manual check button, and — when a newer stable exists — a manual update that downloads the official asset, verifies its SHA-256 against the official release digest, verifies the extracted binary's version, and swaps with rollback (the old core survives any failure). Nothing checks or updates automatically
- Shell: the top-right persistent refresh-status and console controls are gone; errors surface in-page and detail lives in 日志与诊断， which also offers the extended fault-report ZIP (redacted, fail-closed scanning) designed to be handed straight to support/ChatGPT

## Stage 7 — Mihomo core integration and run modes


- Core lifecycle basics (install, version, start/stop, verified manual update) were delivered early in the Stage 6C redo; this stage adds the traffic plane: single core process with per-instance listeners on 127.0.0.1
- Render → `mihomo -t` validate → atomic swap → controller hot-reload; failures keep the previous working config
- Core-level run modes Rule / Global / Direct; core process lifecycle management; per-instance readiness verification (`VerifyInstanceReady`) and fail-closed behaviour

## Stage 8 — Rule management

- Rule sources for Rule mode: airport rules / custom rules / mixed (user rules first, airport base preserved, subscription updates never overwrite user rules)
- Original config, user rules and rendered runtime config kept as three separate artifacts

## Stage 9 — Browser kernels, instance launch and proxy wiring

- Kernel management (verified downloads, `kernels` table), instance start/stop with isolated user-data dirs and localhost CDP
- Per-instance proxy binding (fixed node / native group / follow run-mode); changing one instance never affects others; proxy failure is surfaced and fails closed — never a silent direct leak
- Bookmarks delivered with this stage

## Stage 10 — Multi-window synchronization

- Synchronized actions across multiple running instances, with an explicit synchronization scope, failure handling and instance-isolation boundaries

## Stage 11 — In-app update and release

- In-app update via GitHub Releases only after a code-signing / trusted release-signing solution exists (until then, manual downloads from GitHub Releases); updates verify downloads, preserve all portable data directories, and roll back on failure

## Stage 12 — Full backup and restore (final stage)

Backup & restore is deliberately the last functional stage: it must cover the complete user-data system in its final form (subscriptions, proxy configurations, native proxy-groups, rules, instances, kernels, bookmarks, multi-window state, update state). Fixing a full backup format before those exist would force constant format extensions, so this stage starts with an inventory of everything the finished application persists.

- User-created `.dbak` backup packages (manifest + per-file SHA-256), saveable outside the application folder; the backup format carries its OWN format version, independent of the application version, with compatibility/conversion for historical backup formats
- Independent backup password (PBKDF2-HMAC-SHA256 from the Go standard library + AES-256-GCM) so backups restore across machines, Windows users and application versions — DPAPI alone is not the cross-machine mechanism
- An explicit inventory decides what is backed up versus what is re-downloaded or rebuilt — never a blind copy of the whole program folder
- Restore validates the complete backup in a staging area before any current data is replaced; a failed restore leaves current data untouched
- Backup export, import and restore live here as a standalone feature — they are not part of the startup flow, and no backup/restore user feature is added in Stages 5A–11. (The pre-migration `VACUUM INTO` snapshots, integrity checks and vault mechanisms in the data layer are low-level safety facilities, not this feature.)
