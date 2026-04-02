# Connection Pool Sizing Model

This document describes how Grunyas sizes its connection pools based on the
available resources of both PostgreSQL and Grunyas itself.

## The Model

Three components are involved:

```
Clients  -->  Grunyas (CPU + Memory)  -->  PostgreSQL (CPU + Memory)
```

Each component constrains the system differently:

| Resource | What it constrains |
|---|---|
| PostgreSQL memory | Maximum total connections (`max_connections`) |
| PostgreSQL CPU | Maximum *useful* concurrent active queries |
| Grunyas CPU | Message parsing/forwarding throughput |
| Grunyas memory | Total connections (client + backend) it can hold |

## Formulas

### PostgreSQL max_connections (memory-based)

```
pg_max_connections = 25 * PG_RAM_GB
```

Each PostgreSQL backend process uses ~5-10MB (work_mem, shared buffers, catalog
caches). 25 connections per GB is a practical guideline used by managed providers
like Digital Ocean.

### Session Mode (memory-bound, mostly idle)

In session mode the proxy is pass-through: one client maps to one backend
connection for the session lifetime. Most connections are idle at any given time,
so CPU is not the bottleneck — memory is.

```
backend_max  = 0.75 * pg_max_connections
client_max   = backend_max                   (1:1 ratio)
backend_min  = max(1, backend_max / 4)
```

The 75% cap reserves headroom for replication, monitoring, superuser access,
and admin connections.

### Transaction Mode (CPU-bound, actively executing)

In transaction mode the proxy multiplexes many clients onto few backend
connections. Each backend connection is actively executing queries, so CPU
is the bottleneck. The smaller server (Grunyas or PostgreSQL) determines
the limit.

```
backend_max  = min(2 * PG_vCPUs, 2 * Grunyas_vCPUs)
client_max   = 50 * backend_max
backend_min  = max(1, backend_max / 4)
```

The 2x vCPU factor follows the PostgreSQL community guideline that optimal
active connections ≈ `(CPU cores * 2)`. The 50:1 client-to-backend ratio
is within the typical production range of 20:1 to 100:1.

### Grunyas Memory Constraint

Each connection (client or backend) uses ~50KB for goroutine stacks, protocol
buffers, and session state. This is rarely the bottleneck:

```
max_total_connections = (Grunyas_RAM_MB - 100) / 0.05
```

A 512MB Grunyas instance can hold ~8,200 total connections.

## Examples

### Small setup: PG 2vCPU/2GB + Grunyas 1vCPU/256MB

| | Session | Transaction |
|---|---|---|
| pg_max_connections | 50 | 50 |
| backend_max | 37 | min(4, 2) = **2** |
| backend_min | 9 | 1 |
| client_max | 37 | 100 |

### Medium setup: PG 4vCPU/8GB + Grunyas 2vCPU/512MB

| | Session | Transaction |
|---|---|---|
| pg_max_connections | 200 | 200 |
| backend_max | 150 | min(8, 4) = **4** |
| backend_min | 37 | 1 |
| client_max | 150 | 200 |

### Production setup: PG 16vCPU/64GB + Grunyas 4vCPU/2GB

| | Session | Transaction |
|---|---|---|
| pg_max_connections | 1600 | 1600 |
| backend_max | 1200 | min(32, 8) = **8** |
| backend_min | 300 | 2 |
| client_max | 1200 | 400 |

## Simulator Defaults

The simulators use configurable resource parameters (override via env vars):

```bash
PG_VCPUS=2        # PostgreSQL vCPUs
PG_RAM_GB=2        # PostgreSQL RAM in GB
GRUNYAS_VCPUS=2    # Grunyas vCPUs
GRUNYAS_RAM_MB=512 # Grunyas RAM in MB
CONCURRENCY=100    # Simulator concurrency (capped at client_max)
```

Docker Compose enforces these as actual resource limits via
`deploy.resources.limits`, so the test environment matches the sizing model.
