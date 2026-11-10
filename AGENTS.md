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

Do not implement all stages in one change.

Before each implementation:
1. Inspect the existing repository.
2. Understand current architecture.
3. Identify affected files.
4. Propose a small implementation plan.

After implementation:
1. Run relevant automated checks.
2. Fix failures.
3. Summarize changed files.
4. Document limitations.
5. Create a Pull Request.
6. Wait for project-owner review.
