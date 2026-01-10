# AGENTS.md

This file is a fast-start guide for working on this project with minimal back-and-forth.

## TL;DR (common commands)

- Build: `go build -o grunyas ./cmd`
- Run: `./grunyas` (reads `./config.toml` if present)
- Run w/out console UI: `./grunyas -no-console`
- Tests (unit): `go test ./...`
- Tests (integration tag): `go test -race -tags=integration -v ./...`
- SQL suite (starts proxy + uses running Postgres): `./scripts/run-sql-tests.sh`

## Requirements

- Go: `go 1.25.5` (see `go.mod`).
- PostgreSQL for integration + SQL tests.
- Optional: Docker for `docker-compose.integration.yml`.

## Config + env overrides

- Default config is loaded from `./config.toml` if present (TOML).
- Environment overrides use prefix `GRUNYAS_` and map dots to underscores.
  Example: `GRUNYAS_SERVER_LISTEN_ADDR=0.0.0.0:5711`.
- CLI flag: `-no-console` disables the interactive TUI console.
- Defaults set in `cmd/main.go` for SSL values:
  - `server.ssl_mode=never`, `server.ssl_cert=""`, `server.ssl_key=""`.
- Example config: `config.toml.example`.

## Project map (where to look first)

- Entry point: `cmd/main.go`.
- Config parsing/validation: `config/*.go`.
- Core proxy server: `internal/server/proxy`.
- Session lifecycle + protocol handling: `internal/server/session`.
- Protocol message routing: `internal/server/messaging`.
- Downstream (client wire protocol): `internal/server/downstream_client`.
- Upstream (pool + pgx): `internal/pool` and `internal/pool/upstream_client`.
- Auth implementations: `internal/auth`.
- Logging + telemetry: `internal/logger`.
- Utilities + adapters: `internal/utils`.
- Tests: `internal/**` plus `tests/integration` and `tests/sql`.

## Architecture notes (read these before changing protocol flows)

- Downstream vs upstream split:
  - Downstream is server-side protocol; use `pgproto3.Backend`.
  - Upstream uses `pgxpool/pgconn`; don’t create a separate `pgproto3.Frontend`.
  - See `docs/PROTOCOL_SPLIT.md`.
- Channel/ack design to avoid buffer reuse races in pgproto3:
  - Read loops send messages then wait on ack channel before next Receive.
  - See `docs/CHANNELS.md`.
- Idle sweeper behavior + timeouts:
  - See `docs/IDLE_SWEEPER.md`.

## Testing details

- Unit tests: `go test ./...` (uses `internal/testutil` for goleak in some packages).
- Integration tests:
  - Tagged with `integration` build tag.
  - Expect a running Postgres instance.
  - Use `docker-compose.integration.yml` for local Postgres.
- SQL suite:
  - `./scripts/run-sql-tests.sh` can start the proxy itself.
  - Uses env vars: `PGHOST/PGPORT/PGUSER/PGPASSWORD/PGDATABASE` and
    `PROXY_HOST/PROXY_PORT`.
  - Known error test files are allowed to fail by design (`tests/sql/02_errors.sql`).

## Common pitfalls / gotchas

- pgproto3 `Receive()` reuses buffers; passing messages across goroutines
  must follow the ack-channel pattern (see `docs/CHANNELS.md`).
- Upstream must stay in sync with `pgx` state; avoid creating a
  `pgproto3.Frontend` on upstream connections (see `docs/PROTOCOL_SPLIT.md`).
- The console UI is on by default; add `-no-console` for headless runs.

## When adding features

- Update config structs + validation in `config/`.
- Implement in `internal/server/` or `internal/pool/` as appropriate.
- Add/adjust unit tests; add integration/SQL coverage if behavior is protocol-visible.
- Update `README.md` and/or relevant doc in `docs/` when you change protocol flow.
