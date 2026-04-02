"""Connection storms: rapid connect/disconnect cycles."""

import asyncio
import time

import psycopg


async def _storm(conninfo: str, ops: list, errors: list, latencies: list):
    t = time.monotonic()
    try:
        async with await psycopg.AsyncConnection.connect(conninfo) as conn:
            await conn.set_autocommit(True)
            await conn.execute("SELECT 1")
            ops.append(1)
    except Exception:
        errors.append(1)
        ops.append(1)
    latencies.append((time.monotonic() - t) * 1000)


async def run(config: dict) -> dict:
    conninfo = config["conninfo"]
    ops: list = []
    errors: list = []
    latencies: list = []

    start = time.monotonic()
    storms = config["concurrency"] * 2
    await asyncio.gather(*(_storm(conninfo, ops, errors, latencies) for _ in range(storms)))
    duration = (time.monotonic() - start) * 1000

    return {"total_ops": len(ops), "errors": len(errors), "duration_ms": duration, "latencies": latencies}
