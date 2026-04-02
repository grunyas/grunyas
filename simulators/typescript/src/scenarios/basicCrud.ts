import { Pool } from "pg";
import { Config, RawResult } from "../types";

export async function basicCrud(config: Config): Promise<RawResult> {
  const pool = new Pool({ host: config.host, port: config.port, user: config.user, password: config.password, database: config.database, max: config.concurrency });
  pool.on("error", () => {}); // suppress unhandled pool-level errors
  let ops = 0, errors = 0;
  const latencies: number[] = [];

  const start = Date.now();
  const workers = Array.from({ length: config.concurrency }, (_, i) => (async () => {
    for (let iter = 0; iter < 20; iter++) {
      const email = `crud_${i}_${iter}@test.com`;

      // INSERT
      let t = performance.now();
      let userId: number;
      try {
        const res = await pool.query("INSERT INTO users (name, email, balance) VALUES ($1, $2, $3) RETURNING id", [`crud_user_${i}_${iter}`, email, 100.0]);
        userId = res.rows[0].id;
      } catch { errors++; ops++; latencies.push(performance.now() - t); continue; }
      latencies.push(performance.now() - t); ops++;

      // SELECT
      t = performance.now();
      try { await pool.query("SELECT name FROM users WHERE id = $1", [userId]); }
      catch { errors++; }
      latencies.push(performance.now() - t); ops++;

      // UPDATE
      t = performance.now();
      try { await pool.query("UPDATE users SET balance = balance + 50 WHERE id = $1", [userId]); }
      catch { errors++; }
      latencies.push(performance.now() - t); ops++;

      // DELETE
      t = performance.now();
      try { await pool.query("DELETE FROM users WHERE id = $1", [userId]); }
      catch { errors++; }
      latencies.push(performance.now() - t); ops++;
    }
  })());

  await Promise.all(workers);
  await pool.end();
  return { totalOps: ops, errors, durationMs: Date.now() - start, latencies };
}
