#!/usr/bin/env bash
# 3-proxy × 9-scenario × 5-trial sweep with per-trial postgres restart.
set -uo pipefail
cd "$(dirname "$0")"
export PATH="/usr/local/bin:/Applications/Docker.app/Contents/Resources/bin:${PATH:-}"

export BACKEND_MAX_CONNS=37 BACKEND_MIN_CONNS=9 CLIENT_MAX_CONNS=37
export CONCURRENCY=37 POOL_MODE=session DURATION=10s
export PGBOUNCER_MAX_CLIENT_CONN=57

SCENARIOS_LIST="basic_crud prepared_statements concurrent_rw transactions batch_operations pool_behavior error_handling long_running connection_storms"
TRIALS=5
OUTDIR="variance/proxy_compare"
rm -rf "$OUTDIR"; mkdir -p "$OUTDIR"

run_proxy() {
    local proxy=$1
    local port=$2
    export PROXY=$proxy PROXY_PORT=$port

    echo ""
    echo "######## PROXY: $proxy ########"

    docker compose --profile grunyas --profile pgcat --profile pgbouncer down -v 2>&1 >/dev/null

    for sc in $SCENARIOS_LIST; do
        echo ""
        echo "=== $proxy / $sc ==="
        for trial in $(seq 1 $TRIALS); do
            docker compose --profile $proxy down -v 2>&1 >/dev/null
            docker compose --profile $proxy up -d --no-build postgres $proxy 2>&1 >/dev/null
            sleep 5

            docker compose --profile $proxy run --rm --no-deps \
                -e DURATION=10s -e SCENARIOS=$sc -e POOL_MODE=session -e CONCURRENCY=37 \
                simulator 2>&1 | grep -E "ops/s=" | head -1
            mv results/${proxy}_session.json "$OUTDIR/${proxy}_${sc}_trial${trial}.json" 2>/dev/null || true
        done
    done

    docker compose --profile $proxy down -v 2>&1 >/dev/null
}

run_proxy grunyas 5711
run_proxy pgcat 6432
run_proxy pgbouncer 6432

echo ""
echo "=== 3-PROXY SUMMARY (session mode, 5 trials each, postgres restart per trial) ==="
python3 <<PYEOF
import json, os, statistics
outdir = "$OUTDIR"
scenarios = "$SCENARIOS_LIST".split()
proxies = ["grunyas", "pgcat", "pgbouncer"]

def summarize(proxy, sc):
    ops, p99s = [], []
    for t in range(1, $TRIALS + 1):
        p = f"{outdir}/{proxy}_{sc}_trial{t}.json"
        if not os.path.exists(p): continue
        with open(p) as f: data = json.load(f)
        for run in data.get("runs", []):
            for s in run.get("scenarios", []):
                if s.get("name") == sc:
                    ops.append(s["ops_per_sec"])
                    p99s.append(s["latency"]["p99_ms"])
    if not ops: return None
    mean = statistics.mean(ops)
    stdev = statistics.stdev(ops) if len(ops)>1 else 0
    return {
        "median": statistics.median(ops),
        "cv": stdev/mean*100 if mean else 0,
        "p99": statistics.median(p99s),
    }

# Throughput comparison
print(f"\n{'scenario':22s}  {'grunyas':>18s}  {'pgcat':>18s}  {'pgbouncer':>18s}  {'g vs pgcat':>10s}")
for sc in scenarios:
    cells = []
    grunyas_ops = None
    pgcat_ops = None
    for p in proxies:
        r = summarize(p, sc)
        if r:
            cells.append(f"{r['median']:8.0f} ({r['cv']:4.2f}%)")
            if p == "grunyas": grunyas_ops = r['median']
            if p == "pgcat": pgcat_ops = r['median']
        else:
            cells.append(f"{'NO DATA':>16s}")
    gap = ""
    if grunyas_ops and pgcat_ops:
        pct = (grunyas_ops - pgcat_ops) / pgcat_ops * 100
        gap = f"{pct:+6.1f}%"
    print(f"{sc:22s}  {cells[0]:>18s}  {cells[1]:>18s}  {cells[2]:>18s}  {gap:>10s}")

# p99 comparison
print(f"\n{'scenario':22s}  {'grunyas p99':>12s}  {'pgcat p99':>12s}  {'pgbouncer p99':>14s}")
for sc in scenarios:
    cells = []
    for p in proxies:
        r = summarize(p, sc)
        cells.append(f"{r['p99']:.2f}ms" if r else "NO DATA")
    print(f"{sc:22s}  {cells[0]:>12s}  {cells[1]:>12s}  {cells[2]:>14s}")
PYEOF
