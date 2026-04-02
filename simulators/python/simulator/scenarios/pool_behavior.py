"""Pool behavior: track pg_backend_pid() to observe connection reuse."""

import asyncio
import time

import psycopg


async def _worker(conninfo: str, worker_id: int, ops: list, errors: list, latencies: list, pid_results: dict):
    pids = set()
    async with await psycopg.AsyncConnection.connect(conninfo) as conn:
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

    pid_results[worker_id] = len(pids) > 1


async def run(config: dict) -> dict:
    conninfo = config["conninfo"]
    concurrency = min(config["concurrency"], 50)
    pool_mode = config["pool_mode"]
    ops: list = []
    errors: list = []
    latencies: list = []
    pid_results: dict = {}

    start = time.monotonic()
    sem = asyncio.Semaphore(concurrency)

    async def bounded(wid):
        async with sem:
            await _worker(conninfo, wid, ops, errors, latencies, pid_results)

    await asyncio.gather(*(bounded(i) for i in range(concurrency)))
    duration = (time.monotonic() - start) * 1000

    changed = sum(1 for v in pid_results.values() if v)
    total = len(pid_results)

    notes = []
    if pool_mode == "session":
        if changed > 0:
            notes.append(f"session mode: {changed}/{total} workers saw PID changes (unexpected)")
        else:
            notes.append(f"session mode: all {total} workers maintained same PID (expected)")
    else:
        notes.append(f"transaction mode: {changed}/{total} workers saw PID changes")

    return {"total_ops": len(ops), "errors": len(errors), "duration_ms": duration, "latencies": latencies, "notes": notes}
