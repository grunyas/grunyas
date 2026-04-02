# Simulator Status

Each simulator (Go, Java, Python, TypeScript) runs the same 9 scenarios against Grunyas in both
session and transaction pool modes, producing a JSON report for comparison.

---

## Go — complete

The Go simulator was the first to be implemented and the primary focus of debugging work. It now
passes all scenarios in both modes.

### Results (latest run.sh, CONCURRENCY=20)

| Scenario | Session | Transaction |
|---|---|---|
| basic_crud | pass — 0 errors | pass — 0 errors |
| transactions | pass — 0 errors | pass — 0 errors |
| prepared_statements | pass — 0 errors | partial — ~13 errors |
| concurrent_rw | pass — 0 errors | pass — 0 errors |
| connection_storms | pass — 0 errors | pass — 0 errors |
| long_running | pass — 0 errors | pass — 0 errors |
| error_handling | pass — 0 errors | pass — 0 errors |
| batch_operations | pass — 0 errors | pass — 0 errors |
| pool_behavior | pass — 0 errors | pass — 0 errors |

`prepared_statements` partial in transaction mode is expected: SQL `PREPARE` creates named server-side
state that does not survive backend switches. Unnamed/parameterized queries work correctly.

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
   client before closing.

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
   Notes (which forced a `partial` status even with 0 errors and a correct outcome). Added
   documentation on the reliability requirement: the backend pool must have more than one connection,
   and the false-negative probability per worker is `(1/B)^(N-1)` where B is the backend count and
   N is the number of sequential queries.

---

## Java, Python, TypeScript — not yet validated

All three simulators have code and results on disk, but those results pre-date the Go fixes and
have not been re-run with the corrected Grunyas binary.

### Known issues (from stale results)

| Issue | Java | Python | TypeScript |
|---|---|---|---|
| `transactions` — duplicate key in transaction mode | 100 errors | 100 errors | 100 errors |
| `batch_operations` — unknown errors in both modes | 10 errors | — | — |
| `pool_behavior` — note-based partial instead of pass/fail | yes | yes | yes |

The `transactions` duplicate key issue is the same root cause as Go item 7 above: deterministic
email addresses committed to the database pollute subsequent runs. Each simulator's transactions
scenario needs a random run-scoped prefix.

The Java `batch_operations` errors are uninvestigated.

The `pool_behavior` note-based partial is the same as Go item 9 above: all three simulators need
the same fix (count unexpected PID behavior as errors, remove Notes).

### What needs to be done

- [ ] Fix `transactions` duplicate key in Java, Python, TypeScript (random run ID)
- [ ] Investigate and fix Java `batch_operations` errors
- [ ] Fix `pool_behavior` pass/fail logic in Java, Python, TypeScript
- [ ] Run `./run.sh` in each simulator to produce clean validated results
- [ ] Verify that Python and TypeScript use a transaction-mode-safe query exec mode (equivalent of
      `QueryExecModeCacheDescribe`) — if their drivers use named prepared statements by default,
      they will hit the same backend-switch failures that Go had

---

## Expected partial results (by design)

These are not bugs — they reflect genuine limitations of transaction pool mode:

- **`prepared_statements` — transaction mode**: SQL `PREPARE <name> AS ...` creates a named
  server-side prepared statement scoped to the session. When Grunyas releases the backend after a
  transaction, that named statement is gone. Drivers that fall back to unnamed/parameterized queries
  work fine; this scenario intentionally exercises both paths to confirm which works.

- **`pool_behavior` — transaction mode with backend count ≥ worker count**: each worker may
  consistently get the same backend re-assigned after releasing it, producing no observable PID
  change even though transaction-mode multiplexing is functioning correctly. This is a measurement
  artifact, not a proxy bug.
