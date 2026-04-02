"""Error handling: invalid SQL, constraint violations, verify recovery."""

import asyncio
import time

import psycopg


async def _worker(conninfo: str, worker_id: int, ops: list, errors: list, latencies: list, notes: list):
    async with await psycopg.AsyncConnection.connect(conninfo) as conn:
        await conn.set_autocommit(True)

        # Invalid SQL
        t = time.monotonic()
        try:
            await conn.execute("SELEKT invalid_syntax FROM nowhere")
            errors.append(1)  # should have raised
        except Exception:
            pass  # expected
        latencies.append((time.monotonic() - t) * 1000)
        ops.append(1)

        # Verify connection still works
        t = time.monotonic()
        try:
            cur = await conn.execute("SELECT 1")
            row = await cur.fetchone()
            if row[0] != 1:
                errors.append(1)
        except Exception as e:
            errors.append(1)
            if len(notes) < 5:
                notes.append(f"connection broken after error: {e}")
        latencies.append((time.monotonic() - t) * 1000)
        ops.append(1)

        # Unique constraint violation
        t = time.monotonic()
        try:
            await conn.execute(
                "INSERT INTO users (name, email, balance) VALUES (%s, %s, %s)",
                ("dup_user", "user_1@test.com", 0),
            )
        except Exception:
            pass  # expected
        latencies.append((time.monotonic() - t) * 1000)
        ops.append(1)

        # Verify recovery
        t = time.monotonic()
        try:
            await conn.execute("SELECT 1")
        except Exception:
            errors.append(1)
        latencies.append((time.monotonic() - t) * 1000)
        ops.append(1)

        # Division by zero
        t = time.monotonic()
        try:
            await conn.execute("SELECT 1/0")
            errors.append(1)
        except Exception:
            pass
        latencies.append((time.monotonic() - t) * 1000)
        ops.append(1)

        # Final recovery check
        t = time.monotonic()
        try:
            cur = await conn.execute("SELECT 42")
            row = await cur.fetchone()
            if row[0] != 42:
                errors.append(1)
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
