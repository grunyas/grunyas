import { Pool } from "pg";
import { Config, RawResult } from "../types";

export async function concurrentRw(config: Config): Promise<RawResult> {
  const pool = new Pool({ host: config.host, port: config.port, user: config.user, password: config.password, database: config.database, max: config.concurrency });
  pool.on("error", () => {}); // suppress unhandled pool-level errors
  let ops = 0, errors = 0;
  const latencies: number[] = [];

  const start = Date.now();
  const workers = Array.from({ length: config.concurrency }, (_, i) => (async () => {
    let seed = i;
    const rand = () => { seed = (seed * 1103515245 + 12345) & 0x7fffffff; return seed / 0x7fffffff; };

    for (let iter = 0; iter < 20; iter++) {
      const userId = Math.floor(rand() * 1000) + 1;

      if (rand() < 0.7) {
        // Read
        const t = performance.now();
        try { await pool.query("SELECT balance FROM users WHERE id = $1", [userId]); }
        catch { errors++; }
        latencies.push(performance.now() - t); ops++;
      } else {
        // Write — transfer
        const otherId = Math.floor(rand() * 1000) + 1;
        const amount = rand() * 10;
        const t = performance.now();
        const client = await pool.connect();
        try {
          await client.query("BEGIN");
          await client.query("UPDATE users SET balance = balance - $1 WHERE id = $2", [amount, userId]);
          await client.query("UPDATE users SET balance = balance + $1 WHERE id = $2", [amount, otherId]);
          await client.query("COMMIT");
        } catch { errors++; try { await client.query("ROLLBACK"); } catch {} }
        finally { client.release(); }
        latencies.push(performance.now() - t); ops++;
      }
    }
  })());

  await Promise.all(workers);
  await pool.end();
  return { totalOps: ops, errors, durationMs: Date.now() - start, latencies };
}
