# Simulator Scenarios

Nine scenarios run in sequence against Grunyas in both session and transaction pool modes.
All simulators (Go, Python, TypeScript, Java) implement the same nine scenarios so results
are directly comparable across languages.

Each scenario creates its own connection pool for the duration of the test, then tears it
down. The database schema is shared — see `shared/schema.sql`.

---

## Database schema

```
users   (id, name, email UNIQUE, balance, created_at)
orders  (id, user_id → users, amount, status, created_at)
events  (id, type, payload JSONB, created_at)
```

The `users` table is pre-seeded with 1,000 rows (`user_1@test.com` … `user_1000@test.com`)
at database initialisation time.

---

## 1 · basic_crud

**What it does**

`CONCURRENCY` workers each perform 20 iterations of the full INSERT → SELECT → UPDATE → DELETE
cycle on the `users` table. Each worker uses a unique `(worker_id, iteration)` key so rows don't
collide across workers. If any step fails the worker skips the remaining steps for that iteration
and counts one error.

**Total ops:** `concurrency × 20 × 4 = 1600` (with CONCURRENCY=20)

**Expected outcome:** 0 errors in both pool modes.

**Peculiarities**

- Rows are created and deleted within the same iteration, so the table size stays roughly stable
  across runs.
- Reads and writes are interleaved on the same (pool-acquired) connection, not dedicated clients,
  so in transaction mode each query may land on a different backend — this is exercising the
  basic correctness of stateless autocommit queries.

---

## 2 · transactions

**What it does**

`CONCURRENCY` workers each run 10 iterations of three transaction flows:

1. **Commit** — `BEGIN → INSERT INTO users → COMMIT`. The row is persisted.
2. **Rollback** — `BEGIN → INSERT INTO users → ROLLBACK`. The row is discarded.
3. **Savepoint** — `BEGIN → SAVEPOINT sp1 → INSERT → ROLLBACK TO sp1 → COMMIT`.
   The transaction commits but the row is not persisted because the savepoint was rolled back.

**Total ops:** `concurrency × 10 × 3 = 600` (with CONCURRENCY=20)

**Expected outcome:** 0 errors in both pool modes.

**Peculiarities**

- Email addresses include a random per-run `runID` prefix (`tx_<runID>_<worker>_<iter>@test.com`)
  to prevent `UNIQUE` constraint violations when the simulator is run repeatedly against the same
  database. Committed rows from previous runs would otherwise collide with the same deterministic
  addresses.
- Savepoint semantics are exercised without a rollback of the outer transaction, confirming
  partial-rollback support through the proxy.
- In transaction mode each explicit `BEGIN`/`COMMIT` block pins one backend for the duration
  of the transaction. With the default sizing (4 backends, 20 clients) all workers can be served
  concurrently because individual transactions are short.

---

## 3 · prepared_statements

**What it does**

`CONCURRENCY` workers each exercise two kinds of prepared statements:

1. **Unnamed (parameterized) queries** — 10 iterations of
   `SELECT count(*) FROM users WHERE balance > $1`. These use the driver's extended query
   protocol with an unnamed prepared statement that is re-parsed on every round-trip.
2. **Named prepared statement** — wraps `PREPARE <name> AS SELECT … WHERE id = $1` /
   `EXECUTE <name>(…)` × 5 / `DEALLOCATE <name>` in an explicit `BEGIN`/`COMMIT` transaction
   to pin the backend for the full sequence.

**Total ops:** `concurrency × (10 unnamed + 1 PREPARE + 5 EXECUTE + 1 DEALLOCATE) = 340`
(with CONCURRENCY=20)

**Expected outcome:** 0 errors in both pool modes.

**Core challenge: named statements in transaction pool mode**

SQL `PREPARE <name>` creates a named prepared statement scoped to the current backend connection.
In transaction pool mode Grunyas releases the backend after each transaction's `ReadyForQuery`.
If the `PREPARE`, `EXECUTE`, and `DEALLOCATE` were sent in separate autocommit round-trips, the
`EXECUTE` would land on a whichever backend is next available — which may not be the one that
holds the statement — and fail with `prepared statement "<name>" does not exist`.

