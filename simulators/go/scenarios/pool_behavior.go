package scenarios

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// PoolBehavior verifies connection multiplexing by tracking pg_backend_pid() across queries.
//
// In session mode the same backend must serve all queries on a connection (PID stable).
// In transaction mode the backend is released after each transaction, so PIDs should change.
//
// Reliability requirement: the backend pool must have more than one connection. With a single
// backend, every query on every client connection lands on the same process — PID never changes
// even though Grunyas is correctly releasing and re-acquiring between transactions. This produces
// a false negative in transaction mode (looks like session pinning when it isn't).
//
// False-positive probability in transaction mode: if the pool has B backends and a worker runs
// N sequential queries, the chance of seeing the same PID across all N queries is (1/B)^(N-1).
// With B=4 backends and N=10 queries that's (1/4)^9 ≈ 0.00015% per worker — negligible in
// practice, but the signal degrades sharply as B approaches 1.
func PoolBehavior(ctx context.Context, cfg *Config) (*Result, error) {
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

	type pidResult struct {
		changed bool
		total   int
	}

	pidResults := make([]pidResult, cfg.Concurrency)

	start := time.Now()
	var wg sync.WaitGroup

	workers := min(cfg.Concurrency, 50) // limit for this scenario

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()

			// Acquire a connection and track PID across multiple queries
			conn, err := pool.Acquire(ctx)
			if err != nil {
				if !(cfg.PoolMode == "session" && IsCapacityError(err)) {
					errCount.Add(1)
				}
				ops.Add(1)
				return
			}
			defer conn.Release()

			var firstPID int
			pids := make(map[int]bool)

			for iter := 0; iter < 10; iter++ {
				t := time.Now()
				var pid int
				err := conn.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid)
				d := time.Since(t)
				mu.Lock()
				latencies = append(latencies, d)
				mu.Unlock()
				ops.Add(1)
				if err != nil {
					if !(cfg.PoolMode == "session" && IsCapacityError(err)) {
						errCount.Add(1)
					}
					continue
				}

				pids[pid] = true
				if iter == 0 {
					firstPID = pid
				}
			}

			pidResults[workerID] = pidResult{
				changed: len(pids) > 1,
				total:   len(pids),
			}
			_ = firstPID
		}(i)
	}

	wg.Wait()
	duration := time.Since(start)

	// In session mode: PID changes are unexpected — count them as errors.
	// In transaction mode: no PID changes on a worker means multiplexing wasn't observed — count as errors.
	for i := 0; i < workers; i++ {
		if cfg.PoolMode == "session" && pidResults[i].changed {
			errCount.Add(1)
		} else if cfg.PoolMode == "transaction" && !pidResults[i].changed {
			errCount.Add(1)
		}
	}

	return &Result{
		TotalOps:  int(ops.Load()),
		Errors:    int(errCount.Load()),
		Duration:  duration,
		Latencies: latencies,
	}, nil
}
