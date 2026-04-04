import { Pool } from "pg";
import { Config, RawResult } from "../types";

export async function preparedStatements(config: Config): Promise<RawResult> {
  const pool = new Pool({ host: config.host, port: config.port, user: config.user, password: config.password, database: config.database, max: config.concurrency });
  pool.on("error", () => {}); // suppress unhandled pool-level errors
  let ops = 0, errors = 0;
  const latencies: number[] = [];

  const start = Date.now();

  // Phase 1: unnamed parameterized queries — safe in all pool modes.
  await Promise.all(Array.from({ length: config.concurrency }, (_, i) => (async () => {
    for (let iter = 0; iter < 10; iter++) {
      const t = performance.now();
      try { await pool.query("SELECT count(*) FROM users WHERE balance > $1", [iter * 100]); }
      catch { errors++; }
      latencies.push(performance.now() - t); ops++;
    }
  })()));

  // Phase 2: named prepared statements wrapped in BEGIN/COMMIT to pin the backend.
  // In transaction mode each BEGIN holds a backend until COMMIT, so run serially to
  // avoid exhausting the (small) backend pool.  In session mode run concurrently.
  const namedWorker = async (i: number) => {
    const stmtName = `stmt_w${i}`;
    const client = await pool.connect();
    client.on("error", () => {});
    try {
      await client.query("BEGIN");
      let t = performance.now();
      try {
        await client.query(`PREPARE ${stmtName} AS SELECT id, name, balance FROM users WHERE id = $1`);
      } catch {
        errors++; ops++;
        try { await client.query("ROLLBACK"); } catch {}
        return;
      }
      latencies.push(performance.now() - t); ops++;

      for (let iter = 0; iter < 5; iter++) {
        t = performance.now();
        try { await client.query(`EXECUTE ${stmtName}(${i * 5 + iter + 1})`); }
        catch { errors++; }
        latencies.push(performance.now() - t); ops++;
      }

      try { await client.query(`DEALLOCATE ${stmtName}`); } catch { errors++; }
      ops++;
      await client.query("COMMIT");
    } catch {
      errors++;
      try { await client.query("ROLLBACK"); } catch {}
    } finally { client.release(); }
  };

  const indices = Array.from({ length: config.concurrency }, (_, i) => i);
  if (config.poolMode === "transaction") {
    // Serial: only one BEGIN/COMMIT transaction active at a time.
    for (const i of indices) await namedWorker(i);
  } else {
    await Promise.all(indices.map(namedWorker));
  }

  await pool.end();
  return { totalOps: ops, errors, durationMs: Date.now() - start, latencies };
}
