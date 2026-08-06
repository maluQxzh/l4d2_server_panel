# AGENTS.md

## Scope

These instructions apply to the entire repository.

L4D2 Panel is a self-hosted, single-control-plane server manager. The Go Control service manages a local SQLite database and serves the React application; Windows and Linux Agents connect outbound over WSS and execute a fixed set of game-management jobs. Preserve that trust boundary: this project is not a general-purpose remote shell.

Read `README.md` before substantial work. For protocol, persistence, authorization, filesystem, or process changes, also read the relevant sections of `docs/architecture.md` and `docs/security.md` first.

## Repository map

- `cmd/control`: Control service and owner-bootstrap entry point.
- `cmd/agent`: portable foreground Agent entry point and optional process-manager integration commands.
- `internal/control`: HTTP/WebSocket APIs, RBAC, persistence, scheduling, notifications, and embedded web assets.
- `internal/containerupdate`: constrained Docker Engine API client, Docker-save archive validation, Compose self-recreation worker, persistent update state, and rollback.
- `internal/agent`: lifecycle, SteamCMD, A2S/RCON, packages, backups, files, and platform process management.
- `internal/domain` and `internal/protocol`: shared domain and wire types.
- `migrations/sqlite`: active ordered SQLite migrations. SQLite is the only supported database; add a new numbered migration and never rewrite an applied migration.
- `web`: React 19, TypeScript, Vite, Vitest, and Playwright application.
- `docs/openapi.yaml`: public API contract. `docs/architecture.md` and `docs/security.md` describe invariants and threat boundaries.
- `packaging/windows`: Windows x64 portable Control ZIP build, structure validation, elevated lifecycle/upgrade smoke, and portable deployment documentation.
- `deploy/docker-compose.image.yml`: fixed prebuilt-image Compose deployment used by Linux/macOS offline releases; keep it aligned with the root source-build Compose file.

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
- Workshop installs use Valve's fixed collection/details endpoints and anonymous SteamCMD App 550 downloads. Name a single payload `<published-file-id>_<sanitized-title>.vpk`; if SteamCMD yields multiple `.vpk`/`*_legacy.bin` candidates, append stable `_1`, `_2`, ... suffixes. Remove dots, path/Windows-reserved characters, controls and format characters from titles, normalize whitespace, and bound the UTF-8 component length. Workshop payloads deliberately are not parsed or validated as VPKs and do not use the 512 MiB VPK limit; do not scan other enabled VPKs for virtual-content conflicts. Still require fixed SteamCMD-owned locations, regular no-link source files, SHA-256/size consistency, no-follow destination commits, and no-replace semantics. Direct upload/HTTPS VPKs and version rollback retain full VPK structure and CRC validation.
- Keep HTTPS download and Webhook redirect, DNS-rebinding, and private-network SSRF defenses intact.
- Enforce RBAC in Control handlers and stores, not only in the UI. Resource-scoped events must fail closed when node/instance ownership is unknown.
- Persistent browser events must retain commit-to-broadcast cursor ordering. Ephemeral console/metric frames may be dropped for slow clients and must not be persisted merely to simplify delivery.
- Keep the Control plane single-instance unless the Agent connection registry, event ordering, dispatch leases, and authorization invalidation are explicitly redesigned for HA.
- Docker center self-update is supported only for the fixed Compose `app` service, `/var/run/docker.sock`, local named volumes at `/app/data`, `/app/spool`, and `/app/updates`, and the `l4d2panel/control:offline` release tag. Keep Docker API requests internal and structured; never accept caller-supplied container names, daemon endpoints, images, mounts, host paths, commands, capabilities, networks, or arbitrary Docker JSON.
- Container update archives are single-image gzip-compressed Docker-save packages, bounded to 1 GiB upload/4 GiB expansion/2048 entries, with the fixed offline tag, native `linux/amd64` or `linux/arm64` platform, product/version labels, safe regular paths, and verified content-addressed blob hashes. Continue pinning replacement creation to the validated immutable image ID. Ordered release versions reject downgrades and transitions to unordered development labels; an unordered current development build may move to another development build or a formal release. Restore both persistent data and the offline tag to the old image on failure.
- Windows portable update ZIPs use the same version transition policy: an unordered development build may move to another safe development label or an ordered semantic version, while an ordered version may only be reinstalled or upgraded and cannot return to an unordered label. Keep the PowerShell builder/validator and Go archive validator aligned on accepted version labels.
- The detached container update Worker must stop the only Control before snapshotting SQLite/WAL/SHM and the master key, retain the old stopped container until the new container is ready and terminal success state is durable, and restore the snapshot/old container on any install, migration, readiness, or terminal-state failure. Worker startup and early errors must become durable terminal state rather than leaving an active queued job.
- The official Compose process starts as root only with `CHOWN/FOWNER/SETGID/SETUID` so it can initialize the three fixed volume roots and adopt the Docker Socket group, then irreversibly drops to `panel` with zero effective capabilities. The Docker Socket is nevertheless host-root-equivalent; do not describe the container hardening as isolation from a compromised Control.
- The UI remains responsive, dark, and Simplified Chinese. Do not bundle Valve artwork. `-tickrate` may be exposed as an explicit numeric startup option; Control and Agent must both require one integer value in the supported 1–1000 range and continue passing it as an argument array without a Shell.

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

## Portable Agent builds

