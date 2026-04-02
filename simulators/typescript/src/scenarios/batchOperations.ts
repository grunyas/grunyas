import { Pool } from "pg";
import { Config, RawResult } from "../types";

export async function batchOperations(config: Config): Promise<RawResult> {
  const pool = new Pool({ host: config.host, port: config.port, user: config.user, password: config.password, database: config.database, max: config.concurrency });
  pool.on("error", () => {}); // suppress unhandled pool-level errors
  let ops = 0, errors = 0;
  const latencies: number[] = [];

  const start = Date.now();
  const workers = Array.from({ length: config.concurrency }, (_, i) => (async () => {
    // Bulk INSERT in transaction
    let t = performance.now();
    const client = await pool.connect();
    try {
      await client.query("BEGIN");
      for (let j = 0; j < 100; j++) {
        await client.query("INSERT INTO events (type, payload) VALUES ($1, $2)", [`batch_event_${i}`, JSON.stringify({ worker: i, iter: j })]);
      }
      await client.query("COMMIT");
    } catch { errors++; try { await client.query("ROLLBACK"); } catch {} }
    finally { client.release(); }
    latencies.push(performance.now() - t); ops++;

    // Multi-row VALUES
    t = performance.now();
    try {
      await pool.query(`INSERT INTO events (type, payload) VALUES
        ('multi_1', '{"source":"batch"}'), ('multi_2', '{"source":"batch"}'),
        ('multi_3', '{"source":"batch"}'), ('multi_4', '{"source":"batch"}'),
        ('multi_5', '{"source":"batch"}')`);
    } catch { errors++; }
    latencies.push(performance.now() - t); ops++;

    // Bulk read
    t = performance.now();
    try {
      const res = await pool.query("SELECT id, type, payload FROM events WHERE type = $1 LIMIT 100", [`batch_event_${i}`]);
      void res.rows.length;
    } catch { errors++; }
    latencies.push(performance.now() - t); ops++;
  })());

  await Promise.all(workers);
  await pool.end();
  return { totalOps: ops, errors, durationMs: Date.now() - start, latencies };
}