The fix applied in all four simulators: wrap the entire named-statement sequence in a single
`BEGIN`/`COMMIT` block. Grunyas holds the backend assignment for the lifetime of the transaction,
so all three statements execute on the same backend.

**Driver auto-prepare threshold**

Most drivers silently promote frequently-reused parameterized queries into named server-side
prepared statements:

| Driver | Default threshold | Symptom in transaction mode | Fix |
|---|---|---|---|
| psycopg3 (Python) | 5 uses | `_pg3_0` statement "already exists" after the 5th execution of an identical query | `conn.prepare_threshold = None` |
| pgjdbc (Java) | 5 uses | Named statement `S_1` not found on new backend | `prepareThreshold=0` on datasource |
| node-postgres (TypeScript) | disabled | n/a — uses unnamed extended-query protocol by default | no change needed |
| pgx (Go) | statement cache with named keys | `stmtcache_<hash>` not found | `QueryExecModeCacheDescribe` on pool |

**Transaction mode: backend pool exhaustion**

In transaction mode the backend pool is sized for CPU throughput, not concurrency
(default: `2 × vCPUs = 4` backends). Each `BEGIN` holds a backend for the duration of
the transaction. If all `CONCURRENCY` workers simultaneously enter the named-statement
block, they require `CONCURRENCY` backends simultaneously — which exceeds the pool.

The simulators handle this differently:

- **Go, Java**: the serialization happens naturally because acquiring a pool connection blocks
  until one becomes available (the pool queues requests).
- **Python, TypeScript**: the named-statement block runs **serially** — workers execute it one
  at a time. This avoids oversubscribing the backend pool while still validating that the
  `BEGIN`/`COMMIT` pinning works correctly.

---

## 4 · concurrent_rw

**What it does**

`CONCURRENCY` workers each perform 20 iterations of mixed reads and writes, with a 70/30 ratio
decided per-iteration by a seeded RNG (seeded with `worker_id` for reproducibility):

- **Read (70%)**: `SELECT balance FROM users WHERE id = $1` on a random user from 1..1000.
- **Write (30%)**: balance transfer in a `BEGIN`/`COMMIT` transaction:
  `UPDATE users SET balance = balance - $1 WHERE id = ?` + `UPDATE users SET balance = balance + $1 WHERE id = ?`.

**Total ops:** `concurrency × 20 = 400` (with CONCURRENCY=20)

**Expected outcome:** 0 errors in both pool modes.

**Peculiarities**

- No referential integrity is checked — a write may transfer from/to the same user ID if the RNG
  produces the same value twice. This is intentional; the scenario tests proxy correctness under
  mixed load, not application-level data integrity.
- The seed data contains 1,000 users with positive balances. Balances can go negative over many
  runs if write-heavy workers repeatedly deduct from the same rows; the scenario does not validate
  final balance values, only that queries succeed.

---

## 5 · connection_storms

**What it does**

Fires `CONCURRENCY × 2` simultaneous raw connections (bypassing the pool) all at once. Each
connection opens, executes `SELECT 1`, and closes. All goroutines/tasks start at the same time
with no staggering, simulating a thundering-herd reconnect event.

**Total ops:** `concurrency × 2 = 40` (with CONCURRENCY=20)

**Expected outcome:** 0 errors in both pool modes.

**The capacity-rejection non-error**

In session mode, `CLIENT_MAX_CONNS` (the Grunyas client cap) is derived from
`0.75 × pg_max_connections` (37 for the default 2 GB PostgreSQL). With CONCURRENCY=20, the storm
fires 40 simultaneous connections — 3 over the cap. Grunyas correctly rejects the excess with
SQLSTATE 53300 (`too_many_connections`).

These rejections are **not errors** — they are the proxy enforcing its capacity limit. The
scenarios filter them out. What counts as an error is any failure on a connection that was
*within* the limit, i.e. an unexpected failure.

**Why raw connections (no pool)?**

A pool would queue connection requests and serialize them, defeating the purpose of the scenario.
The storm must attempt all connections truly simultaneously. Each simulator therefore uses the
lowest-level connect API available:

