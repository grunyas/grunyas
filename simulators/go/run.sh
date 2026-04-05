#!/usr/bin/env bash
set -euo pipefail

# Ensure docker is available on macOS where it's in /usr/local/bin
export PATH="/usr/local/bin:/Applications/Docker.app/Contents/Resources/bin:${PATH:-}"

cd "$(dirname "$0")"

# Load sizing model (see shared/SIZING.md)
source ../shared/sizing.sh

# --- Parse arguments ---
PROXY="grunyas"  # default
REQUESTED_CONCURRENCY="${CONCURRENCY:-100}"

while [[ $# -gt 0 ]]; do
  case $1 in
    --proxy)
      PROXY="$2"
      shift 2
      ;;
    *)
      shift
      ;;
  esac
done

if [[ "$PROXY" != "grunyas" && "$PROXY" != "pgbouncer" && "$PROXY" != "pgcat" ]]; then
  echo "Error: --proxy must be one of: grunyas, pgbouncer, pgcat"
  exit 1
fi

# Proxy-specific port
case "$PROXY" in
  grunyas)   PROXY_PORT=5711 ;;
  pgbouncer) PROXY_PORT=6432 ;;
  pgcat)     PROXY_PORT=6432 ;;
esac

export PROXY PROXY_PORT

cleanup() {
    echo "=== Cleaning up ==="
    docker compose --profile "$PROXY" down -v 2>/dev/null || true
}
trap cleanup EXIT

echo "=== Proxy: $PROXY ==="
print_sizing

# --- Session Mode ---
SESSION_CONCURRENCY=$REQUESTED_CONCURRENCY
if [ "$SESSION_CONCURRENCY" -gt "$SESSION_CLIENT_MAX" ]; then
    SESSION_CONCURRENCY=$SESSION_CLIENT_MAX
    echo "Note: Concurrency capped at $SESSION_CLIENT_MAX for session mode"
fi

echo "=== Phase 1: Session Mode ==="
export POOL_MODE=session
export BACKEND_MAX_CONNS=$SESSION_BACKEND_MAX
export BACKEND_MIN_CONNS=$SESSION_BACKEND_MIN
export CLIENT_MAX_CONNS=$SESSION_CLIENT_MAX
export CONCURRENCY=$SESSION_CONCURRENCY
# pgbouncer max_client_conn must accommodate pool_max_conns (concurrency+10) from main.go
export PGBOUNCER_MAX_CLIENT_CONN=$((SESSION_CONCURRENCY + 20))
# Explicitly start postgres and proxy service to ensure they're ready before simulator
# Use --no-build to avoid rebuilding cached images
docker compose --profile "$PROXY" up -d --no-build postgres "$PROXY" 2>&1 | head -20
sleep 5  # Give services time to start
docker compose --profile "$PROXY" up --no-build --abort-on-container-exit simulator
docker compose --profile "$PROXY" down -v

# --- Transaction Mode ---
TX_CONCURRENCY=$REQUESTED_CONCURRENCY
if [ "$TX_CONCURRENCY" -gt "$TX_CLIENT_MAX" ]; then
    TX_CONCURRENCY=$TX_CLIENT_MAX
    echo "Note: Concurrency capped at $TX_CLIENT_MAX for transaction mode"
fi

echo ""
echo "=== Phase 2: Transaction Mode ==="
export POOL_MODE=transaction
export BACKEND_MAX_CONNS=$TX_BACKEND_MAX
export BACKEND_MIN_CONNS=$TX_BACKEND_MIN
export CLIENT_MAX_CONNS=$TX_CLIENT_MAX
export CONCURRENCY=$TX_CONCURRENCY
# pgbouncer max_client_conn must accommodate pool_max_conns (concurrency+10) from main.go
export PGBOUNCER_MAX_CLIENT_CONN=$((TX_CONCURRENCY + 20))
# Explicitly start postgres and proxy service to ensure they're ready before simulator
# Use --no-build to avoid rebuilding cached images
docker compose --profile "$PROXY" up -d --no-build postgres "$PROXY" 2>&1 | head -20
sleep 5  # Give services time to start
docker compose --profile "$PROXY" up --no-build --abort-on-container-exit simulator
docker compose --profile "$PROXY" down -v

echo ""
echo "=== Merging results ==="
SESSION_FILE="results/${PROXY}_session.json"
TX_FILE="results/${PROXY}_transaction.json"
REPORT_FILE="results/${PROXY}_report.json"

if [ -f "$SESSION_FILE" ] && [ -f "$TX_FILE" ]; then
    python3 -c "
import json, sys
with open('$SESSION_FILE') as f:
    session = json.load(f)
with open('$TX_FILE') as f:
    transaction = json.load(f)
report = {
    'simulator': session['simulator'],
    'timestamp': session['timestamp'],
    'config': session['config'],
    'runs': session['runs'] + transaction['runs']
}
json.dump(report, sys.stdout, indent=2)
" > "$REPORT_FILE" 2>/dev/null && echo "Results written to $REPORT_FILE" || echo "Merge failed - check individual result files"
else
    echo "Warning: Missing result files."
    ls -la results/ 2>/dev/null || true
fi
