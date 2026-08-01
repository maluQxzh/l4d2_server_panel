# AGENTS.md

## Scope

These instructions apply to the entire repository.

L4D2 Panel is a self-hosted, single-control-plane server manager. The Go Control service manages a local SQLite database and serves the React application; Windows and Linux Agents connect outbound over WSS and execute a fixed set of game-management jobs. Preserve that trust boundary: this project is not a general-purpose remote shell.

Read `README.md` before substantial work. For protocol, persistence, authorization, filesystem, or process changes, also read the relevant sections of `docs/architecture.md` and `docs/security.md` first.

## Repository map

- `cmd/control`: Control service and owner-bootstrap entry point.
- `cmd/agent`: foreground and Windows Service/systemd Agent entry point.
- `internal/control`: HTTP/WebSocket APIs, RBAC, persistence, scheduling, notifications, and embedded web assets.
- `internal/agent`: lifecycle, SteamCMD, A2S/RCON, packages, backups, files, and platform process management.
- `internal/domain` and `internal/protocol`: shared domain and wire types.
- `migrations/sqlite`: active ordered SQLite migrations. Root-level PostgreSQL migrations are immutable history only and are never built or executed. Add a new numbered SQLite migration; never rewrite an applied migration.
- `web`: React 19, TypeScript, Vite, Vitest, and Playwright application.
- `docs/openapi.yaml`: public API contract. `docs/architecture.md` and `docs/security.md` describe invariants and threat boundaries.
- `scripts`: Windows and Linux Agent installers.
- `packaging/windows`: WiX 7 native Windows Control MSI, installer, upgrade, and explicit data-purge scripts.

## Required toolchain

- Go 1.25.12 or a later security patch in the 1.25 line.
- Node.js 24 with `npm ci` based on `web/package-lock.json`.
- No external database is required. SQLite integration and fleet-acceptance tests use isolated local files.
- Docker Engine 24+ on Linux or Docker Desktop on macOS, with Docker Compose v2, for production-style validation.

Do not weaken or remove pinned security versions merely to make an older local toolchain pass.

## Implementation rules

- Keep REST endpoints under `/api/v1`. Long-running writes return `202` plus `job_id`, and mutating APIs must preserve `Idempotency-Key` behavior.
- Keep the Agent protocol compatible with the current and previous minor version. Gate new task types through capability negotiation so an older Agent never receives an unsupported command.
- Keep job state transitions within `queued/dispatched/running/succeeded/failed/canceled`. Agent terminal results remain durable until Control persists the projection and sends `job_ack`.
- Maintain one concurrent instance write job and the node-level SteamCMD concurrency limit. Read-only probes must not acquire the instance write barrier.
- Agents may execute only known task types using argument arrays. Do not add arbitrary shell-command execution or accept caller-supplied executables/environments for game processes.
- All instance filesystem access must stay beneath configured roots. Preserve handle-relative/dirfd or Windows NT `RootDirectory` operations, no-follow checks, archive traversal and bomb limits, atomic commits, and symlink/reparse-point escape defenses. Reject absolute paths, `..`, Windows device names and alternate data streams. A lexical path-prefix check alone is not sufficient.
- Windows game processes must start suspended, enter the kill-on-close Job Object, and only then resume. Linux game processes remain in managed process groups. Stop through RCON `quit` before forced tree termination.
- Treat RCON passwords, SMTP credentials, Webhook secrets, node credentials, API tokens, upload locators, and generated secret configuration as sensitive. Store only hashes where applicable, encrypt recoverable secrets, redact API/event/audit output, and never log plaintext.
- Keep HTTPS download and Webhook redirect, DNS-rebinding, and private-network SSRF defenses intact.
- Enforce RBAC in Control handlers and stores, not only in the UI. Resource-scoped events must fail closed when node/instance ownership is unknown.
- Persistent browser events must retain commit-to-broadcast cursor ordering. Ephemeral console/metric frames may be dropped for slow clients and must not be persisted merely to simplify delivery.
- Keep the Control plane single-instance unless the Agent connection registry, event ordering, dispatch leases, and authorization invalidation are explicitly redesigned for HA.
- The UI remains responsive, dark, and Simplified Chinese. Do not bundle Valve artwork or expose `-tickrate` as a valid L4D2 setting.

## Code and schema conventions

