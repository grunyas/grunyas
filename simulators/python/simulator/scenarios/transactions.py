"""Transaction scenarios: BEGIN/COMMIT, ROLLBACK, SAVEPOINTs."""

import asyncio
import random
import time

import psycopg


async def _worker(conninfo: str, worker_id: int, run_id: int, pool_mode: str, ops: list, errors: list, latencies: list):
    async with await psycopg.AsyncConnection.connect(conninfo) as conn:
        if pool_mode == "transaction":
            conn.prepare_threshold = None
        for i in range(10):
            # Commit flow
            t = time.monotonic()
            try:
                async with conn.transaction():
                    await conn.execute(
                        "INSERT INTO users (name, email, balance) VALUES (%s, %s, %s)",
                        (f"tx_user_{worker_id}_{i}", f"tx_{run_id}_{worker_id}_{i}@test.com", 500.00),
                    )
                ops.append(1)
            except Exception:
                errors.append(1)
                ops.append(1)
            latencies.append((time.monotonic() - t) * 1000)

            # Rollback flow
            t = time.monotonic()
            try:
                async with conn.transaction():
                    await conn.execute(
                        "INSERT INTO users (name, email, balance) VALUES (%s, %s, %s)",
                        ("will_rollback", f"rb_{worker_id}_{i}@test.com", 999.99),
                    )
                    raise psycopg.errors.QueryCanceled("intentional rollback")
            except psycopg.errors.QueryCanceled:
                pass
            except Exception:
                pass  # transaction rolled back
            ops.append(1)
            latencies.append((time.monotonic() - t) * 1000)

            # Savepoint flow
            t = time.monotonic()
            try:
                async with conn.transaction():
                    async with conn.transaction(savepoint_name="sp1"):
                        await conn.execute(
                            "INSERT INTO users (name, email, balance) VALUES (%s, %s, %s)",
                            ("sp_user", f"sp_{worker_id}_{i}@test.com", 100.00),
                        )
                        raise Exception("rollback to savepoint")
            except Exception:
                pass
            ops.append(1)
            latencies.append((time.monotonic() - t) * 1000)


async def run(config: dict) -> dict:
    conninfo = config["conninfo"]
    concurrency = config["concurrency"]
    pool_mode = config["pool_mode"]
    # Unique per-run prefix prevents duplicate key violations on repeated runs.
    run_id = random.randrange(2**32)
    ops: list = []
    errors: list = []
    latencies: list = []

    start = time.monotonic()
    sem = asyncio.Semaphore(concurrency)

    async def bounded(wid):
        async with sem:
            await _worker(conninfo, wid, run_id, pool_mode, ops, errors, latencies)

    await asyncio.gather(*(bounded(i) for i in range(concurrency)))
    duration = (time.monotonic() - start) * 1000

    return {"total_ops": len(ops), "errors": len(errors), "duration_ms": duration, "latencies": latencies}
