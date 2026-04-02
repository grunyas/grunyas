"""Prepared statement scenarios: named/unnamed, reuse."""

import asyncio
import time

import psycopg


async def _worker(conninfo: str, worker_id: int, ops: list, errors: list, latencies: list, notes: list):
    async with await psycopg.AsyncConnection.connect(conninfo) as conn:
        await conn.set_autocommit(True)

        # Unnamed prepared (parameterized queries — always safe)
        for i in range(10):
            t = time.monotonic()
            try:
                await conn.execute("SELECT count(*) FROM users WHERE balance > %s", (float(i * 100),))
            except Exception:
                errors.append(1)
            latencies.append((time.monotonic() - t) * 1000)
            ops.append(1)

        # Named prepared statement
        stmt_name = f"stmt_w{worker_id}"
        t = time.monotonic()
        try:
            # psycopg3 uses server-side prepared statements via prepare=True
            cur = await conn.execute(
                "SELECT id, name, balance FROM users WHERE id = %s",
                (worker_id + 1,),
                prepare=True,
            )
            ops.append(1)
        except Exception as e:
            errors.append(1)
            ops.append(1)
            if not notes:
                notes.append(f"prepared statement failed: {e}")
            return
        latencies.append((time.monotonic() - t) * 1000)

        # Reuse prepared statement
        for i in range(5):
            t = time.monotonic()
            try:
                await conn.execute(
                    "SELECT id, name, balance FROM users WHERE id = %s",
                    (worker_id * 5 + i + 1,),
                    prepare=True,
                )
            except Exception:
                errors.append(1)
            latencies.append((time.monotonic() - t) * 1000)
            ops.append(1)


async def run(config: dict) -> dict:
    conninfo = config["conninfo"]
    concurrency = config["concurrency"]
    ops: list = []
    errors: list = []
    latencies: list = []
    notes: list = []

    start = time.monotonic()
    sem = asyncio.Semaphore(concurrency)

    async def bounded(wid):
        async with sem:
            await _worker(conninfo, wid, ops, errors, latencies, notes)

    await asyncio.gather(*(bounded(i) for i in range(concurrency)))
    duration = (time.monotonic() - start) * 1000

    return {"total_ops": len(ops), "errors": len(errors), "duration_ms": duration, "latencies": latencies, "notes": notes}
