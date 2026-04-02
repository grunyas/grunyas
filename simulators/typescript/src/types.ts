export interface Config {
  host: string;
  port: number;
  user: string;
  password: string;
  database: string;
  concurrency: number;
  poolMode: string;
}

export interface LatencyStats {
  min_ms: number;
  max_ms: number;
  avg_ms: number;
  p50_ms: number;
  p95_ms: number;
  p99_ms: number;
}

export interface ScenarioResult {
  name: string;
  status: "pass" | "fail" | "partial";
  duration_ms: number;
  total_ops: number;
  ops_per_sec: number;
  errors: number;
  latency: LatencyStats;
  notes: string[];
}

export interface RunSummary {
  total_duration_ms: number;
  total_ops: number;
  total_errors: number;
  scenarios_passed: number;
  scenarios_failed: number;
}

export interface Run {
  pool_mode: string;
  scenarios: ScenarioResult[];
  summary: RunSummary;
}

export interface Report {
  simulator: string;
  timestamp: string;
  config: { concurrency: number; driver: string };
  runs: Run[];
}

export interface RawResult {
  totalOps: number;
  errors: number;
  durationMs: number;
  latencies: number[];
  notes?: string[];
}

export function computeLatency(latencies: number[]): LatencyStats {
  if (latencies.length === 0) {
    return { min_ms: 0, max_ms: 0, avg_ms: 0, p50_ms: 0, p95_ms: 0, p99_ms: 0 };
  }
  const sorted = [...latencies].sort((a, b) => a - b);
  const n = sorted.length;
  const sum = sorted.reduce((a, b) => a + b, 0);
  return {
    min_ms: round(sorted[0]),
    max_ms: round(sorted[n - 1]),
    avg_ms: round(sum / n),
    p50_ms: round(sorted[Math.floor(n * 0.5)]),
    p95_ms: round(sorted[Math.floor(n * 0.95)]),
    p99_ms: round(sorted[Math.min(Math.floor(n * 0.99), n - 1)]),
  };
}

function round(v: number): number {
  return Math.round(v * 1000) / 1000;
}
