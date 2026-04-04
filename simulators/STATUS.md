# Simulator Status

Each simulator (Go, Java, Python, TypeScript) runs the same 9 scenarios against Grunyas in both
session and transaction pool modes, producing a JSON report for comparison.

All four simulators pass all 9 scenarios with 0 errors in both session and transaction mode.

---

## Go — complete

| Scenario | Session | Transaction |
|---|---|---|
| basic_crud | pass — 0 errors | pass — 0 errors |
| transactions | pass — 0 errors | pass — 0 errors |
| prepared_statements | pass — 0 errors | pass — 0 errors |
| concurrent_rw | pass — 0 errors | pass — 0 errors |
| connection_storms | pass — 0 errors | pass — 0 errors |
| long_running | pass — 0 errors | pass — 0 errors |
| error_handling | pass — 0 errors | pass — 0 errors |
| batch_operations | pass — 0 errors | pass — 0 errors |
| pool_behavior | pass — 0 errors | pass — 0 errors |

### Bugs fixed

**Grunyas (proxy layer)**

1. `internal/server/messaging/dispatch.go` — Parse, Bind, and Describe messages were returning
   `m.Name != ""` as the session-pin signal. pgx auto-names all prepared statements
   `stmtcache_<hash>`, so every single query permanently pinned the session. Fixed to always return
   `false` for these message types: they are extended-protocol framing, not SQL `PREPARE`.

2. `internal/pool/upstream_client/session_client.go` — `DISCARD ALL` was running on every
   connection release regardless of pool mode. Added a `discardAll bool` field; `DISCARD ALL` now
   only runs in session mode. In transaction mode it was adding a full round-trip per query.

3. `internal/pool/manager/pool_manager.go` — wired the `discardAll` flag through from config;
   added pool stats logging on acquire failure so exhaustion is visible without full debug logging.

4. `internal/server/session/session.go` — `acquireUpstream()` failure in the transaction mode loop
   was silently dropping the TCP connection. Now sends a proper PostgreSQL error response to the
   client before closing. Also fixed a build break: `Startup()` call was missing the `authMethod`
   argument, and `AuthenticateUser` was renamed to `Authenticate` in the interface.

**Go simulator**

5. `simulators/go/scenarios/types.go` — pgx pools in transaction mode now use
   `QueryExecModeCacheDescribe` instead of the default `QueryExecModeCacheStatement`. With the
   default mode, pgx names all auto-prepared statements `stmtcache_<hash>`. These named statements
   don't exist on a different backend after a connection release, causing every query to fail.
   `QueryExecModeCacheDescribe` uses unnamed statements that are scoped to the current
   Parse/Bind/Execute sequence and work correctly across backend switches.

6. `simulators/go/scenarios/connection_storms.go` — raw `pgx.Connect()` connections (not pool)
   have an empty statement cache on every new connection. With `QueryExecModeCacheDescribe`, the
   first use of any query still does a two-phase prepare: Parse+Describe+Sync (ends with
   ReadyForQuery → Grunyas releases backend), then Bind+Execute+Sync (goes to a different backend
   that has no prepared statement). Fixed by using `QueryExecModeSimpleProtocol` for raw connections
   in transaction mode — simple queries are a single round-trip with no prepare step.

7. `simulators/go/scenarios/transactions.go` — used deterministic email addresses
   (`tx_commit_{workerID}_{iter}@test.com`). Committed rows from a previous run caused duplicate key
   violations on every subsequent run. Fixed with a random `runID` prefix per execution.

8. `simulators/go/main.go` — the startup retry loop called `pool.Close()` but left the `pool`
   pointer non-nil, so the `if pool == nil` fatal check never fired after 30 failed attempts. The
   simulator would proceed with a closed pool and every scenario would fail immediately with
   misleading errors. Fixed by setting `pool = nil` after `Close()`.

9. `simulators/go/scenarios/pool_behavior.go` — unexpected PID changes in session mode and absent
   PID changes in transaction mode now increment `errCount` directly instead of being reported as
   Notes (which forced a `partial` status even with 0 errors and a correct outcome).

10. `simulators/go/scenarios/prepared_statements.go` — the PREPARE → EXECUTE × 5 → DEALLOCATE
    sequence was split across multiple autocommit queries. In transaction mode, Grunyas releases the
    backend after each ReadyForQuery, so the EXECUTE lands on a different backend that has no
    knowledge of the named statement. Fixed by wrapping the entire sequence in a `BEGIN`/`COMMIT` to
    pin the backend throughout.

---

## Python — complete

| Scenario | Session | Transaction |
|---|---|---|
| basic_crud | pass — 0 errors | pass — 0 errors |
| transactions | pass — 0 errors | pass — 0 errors |
| prepared_statements | pass — 0 errors | pass — 0 errors |
| concurrent_rw | pass — 0 errors | pass — 0 errors |
| connection_storms | pass — 0 errors | pass — 0 errors |
| long_running | pass — 0 errors | pass — 0 errors |
| error_handling | pass — 0 errors | pass — 0 errors |
| batch_operations | pass — 0 errors | pass — 0 errors |
| pool_behavior | pass — 0 errors | pass — 0 errors |

### Bugs fixed

1. `scenarios/transactions.py` — deterministic email addresses caused duplicate key violations on
   repeated runs. Fixed with a random `run_id` prefix per execution. Also added
   `conn.prepare_threshold = None` in transaction mode to prevent psycopg3 from creating named
   server-side prepared statements that don't survive backend switches.

