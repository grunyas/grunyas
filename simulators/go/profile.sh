#!/usr/bin/env bash
set -euo pipefail

# Profile Grunyas while running the Go simulator.
#
# Usage:
#   ./profile.sh                          # CPU + allocs + block profiles, session mode
#   ./profile.sh --mode transaction       # transaction mode
#   ./profile.sh --seconds 60             # longer capture window
#   ./profile.sh --type cpu               # only CPU profile
#   ./profile.sh --scenario prepared_statements  # profile a single scenario
#
# Profiles are saved to ./profiles/ and opened in the browser automatically.

export PATH="/usr/local/bin:/Applications/Docker.app/Contents/Resources/bin:${PATH:-}"

cd "$(dirname "$0")"
source ../shared/sizing.sh

# --- Defaults ---
POOL_MODE="session"
PPROF_SECONDS=30
PROFILE_TYPES="cpu allocs block"
PPROF_ADDR="localhost:6060"
PROFILES_DIR="./profiles"
SCENARIO=""

# --- Parse arguments ---
while [[ $# -gt 0 ]]; do
  case $1 in
    --mode)       POOL_MODE="$2";      shift 2 ;;
    --seconds)    PPROF_SECONDS="$2";  shift 2 ;;
    --type)       PROFILE_TYPES="$2";  shift 2 ;;
    --scenario)   SCENARIO="$2";       shift 2 ;;
    *)            echo "Unknown option: $1"; exit 1 ;;
  esac
done

export SCENARIOS="$SCENARIO"

# --- Sizing based on pool mode ---
if [[ "$POOL_MODE" == "session" ]]; then
  export BACKEND_MAX_CONNS=$SESSION_BACKEND_MAX
  export BACKEND_MIN_CONNS=$SESSION_BACKEND_MIN
  export CLIENT_MAX_CONNS=$SESSION_CLIENT_MAX
  export CONCURRENCY=$SESSION_CLIENT_MAX
else
  export BACKEND_MAX_CONNS=$TX_BACKEND_MAX
  export BACKEND_MIN_CONNS=$TX_BACKEND_MIN
  export CLIENT_MAX_CONNS=$TX_CLIENT_MAX
  export CONCURRENCY=$TX_CLIENT_MAX
fi

export PROXY=grunyas PROXY_PORT=5711 POOL_MODE

cleanup() {
  echo ""
  echo "=== Cleaning up ==="
  docker compose --profile grunyas down -v 2>/dev/null || true
}
trap cleanup EXIT

echo "=== Grunyas Profiler ==="
echo "  Mode:       $POOL_MODE"
echo "  Scenario:   ${SCENARIO:-all}"
echo "  Capture:    $PROFILE_TYPES"
echo "  Duration:   ${PPROF_SECONDS}s"
echo "  Concurrency: $CONCURRENCY"
print_sizing

# --- Start infrastructure ---
echo "=== Starting postgres + grunyas ==="
docker compose --profile grunyas up -d --build postgres grunyas 2>&1 | tail -5
echo "Waiting for services..."
sleep 5

# Verify pprof is reachable
if ! curl -sf "http://${PPROF_ADDR}/debug/pprof/" > /dev/null 2>&1; then
  echo "ERROR: pprof not reachable at ${PPROF_ADDR}"
  echo "       Check that GRUNYAS_SERVER_PPROF_ADDR is set and port 6060 is exposed."
  exit 1
fi
echo "pprof is live at http://${PPROF_ADDR}/debug/pprof/"

# --- Capture profiles in background while simulator runs ---
mkdir -p "$PROFILES_DIR"
TIMESTAMP=$(date +%Y%m%d_%H%M%S)
PPROF_PIDS=()

capture_profile() {
  local type=$1
  local endpoint=$2
  local outfile="${PROFILES_DIR}/${POOL_MODE}_${type}_${TIMESTAMP}.prof"

  echo "  Capturing ${type} profile (${PPROF_SECONDS}s) → ${outfile}"
  curl -sf "http://${PPROF_ADDR}/debug/pprof/${endpoint}?seconds=${PPROF_SECONDS}" -o "$outfile" &
  PPROF_PIDS+=($!)
}

echo ""
echo "=== Starting profile capture ==="
for t in $PROFILE_TYPES; do
  case $t in
    cpu)    capture_profile cpu    profile ;;
    allocs) capture_profile allocs allocs  ;;
    block)  capture_profile block  block   ;;
    mutex)  capture_profile mutex  mutex   ;;
    *)      echo "  Unknown profile type: $t (skipping)" ;;
  esac
done

# --- Run the simulator ---
echo ""
echo "=== Running simulator ==="
docker compose --profile grunyas up --no-build --abort-on-container-exit simulator || true

# --- Wait for profile captures to finish ---
echo ""
echo "=== Waiting for profile captures to complete ==="
for pid in "${PPROF_PIDS[@]}"; do
  wait "$pid" 2>/dev/null || true
done

# --- Show results ---
echo ""
echo "=== Profiles saved ==="
ls -lh "$PROFILES_DIR"/*_${TIMESTAMP}.prof 2>/dev/null || echo "No profiles captured."

echo ""
echo "To view a profile:"
echo "  go tool pprof -http=:8080 ${PROFILES_DIR}/<file>.prof"
echo ""
echo "Quick view all captured profiles:"
PORT=8080
for f in "$PROFILES_DIR"/*_${TIMESTAMP}.prof; do
  [ -f "$f" ] || continue
  echo "  go tool pprof -http=:${PORT} ${f}"
  PORT=$((PORT + 1))
done
