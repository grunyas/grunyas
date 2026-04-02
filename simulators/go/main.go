package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"github.com/grunyas/simulator/scenarios"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Report types matching the shared JSON schema.
type LatencyStats struct {
	MinMs float64 `json:"min_ms"`
	MaxMs float64 `json:"max_ms"`
	AvgMs float64 `json:"avg_ms"`
	P50Ms float64 `json:"p50_ms"`
	P95Ms float64 `json:"p95_ms"`
	P99Ms float64 `json:"p99_ms"`
}

type ScenarioResult struct {
	Name       string       `json:"name"`
	Status     string       `json:"status"`
	DurationMs float64      `json:"duration_ms"`
	TotalOps   int          `json:"total_ops"`
	OpsPerSec  float64      `json:"ops_per_sec"`
	Errors     int          `json:"errors"`
	Latency    LatencyStats `json:"latency"`
	Notes      []string     `json:"notes"`
}

type RunSummary struct {
	TotalDurationMs float64 `json:"total_duration_ms"`
	TotalOps        int     `json:"total_ops"`
	TotalErrors     int     `json:"total_errors"`
	ScenariosPassed int     `json:"scenarios_passed"`
	ScenariosFailed int     `json:"scenarios_failed"`
}

type Run struct {
	PoolMode  string           `json:"pool_mode"`
	Scenarios []ScenarioResult `json:"scenarios"`
	Summary   RunSummary       `json:"summary"`
}

type Report struct {
	Simulator string                 `json:"simulator"`
	Timestamp string                 `json:"timestamp"`
	Config    map[string]interface{} `json:"config"`
	Runs      []Run                  `json:"runs"`
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		n, err := strconv.Atoi(v)
		if err == nil {
			return n
		}
	}
	return fallback
}

