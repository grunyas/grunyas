import { Pool } from "pg";
import { Config, RawResult } from "../types";

export async function transactions(config: Config): Promise<RawResult> {
  const pool = new Pool({ host: config.host, port: config.port, user: config.user, password: config.password, database: config.database, max: config.concurrency });
  pool.on("error", () => {}); // suppress unhandled pool-level errors
  let ops = 0, errors = 0;
  const latencies: number[] = [];

  const start = Date.now();
  const workers = Array.from({ length: config.concurrency }, (_, i) => (async () => {
    for (let iter = 0; iter < 10; iter++) {
      // Commit flow
      let t = performance.now();
      const client = await pool.connect();
      try {
        await client.query("BEGIN");
        await client.query("INSERT INTO users (name, email, balance) VALUES ($1, $2, $3)", [`tx_user_${i}_${iter}`, `tx_commit_${i}_${iter}@test.com`, 500.0]);
        await client.query("COMMIT");
      } catch { errors++; try { await client.query("ROLLBACK"); } catch {} }
      finally { client.release(); }
      latencies.push(performance.now() - t); ops++;

      // Rollback flow
      t = performance.now();
      const client2 = await pool.connect();
      try {
        await client2.query("BEGIN");
        await client2.query("INSERT INTO users (name, email, balance) VALUES ($1, $2, $3)", ["will_rollback", `rb_${i}_${iter}@test.com`, 999.99]);
        await client2.query("ROLLBACK");
      } catch { try { await client2.query("ROLLBACK"); } catch {} }
      finally { client2.release(); }
      latencies.push(performance.now() - t); ops++;

      // Savepoint flow
      t = performance.now();
      const client3 = await pool.connect();
      try {
        await client3.query("BEGIN");
        await client3.query("SAVEPOINT sp1");
        await client3.query("INSERT INTO users (name, email, balance) VALUES ($1, $2, $3)", ["sp_user", `sp_${i}_${iter}@test.com`, 100.0]);
        await client3.query("ROLLBACK TO SAVEPOINT sp1");
        await client3.query("COMMIT");
      } catch { errors++; try { await client3.query("ROLLBACK"); } catch {} }
      finally { client3.release(); }
      latencies.push(performance.now() - t); ops++;
    }
  })());

  await Promise.all(workers);
  await pool.end();
  return { totalOps: ops, errors, durationMs: Date.now() - start, latencies };
}
