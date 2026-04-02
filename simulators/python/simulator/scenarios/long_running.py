"""Long-running queries: pg_sleep and large result sets."""

import asyncio
import time

import psycopg


async def run(config: dict) -> dict:
    conninfo = config["conninfo"]
    workers = min(config["concurrency"], 20)
    ops: list = []
    errors: list = []
    latencies: list = []

    async def sleep_worker():
        t = time.monotonic()
        try:
            async with await psycopg.AsyncConnection.connect(conninfo) as conn:
                await conn.set_autocommit(True)
                await conn.execute("SELECT pg_sleep(1)")
                ops.append(1)
        except Exception:
            errors.append(1)
            ops.append(1)
        latencies.append((time.monotonic() - t) * 1000)

    async def large_result_worker():
        t = time.monotonic()
        try:
            async with await psycopg.AsyncConnection.connect(conninfo) as conn:
                await conn.set_autocommit(True)
                cur = await conn.execute("SELECT generate_series(1, 10000)")
                rows = await cur.fetchall()
                _ = len(rows)
                ops.append(1)
        except Exception:
            errors.append(1)
            ops.append(1)
        latencies.append((time.monotonic() - t) * 1000)

    async def quick_worker():
        async with await psycopg.AsyncConnection.connect(conninfo) as conn:
            await conn.set_autocommit(True)
            for _ in range(5):
                t = time.monotonic()
                try:
                    await conn.execute("SELECT 1")
                except Exception:
                    errors.append(1)
                latencies.append((time.monotonic() - t) * 1000)
                ops.append(1)

    start = time.monotonic()
    tasks = []
    for _ in range(workers // 2):
        tasks.append(sleep_worker())
    for _ in range(workers // 2):
        tasks.append(large_result_worker())
    for _ in range(workers):
        tasks.append(quick_worker())

    await asyncio.gather(*tasks)
    duration = (time.monotonic() - start) * 1000

    return {"total_ops": len(ops), "errors": len(errors), "duration_ms": duration, "latencies": latencies}