| Simulator | Connect API |
|---|---|
| Go | `pgx.ConnectConfig()` |
| Python | `psycopg.AsyncConnection.connect()` |
| TypeScript | `new Client().connect()` |
| Java | `DriverManager.getConnection()` |

**Transaction mode quirk (Go)**

pgx uses a two-phase protocol for fresh connections without a statement-description cache:
first `Parse+Describe+Sync` (which ends with `ReadyForQuery` → Grunyas releases backend),
then `Bind+Execute+Sync` (which arrives on a different backend with no prepared statement).
The Go simulator switches to `QueryExecModeSimpleProtocol` for raw connections in transaction
mode — simple queries are a single round-trip with no prepare phase.

**psycopg3 SSL exchange error (Python)**

Grunyas checks the client cap before reading the startup message from the client. When the cap
is full it sends the `ErrorResponse` (SQLSTATE 53300) immediately, before the client finishes
SSL negotiation. psycopg3 receives the error while waiting for the SSL handshake response and
raises a generic `OperationalError("server sent an error response during SSL exchange")` with
no `sqlstate` attribute. The scenario matches on this message substring in addition to SQLSTATE
53300.

---

## 6 · long_running

**What it does**

Runs three groups of workers concurrently to verify that slow queries don't starve fast ones:

1. **Sleep workers** (`workers / 2`): `SELECT pg_sleep(1)` — each holds a backend for ~1 s.
2. **Large result workers** (`workers / 2`): `SELECT generate_series(1, 10000)` — streams 10,000
   rows to exhaust buffering.
3. **Quick query workers** (`workers`): five fast `SELECT 1` queries each, interleaved with
   the slow queries.

`workers = min(CONCURRENCY, 20)` to avoid opening too many long-lived connections.

**Total ops:** `workers/2 + workers/2 + workers × 5 = workers × 6 = 120` (with workers=20)

**Expected outcome:** 0 errors in both pool modes.

**Peculiarities**

- The test validates that the pool scheduler does not starve the `SELECT 1` workers behind the
  sleeping ones. If the pool blocks all connections on `pg_sleep`, the fast workers will time out
  or error.
- In session mode the pool must be large enough to serve all groups simultaneously. The sizing
  model ensures `CLIENT_MAX_CONNS ≥ CONCURRENCY`, so this is satisfied by construction.
- In transaction mode the backend pool is small (4 by default). `pg_sleep(1)` holds a backend
  for the full second; with `workers/2 = 10` sleep workers and 4 backends, some sleep workers
  will queue. The quick-query workers can still proceed because their transactions are short and
  release backends between queries. The scenario uses a shared `AsyncConnectionPool` (Python)
  or `pgxpool` (Go) to let the pool manage backend allocation.

**Connection count budget (Python)**

An earlier version of this scenario opened `workers/2 + workers/2 + workers = 2 × workers`
concurrent raw connections. With `workers=20` that is 40 connections — exceeding the session-mode
cap of 37. The scenario now uses a single shared connection pool that internally bounds
concurrency, preventing cap exhaustion.

---

## 7 · error_handling

**What it does**

`CONCURRENCY` workers each exercise three error conditions and verify that the connection remains
usable after each one:

1. **Syntax error** — `SELEKT invalid_syntax FROM nowhere`. Expects PostgreSQL to reject it;
   counts an error if it unexpectedly succeeds. Follows with `SELECT 1` to verify the
   connection is still alive.
2. **Unique constraint violation** — `INSERT INTO users (email, …) VALUES ('user_1@test.com', …)`.
   `user_1@test.com` is a seed row, so this should fail with SQLSTATE 23505. Follows with
   `SELECT 1`.
3. **Division by zero** — `SELECT 1/0`. Expects PostgreSQL SQLSTATE 22012. Follows with
   `SELECT 42` and checks the returned value is exactly 42.

**Total ops:** `concurrency × 6 = 120` (with CONCURRENCY=20)

**Expected outcome:** 0 errors in both pool modes.

**Peculiarities**

- The constraint violation step may unexpectedly *succeed* if another worker (or a previous
  scenario) deleted `user_1@test.com`. The scenario treats this as non-fatal — it only counts
  an error if the subsequent health-check `SELECT 1` fails.
