# DeBrowser

A lightweight, local-first desktop application for managing multiple isolated browser profiles.

DeBrowser is built for **Windows 11 x64**. Each browser instance gets its own user data directory, configuration and process — profiles are never shared between running instances.

> **Status: Stages 1–6 and the PR #9 product-scope cleanup are delivered and merged; Stage 9A (manual browser registration and kernel management) is in review.** The repository contains the application shell, the SQLite data layer, instance management foundations, the credential vault as a data-compatibility layer, and structured logging / operation audit / diagnostics (unified under the 「数据管理」 sidebar entry). By owner decision, proxy server management, subscription management and config import, the unified proxy configuration UI and the Mihomo core runtime are **not** part of the current product scope: they were removed in PR #9, and the 「代理配置」 sidebar entry is a placeholder page stating the feature is not yet open. The sidebar is fixed at exactly eight entries: 总览、浏览器实例、代理配置、浏览器内核、书签管理、多开同步、数据管理、系统设置. Stage 9A does not detect browsers automatically — no registry scanning, no disk scanning, no auto-discovery of any kind. The user adds a browser in one of two explicitly separate ways: (1) manual registration is a **pure path registration** — the user picks any program executable in the file picker and DeBrowser stores only its path and a display name (persisted across restarts), with **no** identity, version, signature or publisher verification of any kind; the registered program is never launched, copied, modified or deleted by DeBrowser, and removing a registration deletes only the database record; or (2) the user explicitly downloads a standalone Chromium build that DeBrowser manages from the official Chrome for Testing channel — that managed download/install flow, and only that flow, includes download and installation verification (including Authenticode/Google publisher checks on the managed build). Downloading is never a first-run prerequisite. Real multi-instance launching follows in Stage 9B; bookmarks, synchronization, in-app update and (last) full backup & restore follow in later stages — see [docs/ROADMAP.md](docs/ROADMAP.md). Whether proxy capability returns is re-evaluated in a future stage. No placeholder data stands in for undelivered features.

## Tech stack

| Layer | Technology | Version |
| --- | --- | --- |
| Desktop framework | Wails | v3.0.0 |
| Backend | Go | 1.26 (module declares `go 1.26.0`) |
| Frontend | React + TypeScript | React 18.3.1, TypeScript 5.9.3 |
| Build tool | Vite | 8.3.7 |
| Styling | Tailwind CSS | 4.3.5 |
| Icons | lucide-react | 0.469.0 |
| State management | Zustand | 5.1.0 |
| Database | SQLite via modernc.org/sqlite | 1.62.0 |
| Proxy engine | — | not in current scope (removed in PR #9; future re-evaluation) |
| Browser control | Chrome DevTools Protocol | arrives in Stage 9 |

All dependency versions are pinned (Go: `go.mod`/`go.sum`, frontend: `package.json`/`package-lock.json`). Read [AGENTS.md](AGENTS.md) before contributing — it defines the project's engineering rules.

## Quick start

Prerequisites (Windows 11 x64): Go 1.26+, Node.js 22+ with npm, Git, and the WebView2 runtime (preinstalled on Windows 11).

```powershell
go install github.com/wailsapp/wails/v3/cmd/wails3@v3.0.0
git clone https://github.com/onbeee/DeBrowser.git
cd DeBrowser
wails3 dev          # development mode with hot reload
```

Production build and further workflows are documented in [docs/DEVELOPMENT.md](docs/DEVELOPMENT.md).

## Documentation

- [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) — system design and module map
- [docs/ROADMAP.md](docs/ROADMAP.md) — staged delivery plan
- [docs/DATABASE.md](docs/DATABASE.md) — SQLite design, migrations, backup rules
- [docs/DEVELOPMENT.md](docs/DEVELOPMENT.md) — environment setup, checks, CI, branching

## Data & privacy

DeBrowser is portable-first and local-first. Everything lives next to the executable - wherever you put the application folder, its data follows:

```
<application folder>\
  debrowser.exe
  data\            debrowser.db, backups\, logs\, profiles\
  kernels\         DeBrowser-managed Chromium builds (only when the user downloads one)
  webview2-data\   WebView2 user data for the app window
  config\          settings.json (language, theme)
```

Nothing is written to `%AppData%` or `%LocalAppData%`; moving or copying the folder to another drive or PC keeps everything working (a system WebView2 Runtime is required). If the folder is not writable, the app reports an error instead of silently storing data elsewhere. On first launch the app directly creates its portable data directory and initializes the database and credential vault automatically — no setup choices are required, and no data is scanned, detected or migrated from `%AppData%` or any other location. The UI is Simplified Chinese only and ships with dark, light and warm themes. User databases, browser profiles, cookies and credentials are never committed to this repository.

## License

No license has been selected yet. Until the project owner chooses one, all rights are reserved.
