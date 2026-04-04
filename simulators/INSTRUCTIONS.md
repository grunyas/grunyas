# Simulator Instructions

These simulators validate that Grunyas correctly handles all major database client patterns across Go, Python, TypeScript, and Java. Each simulator runs the same 9 scenarios against Grunyas in both session and transaction pool modes.

For details on what each scenario tests, see [SCENARIOS.md](SCENARIOS.md).
For the status of all simulators and bugs fixed, see [STATUS.md](STATUS.md).

---

## Prerequisites

- **Docker** and **Docker Compose** installed
- **Python 3** (for result merging; should already be on your system)
- A working **Grunyas** setup with PostgreSQL backend

The simulators use Docker to isolate each environment. Inside the container:
- Grunyas runs as the proxy layer
- PostgreSQL runs as the backend
- The language-specific simulator connects through Grunyas

---

## Quick Start

To run all simulators with default settings:

```bash
cd simulators/go && ./run.sh
cd simulators/python && ./run.sh
cd simulators/typescript && ./run.sh
cd simulators/java && ./run.sh
```

Each simulator will:
1. Run all 9 scenarios in **session mode**
2. Run all 9 scenarios in **transaction mode**
3. Write results to `results/report.json`
4. Clean up Docker containers and volumes on exit

---

## Running a Single Simulator

### Go
```bash
cd simulators/go
./run.sh
```

### Python
```bash
cd simulators/python
./run.sh
```

### TypeScript
```bash
cd simulators/typescript
./run.sh
```

### Java
```bash
cd simulators/java
./run.sh
```

---

## Configuration

All simulators respect the same environment variables. Override them before running:

```bash
cd simulators/go
CONCURRENCY=50 PG_VCPUS=4 PG_RAM_GB=4 ./run.sh
```

### Available Variables

| Variable | Default | Description |
|---|---|---|
| `CONCURRENCY` | `100` | Simulator concurrency (capped by pool sizing model) |
| `PG_VCPUS` | `2` | PostgreSQL vCPUs (affects pool sizing) |
| `PG_RAM_GB` | `2` | PostgreSQL RAM in GB (affects pool sizing) |
| `GRUNYAS_VCPUS` | `2` | Grunyas vCPUs (affects pool sizing) |
| `GRUNYAS_RAM_MB` | `512` | Grunyas RAM in MB (affects pool sizing) |

For details on how these affect connection pool sizing, see `shared/SIZING.md`.

---

## Understanding Results

Each simulator generates `results/report.json` with the following structure:

```json
{
  "simulator": "go",
  "timestamp": "2026-04-04T10:30:00Z",
  "config": {
    "pool_mode": "session",
    "concurrency": 100,
    "backends": 37,
    "clients": 37
  },
  "runs": [
    {
      "scenario": "basic_crud",
      "pool_mode": "session",
      "status": "pass",
      "error_count": 0,
      "notes": []
    },
    ...
  ]
}
```

### Status Values

- **pass** — All queries succeeded with 0 errors
- **partial** — Most queries succeeded but there were non-fatal issues (logged as notes)
- **fail** — The scenario encountered errors and did not complete

### What "0 errors" Means

An "error" is counted when:
- A query fails unexpectedly (syntax error, connection drop, etc.)
- A capacity rejection (SQLSTATE 53300) occurs in session mode
- An expected query result doesn't match (e.g., wrong return value)

Capacity rejections and expected errors (like constraint violations) **do not** count as errors.

---

## Interpreting the Output

Each simulator prints progress as it runs:

```
=== Sizing Model ===
PG: 2 vCPUs, 2 GB RAM → pg_max_connections = 50
Grunyas: 2 vCPUs, 512 MB RAM

=== Session Mode ===
PG: backend_max = 37, client_max = 37
Grunyas: backend_max = 37, client_max = 37
Concurrency: 100 → capped to 37

Building simulator image...
Running 9 scenarios...
✓ basic_crud: 0 errors
✓ transactions: 0 errors
...
Results written to results/report.json
```