- The core assertion is: after any of these errors, the proxy must return a connection that
  is in a clean, usable state. A broken connection pool (one that returns a connection still
  in an error state) would cause the `SELECT 1` follow-up to fail.
- In transaction mode, errors inside a transaction leave PostgreSQL in an aborted state
  (`current transaction is aborted`). The proxy must handle this cleanly — either the pool
  discards the aborted backend or the client explicitly issues `ROLLBACK`. The scenario uses
  autocommit (one statement per round-trip) so each error is self-contained.

---

## 8 · batch_operations

**What it does**

`CONCURRENCY` workers each run three batch-oriented operations:

1. **Transactional bulk INSERT** — `BEGIN`, then 100 × `INSERT INTO events (type, payload) VALUES …`,
   then `COMMIT`. All 100 rows land in one transaction.
2. **Multi-row VALUES INSERT** — a single statement inserting 5 rows:
   `INSERT INTO events … VALUES ('multi_1', …), ('multi_2', …), … ('multi_5', …)`.
3. **Bulk read** — `SELECT id, type, payload FROM events WHERE type = $1 LIMIT 100` to read back
   the rows inserted in step 1.

**Total ops:** `concurrency × 3 = 60` (with CONCURRENCY=20)

**Expected outcome:** 0 errors in both pool modes.

**Peculiarities**

- The `payload` column is type `JSONB`. Some drivers send string parameters as `varchar`, which
  PostgreSQL rejects for a JSONB column without an explicit cast. The Java simulator uses
  `?::jsonb` in the SQL text to force the cast; Go passes the value as a Go `string` which
  pgx maps correctly.
- The transactional bulk INSERT counts as one op (the whole transaction), not 100. This means
  a single transactional failure writes zero of the 100 rows to the database and counts as one
  error.
- In transaction mode, the `BEGIN`/`COMMIT` block in step 1 holds one backend for the entire
  100-INSERT sequence. With the default 4-backend pool and 20 concurrent workers, all workers
  will be served — they just queue if all backends are busy. Because each transaction is fast
  (100 parameterized INSERTs in a batch), contention is brief.

---

## 9 · pool_behavior

**What it does**

`min(CONCURRENCY, 50)` workers each acquire a **dedicated connection** (not pooled queries)
and run 10 consecutive `SELECT pg_backend_pid()` queries on it. The backend PID is the
PostgreSQL process ID — different backends have different PIDs.

After all workers complete, the scenario checks the PID distribution per worker:

- **Session mode**: all 10 queries on a worker must return the same PID. If the PID changes,
  the proxy incorrectly released the backend mid-session — counted as one error per worker.
- **Transaction mode**: the 10 queries should span multiple different PIDs. If all 10 return
  the same PID, multiplexing was not observed — counted as one error per worker.

**Total ops:** `workers × 10 = 200` (with workers=20)

**Expected outcome:** 0 errors in both pool modes.

**Peculiarities**

This scenario is the most sensitive to pool configuration. Its signal degrades in two ways:

**False negative in transaction mode (too few backends):**

If the backend pool has only one backend, every query from every client lands on that one
PostgreSQL process. The PID never changes even though Grunyas is correctly releasing and
re-acquiring the backend between queries. The scenario would report errors for all workers
even though the proxy is functioning correctly. This cannot happen with the default sizing
(4 backends), but matters when running with minimal resources.

**False positive in transaction mode (PID collision):**

Even with B backends, a worker might see the same PID for all 10 queries by chance — if the
pool keeps assigning the same backend after each release. The probability is `(1/B)^9`.
With B=4 that is ~0.00015% per worker, negligible. But with B=2 it rises to ~0.2% per worker —
at CONCURRENCY=50 workers, roughly 1 worker would false-positive per run on average.

**Driver auto-prepare interaction (Python):**

psycopg3 auto-prepares queries executed ≥ 5 times on the same connection as named server-side
prepared statements (`_pg3_0`, etc.). In transaction mode, `pg_backend_pid()` is run 10 times
on the same client connection — the 6th call triggers auto-prepare. The named statement lands on
backend B1; after the next backend release it may land on B2, where it doesn't exist. Fix:
`conn.prepare_threshold = None` in transaction mode.