- Portable beside-binary deployment is the only supported installation and upgrade flow for new Windows and Linux Agents. Do not reintroduce installer scripts as a required deployment layer.
- Use `make build-agent-portable VERSION=<version> COMMIT=<commit>` as the canonical local portable Agent build. Keep this target aligned with `.github/workflows/release.yml`.
- Every portable build produces both `linux/amd64` and `windows/amd64` with `CGO_ENABLED=0`, `-trimpath`, and explicit version, commit, and UTC build-date metadata.
- Write the fixed flat artifact set beneath `dist/agents`: `l4d2panel-agent-linux-amd64`, `l4d2panel-agent-linux-amd64.sha256`, `l4d2panel-agent-windows-amd64.exe`, `l4d2panel-agent-windows-amd64.exe.sha256`, and `agent.yaml.example` copied from `configs/agent.portable.example.yaml`.
- Generate checksum files in `sha256sum` format with LF line endings. Validate both checksums, run the Windows binary's `--version`, and run the Linux binary's `--version` on native Linux or WSL before handoff.
- Portable upgrades must preserve beside-binary `agent.yaml` and `data`, validate the staged binary and checksum before stopping the old process, keep one previous binary for rollback, and replace only the platform binary. Never require a new enrollment token for an already enrolled node.
- `dist` remains ignored build output. Do not add portable Agent binaries or generated checksums to source commits.

## Current project status (2026-08-07)

- The main Go Control/Agent, React web application, SQLite persistence, Docker deployment, and Windows/Linux Agent packaging are present. Treat the project as under active development rather than production-complete.
- Linux and macOS center deployments use Docker Compose. The default offline flow is `docker load` followed by `docker compose up -d`; first start creates a persistent `0600` master-key file, opens SQLite, applies migrations, runs readiness checks, and then exposes `/setup`. The published Control port defaults to host loopback and remains configurable for an administrator-managed HTTPS proxy.
- The fixed Compose deployment mounts `/var/run/docker.sock` and an `app_updates` volume. Global owners can upload a native-architecture `l4d2panel/control:offline` Docker-save `.tar.gz` in Settings; Control validates it through the constrained Docker API client and launches a detached Worker that recreates only its current Compose `app` container by immutable image ID.
- Docker update success and failure paths were exercised against the native Docker Engine in the `l4d2` WSL2 distribution: same-version upload/recreation preserved the owner session and SQLite volumes; a deliberately broken image failed to start, restored the database/key snapshot, old container/name/readiness, and the offline tag. PID 1 ran as UID/GID `100:101`, retained the Socket GID only as a supplementary group, had `CapEff=0`, and kept `NoNewPrivs=1` after bootstrap.
- Local offline artifacts were built and archive-validated for `linux/amd64` and `linux/arm64`; the ARM64 binary was executed through registered QEMU/binfmt. Generated images/checksums remain ignored beneath `dist/container` and are not source commits or registry publications.
- Windows center releases are x64 portable ZIPs for Windows 10/11 and Windows Server 2019+. The fixed beside-EXE layout contains Control, SQLite migrations, licenses, SBOM, an unsigned integrity manifest, and `data`; HTTPS remains administrator-managed.
- No-argument launch requests UAC, registers only `l4d2panel-control`, fixes Control at `0.0.0.0:19090`, applies restricted ACLs, and creates the TCP 19090 firewall rule. SQLite has no listener.
- Global owners can upload a trusted ZIP or reinstall the same version. Unordered Windows development builds may move to another development label or an ordered semantic version; ordered versions reject downgrades and transitions back to development labels. The detached worker validates package limits and hashes, checkpoints SQLite, snapshots managed files/config/database, replaces only manifest-managed files, runs forward migrations and readiness, and fully rolls back on failure. It retains the newest three snapshots and durable sanitized state.
- `packaging/windows/Build-WindowsPortable.ps1` creates the release ZIP. `Test-WindowsPortablePackage.ps1` validates its exact structure and hashes; `Test-WindowsPortableInstall.ps1` performs an elevated real install, first-owner, same-version upgrade, unregister, and purge smoke.
- The portable deployment deliberately does not import, uninstall, or clean up legacy Windows center products or `%ProgramData%` data. A conflicting fixed service name is rejected and must be removed through the old deployment method first.
- SQLite validation includes ordinary Go tests, isolated migration/credential/update integration, and the opt-in 20-node/100-instance fleet test. PowerShell parsing, portable structure, elevated lifecycle/upgrade, and Docker runtime gates remain required in proportion to changed delivery code.
- Outstanding release gates include a real SteamCMD App 222860 install/start/A2S/RCON/update/restart test, the Windows 10/11 and Server 2019/2022/2025 VM matrix, native macOS Docker Desktop runtime/self-update validation, elevated portable rollback smoke, and signed release artifacts. The current container packages have SHA-256 checksums but no publisher signature and have not been published to a registry.

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

For Agent or platform changes, test on Windows and Linux and cross-build both amd64 targets with `CGO_ENABLED=0`. Native Windows Center packaging changes require an x64 Control cross-build, portable ZIP structure/hash validation, and elevated install/upgrade/rollback VM smoke tests when release payloads are available. Container changes require `docker compose config --quiet` and production image builds for both `linux/amd64` and `linux/arm64`.

If the local environment lacks Docker, PowerShell 7.4, SteamCMD, or a supported game host, report the skipped gate explicitly. Never describe mocked Playwright flows or cross-compilation as a real App 222860 end-to-end validation.

`Makefile` targets are Unix-oriented (`rm`, `cp`, and shell date syntax). On Windows PowerShell, run the equivalent Go and npm commands directly unless a compatible Unix shell is available.
