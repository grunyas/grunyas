import { Pool } from "pg";
import { Config, RawResult } from "../types";

export async function errorHandling(config: Config): Promise<RawResult> {
  const pool = new Pool({ host: config.host, port: config.port, user: config.user, password: config.password, database: config.database, max: config.concurrency });
  pool.on("error", () => {}); // suppress unhandled pool-level errors
  let ops = 0, errors = 0;
  const latencies: number[] = [];
  const notes: string[] = [];

  const start = Date.now();
  const workers = Array.from({ length: config.concurrency }, (_, i) => (async () => {
    // Invalid SQL
    let t = performance.now();
    try { await pool.query("SELEKT invalid_syntax FROM nowhere"); errors++; }
    catch {} // expected
    latencies.push(performance.now() - t); ops++;

    // Verify recovery
    t = performance.now();
    try {
      const res = await pool.query("SELECT 1");
      if (res.rows[0]["?column?"] !== 1) errors++;
    } catch (e: any) {
      errors++;
      if (notes.length < 5) notes.push(`connection broken after error: ${e.message}`);
    }
    latencies.push(performance.now() - t); ops++;

    // Unique violation
    t = performance.now();
    try { await pool.query("INSERT INTO users (name, email, balance) VALUES ($1, $2, $3)", ["dup", "user_1@test.com", 0]); }
    catch {} // expected or not
    latencies.push(performance.now() - t); ops++;

    // Verify recovery
    t = performance.now();
    try { await pool.query("SELECT 1"); }
    catch { errors++; }
    latencies.push(performance.now() - t); ops++;

    // Division by zero
    t = performance.now();
    try { await pool.query("SELECT 1/0"); errors++; }
    catch {} // expected
    latencies.push(performance.now() - t); ops++;

    // Final recovery
    t = performance.now();
    try {
      const res = await pool.query("SELECT 42 as v");
      if (res.rows[0].v !== 42) errors++;
    } catch { errors++; }
    latencies.push(performance.now() - t); ops++;
  })());

  await Promise.all(workers);
  await pool.end();
  return { totalOps: ops, errors, durationMs: Date.now() - start, latencies, notes };
}
