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
    except Exception as e:
        # Grunyas sends SQLSTATE 53300 (too_many_connections) when the client cap is hit.
        # psycopg3 can't extract the SQLSTATE because Grunyas sends the error response
        # before the SSL handshake completes -- the message arrives as an OperationalError
        # with the text "server sent an error response during SSL exchange".
        # This is correct proxy behaviour -- not an error in the scenario.
        msg = str(e)
        sqlstate = getattr(e, "sqlstate", None) or getattr(e, "pgcode", None)
        if (sqlstate == "53300"
                or "connection pool exhausted" in msg
                or "error response during SSL" in msg):
            pass
        else:
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
