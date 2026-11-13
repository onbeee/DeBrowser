# DeBrowser

A lightweight, local-first desktop application for managing multiple isolated browser profiles.

DeBrowser is built for **Windows 11 x64**. Each browser instance gets its own user data directory, configuration, process and optional proxy — profiles are never shared between running instances.

> **Status: Stage 1 — project foundation.** The repository currently contains the runnable application shell (Go backend, React frontend, SQLite foundation, CI). Business features (browser instances, proxies, kernels, bookmarks, synchronization, backup) are delivered in later stages; see [docs/ROADMAP.md](docs/ROADMAP.md). No placeholder data stands in for those features.

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
| Proxy engine | Mihomo | arrives in Stage 4 |
| Browser control | Chrome DevTools Protocol | arrives in Stage 3 |

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
  data\            debrowser.db, backups\, logs\, profiles\, mihomo\
  webview2-data\   WebView2 user data for the app window
  config\          settings.json (language, theme)
```

Nothing is written to `%AppData%` or `%LocalAppData%`; moving or copying the folder to another drive or PC keeps everything working (a system WebView2 Runtime is required). If the folder is not writable, the app reports an error instead of silently storing data elsewhere. On first launch the app directly creates its portable data directory and initializes the database and credential vault automatically — no setup choices are required, and no data is scanned, detected or migrated from `%AppData%` or any other location. The UI is Simplified Chinese only and ships with dark, light and warm themes. User databases, browser profiles, cookies, credentials and proxy subscription URLs are never committed to this repository.

## License

No license has been selected yet. Until the project owner chooses one, all rights are reserved.
