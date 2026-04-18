#!/usr/bin/env bash
# Per-scenario CV measurement against grunyas, duration mode.
# Restarts postgres + grunyas between EVERY trial so each trial sees a clean,
# identical starting state (fresh heap, empty caches, no accumulated rows).
set -uo pipefail
cd "$(dirname "$0")"
export PATH="/usr/local/bin:/Applications/Docker.app/Contents/Resources/bin:${PATH:-}"

export BACKEND_MAX_CONNS=37 BACKEND_MIN_CONNS=9 CLIENT_MAX_CONNS=37
export CONCURRENCY=37 POOL_MODE=session DURATION=10s
export PROXY=grunyas PROXY_PORT=5711

SCENARIOS_LIST="basic_crud prepared_statements concurrent_rw transactions batch_operations pool_behavior error_handling long_running connection_storms"
TRIALS=5
OUTDIR="variance/all_scenarios"
rm -rf "$OUTDIR"; mkdir -p "$OUTDIR"

# Start from a clean slate
docker compose --profile grunyas --profile pgcat --profile pgbouncer down -v 2>&1 | tail -2

for sc in $SCENARIOS_LIST; do
    echo ""
    echo "=== $sc ==="
    for trial in $(seq 1 $TRIALS); do
        # Fresh postgres + grunyas per trial so every trial starts identical.
        docker compose --profile grunyas down -v 2>&1 >/dev/null
        docker compose --profile grunyas up -d --no-build postgres grunyas 2>&1 >/dev/null
        sleep 5

        docker compose --profile grunyas run --rm --no-deps \
            -e DURATION=10s -e SCENARIOS=$sc -e POOL_MODE=session -e CONCURRENCY=37 \
            simulator 2>&1 | grep -E "ops/s=" | head -1
        mv results/grunyas_session.json "$OUTDIR/${sc}_trial${trial}.json" 2>/dev/null || true
    done
done

docker compose --profile grunyas down -v 2>&1 | tail -2

echo ""
echo "=== CV SUMMARY (grunyas session mode, 5 trials each, postgres restarted between trials) ==="
python3 <<PYEOF
import json, os, statistics
outdir = "$OUTDIR"
scenarios = "$SCENARIOS_LIST".split()
print(f"{'scenario':22s}  {'median':>8s}  {'mean':>8s}  {'stdev':>6s}  {'cv':>7s}  {'min':>8s}  {'max':>8s}  {'p99_ms':>7s}  errs")
for sc in scenarios:
    ops, p99s, errs = [], [], []
    for t in range(1, $TRIALS + 1):
        p = f"{outdir}/{sc}_trial{t}.json"
        if not os.path.exists(p): continue
        with open(p) as f: data = json.load(f)
        for run in data.get("runs", []):
            for s in run.get("scenarios", []):
                if s.get("name") == sc:
                    ops.append(s["ops_per_sec"])
                    p99s.append(s["latency"]["p99_ms"])
                    errs.append(s["errors"])
    if ops:
        mean = statistics.mean(ops)
        stdev = statistics.stdev(ops) if len(ops)>1 else 0
        cv = stdev/mean*100 if mean else 0
        mark = " ✓" if cv < 2.0 else " ✗"
        print(f"{sc:22s}  {statistics.median(ops):8.0f}  {mean:8.0f}  {stdev:6.0f}  {cv:6.2f}%{mark}  {min(ops):8.0f}  {max(ops):8.0f}  {statistics.median(p99s):7.3f}  {sum(errs)}")
    else:
        print(f"{sc:22s}  NO DATA")
PYEOF
