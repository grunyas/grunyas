from . import (
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


def is_capacity_error(err: Exception) -> bool:
    """SQLSTATE 53300 (too_many_connections) means Grunyas is correctly enforcing
    its client cap. Not a real error in any pool mode."""
    sqlstate = getattr(err, "sqlstate", None) or getattr(err, "pgcode", None)
    msg = str(err).lower()
    return (sqlstate == "53300"
            or "connection pool exhausted" in msg
            or "error response during ssl" in msg)


__all__ = [
    "basic_crud",
    "transactions",
    "prepared_statements",
    "concurrent_rw",
    "connection_storms",
    "long_running",
    "error_handling",
    "batch_operations",
    "pool_behavior",
    "is_capacity_error",
]
