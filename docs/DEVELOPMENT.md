# DeBrowser Development Guide

## Prerequisites

Target platform: **Windows 11 x64**.

| Tool | Version | Notes |
| --- | --- | --- |
| Go | 1.26.x | Wails v3.0.0 requires Go 1.26 or newer |
| Node.js | 22 LTS or newer | Includes npm; used for the frontend and Vite |
| Wails CLI | v3.0.0 | `go install github.com/wailsapp/wails/v3/cmd/wails3@v3.0.0` |
| Task (optional) | any recent | The scaffold workflows are Taskfiles: https://taskfile.dev |
| WebView2 runtime | — | Preinstalled on Windows 11 |
| Git | any recent | |

Check your setup with `wails3 doctor`.

## Getting started

```powershell
git clone https://github.com/onbeee/DeBrowser.git
cd DeBrowser
git checkout feature/<your-feature>   # never work directly on main
cd frontend; npm install; cd ..
wails3 dev                            # dev mode: hot-reload frontend, auto-restart backend
```

## Environment variables

| Variable | Effect |
| --- | --- |
| `DEBROWSER_DATA_DIR` | Overrides the data directory (default `%AppData%\DeBrowser`) |
| `DEBROWSER_LOG_LEVEL` | `debug` / `info` / `warn` / `error` (default `info`) |

## Common tasks

```powershell
task build            # production Windows x64 build via build/windows/Taskfile.yml
task package          # NSIS installer (requires makensis)
wails3 generate bindings   # regenerate frontend/bindings after changing Go services
```

The full `task build` also generates the Windows icon and `.syso` resources, which requires `build/appicon.png` (a 1024×1024 PNG app icon). That binary asset is **not in the repository yet** — see "Known limitations" below. Until it is added, generate Wails' example icon as a placeholder: `wails3 generate icons -example` (run inside `build/`), which is exactly what CI does.

## Checks (run before opening a PR)

```powershell
gofmt -l .                       # must print nothing
go vet ./...
go test ./...
cd frontend
npm run typecheck                # tsc --noEmit
npm run build                    # production frontend build
cd ..
```

## CI

GitHub Actions (`.github/workflows/ci.yml`) runs on a Windows x64 runner (`windows-latest`) for pushes to `main`/`develop` and for every Pull Request:

1. Go format check (`gofmt`)
2. Frontend install, bindings generation, type check and production build
3. `go vet ./...` and `go test ./...`
4. Install Wails CLI v3.0.0, generate a placeholder icon (see below) and run the full official `wails3 build` for Windows x64 (bindings → frontend → icons → `.syso` → Go build)
5. Upload the built executable as a workflow artifact for review

Nothing is published automatically; releases are a manual, approved step (AGENTS.md section 8).

Version compatibility: Go 1.26 + Wails v3.0.0 (the first Wails release that requires Go 1.26), Node.js 22 LTS with Vite 8 / React 18 — the same versions pinned in `go.mod` and `frontend/package-lock.json`.

## Git workflow (AGENTS.md section 7)

- `main` is the stable production branch; `develop` is the integration branch; features live on `feature/*` branches.
- Every feature ships through a Pull Request with automated checks green.
- Never force-push or delete branches without approval; never commit directly to `main`; never merge a PR without explicit project-owner approval.

## Known limitations (Stage 1)

- **Application icon**: `build/appicon.png` and `build/windows/icon.ico` are binary assets and have not been committed yet (both are git-ignored as generated files). CI and local builds generate Wails' example icon as a placeholder so the full build — including `.syso` version resources — runs end to end; the placeholder is not DeBrowser branding. When the real icon is ready, add it with `git add -f build/appicon.png` and remove the two icon lines from `.gitignore`.
- The NSIS installer task (`task package`) is defined but not exercised in CI yet.
- Business modules (instances, proxies, kernels, bookmarks, synchronization, backup UI) are later stages — see `docs/ROADMAP.md`.
