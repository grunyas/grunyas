"""Basic CRUD operations: INSERT, SELECT, UPDATE, DELETE."""

import asyncio
import time

import psycopg


async def _worker(conninfo: str, worker_id: int, ops: list, errors: list, latencies: list):
    async with await psycopg.AsyncConnection.connect(conninfo) as conn:
        await conn.set_autocommit(True)
        for i in range(20):
            email = f"crud_{worker_id}_{i}@test.com"

            # INSERT
            t = time.monotonic()
            try:
                cur = await conn.execute(
                    "INSERT INTO users (name, email, balance) VALUES (%s, %s, %s) RETURNING id",
                    (f"crud_user_{worker_id}_{i}", email, 100.00),
                )
                row = await cur.fetchone()
                user_id = row[0]
            except Exception:
                errors.append(1)
                ops.append(1)
                latencies.append((time.monotonic() - t) * 1000)
                continue
            latencies.append((time.monotonic() - t) * 1000)
            ops.append(1)

            # SELECT
            t = time.monotonic()
            try:
                await conn.execute("SELECT name FROM users WHERE id = %s", (user_id,))
            except Exception:
                errors.append(1)
            latencies.append((time.monotonic() - t) * 1000)
            ops.append(1)

            # UPDATE
            t = time.monotonic()
            try:
                await conn.execute("UPDATE users SET balance = balance + 50 WHERE id = %s", (user_id,))
            except Exception:
                errors.append(1)
            latencies.append((time.monotonic() - t) * 1000)
            ops.append(1)

            # DELETE
            t = time.monotonic()
            try:
                await conn.execute("DELETE FROM users WHERE id = %s", (user_id,))
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

    start = time.monotonic()
    sem = asyncio.Semaphore(concurrency)

    async def bounded_worker(wid):
        async with sem:
            await _worker(conninfo, wid, ops, errors, latencies)

    await asyncio.gather(*(bounded_worker(i) for i in range(concurrency)))
    duration = (time.monotonic() - start) * 1000

    return {
        "total_ops": len(ops),
        "errors": len(errors),
        "duration_ms": duration,
        "latencies": latencies,
    }
