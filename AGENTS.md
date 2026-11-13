# DeBrowser — Project Engineering Rules

## 1. Project Identity

Project name: DeBrowser

Product: A lightweight, local-first desktop multi-profile browser management application.

Primary platform: Windows 11 x64.

The application must prioritize:
- Stability
- Performance
- Low resource consumption
- Data integrity
- Security
- Maintainability
- Simple architecture

## 2. Technology Stack

- Desktop framework: Wails v3.0.0
- Backend: Go 1.26+
- Frontend: React 18
- Language: TypeScript
- Build tool: Vite
- Styling: Tailwind CSS
- Icons: lucide-react
- Database: SQLite
- SQLite driver: modernc.org/sqlite
- State management: Zustand
- Browser control: Chrome DevTools Protocol (CDP)
- Proxy engine: Mihomo

Use stable, compatible, pinned dependency versions. Do not upgrade major dependencies without approval.

## 3. Architecture

Use a modular monolithic architecture.

Go owns all core business logic:
- Browser process management
- Instance lifecycle
- Profile isolation
- Proxy management
- Kernel management
- Bookmark management
- Synchronization
- Database operations
- Backup and recovery

React owns:
- UI rendering
- User interactions
- Forms
- Tables
- Frontend state
- Notifications

Use Wails bindings for frontend-to-backend calls and Wails events for asynchronous state notifications.

Do not introduce unnecessary microservices, Redis, MongoDB, Docker or GraphQL.

## 4. Browser Isolation

Each browser instance must have:
- A unique instance ID
- An independent user data directory
- An independent configuration
- An independently managed browser process
- An optional independent proxy assignment

Never share a Chromium user data directory between running instances.

Never copy a live browser profile while Chromium is using it.

Do not claim browser fingerprint isolation unless it has been explicitly implemented and tested.

## 5. Database

Use SQLite with:
- WAL mode
- Foreign key constraints
- Database migrations
- Transactional writes
- Appropriate indexes
- Backup and restore support

Never store browser cookies or browser profile contents in SQLite.

Never commit user databases, browser profiles, credentials, subscription URLs or private proxy configurations.

## 6. Engineering Rules

- Keep code modular and readable.
- Avoid unnecessary abstractions.
- Handle errors explicitly.
- Use context cancellation for long-running tasks.
- Protect concurrent access to instance state.
- Do not block the UI thread.
- Add unit tests for business logic.
- Add integration tests for critical workflows.
- Use structured logging.
- Validate all external input.
- Never expose CDP debugging ports publicly.
- Do not execute arbitrary commands from frontend input.

## 7. GitHub Workflow

- main: stable production branch.
- develop: integration branch.
- feature/*: isolated feature branches.
- Every feature must use a Pull Request.
- Run automated checks before merging.
- Never force-push or delete branches without approval.
- Never commit directly to main.
- Do not merge a Pull Request without explicit project-owner approval.

## 8. CI/CD

Use GitHub Actions.

Required checks:
- Go formatting
- Go tests
- Go vet
- Frontend TypeScript checks
- Frontend production build
- Windows x64 application build

Build artifacts must be uploaded for review.

Do not publish public releases automatically.

## 9. Development Strategy

Implement in small, testable stages.

Stage 1: Repository foundation and application shell.
Stage 2: SQLite database and migrations.
Stage 3: Browser kernel detection and instance lifecycle.
Stage 4: Proxy and Mihomo integration.
Stage 5: Bookmarks and kernel management.
Stage 6: Multi-window synchronization.
Stage 7: Backup, recovery, performance and release testing.

> Historical note (2026-11-13): the seven lines above are the ORIGINAL stage plan recorded when this file was written (2026-11-10) and are kept as a historical record only — the project no longer follows these numbers (Stages 3 and 4, for example, were delivered with different scopes). The authoritative current roadmap, with the final stage numbering fixed by the product owner on 2026-11-13 (Stage 5A direct startup … Stage 12 full backup & restore, last), is `docs/ROADMAP.md`.

Do not implement all stages in one change.

Working agreement (project-owner decision, 2026-11-13) — how implementation work is assigned and executed:

1. 产品范围、架构设计、技术选型、实施方案和具体任务由项目负责人制定并提供。
2. Muse 必须按照项目负责人提供的具体指令执行，不得自行提出替代产品方案、技术方案或重新设计任务范围。
3. 执行前核对指定基线、当前分支、工作树和指令约束。
4. 如果指令存在缺失、矛盾或无法执行的关键条件，停止相关操作并报告具体阻塞点，不得擅自补充设计或扩大范围。
5. 执行后按指令完成测试、检查、提交和汇报。

After implementation:
1. Run relevant automated checks.
2. Fix failures.
3. Summarize changed files.
4. Document limitations.
5. Create a Pull Request.
6. Wait for project-owner review.
