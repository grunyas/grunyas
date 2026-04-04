"""Long-running queries: pg_sleep and large result sets."""

import asyncio
import time

from psycopg_pool import AsyncConnectionPool


async def run(config: dict) -> dict:
    conninfo = config["conninfo"]
    workers = min(config["concurrency"], 20)
    pool_mode = config["pool_mode"]
    ops: list = []
    errors: list = []
    latencies: list = []

    # Use a shared pool so concurrent workers never exceed the concurrency cap.
    # This mirrors Go's long_running which uses pgxpool throughout.
    # In transaction mode, disable server-side prepared statements so queries
    # work correctly across backend switches.
    pool_kwargs = {"prepare_threshold": None} if pool_mode == "transaction" else {}

    async with AsyncConnectionPool(conninfo, min_size=1, max_size=workers, kwargs=pool_kwargs) as pool:

        async def sleep_worker():
            t = time.monotonic()
            try:
                async with pool.connection() as conn:
                    await conn.execute("SELECT pg_sleep(1)")
                    ops.append(1)
            except Exception:
                errors.append(1)
                ops.append(1)
            latencies.append((time.monotonic() - t) * 1000)

        async def large_result_worker():
            t = time.monotonic()
            try:
                async with pool.connection() as conn:
                    cur = await conn.execute("SELECT generate_series(1, 10000)")
                    rows = await cur.fetchall()
                    _ = len(rows)
                    ops.append(1)
            except Exception:
                errors.append(1)
                ops.append(1)
            latencies.append((time.monotonic() - t) * 1000)

        async def quick_worker():
            async with pool.connection() as conn:
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
