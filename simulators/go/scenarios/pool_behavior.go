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
// In transaction mode the backend is released after each transaction, so PIDs should change
// across bare (autocommit) queries on the same client connection.
//
// Reliability requirement: the backend pool must have more than one connection. With a single
// backend every query lands on the same process — PID never changes even though the proxy is
// correctly releasing and re-acquiring between transactions. This produces a false negative in
// transaction mode (looks like session pinning when it isn't).
//
// False-positive probability in transaction mode: if the pool has B backends and a worker runs
// N sequential queries, the chance of seeing the same PID across all N queries is (1/B)^(N-1).
// With B=4 backends and N=10 queries that's (1/4)^9 ≈ 0.00015% per worker — negligible.
func PoolBehavior(ctx context.Context, cfg *Config) (*Result, error) {
	var (
		ops       atomic.Int64
		errCount  atomic.Int64
		mu        sync.Mutex
		latencies []time.Duration
	)
	errs := NewErrorSampler(10)

	type pidResult struct {
		changed bool
		total   int
	}

	workers := min(cfg.Concurrency, 50)
	pidResults := make([]pidResult, workers)

	start := time.Now()
	var wg sync.WaitGroup

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()

			conn, err := ConnectClient(ctx, cfg)
			if err != nil {
				ops.Add(1)
				errCount.Add(1)
				return
			}
			defer conn.Close(ctx)

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
					errCount.Add(1)
					errs.Record(err)
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
			errs.Record(fmt.Errorf("session mode: worker %d saw %d different PIDs (expected 1)", i, pidResults[i].total))
		} else if cfg.PoolMode == "transaction" && !pidResults[i].changed {
			errCount.Add(1)
			errs.Record(fmt.Errorf("transaction mode: worker %d saw only 1 PID (expected multiplexing)", i))
		}
	}

	return &Result{
		TotalOps:  int(ops.Load()),
		Errors:    int(errCount.Load()),
		Duration:  duration,
		Latencies: latencies,
		Notes:     errs.Notes(),
	}, nil
}