If all scenarios show `0 errors`, the simulator has passed.

---

## Running All Simulators (Batch)

To run all four simulators sequentially:

```bash
#!/bin/bash
for lang in go python typescript java; do
  echo "=== Running $lang simulator ==="
  cd simulators/$lang
  ./run.sh
  cd ../..
  echo ""
done
echo "All simulators complete!"
```

---

## Comparing Results Across Simulators

All simulators implement the same 9 scenarios, so you can compare:

```bash
# Extract error counts from each
jq '.runs[] | select(.pool_mode == "session") | {scenario, error_count}' \
  simulators/go/results/report.json \
  simulators/python/results/report.json \
  simulators/typescript/results/report.json \
  simulators/java/results/report.json
```

Expected output: all should show `error_count: 0`.

---

## Database Schema

All simulators use the same PostgreSQL schema, defined in `shared/schema.sql`:

```sql
users   (id, name, email UNIQUE, balance, created_at)
orders  (id, user_id → users, amount, status, created_at)
events  (id, type, payload JSONB, created_at)
```

The `users` table is pre-seeded with 1,000 rows (`user_1@test.com` … `user_1000@test.com`).

---

## Troubleshooting

### "Connection refused" or "Cannot connect to Grunyas"

- Verify Docker is running: `docker ps`
- Check if the Grunyas container started: `docker compose logs grunyas`
- The simulator tries to connect for ~30 seconds. If Grunyas is still starting, wait a moment and re-run.

### "Too many connections" errors in session mode

- This is expected when `CONCURRENCY` exceeds `client_max` for the pool sizing.
- The simulator automatically caps concurrency, so if you see this error during the run, check the sizing output at the start.
- Example: if `client_max = 37` but you requested `CONCURRENCY = 100`, the simulator caps to 37.

### "Prepared statement does not exist"

- This indicates an issue with transaction-mode statement caching.
- Ensure the simulator matches the expected behavior in [SCENARIOS.md](SCENARIOS.md) (e.g., named statements should be wrapped in `BEGIN`/`COMMIT`).
- Check [STATUS.md](STATUS.md) for which bugs have already been fixed and verified.

### "Timeout" or "context deadline exceeded"

- The simulator ran out of time waiting for connections.
- Increase resource limits (increase `GRUNYAS_RAM_MB`, `GRUNYAS_VCPUS`, or `PG_VCPUS`).
- Or reduce `CONCURRENCY` to lower the load.

### "Out of memory"

- The Docker container ran out of memory.
- Increase `GRUNYAS_RAM_MB` or `PG_RAM_GB`.
- Or reduce `CONCURRENCY`.

### Results file is missing or incomplete

- Check `results/` directory for partial files: `session.json`, `transaction.json`
- If only one exists, the corresponding pool mode failed. Check the Docker container logs:
  ```bash
  docker compose logs simulator
  docker compose logs grunyas
  docker compose logs postgres
  ```

---

## For Developers

### Adding a New Scenario

1. Create a new scenario file in the language-specific `scenarios/` directory
2. Follow the pattern in existing scenarios (e.g., `basic_crud.go`, `basic_crud.py`)
3. Implement the same logic in all four languages (Go, Python, TypeScript, Java)
4. Add an entry in the `scenarios` list in the main simulator file
5. Document it in [SCENARIOS.md](SCENARIOS.md)

### Modifying the Database Schema

1. Edit `shared/schema.sql`
2. Re-run all simulators (the schema is applied fresh on each run)

### Adjusting Pool Sizing

Edit `shared/sizing.sh` and `shared/SIZING.md` to change how connection pools are sized based on system resources.

---

## Performance Expectations

- **Session Mode**: ~100–200 ops/sec (mostly network latency)
- **Transaction Mode**: ~500–2000 ops/sec (multiplexed, less per-op overhead)

Actual throughput depends on hardware, `CONCURRENCY`, and scenario complexity.
