"""Batch operations: bulk inserts and multi-row queries."""

import asyncio
import time

import psycopg


async def _worker(conninfo: str, worker_id: int, pool_mode: str, ops: list, errors: list, latencies: list):
    async with await psycopg.AsyncConnection.connect(conninfo) as conn:
        if pool_mode == "transaction":
            conn.prepare_threshold = None
        # Bulk INSERT in a transaction
        t = time.monotonic()
        try:
            async with conn.transaction():
                for j in range(100):
                    await conn.execute(
                        "INSERT INTO events (type, payload) VALUES (%s, %s)",
                        (f"batch_event_{worker_id}", f'{{"worker":{worker_id},"iter":{j}}}'),
                    )
            ops.append(1)
        except Exception:
            errors.append(1)
            ops.append(1)
        latencies.append((time.monotonic() - t) * 1000)

        # Multi-row INSERT
        await conn.set_autocommit(True)
        t = time.monotonic()
        try:
            await conn.execute(
                """INSERT INTO events (type, payload) VALUES
                   ('multi_1', '{"source":"batch"}'),
                   ('multi_2', '{"source":"batch"}'),
                   ('multi_3', '{"source":"batch"}'),
                   ('multi_4', '{"source":"batch"}'),
                   ('multi_5', '{"source":"batch"}')"""
            )
            ops.append(1)
        except Exception:
            errors.append(1)
            ops.append(1)
        latencies.append((time.monotonic() - t) * 1000)

        # Bulk read
        t = time.monotonic()
        try:
            cur = await conn.execute(
                "SELECT id, type, payload FROM events WHERE type = %s LIMIT 100",
                (f"batch_event_{worker_id}",),
            )
            rows = await cur.fetchall()
            _ = len(rows)
            ops.append(1)
        except Exception:
            errors.append(1)
            ops.append(1)
        latencies.append((time.monotonic() - t) * 1000)


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
