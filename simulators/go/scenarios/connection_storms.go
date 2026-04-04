package scenarios

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
)

// ConnectionStorms simulates rapid connect/disconnect cycles — the worst case for a connection pooler.
func ConnectionStorms(ctx context.Context, cfg *Config) (*Result, error) {
	var (
		ops       atomic.Int64
		errCount  atomic.Int64
		mu        sync.Mutex
		latencies []time.Duration
	)

	connStr := fmt.Sprintf("postgres://%s:%s@%s:%s/%s",
		cfg.DBUser, cfg.DBPass, cfg.DBHost, cfg.DBPort, cfg.DBName)

	// In transaction mode, pgx uses a two-phase prepare (Parse+Describe+Sync then Bind+Execute+Sync)
	// for fresh connections with no cached descriptions. Grunyas releases the backend after each
	// ReadyForQuery, so phase 2 lands on a different backend that has no prepared statement.
	// Simple protocol avoids this by sending a single Query message with no prepare step.
	connCfg, err := pgx.ParseConfig(connStr)
	if err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if cfg.PoolMode == "transaction" {
		connCfg.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	}

	start := time.Now()
	var wg sync.WaitGroup

	// Each connection opens, runs one query, closes
	storms := cfg.Concurrency * 2
	for i := 0; i < storms; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			t := time.Now()
			conn, err := pgx.ConnectConfig(ctx, connCfg)
			if err != nil {
				// Filter capacity rejections (SQLSTATE 53300) in session mode
				if !(cfg.PoolMode == "session" && IsCapacityError(err)) {
					errCount.Add(1)
				}
				ops.Add(1)
				return
			}

			var result int
			err = conn.QueryRow(ctx, "SELECT 1").Scan(&result)
			ops.Add(1)
			if err != nil {
				if !(cfg.PoolMode == "session" && IsCapacityError(err)) {
					errCount.Add(1)
				}
			}

			_ = conn.Close(ctx)
			d := time.Since(t)
			mu.Lock()
			latencies = append(latencies, d)
			mu.Unlock()
		}()
	}

	wg.Wait()
	duration := time.Since(start)

	return &Result{
		TotalOps:  int(ops.Load()),
		Errors:    int(errCount.Load()),
		Duration:  duration,
		Latencies: latencies,
	}, nil
}
