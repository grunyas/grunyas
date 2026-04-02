#!/usr/bin/env bash
# Connection pool sizing calculator.
# Source this file from simulator run.sh scripts.
# See SIZING.md for the full model.

# Resource configuration (override via env vars)
PG_VCPUS="${PG_VCPUS:-2}"
PG_RAM_GB="${PG_RAM_GB:-2}"
GRUNYAS_VCPUS="${GRUNYAS_VCPUS:-2}"
GRUNYAS_RAM_MB="${GRUNYAS_RAM_MB:-512}"

# PostgreSQL max_connections (25 per GB of RAM)
PG_MAX_CONN=$((25 * PG_RAM_GB))

# --- Session mode sizing ---
SESSION_BACKEND_MAX=$(( PG_MAX_CONN * 3 / 4 ))
SESSION_BACKEND_MIN=$(( SESSION_BACKEND_MAX / 4 ))
[ "$SESSION_BACKEND_MIN" -lt 1 ] && SESSION_BACKEND_MIN=1
SESSION_CLIENT_MAX=$SESSION_BACKEND_MAX

# --- Transaction mode sizing ---
TX_BACKEND_PG=$((2 * PG_VCPUS))
TX_BACKEND_GRUNYAS=$((2 * GRUNYAS_VCPUS))
if [ "$TX_BACKEND_PG" -lt "$TX_BACKEND_GRUNYAS" ]; then
    TX_BACKEND_MAX=$TX_BACKEND_PG
else
    TX_BACKEND_MAX=$TX_BACKEND_GRUNYAS
fi
TX_BACKEND_MIN=$(( TX_BACKEND_MAX / 4 ))
[ "$TX_BACKEND_MIN" -lt 1 ] && TX_BACKEND_MIN=1
TX_CLIENT_MAX=$(( 50 * TX_BACKEND_MAX ))

print_sizing() {
    echo "=== Resource Configuration ==="
    echo "  PostgreSQL:  ${PG_VCPUS} vCPU, ${PG_RAM_GB}GB RAM (max_connections=${PG_MAX_CONN})"
    echo "  Grunyas:     ${GRUNYAS_VCPUS} vCPU, ${GRUNYAS_RAM_MB}MB RAM"
    echo ""
    echo "=== Derived Pool Sizing ==="
    echo "  Session mode:     backend=${SESSION_BACKEND_MAX} (min=${SESSION_BACKEND_MIN}), clients=${SESSION_CLIENT_MAX}"
    echo "  Transaction mode: backend=${TX_BACKEND_MAX} (min=${TX_BACKEND_MIN}), clients=${TX_CLIENT_MAX}"
    echo ""
}

# Export for docker-compose interpolation
export PG_VCPUS PG_RAM_GB PG_MAX_CONN
export GRUNYAS_VCPUS GRUNYAS_RAM_MB