- Run `gofmt` on changed Go files. Prefer small packages, explicit errors, bounded inputs, and context-aware I/O.
- Keep platform behavior in `_windows.go` and `_linux.go` files where appropriate, and add platform-specific tests for security-sensitive behavior.
- Use parameterized SQL with `?1` placeholders. All read-before-write workflows use immediate transactions; preserve deterministic ordering and generated-column task lock semantics.
- Database changes require a forward-only migration, a matching checksum entry in `internal/control/migrations_test.go`, and store/integration tests. Migration startup must continue to fail closed on checksum mismatch or unsafe legacy data.
- Keep TypeScript strict and use the existing API/query/component patterns. Update unit tests and Playwright flows when behavior visible to users changes.
- Do not hand-edit generated files in `web/dist` or replace the fallback `internal/control/static/index.html`; use the web build or Docker build pipeline.
- Update `docs/openapi.yaml` for API changes and update architecture/security documentation whenever an invariant or trust boundary changes.
- Do not commit `.env`, credentials, runtime data, `tmp`, `bin`, `web/node_modules`, `web/dist`, Playwright output, or local logs.
- Preserve unrelated work already present in the workspace; do not use reset/checkout to erase it.

## Current project status (2026-08-01)

- The main Go Control/Agent, React web application, SQLite persistence, Docker deployment, and Windows/Linux Agent packaging are present. Treat the project as under active development rather than production-complete.
- Linux and macOS center deployments use Docker Compose. The first owner is created at `/setup`, and the published HTTP/HTTPS ports are configurable.
- The native Windows center installer is implemented as a WiX 7 x64 MSI for Windows 10/11 and Windows Server 2019+. It bundles Control and Caddy 2.11.3, uses `%ProgramData%\L4D2Panel\data\l4d2panel.sqlite3`, and retains the external reverse-proxy mode.
- The MSI supports a configurable internal `CONTROL_PORT` (default `18080`) and configurable public HTTP/HTTPS ports (defaults `8088`/`8443`). The generated Control configuration, Caddy upstream, and readiness checks use the same internal port.
- Windows services use the fixed names `l4d2panel-control` and `l4d2panel-caddy` with their corresponding virtual service accounts. Control listens on loopback only; SQLite has no listener and firewall rules cover only the selected public ports.
- Installer hardening includes native x64 detection, Caddy checksum validation, corrected custom-action ordering, propagated SQLite/config ACLs, service executable read permissions, rollback cleanup, bounded readiness retries, disabled unwanted Caddy redirects, embedded Go timezone data, and pre-upgrade SQLite checkpoints.
- `packaging/windows/Build-WindowsMSI.ps1 -Fast` supports frequent local iteration. Release builds omit `-Fast`; every build removes legacy database staging and rebuilds cabinet cache so stale payload cannot leak into the MSI.
- `packaging/windows/Test-WindowsMSI.ps1` performs structural package validation and runs as part of the build. `packaging/windows/Test-WindowsMSIInstall.ps1` performs an elevated real install/uninstall smoke test.
- Any prior PostgreSQL MSI artifact or smoke result is obsolete. Rebuild the SQLite package and run a fresh elevated smoke test before release; `dist/` remains ignored.
- SQLite validation includes ordinary Go tests, isolated migration/credential integration, and the opt-in 20-node/100-instance fleet test. PowerShell parser, WiX XML/build, structural MSI, elevated install, and Docker runtime gates remain required in proportion to changed delivery code.
- Outstanding release gates include a real SteamCMD App 222860 install/start/A2S/RCON/update/restart test, the Windows 10/11 and Server 2019/2022/2025 VM matrix, Linux/macOS runtime revalidation, Docker restart persistence, elevated SQLite MSI smoke, and signed release artifacts.

## Validation

Run the smallest relevant tests while iterating, then the appropriate release checks before handoff.

```text
go test ./...
go test -race ./...
go vet ./...
go run golang.org/x/vuln/cmd/govulncheck@v1.6.0 ./...

cd web
npm test -- --run
npm audit --audit-level=high
npm run build
npm run test:e2e
```

For SQLite persistence changes run:

```text
go test ./internal/control -run TestSQLiteMigrationsAndCredentialLifecycle -count=1
RUN_FLEET_ACCEPTANCE=1 go test ./internal/control -run TestFleetScaleAcceptance -count=1 -timeout=2m
```

For Agent or platform changes, test on Windows and Linux and cross-build both amd64 targets with `CGO_ENABLED=0`. Installer changes require PowerShell parser validation and `sh -n`; CI additionally runs ShellCheck. Native Windows Center packaging changes require an x64 Control cross-build, WiX 7 compilation with OSMF/EULA compliance, and MSI VM smoke tests when release payloads are available. Container changes require `docker compose config --quiet` and production image builds for both `linux/amd64` and `linux/arm64`.

If the local environment lacks Docker, WiX, SteamCMD, or a supported game host, report the skipped gate explicitly. Never describe mocked Playwright flows or cross-compilation as a real App 222860 end-to-end validation.

`Makefile` targets are Unix-oriented (`rm`, `cp`, and shell date syntax). On Windows PowerShell, run the equivalent Go and npm commands directly unless a compatible Unix shell is available.
