import { Pool } from "pg";
import { Config, RawResult } from "../types";

export async function poolBehavior(config: Config): Promise<RawResult> {
  const pool = new Pool({ host: config.host, port: config.port, user: config.user, password: config.password, database: config.database, max: config.concurrency });
  pool.on("error", () => {}); // suppress unhandled pool-level errors
  let ops = 0, errors = 0;
  const latencies: number[] = [];
  const workers = Math.min(config.concurrency, 50);
  const pidResults: boolean[] = new Array(workers).fill(false);

  const start = Date.now();
  const tasks = Array.from({ length: workers }, (_, i) => (async () => {
    const pids = new Set<number>();
    const client = await pool.connect();
    try {
      for (let iter = 0; iter < 10; iter++) {
        const t = performance.now();
        try {
          const res = await client.query("SELECT pg_backend_pid() as pid");
          pids.add(res.rows[0].pid);
        } catch { errors++; }
        latencies.push(performance.now() - t); ops++;
      }
    } finally { client.release(); }
    pidResults[i] = pids.size > 1;
  })());

  await Promise.all(tasks);
  await pool.end();

  // In session mode, PID changes are unexpected — count as errors.
  // In transaction mode, no PID change means multiplexing wasn't observed — count as errors.
  for (let i = 0; i < workers; i++) {
    if (config.poolMode === "session" && pidResults[i]) {
      errors++;
    } else if (config.poolMode === "transaction" && !pidResults[i]) {
      errors++;
    }
  }

  return { totalOps: ops, errors, durationMs: Date.now() - start, latencies };
}
