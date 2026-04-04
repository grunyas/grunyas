package scenarios

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// LongRunning tests pg_sleep and large result set queries to ensure they don't block the pool.
func LongRunning(ctx context.Context, cfg *Config) (*Result, error) {
	pool, err := NewPool(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}
	defer pool.Close()

	var (
		ops        atomic.Int64
		errCount   atomic.Int64
		mu         sync.Mutex
		latencies  []time.Duration
	)

	start := time.Now()
	var wg sync.WaitGroup

	// Half the workers do pg_sleep, half do large result sets
	workers := min(cfg.Concurrency, 20) // limit to avoid too many long queries

	// pg_sleep workers
	for i := 0; i < workers/2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			t := time.Now()
			_, err := pool.Exec(ctx, "SELECT pg_sleep(1)")
			d := time.Since(t)
			mu.Lock()
			latencies = append(latencies, d)
			mu.Unlock()
			ops.Add(1)
			if err != nil {
				if !(cfg.PoolMode == "session" && IsCapacityError(err)) {
					errCount.Add(1)
				}
			}
		}()
	}

	// Large result set workers
	for i := 0; i < workers/2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			t := time.Now()
			rows, err := pool.Query(ctx, "SELECT generate_series(1, 10000)")
			if err != nil {
				if !(cfg.PoolMode == "session" && IsCapacityError(err)) {
					errCount.Add(1)
				}
				ops.Add(1)
				return
			}
			count := 0
			for rows.Next() {
				count++
			}
			rows.Close()
			d := time.Since(t)
			mu.Lock()
			latencies = append(latencies, d)
			mu.Unlock()
			ops.Add(1)
			if err := rows.Err(); err != nil {
				if !(cfg.PoolMode == "session" && IsCapacityError(err)) {
					errCount.Add(1)
				}
			}
		}()
	}

	// Meanwhile, other workers should still be able to run quick queries
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			for j := 0; j < 5; j++ {
				t := time.Now()
				var v int
				err := pool.QueryRow(ctx, "SELECT 1").Scan(&v)
				d := time.Since(t)
				mu.Lock()
				latencies = append(latencies, d)
				mu.Unlock()
				ops.Add(1)
				if err != nil {
					if !(cfg.PoolMode == "session" && IsCapacityError(err)) {
						errCount.Add(1)
					}
				}
			}
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
