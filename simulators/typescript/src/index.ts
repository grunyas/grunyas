import { Pool } from "pg";
import * as fs from "fs";
import * as path from "path";
import { Config, Report, ScenarioResult, RawResult, computeLatency } from "./types";
import { basicCrud } from "./scenarios/basicCrud";
import { transactions } from "./scenarios/transactions";
import { preparedStatements } from "./scenarios/preparedStatements";
import { concurrentRw } from "./scenarios/concurrentRw";
import { connectionStorms } from "./scenarios/connectionStorms";
import { longRunning } from "./scenarios/longRunning";
import { errorHandling } from "./scenarios/errorHandling";
import { batchOperations } from "./scenarios/batchOperations";
import { poolBehavior } from "./scenarios/poolBehavior";

function env(key: string, fallback: string): string {
  return process.env[key] || fallback;
}

async function waitForDb(config: Config, retries = 30): Promise<void> {
  for (let i = 0; i < retries; i++) {
    try {
      const pool = new Pool({ host: config.host, port: config.port, user: config.user, password: config.password, database: config.database, max: 1 });
      await pool.query("SELECT 1");
      await pool.end();
      return;
    } catch {
      console.log(`Waiting for database... attempt ${i + 1}/${retries}`);
      await new Promise((r) => setTimeout(r, 2000));
    }
  }
  throw new Error("Failed to connect to database after retries");
}

function toScenarioResult(name: string, raw: RawResult): ScenarioResult {
  const opsPerSec = raw.durationMs > 0 ? (raw.totalOps / raw.durationMs) * 1000 : 0;
  let status: "pass" | "fail" | "partial" = "pass";
  if (raw.errors > 0 && raw.errors >= raw.totalOps / 2) status = "fail";
  else if (raw.errors > 0 || (raw.notes && raw.notes.length > 0)) status = "partial";

  return {
    name,
    status,
    duration_ms: Math.round(raw.durationMs * 100) / 100,
    total_ops: raw.totalOps,
    ops_per_sec: Math.round(opsPerSec * 100) / 100,
    errors: raw.errors,
    latency: computeLatency(raw.latencies),
    notes: raw.notes || [],
  };
}

async function main(): Promise<void> {
  const config: Config = {
    host: env("DB_HOST", "localhost"),
    port: parseInt(env("DB_PORT", "5711")),
    user: env("DB_USER", "postgres"),
    password: env("DB_PASSWORD", "postgres"),
    database: env("DB_NAME", "simulator"),
    concurrency: parseInt(env("CONCURRENCY", "100")),
    poolMode: env("POOL_MODE", "session"),
  };

  console.log(`TypeScript Simulator starting: pool_mode=${config.poolMode} concurrency=${config.concurrency} host=${config.host}:${config.port}`);

  await waitForDb(config);
  console.log("Connected successfully, running scenarios...");

  const allScenarios: [string, (cfg: Config) => Promise<RawResult>][] = [
    ["basic_crud", basicCrud],
    ["transactions", transactions],
    ["prepared_statements", preparedStatements],
    ["concurrent_rw", concurrentRw],
    ["connection_storms", connectionStorms],
    ["long_running", longRunning],
    ["error_handling", errorHandling],
    ["batch_operations", batchOperations],
    ["pool_behavior", poolBehavior],
  ];

  const results: ScenarioResult[] = [];

  for (const [name, fn] of allScenarios) {
    console.log(`  Running scenario: ${name}`);
    try {
      const raw = await fn(config);
      const sr = toScenarioResult(name, raw);
      console.log(`  ${name}: status=${sr.status} ops=${sr.total_ops} errors=${sr.errors} ops/s=${sr.ops_per_sec}`);
      results.push(sr);
    } catch (e: any) {
      console.log(`  FAIL: ${name}: ${e.message}`);
      results.push({
        name,
        status: "fail",
        duration_ms: 0,
        total_ops: 0,
        ops_per_sec: 0,
        errors: 1,
        latency: computeLatency([]),
        notes: [`fatal error: ${e.message}`],
      });
    }
  }

  const summary = {
    total_duration_ms: results.reduce((s, r) => s + r.duration_ms, 0),
    total_ops: results.reduce((s, r) => s + r.total_ops, 0),
    total_errors: results.reduce((s, r) => s + r.errors, 0),
    scenarios_passed: results.filter((r) => r.status === "pass" || r.status === "partial").length,
    scenarios_failed: results.filter((r) => r.status === "fail").length,
  };

  const report: Report = {
    simulator: "typescript",
    timestamp: new Date().toISOString(),
    config: { concurrency: config.concurrency, driver: "node-postgres" },
    runs: [{ pool_mode: config.poolMode, scenarios: results, summary }],
  };

  const outDir = path.join("results");
  fs.mkdirSync(outDir, { recursive: true });
  const outPath = path.join(outDir, `${config.poolMode}.json`);
  fs.writeFileSync(outPath, JSON.stringify(report, null, 2));

  console.log(`Results written to ${outPath}`);
  console.log(`Summary: ${summary.scenarios_passed} passed, ${summary.scenarios_failed} failed, ${summary.total_ops} ops, ${summary.total_errors} errors`);
}

main().catch((e) => {
  console.error("Fatal:", e);
  process.exit(1);
});
