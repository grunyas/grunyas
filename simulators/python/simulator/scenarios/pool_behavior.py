"""Pool behavior: track pg_backend_pid() to observe connection reuse."""

import asyncio
import time

import psycopg


async def _worker(conninfo: str, worker_id: int, pool_mode: str, ops: list, errors: list, latencies: list):
    pids = set()
    async with await psycopg.AsyncConnection.connect(conninfo) as conn:
        if pool_mode == "transaction":
            conn.prepare_threshold = None
        await conn.set_autocommit(True)
        for _ in range(10):
            t = time.monotonic()
            try:
                cur = await conn.execute("SELECT pg_backend_pid()")
                row = await cur.fetchone()
                pids.add(row[0])
            except Exception:
                errors.append(1)
            latencies.append((time.monotonic() - t) * 1000)
            ops.append(1)

    # In session mode, PID changes are unexpected — count as errors.
    # In transaction mode, no PID change means multiplexing wasn't observed — count as errors.
    if pool_mode == "session" and len(pids) > 1:
        errors.append(1)
    elif pool_mode == "transaction" and len(pids) <= 1:
        errors.append(1)


async def run(config: dict) -> dict:
    conninfo = config["conninfo"]
    concurrency = min(config["concurrency"], 50)
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
