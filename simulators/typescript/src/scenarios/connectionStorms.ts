import { Client } from "pg";
import { Config, RawResult } from "../types";

export async function connectionStorms(config: Config): Promise<RawResult> {
  let ops = 0, errors = 0;
  const latencies: number[] = [];

  const start = Date.now();
  const storms = Array.from({ length: config.concurrency * 2 }, () => (async () => {
    const t = performance.now();
    const client = new Client({ host: config.host, port: config.port, user: config.user, password: config.password, database: config.database });
    client.on("error", () => {}); // suppress unhandled error events
    try {
      await client.connect();
      await client.query("SELECT 1");
      ops++;
    } catch { errors++; ops++; }
    finally { try { await client.end(); } catch {} }
    latencies.push(performance.now() - t);
  })());

  await Promise.all(storms);
  return { totalOps: ops, errors, durationMs: Date.now() - start, latencies };
}
