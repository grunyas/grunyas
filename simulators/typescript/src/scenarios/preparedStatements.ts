import { Pool } from "pg";
import { Config, RawResult } from "../types";

export async function preparedStatements(config: Config): Promise<RawResult> {
  const pool = new Pool({ host: config.host, port: config.port, user: config.user, password: config.password, database: config.database, max: config.concurrency });
  pool.on("error", () => {}); // suppress unhandled pool-level errors
  let ops = 0, errors = 0;
  const latencies: number[] = [];
  const notes: string[] = [];

  const start = Date.now();
  const workers = Array.from({ length: config.concurrency }, (_, i) => (async () => {
    // Unnamed prepared (parameterized queries)
    for (let iter = 0; iter < 10; iter++) {
      const t = performance.now();
      try { await pool.query("SELECT count(*) FROM users WHERE balance > $1", [iter * 100]); }
      catch { errors++; }
      latencies.push(performance.now() - t); ops++;
    }

    // Named prepared statement
    const client = await pool.connect();
    try {
      const stmtName = `stmt_w${i}`;
      let t = performance.now();
      try {
        await client.query({ name: stmtName, text: "SELECT id, name, balance FROM users WHERE id = $1", values: [i + 1] });
      } catch (e: any) {
        errors++; ops++;
        if (notes.length === 0) notes.push(`named prepare failed: ${e.message}`);
        return;
      }
      latencies.push(performance.now() - t); ops++;

      // Reuse
      for (let iter = 0; iter < 5; iter++) {
        t = performance.now();
        try { await client.query({ name: stmtName, text: "SELECT id, name, balance FROM users WHERE id = $1", values: [i * 5 + iter + 1] }); }
        catch { errors++; }
        latencies.push(performance.now() - t); ops++;
      }

      // Deallocate
      try { await client.query(`DEALLOCATE ${stmtName}`); }
      catch { errors++; }
      ops++;
    } finally { client.release(); }
  })());

  await Promise.all(workers);
  await pool.end();
  return { totalOps: ops, errors, durationMs: Date.now() - start, latencies, notes };
}
