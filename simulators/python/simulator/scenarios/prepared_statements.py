"""Prepared statement scenarios: named/unnamed, reuse."""

import asyncio
import time

import psycopg


async def _worker(conninfo: str, worker_id: int, pool_mode: str, ops: list, errors: list, latencies: list):
    async with await psycopg.AsyncConnection.connect(conninfo) as conn:
        if pool_mode == "transaction":
            conn.prepare_threshold = None
        await conn.set_autocommit(True)

        # Unnamed prepared (parameterized queries — always safe in all pool modes)
        for i in range(10):
            t = time.monotonic()
            try:
                await conn.execute("SELECT count(*) FROM users WHERE balance > %s", (float(i * 100),))
            except Exception:
                errors.append(1)
            latencies.append((time.monotonic() - t) * 1000)
            ops.append(1)

        # Named prepared statement — wrapped in explicit BEGIN/COMMIT so the backend
        # is pinned for the full PREPARE → EXECUTE × 5 → DEALLOCATE sequence.
        # In transaction pool mode, Grunyas releases the backend after each ReadyForQuery;
        # a named statement on backend B1 won't exist on backend B2. Holding an open
        # transaction keeps the same backend throughout.
        stmt_name = f"stmt_w{worker_id}"
        t = time.monotonic()
        try:
            await conn.execute("BEGIN")
            await conn.execute(
                f"PREPARE {stmt_name} AS SELECT id, name, balance FROM users WHERE id = $1"
            )
            latencies.append((time.monotonic() - t) * 1000)
            ops.append(1)

            for i in range(5):
                t = time.monotonic()
                try:
                    await conn.execute(f"EXECUTE {stmt_name}({worker_id * 5 + i + 1})")
                except Exception:
                    errors.append(1)
                latencies.append((time.monotonic() - t) * 1000)
                ops.append(1)

            await conn.execute(f"DEALLOCATE {stmt_name}")
            ops.append(1)
            await conn.execute("COMMIT")
        except Exception:
            errors.append(1)
            ops.append(1)
            try:
                await conn.execute("ROLLBACK")
            except Exception:
                pass


async def run(config: dict) -> dict:
    conninfo = config["conninfo"]
    concurrency = config["concurrency"]
    pool_mode = config["pool_mode"]
    ops: list = []
    errors: list = []
    latencies: list = []

    start = time.monotonic()
    sem = asyncio.Semaphore(concurrency)

    async def bounded(wid):
        async with sem:
            await _worker(conninfo, wid, pool_mode, ops, errors, latencies)

    await asyncio.gather(*(bounded(i) for i in range(concurrency)))
    duration = (time.monotonic() - start) * 1000

    return {"total_ops": len(ops), "errors": len(errors), "duration_ms": duration, "latencies": latencies}
