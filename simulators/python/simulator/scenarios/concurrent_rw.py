"""Concurrent reads and writes with balance transfers."""

import asyncio
import random
import time

import psycopg


async def _worker(conninfo: str, worker_id: int, pool_mode: str, ops: list, errors: list, latencies: list):
    rng = random.Random(worker_id)
    async with await psycopg.AsyncConnection.connect(conninfo) as conn:
        if pool_mode == "transaction":
            conn.prepare_threshold = None
        for _ in range(20):
            user_id = rng.randint(1, 1000)

            if rng.random() < 0.7:
                # Read
                t = time.monotonic()
                try:
                    await conn.execute("SELECT balance FROM users WHERE id = %s", (user_id,))
                    await conn.commit()
                except Exception:
                    errors.append(1)
                latencies.append((time.monotonic() - t) * 1000)
                ops.append(1)
            else:
                # Write — transfer
                other_id = rng.randint(1, 1000)
                amount = rng.random() * 10
                t = time.monotonic()
                try:
                    async with conn.transaction():
                        await conn.execute("UPDATE users SET balance = balance - %s WHERE id = %s", (amount, user_id))
                        await conn.execute("UPDATE users SET balance = balance + %s WHERE id = %s", (amount, other_id))
                except Exception:
                    errors.append(1)
                latencies.append((time.monotonic() - t) * 1000)
                ops.append(1)


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
