"""Grunyas Python Simulator — entry point."""

import asyncio
import json
import os
import time
from datetime import datetime, timezone
from pathlib import Path

import psycopg
from psycopg_pool import AsyncConnectionPool

from simulator.scenarios import (
    basic_crud,
    transactions,
    prepared_statements,
    concurrent_rw,
    connection_storms,
    long_running,
    error_handling,
    batch_operations,
    pool_behavior,
)


def env(key: str, default: str = "") -> str:
    return os.environ.get(key, default)


def env_int(key: str, default: int = 0) -> int:
    v = os.environ.get(key)
    return int(v) if v else default


async def wait_for_db(conninfo: str, retries: int = 30) -> None:
    for i in range(retries):
        try:
            async with await psycopg.AsyncConnection.connect(conninfo) as conn:
                await conn.execute("SELECT 1")
                return
        except Exception:
            print(f"Waiting for database... attempt {i + 1}/{retries}")
            await asyncio.sleep(2)
    raise RuntimeError("Failed to connect to database after retries")


def compute_latency(latencies: list[float]) -> dict:
    if not latencies:
        return {"min_ms": 0, "max_ms": 0, "avg_ms": 0, "p50_ms": 0, "p95_ms": 0, "p99_ms": 0}
    s = sorted(latencies)
    n = len(s)
    return {
        "min_ms": round(s[0], 3),
        "max_ms": round(s[-1], 3),
        "avg_ms": round(sum(s) / n, 3),
        "p50_ms": round(s[n * 50 // 100], 3),
        "p95_ms": round(s[n * 95 // 100], 3),
        "p99_ms": round(s[min(n * 99 // 100, n - 1)], 3),
    }


async def main() -> None:
    db_host = env("DB_HOST", "localhost")
    db_port = env("DB_PORT", "5711")
    db_user = env("DB_USER", "postgres")
    db_password = env("DB_PASSWORD", "postgres")
    db_name = env("DB_NAME", "simulator")
    concurrency = env_int("CONCURRENCY", 100)
    pool_mode = env("POOL_MODE", "session")

    conninfo = f"host={db_host} port={db_port} user={db_user} password={db_password} dbname={db_name}"

    print(f"Python Simulator starting: pool_mode={pool_mode} concurrency={concurrency} host={db_host}:{db_port}")

    await wait_for_db(conninfo)
    print("Connected successfully, running scenarios...")

    config = {
        "conninfo": conninfo,
        "concurrency": concurrency,
        "pool_mode": pool_mode,
        "db_host": db_host,
        "db_port": db_port,
        "db_user": db_user,
        "db_password": db_password,
        "db_name": db_name,
    }

    all_scenarios = [
        ("basic_crud", basic_crud.run),
        ("transactions", transactions.run),
        ("prepared_statements", prepared_statements.run),
        ("concurrent_rw", concurrent_rw.run),
        ("connection_storms", connection_storms.run),
        ("long_running", long_running.run),
        ("error_handling", error_handling.run),
        ("batch_operations", batch_operations.run),
        ("pool_behavior", pool_behavior.run),
    ]

    results = []
    for name, fn in all_scenarios:
        print(f"  Running scenario: {name}")
        try:
            result = await fn(config)
            duration_ms = result["duration_ms"]
            total_ops = result["total_ops"]
            errors = result["errors"]
            ops_per_sec = total_ops / (duration_ms / 1000) if duration_ms > 0 else 0
            notes = result.get("notes", [])

            if errors == 0 and not notes:
                status = "pass"
            elif errors < total_ops / 2:
                status = "partial"
            else:
                status = "fail"

            scenario_result = {
                "name": name,
                "status": status,
                "duration_ms": round(duration_ms, 2),
                "total_ops": total_ops,
                "ops_per_sec": round(ops_per_sec, 2),
                "errors": errors,
                "latency": compute_latency(result.get("latencies", [])),
                "notes": notes,
            }
            print(f"  {name}: status={status} ops={total_ops} errors={errors} ops/s={ops_per_sec:.0f}")
            results.append(scenario_result)
        except Exception as e:
            print(f"  FAIL: {name}: {e}")
            results.append({
                "name": name,
                "status": "fail",
                "duration_ms": 0,
                "total_ops": 0,
                "ops_per_sec": 0,
                "errors": 1,
                "latency": compute_latency([]),
                "notes": [f"fatal error: {e}"],
            })

    # Build summary
    total_ops = sum(r["total_ops"] for r in results)
    total_errors = sum(r["errors"] for r in results)
    total_duration = sum(r["duration_ms"] for r in results)
    passed = sum(1 for r in results if r["status"] in ("pass", "partial"))
    failed = sum(1 for r in results if r["status"] == "fail")

    report = {
        "simulator": "python",
        "timestamp": datetime.now(timezone.utc).isoformat(),
        "config": {"concurrency": concurrency, "driver": "psycopg3"},
        "runs": [
            {
                "pool_mode": pool_mode,
                "scenarios": results,
                "summary": {
                    "total_duration_ms": round(total_duration, 2),
                    "total_ops": total_ops,
                    "total_errors": total_errors,
                    "scenarios_passed": passed,
                    "scenarios_failed": failed,
                },
            }
        ],
    }

    out_path = Path("results") / f"{pool_mode}.json"
    out_path.parent.mkdir(parents=True, exist_ok=True)
    out_path.write_text(json.dumps(report, indent=2))
    print(f"Results written to {out_path}")
    print(f"Summary: {passed} scenarios passed, {failed} failed, {total_ops} total ops, {total_errors} errors")


if __name__ == "__main__":
    asyncio.run(main())