2. `scenarios/pool_behavior.py` — rewritten to count unexpected PID behaviour as errors (instead
   of notes). Added `prepare_threshold = None` in transaction mode — the 10 repeated
   `pg_backend_pid()` calls trigger psycopg3's auto-prepare after the 5th use, creating `_pg3_0`
   etc. on B1 which don't exist on B2 after a backend switch.

3. `scenarios/basic_crud.py`, `scenarios/concurrent_rw.py`, `scenarios/batch_operations.py` —
   added `prepare_threshold = None` in transaction mode for the same reason.

4. `scenarios/long_running.py` — rewrote to use a shared `AsyncConnectionPool` (from
   `psycopg_pool`) instead of raw connections per worker. The original code opened
   `concurrency × 2` concurrent connections, exceeding the session-mode client cap, causing the
   scenario to crash and leaving Grunyas's session counter inflated — making all subsequent
   scenarios fail with "server sent error during SSL exchange".

5. `scenarios/prepared_statements.py` — added `conn.prepare_threshold = None` in transaction mode.
   The 10 unnamed parameterized queries in the loop triggered psycopg3 auto-prepare after 5 uses,
   creating `_pg3_0` named statements on one backend that didn't exist on the next. Also wrapped
   the SQL `PREPARE`/`EXECUTE`/`DEALLOCATE` sequence in explicit `BEGIN`/`COMMIT` to pin the
   backend, same as Go.

6. `scenarios/connection_storms.py` — Grunyas sends SQLSTATE 53300 (too_many_connections) when
   the session-mode client cap is hit. These are correct proxy rejections, not errors. psycopg3
   can't extract the SQLSTATE because Grunyas sends the error before the SSL handshake completes;
   the exception arrives as `OperationalError` with message "server sent an error response during
   SSL exchange". Filter now matches on this message text in addition to SQLSTATE 53300.

---

## TypeScript — complete

| Scenario | Session | Transaction |
|---|---|---|
| basic_crud | pass — 0 errors | pass — 0 errors |
| transactions | pass — 0 errors | pass — 0 errors |
| prepared_statements | pass — 0 errors | pass — 0 errors |
| concurrent_rw | pass — 0 errors | pass — 0 errors |
| connection_storms | pass — 0 errors | pass — 0 errors |
| long_running | pass — 0 errors | pass — 0 errors |
| error_handling | pass — 0 errors | pass — 0 errors |
| batch_operations | pass — 0 errors | pass — 0 errors |
| pool_behavior | pass — 0 errors | pass — 0 errors |

### Bugs fixed

1. `scenarios/transactions.ts` — deterministic email addresses caused duplicate key violations on
   repeated runs. Fixed with a random `runId` prefix per execution.

2. `scenarios/poolBehavior.ts` — rewritten to count unexpected PID behaviour as errors instead of
   notes.

3. `scenarios/connectionStorms.ts` — `node-postgres` exposes the error code as `e.code`. Filter
   added to skip `code === "53300"` (too_many_connections) rejections from Grunyas capacity limits.

4. `scenarios/preparedStatements.ts` — the PREPARE/EXECUTE/DEALLOCATE sequence was wrapped in
   `BEGIN`/`COMMIT` to pin the backend in transaction mode (same pattern as Go). Added
   `client.on("error", () => {})` on checked-out pool clients to suppress unhandled error events
   when Grunyas closes a connection mid-scenario. In transaction mode the named-statement block now
   runs serially: all 20 workers entering `BEGIN` simultaneously would require 20 backends but
   the transaction-mode pool has only 4, causing Grunyas to reject 16 with "connection pool
   exhausted". Serial execution ensures at most 1 backend is held at a time.

`node-postgres` (`pg`) uses unnamed extended-query-protocol messages for parameterized queries by
default (no auto-caching of named server-side statements), so no `prepareThreshold` equivalent was
needed for the other scenarios.

---

## Java — complete

| Scenario | Session | Transaction |
|---|---|---|
| basic_crud | pass — 0 errors | pass — 0 errors |
| transactions | pass — 0 errors | pass — 0 errors |
| prepared_statements | pass — 0 errors | pass — 0 errors |
| concurrent_rw | pass — 0 errors | pass — 0 errors |
| connection_storms | pass — 0 errors | pass — 0 errors |
| long_running | pass — 0 errors | pass — 0 errors |
| error_handling | pass — 0 errors | pass — 0 errors |
| batch_operations | pass — 0 errors | pass — 0 errors |
| pool_behavior | pass — 0 errors | pass — 0 errors |

Java simulator was written from scratch (only `pom.xml` and `Dockerfile` existed previously).
Uses HikariCP for connection pooling and pgjdbc for the PostgreSQL driver.

### Implementation notes

- `prepareThreshold=0` set on the HikariCP datasource in transaction mode to disable pgjdbc's
  server-side statement caching. pgjdbc names auto-prepared statements `S_1`, `S_2`, etc.; these
  don't survive backend switches in transaction mode.

- `prepared_statements`: SQL `PREPARE`/`EXECUTE`/`DEALLOCATE` wrapped in `BEGIN`/`COMMIT` to pin
  the backend for the full sequence — same pattern as Go.

- `batch_operations`: bulk INSERT uses `?::jsonb` cast in the SQL because pgjdbc sends `setString`
  parameters as `varchar`, which PostgreSQL rejects for JSONB columns without an explicit cast.

- `connection_storms`: filters `SQLState == "53300"` to exclude Grunyas capacity rejections from
  the error count.
