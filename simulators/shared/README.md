# Grunyas Simulator Shared Artifacts

## Scenario Contract

Each simulator must implement these 9 scenarios with identical behavior:

| # | Name | Description |
|---|------|-------------|
| 1 | `basic_crud` | INSERT/SELECT/UPDATE/DELETE on `users` table |
| 2 | `transactions` | BEGIN/COMMIT, BEGIN/ROLLBACK, SAVEPOINTs |
| 3 | `prepared_statements` | Named + unnamed prepared statements, reuse |
| 4 | `concurrent_rw` | N concurrent workers doing mixed reads + writes |
| 5 | `connection_storms` | Rapid open/close cycles (200 connections) |
| 6 | `long_running` | `pg_sleep(2)` + large result set queries |
| 7 | `error_handling` | Invalid SQL, unique violations, deadlocks |
| 8 | `batch_operations` | Bulk INSERT (1000+ rows), multi-row VALUES |
| 9 | `pool_behavior` | Track `pg_backend_pid()` across queries |

## Files

- `schema.sql` - Common test schema, mounted into PostgreSQL's `docker-entrypoint-initdb.d/`
- `results-schema.json` - JSON Schema for validating simulator output

## Environment Variables

Each simulator reads:
- `DB_HOST` - Grunyas host (default: `grunyas`)
- `DB_PORT` - Grunyas port (default: `5711`)
- `DB_USER` - Database user (default: `postgres`)
- `DB_PASSWORD` - Database password (default: `postgres`)
- `DB_NAME` - Database name (default: `simulator`)
- `CONCURRENCY` - Number of concurrent workers (default: `100`)
- `POOL_MODE` - Current pool mode label for output (default: `session`)