func main() {
	dbHost := env("DB_HOST", "localhost")
	dbPort := env("DB_PORT", "5711")
	dbUser := env("DB_USER", "postgres")
	dbPass := env("DB_PASSWORD", "postgres")
	dbName := env("DB_NAME", "simulator")
	concurrency := envInt("CONCURRENCY", 100)
	poolMode := env("POOL_MODE", "session")

	connStr := fmt.Sprintf("postgres://%s:%s@%s:%s/%s?pool_max_conns=%d",
		dbUser, dbPass, dbHost, dbPort, dbName, concurrency+10)

	log.Printf("Go Simulator starting: pool_mode=%s concurrency=%d host=%s:%s",
		poolMode, concurrency, dbHost, dbPort)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	// Wait for Grunyas to be ready
	var pool *pgxpool.Pool
	for i := 0; i < 30; i++ {
		var err error
		pool, err = pgxpool.New(ctx, connStr)
		if err == nil {
			if err = pool.Ping(ctx); err == nil {
				break
			}
			pool.Close()
			pool = nil
		}
		log.Printf("Waiting for database... attempt %d/30", i+1)
		time.Sleep(2 * time.Second)
	}
	if pool == nil {
		log.Fatal("Failed to connect to database after 30 attempts")
	}
	defer pool.Close()

	log.Println("Connected successfully, running scenarios...")

	cfg := &scenarios.Config{
		ConnStr:     connStr,
		Concurrency: concurrency,
		PoolMode:    poolMode,
		DBHost:      dbHost,
		DBPort:      dbPort,
		DBUser:      dbUser,
		DBPass:      dbPass,
		DBName:      dbName,
	}

	allScenarios := []struct {
		name string
		fn   func(ctx context.Context, cfg *scenarios.Config) (*scenarios.Result, error)
	}{
		{"basic_crud", scenarios.BasicCRUD},
		{"transactions", scenarios.Transactions},
		{"prepared_statements", scenarios.PreparedStatements},
		{"concurrent_rw", scenarios.ConcurrentRW},
		{"connection_storms", scenarios.ConnectionStorms},
		{"long_running", scenarios.LongRunning},
		{"error_handling", scenarios.ErrorHandling},
		{"batch_operations", scenarios.BatchOperations},
		{"pool_behavior", scenarios.PoolBehavior},
	}

	var results []ScenarioResult
	for _, s := range allScenarios {
		log.Printf("  Running scenario: %s", s.name)
		result, err := s.fn(ctx, cfg)
		if err != nil {
			log.Printf("  FAIL: %s: %v", s.name, err)
			results = append(results, ScenarioResult{
				Name:   s.name,
				Status: "fail",
				Notes:  []string{fmt.Sprintf("fatal error: %v", err)},
			})
			continue
		}
		sr := toScenarioResult(s.name, result)
		log.Printf("  %s: status=%s ops=%d errors=%d ops/s=%.0f",
			s.name, sr.Status, sr.TotalOps, sr.Errors, sr.OpsPerSec)
		results = append(results, sr)
	}

	// Build run summary
	summary := RunSummary{}
	for _, r := range results {
		summary.TotalOps += r.TotalOps
		summary.TotalErrors += r.Errors
		summary.TotalDurationMs += r.DurationMs
		if r.Status == "pass" || r.Status == "partial" {
			summary.ScenariosPassed++
		} else {
			summary.ScenariosFailed++
		}
	}

	run := Run{
		PoolMode:  poolMode,
		Scenarios: results,
		Summary:   summary,
	}

	report := Report{
		Simulator: "go",
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Config: map[string]interface{}{
			"concurrency": concurrency,
			"driver":      "pgx/v5",
		},
		Runs: []Run{run},
	}

	// Write results
	outPath := filepath.Join("results", poolMode+".json")
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		log.Fatalf("Failed to marshal results: %v", err)
	}
	if err := os.MkdirAll("results", 0o755); err != nil {
		log.Fatalf("Failed to create results dir: %v", err)
	}
	if err := os.WriteFile(outPath, data, 0o644); err != nil {
		log.Fatalf("Failed to write results: %v", err)
	}

	log.Printf("Results written to %s", outPath)
	log.Printf("Summary: %d scenarios passed, %d failed, %d total ops, %d errors",
		summary.ScenariosPassed, summary.ScenariosFailed, summary.TotalOps, summary.TotalErrors)
}

func toScenarioResult(name string, r *scenarios.Result) ScenarioResult {
	sr := ScenarioResult{
		Name:       name,
		TotalOps:   r.TotalOps,
		Errors:     r.Errors,
		DurationMs: float64(r.Duration.Milliseconds()),
		Notes:      r.Notes,
	}

	if r.Duration > 0 {
		sr.OpsPerSec = float64(r.TotalOps) / r.Duration.Seconds()
	}

	sr.Latency = computeLatency(r.Latencies)

	if r.Errors == 0 {
		sr.Status = "pass"
	} else if r.Errors < r.TotalOps/2 {
		sr.Status = "partial"
	} else {
		sr.Status = "fail"
	}

	if len(r.Notes) > 0 {
		sr.Status = "partial"
	}

	return sr
}

func computeLatency(latencies []time.Duration) LatencyStats {
	if len(latencies) == 0 {
		return LatencyStats{}
	}

	sorted := make([]time.Duration, len(latencies))
	copy(sorted, latencies)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	var total time.Duration
	for _, l := range sorted {
		total += l
	}

	n := len(sorted)
	return LatencyStats{
		MinMs: float64(sorted[0].Microseconds()) / 1000,
		MaxMs: float64(sorted[n-1].Microseconds()) / 1000,
		AvgMs: float64(total.Microseconds()) / float64(n) / 1000,
		P50Ms: float64(sorted[n*50/100].Microseconds()) / 1000,
		P95Ms: float64(sorted[n*95/100].Microseconds()) / 1000,
		P99Ms: float64(sorted[min(n*99/100, n-1)].Microseconds()) / 1000,
	}
}

