# grunyas

[![Go Reference](https://pkg.go.dev/badge/github.com/grunyas/grunyas.svg)](https://pkg.go.dev/github.com/grunyas/grunyas)
[![Go Report Card](https://goreportcard.com/badge/github.com/grunyas/grunyas)](https://goreportcard.com/report/github.com/grunyas/grunyas)

A PostgreSQL protocol-aware proxy server written in Go, designed to sit between PostgreSQL clients and servers with support for connection pooling, authentication, and protocol inspection.

## Features

### ✅ Implemented

- **Protocol Handling**

  - Full PostgreSQL wire protocol support (startup, simple query, extended query protocol)
  - SSL/GSS encryption request handling
  - Proper message parsing and forwarding
  - Query forwarding to backend PostgreSQL server
  - **SSL/TLS Support**
    - Configurable SSL modes: `never`, `optional`, `mandatory`
    - Certificate and key loading for secure connections

- **Authentication**

  - Multiple authentication methods: plain, MD5, SCRAM-SHA-256
  - Configurable authentication backend
  - User validation and session establishment

- **Connection Management**

  - Connection pooling using `pgxpool` with configurable pool settings
  - Session lifecycle management with proper cleanup
  - Idle connection sweeper with configurable timeout
  - Maximum session limits with graceful rejection

- **Session & Transaction Pooling**

  - Session-mode (one upstream per client session) and transaction-mode (upstream leased per transaction) pooling
  - Lazy upstream acquisition — connections are taken from the pool only on first query
  - Extended query protocol support (Parse/Bind/Describe/Execute/Sync/Close/Flush) with per-message dispatch

- **Architecture**
  - Clean separation of concerns with interface-based design
  - No circular dependencies (using dependency injection pattern)
  - Comprehensive test coverage for core components
  - Structured logging with zap
  - Optional OpenTelemetry integration
  - Optional Go `pprof` HTTP endpoint for profiling

### 📋 Planned

- Query inspection and logging
- Metrics and observability
- Admin API for runtime management
- Connection multiplexing optimizations

## Architecture

```
┌─────────────┐
│   Client    │
└──────┬──────┘
       │
       ▼
┌─────────────────────────────────────┐
│         Proxy Server                │
│  ┌──────────────────────────────┐   │
│  │  Session Management          │   │
│  │  - Protocol handling         │   │
│  │  - Message parsing           │   │
│  │  - Idle timeout tracking     │   │
│  └──────────────────────────────┘   │
│  ┌──────────────────────────────┐   │
│  │  Connection Pool (pgxpool)   │   │
│  │  - Min/max connections       │   │
│  │  - Health checks             │   │
│  │  - Lifecycle management      │   │
│  └──────────────────────────────┘   │
└─────────────┬───────────────────────┘
              │
              ▼
       ┌─────────────┐
       │  PostgreSQL │
       │   Backend   │
       └─────────────┘
```

## Project Structure

```
.
├── cmd/
│   └── main.go                    # Application entry point
├── config/                        # Configuration (TOML + env var overrides)
│   ├── config.go                  # Top-level config, defaults & validation
│   ├── auth_config.go             # Authentication settings
│   ├── database_config.go         # Backend connection & pool settings
│   ├── server_config.go           # Listener, SSL, pool mode, pprof
│   ├── logging_config.go          # Logging configuration
│   └── telemetry_config.go        # OpenTelemetry settings
├── internal/
│   ├── auth/                      # Plain, MD5, and SCRAM-SHA-256 auth
│   ├── console/                   # Interactive runtime console
│   ├── logger/                    # Zap + OpenTelemetry integration
│   ├── pool/                      # Upstream connection pooling
│   │   ├── manager/               # pgxpool-backed pool manager
│   │   └── upstream_client/       # Session & transaction-mode clients
│   ├── server/                    # Core server logic
│   │   ├── downstream_client/     # Client wire protocol handling
│   │   ├── messaging/             # Per-message dispatch (Parse, Bind, Execute, …)
│   │   ├── proxy/                 # Main proxy listener & idle sweeper
│   │   ├── session/               # Client session lifecycle
│   │   └── types/                 # Shared interfaces
│   ├── testutil/                  # Test helpers (goleak, etc.)
│   └── utils/
│       └── pgx_log_adapter/       # Zap adapter for pgx
├── scripts/
│   ├── benchmark.sh               # pgbench comparison vs pgbouncer / pgcat
│   └── run-sql-tests.sh           # Runs tests/sql against a running proxy
├── simulators/                    # Multi-language client simulators (Go, Python, TypeScript, Java)
├── tests/
│   ├── integration/               # pgproto3 integration tests
│   └── sql/                       # End-to-end SQL test suite
└── config.toml.example            # Example configuration
```

## Configuration

Create a `config.toml` file based on `config.toml.example`:

```toml
[server]
listen_addr = "0.0.0.0:5711"
admin_addr = "0.0.0.0:5712"
max_sessions = 1000
client_idle_timeout = 300
keep_alive_timeout = 15
keep_alive_interval = 15
keep_alive_count = 9
pool_mode = "session"           # options: session, transaction
ssl_mode = "optional"           # options: never, optional, mandatory
ssl_cert = "server.crt"         # path to certificate file (required for optional/mandatory)
ssl_key = "server.key"          # path to key file (required for optional/mandatory)
# pprof_addr = "127.0.0.1:6060" # optional: enables Go pprof HTTP server

[logging]
level = "info"
development = true

[telemetry]
otlp_endpoint = ""              # Leave empty to disable
insecure = true
service_name = "grunyas"

[auth]
method = "scram-sha-256"        # options: plain, md5, scram-sha-256
username = "postgres"
password = "postgres"

[backend]
host = "127.0.0.1"
port = 5432
user = "postgres"
password = ""
database = "postgres"
connect_timeout_seconds = 5

# Connection pool settings
pool_min_conns = 2
pool_max_conns = 10
pool_max_conn_lifetime = 3600
pool_max_conn_idle_time = 1800
pool_health_check_period = 60
```

## Build & Run

Any key in `config.toml` can be overridden via environment variables using the
`GRUNYAS_` prefix with `.` replaced by `_` (e.g. `GRUNYAS_SERVER_LISTEN_ADDR`,
`GRUNYAS_BACKEND_HOST`).

```bash
# Build
go build -o grunyas ./cmd

# Run (reads ./config.toml if present)
./grunyas

# Run without the interactive console
./grunyas -no-console
```

## Testing

The project has comprehensive test coverage:

```bash
# Run all tests
go test ./...

# Run with verbose output
go test -v ./...

# Run specific package tests
go test -v ./internal/server/session/...
go test -v ./internal/server/proxy/...

# Run with coverage
go test -cover ./...
```

### Integration tests

Integration tests are behind the `integration` build tag and expect a running
PostgreSQL instance. For local runs, you can use the provided Docker Compose
file.

```bash
# Start PostgreSQL for integration tests
docker compose -f docker-compose.integration.yml up -d

# Run integration tests
PGHOST=127.0.0.1 PGPORT=5432 PGUSER=postgres PGPASSWORD=postgres PGDATABASE=postgres \
  go test -race -tags=integration -v ./...
```

### End-to-End SQL Tests

The project includes a suite of SQL files in `tests/sql/` to verify proxy behavior against a live server.

```bash
# Start PostgreSQL (e.g. via docker-compose) and run the SQL suite via the helper script.
docker compose -f docker-compose.integration.yml up -d
PGHOST=127.0.0.1 PGPORT=5432 PGUSER=postgres PGPASSWORD=postgres PGDATABASE=postgres \
  ./scripts/run-sql-tests.sh

# Run basic queries
psql "host=127.0.0.1 port=5711 user=postgres password=postgres sslmode=disable" -f tests/sql/01_basic.sql

# Run all tests
for file in tests/sql/*.sql; do
    echo "Running $file..."
    psql "host=127.0.0.1 port=5711 user=postgres password=postgres sslmode=disable" -f "$file"
done
```

**Current Test Coverage:**

- ✅ Session lifecycle (startup, auth, query dispatch, shutdown)
- ✅ Server initialization and proxy wiring
- ✅ Idle connection sweeping
- ✅ Authentication flow (plain, MD5, SCRAM-SHA-256)
- ✅ Protocol message handling (simple + extended query)
- ✅ Upstream pool / session client behavior
- ✅ Downstream client wire protocol
- ✅ Config validation and defaults

### Client Simulators

The `simulators/` directory contains containerized client simulators in Go, Python, TypeScript, and Java. Each runs the same set of scenarios against grunyas in both session and transaction pool modes, to validate behavior across real database drivers.

```bash
cd simulators/go && ./run.sh
cd simulators/python && ./run.sh
cd simulators/typescript && ./run.sh
cd simulators/java && ./run.sh
```

See [simulators/INSTRUCTIONS.md](simulators/INSTRUCTIONS.md) for details.

### Performance Benchmarking

Compare performance against pgbouncer and pgcat using `pgbench`:

```bash
# Run the full benchmark suite (requires Docker)
./scripts/benchmark.sh

# Customize benchmark parameters
./scripts/benchmark.sh -c 100 -t 60  # 100 clients, 60 seconds

# Keep containers running after benchmark for debugging
./scripts/benchmark.sh --keep
```

**Options:**
- `-c, --clients NUM` - Number of concurrent clients (default: 50)
- `-j, --jobs NUM` - Number of threads (default: 4)
- `-t, --time SECONDS` - Duration in seconds (default: 30)
- `-s, --scale NUM` - pgbench scale factor (default: 10)
- `--skip-init` - Skip pgbench initialization
- `--keep` - Keep containers running after benchmark

## Development

### Key Design Decisions

1. **Interface-Based Architecture**: The interfaces in `internal/server/types` (e.g. `ProxyInterface`, `UpstreamClientInterface`, `DownstreamClientInterface`) break circular dependencies between packages and enable clean separation and testability.

2. **Lazy Connection Acquisition**: Upstream connections are acquired from the pool only on the first query, reducing resource usage for idle sessions.

3. **Buffered Send / Flush**: Both the upstream and downstream clients batch `Send` calls and only issue a write syscall on explicit `Flush` at protocol boundaries (ReadyForQuery, Sync, etc.), reducing syscall overhead.

4. **Structured Logging**: All components use structured logging with context propagation for better observability.

5. **Graceful Degradation**: The server handles capacity limits gracefully, rejecting new connections with proper PostgreSQL error codes.

### Adding New Features

When extending the proxy:

1. Add configuration options to appropriate `config/*_config.go` files
2. Implement core logic in `internal/server/`
3. Add tests alongside implementation
4. Update this README with feature status

## License

MIT
