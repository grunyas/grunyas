#!/usr/bin/env bash
#
# Performance benchmark comparing grunyas, pgbouncer, and pgcat
#
# Usage: ./scripts/benchmark.sh [OPTIONS]
#
# Options:
#   -c, --clients NUM     Number of concurrent clients (default: 50)
#   -j, --jobs NUM        Number of threads (default: 4)
#   -t, --time SECONDS    Duration in seconds (default: 30)
#   -s, --scale NUM       pgbench scale factor (default: 10)
#   --skip-init           Skip pgbench initialization
#   --keep                Keep containers running after benchmark
#   -h, --help            Show this help message
#
set -euo pipefail

# Defaults
CLIENTS=50
JOBS=4
DURATION=30
SCALE=10
SKIP_INIT=0
KEEP_CONTAINERS=0

# Parse arguments
while [[ $# -gt 0 ]]; do
  case $1 in
    -c|--clients)
      CLIENTS="$2"
      shift 2
      ;;
    -j|--jobs)
      JOBS="$2"
      shift 2
      ;;
    -t|--time)
      DURATION="$2"
      shift 2
      ;;
    -s|--scale)
      SCALE="$2"
      shift 2
      ;;
    --skip-init)
      SKIP_INIT=1
      shift
      ;;
    --keep)
      KEEP_CONTAINERS=1
      shift
      ;;
    -h|--help)
      head -n 16 "$0" | tail -n 14
      exit 0
      ;;
    *)
      echo "Unknown option: $1"
      exit 1
      ;;
  esac
done

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"
COMPOSE_FILE="$PROJECT_DIR/docker-compose.benchmark.yml"

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

log_info() {
  echo -e "${BLUE}[INFO]${NC} $1"
}

log_success() {
  echo -e "${GREEN}[OK]${NC} $1"
}

log_warn() {
  echo -e "${YELLOW}[WARN]${NC} $1"
}

log_error() {
  echo -e "${RED}[ERROR]${NC} $1"
}

cleanup() {
  if [[ "$KEEP_CONTAINERS" == "0" ]]; then
    log_info "Cleaning up containers..."
    # docker compose -f "$COMPOSE_FILE" down -v --remove-orphans >/dev/null 2>&1 || true
  else
    log_info "Keeping containers running (--keep specified)"
  fi
}
trap cleanup EXIT

wait_for_service() {
  local name="$1"
  local host="$2"
  local port="$3"
  local max_attempts=60

  log_info "Waiting for $name ($host:$port)..."
  for i in $(seq 1 $max_attempts); do
    if pg_isready -h "$host" -p "$port" -U postgres >/dev/null 2>&1; then
      log_success "$name is ready"
      return 0
    fi
    sleep 1
  done
  log_error "$name failed to become ready"
  return 1
}

run_pgbench() {
  local name="$1"
  local host="$2"
  local port="$3"

  echo ""
  log_info "Running benchmark: $name ($host:$port)"
  echo "  Clients: $CLIENTS, Threads: $JOBS, Duration: ${DURATION}s"
  echo ""

  local output
  if output=$(PGPASSWORD=postgres PGSSLMODE=disable pgbench \
    -h "$host" \
    -p "$port" \
    -U postgres \
    -c "$CLIENTS" \
    -j "$JOBS" \
    -T "$DURATION" \
    --no-vacuum \
    postgres 2>&1); then

    # Extract key metrics (pgbench 18 format)
    local tps=$(echo "$output" | grep "tps = " | grep "without initial" | awk '{print $3}')
    local latency_avg=$(echo "$output" | grep "latency average" | awk '{print $4}')
    local init_time=$(echo "$output" | grep "initial connection time" | awk '{print $5}')

    echo "  Results:"
    echo "    TPS: $tps"
    echo "    Latency avg: ${latency_avg}ms"
    echo "    Init connection time: ${init_time}ms"

    # Store for summary
    RESULTS+=("$name|$tps|$latency_avg|$init_time")
    return 0
  else
    log_error "pgbench failed for $name"
    echo "$output"
    return 1
  fi
}

print_summary() {
  echo ""
  echo "═══════════════════════════════════════════════════════════════════════════════"
  echo "                           BENCHMARK SUMMARY"
  echo "═══════════════════════════════════════════════════════════════════════════════"
  echo ""
  printf "%-15s %15s %15s %15s\n" "Proxy" "TPS" "Latency" "Init Time"
  echo "───────────────────────────────────────────────────────────────────────────────"

  for result in "${RESULTS[@]}"; do
    IFS='|' read -r name tps lat_avg init_time <<< "$result"
    printf "%-15s %15s %13sms %13sms\n" "$name" "$tps" "$lat_avg" "$init_time"
  done

  echo ""
  echo "Configuration: $CLIENTS clients, $JOBS threads, ${DURATION}s duration, scale $SCALE"
  echo "═══════════════════════════════════════════════════════════════════════════════"
}

# Main execution
echo ""
echo "╔═══════════════════════════════════════════════════════════════════════════════╗"
echo "║         PostgreSQL Connection Pooler Benchmark                                ║"
echo "║         grunyas vs pgbouncer vs pgcat                                    ║"
echo "╚═══════════════════════════════════════════════════════════════════════════════╝"
echo ""

# Check for required tools
for cmd in docker pg_isready pgbench; do
  if ! command -v "$cmd" &>/dev/null; then
    log_error "Required command not found: $cmd"
    exit 1
  fi
done

# Start containers
log_info "Starting Docker containers..."
docker compose -f "$COMPOSE_FILE" up -d --build

# Wait for all services
wait_for_service "PostgreSQL" "127.0.0.1" 5432
wait_for_service "grunyas" "127.0.0.1" 5711
wait_for_service "pgbouncer" "127.0.0.1" 6432
wait_for_service "pgcat" "127.0.0.1" 6433

# Initialize pgbench tables (directly to Postgres, not through proxies)
if [[ "$SKIP_INIT" == "0" ]]; then
  log_info "Initializing pgbench tables (scale factor: $SCALE)..."
  PGPASSWORD=postgres PGSSLMODE=disable pgbench -h 127.0.0.1 -p 5432 -U postgres -i -s "$SCALE" postgres >/dev/null 2>&1
  log_success "pgbench initialized"
else
  log_info "Skipping pgbench initialization (--skip-init)"
fi

# Results array
declare -a RESULTS

# Run benchmarks
run_pgbench "grunyas" "127.0.0.1" 5711
run_pgbench "pgbouncer" "127.0.0.1" 6432
run_pgbench "pgcat" "127.0.0.1" 6433

# Print summary
print_summary

log_success "Benchmark complete!"
