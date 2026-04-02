import { Pool } from "pg";
import { Config, RawResult } from "../types";

export async function longRunning(config: Config): Promise<RawResult> {
  const pool = new Pool({ host: config.host, port: config.port, user: config.user, password: config.password, database: config.database, max: config.concurrency });
  pool.on("error", () => {}); // suppress unhandled pool-level errors
  let ops = 0, errors = 0;
  const latencies: number[] = [];
  const workers = Math.min(config.concurrency, 20);

  const start = Date.now();
  const tasks: Promise<void>[] = [];

  // pg_sleep workers
  for (let i = 0; i < Math.floor(workers / 2); i++) {
    tasks.push((async () => {
      const t = performance.now();
      try { await pool.query("SELECT pg_sleep(1)"); }
      catch { errors++; }
      latencies.push(performance.now() - t); ops++;
    })());
  }

  // Large result set workers
  for (let i = 0; i < Math.floor(workers / 2); i++) {
    tasks.push((async () => {
      const t = performance.now();
      try {
        const res = await pool.query("SELECT generate_series(1, 10000)");
        void res.rows.length;
      } catch { errors++; }
      latencies.push(performance.now() - t); ops++;
    })());
  }

  // Quick query workers
  for (let i = 0; i < workers; i++) {
    tasks.push((async () => {
      for (let j = 0; j < 5; j++) {
        const t = performance.now();
        try { await pool.query("SELECT 1"); }
        catch { errors++; }
        latencies.push(performance.now() - t); ops++;
      }
    })());
  }

  await Promise.all(tasks);
  await pool.end();
  return { totalOps: ops, errors, durationMs: Date.now() - start, latencies };
}
