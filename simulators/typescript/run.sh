#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")"

# Load sizing model (see shared/SIZING.md)
source ../shared/sizing.sh

REQUESTED_CONCURRENCY="${CONCURRENCY:-100}"

cleanup() {
    echo "=== Cleaning up ==="
    docker compose down -v 2>/dev/null || true
}
trap cleanup EXIT

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
docker compose up --build --abort-on-container-exit simulator
docker compose down -v

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
docker compose up --abort-on-container-exit simulator
docker compose down -v

echo ""
echo "=== Merging results ==="
if [ -f results/session.json ] && [ -f results/transaction.json ]; then
    python3 -c "
import json, sys
with open('results/session.json') as f:
    session = json.load(f)
with open('results/transaction.json') as f:
    transaction = json.load(f)
report = {
    'simulator': session['simulator'],
    'timestamp': session['timestamp'],
    'config': session['config'],
    'runs': session['runs'] + transaction['runs']
}
json.dump(report, sys.stdout, indent=2)
" > results/report.json 2>/dev/null && echo "Results written to results/report.json" || echo "Merge failed - check individual result files"
else
    echo "Warning: Missing result files."
    ls -la results/ 2>/dev/null || true
fi
