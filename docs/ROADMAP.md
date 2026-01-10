# Roadmap to v1.0: pgbouncer + pganalyze parity (on-prem, solo-dev scope)

## Guiding assumptions
- Target: on-prem deployments on Linux (binary and Docker), single instance only for v1.0.
- Database scope: PostgreSQL versions currently supported by the community; no extensions (e.g., `pg_stat_statements`) for v1.0.
- Auth: password-based credentials (SCRAM-SHA-256 & MD5) supplied via config file or environment variables.
- Observability: OpenTelemetry (OTLP) for metrics, logs, and traces.
- Interface: CLI-driven configuration and administration; no UI/API in v1.0.
- Priority: lowest-effort/high-impact features first to reach basic pgbouncer (session pooling) and pganalyze-style insights without extensions.

## Milestone outline (semantic versions)

### v0.1 – Foundation
- Clean config system (file + env overrides), validation, and defaults for listener/backend addresses.
- Basic proxy lifecycle: start/stop, graceful shutdown, and health pings to PostgreSQL.
- Protocol correctness baseline: handle Startup/Auth (password, including SCRAM-SHA-256), simple Query/Parse/Bind/Execute/Sync, and clean error surfacing.
- Logging scaffolding (structured logs) and minimal OpenTelemetry wiring (exporter config stubs, no emitted metrics yet).

### v0.2 – Session pooling (pgbouncer parity)
- Implement session pooling: connection pool to PostgreSQL with leasing per client session; pool sizing, timeouts, idle reaping.
- Auth handoff: reuse configured password to authenticate pooled backend connections; reject clients early on config mismatch.
- Basic pool stats counters (borrow/return, wait time) emitted via OTLP metrics.

### v0.3 – Connection management & reliability
- Client-side TLS passthrough/termination optional; TLS to PostgreSQL backend (configurable).
- Admin CLI commands for live pool stats, drains, and controlled shutdowns.

### v0.4 – Observability baseline (OpenTelemetry)
- Emit OTLP metrics: connection counts, pool utilization, wait times, query latency histograms (from parsed messages), error rates.
- Structured OTLP logs with correlation IDs; configurable sampling for high-volume events.
- Tracing hooks around client → pool → backend flow (spans for checkout, execute, response) with traceparent propagation when present.

### v0.5 – Lightweight performance insights (pganalyze-inspired)
- Collect safe core views (no extensions): `pg_stat_activity`, `pg_stat_database`, `pg_settings` sampling.
- Slow-query log tailer (Postgres log file or syslog receiver) with parsing into structured events; configurable thresholds.
- CLI report: top databases by load, connections, and slow-query count; export metrics to OTLP for dashboards.

### v0.6 – Operability, packaging, and QA
- Release artifacts: static Linux binary, Docker image, example systemd unit, sample config.
- Distribution: Homebrew tap and simple install script.
- Integration tests: session pooling correctness, TLS handshake paths, config reloads.
- Benchmark harness (pgbench-based) with published baseline targets (latency overhead and QPS vs direct Postgres) for regression tracking.

### v1.0 – Hardened parity release
- Performance tuning to meet baseline: target <1 ms added p95 latency in session mode under typical workload envelope; document tested QPS/connection limits.
- Robustness: connection leak detection, panic recovery guards, defensive protocol parsing, fuzzing of startup messages.
- Documentation: admin/ops guide (runbooks, troubleshooting, tuning), security notes, and OTel dashboard examples.
- Support policy: documented PostgreSQL versions, config compatibility matrix, and deprecation policy for future releases.

## Feature prioritization rationale (low effort / high benefit)
- Session pooling before deeper protocol work: highest immediate value for connection-heavy apps.
- OTLP metrics/logs early to keep visibility while iterating; traces follow once pool flow is stable.
- Extension-free insights by sampling `pg_stat_*` and logs avoid dependency risk while giving actionable signals.
- Packaging and test harness late-stage to avoid churn until core behaviors settle.
