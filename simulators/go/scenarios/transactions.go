package scenarios

import (
	"context"
	"fmt"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"
)

// Transactions exercises BEGIN/COMMIT, BEGIN/ROLLBACK, and SAVEPOINT flows.
func Transactions(ctx context.Context, cfg *Config) (*Result, error) {
	pool, err := NewPool(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}
	defer pool.Close()

	// Unique per-run prefix prevents duplicate key violations on repeated runs.
	runID := rand.Uint64()

	var (
		ops        atomic.Int64
		errCount   atomic.Int64
		mu         sync.Mutex
		latencies  []time.Duration
	)

	start := time.Now()
	var wg sync.WaitGroup

	for i := 0; i < cfg.Concurrency; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()

			for iter := 0; iter < 10; iter++ {
				// --- Commit flow ---
				t := time.Now()
				tx, err := pool.Begin(ctx)
				if err != nil {
					if !(cfg.PoolMode == "session" && IsCapacityError(err)) {
						errCount.Add(1)
					}
					ops.Add(1)
					continue
				}

				email := fmt.Sprintf("tx_%d_%d_%d@test.com", runID, workerID, iter)
				_, err = tx.Exec(ctx, "INSERT INTO users (name, email, balance) VALUES ($1, $2, $3)",
					fmt.Sprintf("tx_user_%d_%d", workerID, iter), email, 500.00)
				if err != nil {
					_ = tx.Rollback(ctx)
					if !(cfg.PoolMode == "session" && IsCapacityError(err)) {
						errCount.Add(1)
					}
					ops.Add(1)
					continue
				}

				err = tx.Commit(ctx)
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

				// --- Rollback flow ---
				t = time.Now()
				tx, err = pool.Begin(ctx)
				if err != nil {
					if !(cfg.PoolMode == "session" && IsCapacityError(err)) {
						errCount.Add(1)
					}
					ops.Add(1)
					continue
				}

				_, _ = tx.Exec(ctx, "INSERT INTO users (name, email, balance) VALUES ($1, $2, $3)",
					"will_rollback", fmt.Sprintf("rb_%d_%d@test.com", workerID, iter), 999.99)

				err = tx.Rollback(ctx)
				d = time.Since(t)
				mu.Lock()
				latencies = append(latencies, d)
				mu.Unlock()
				ops.Add(1)
				if err != nil {
					if !(cfg.PoolMode == "session" && IsCapacityError(err)) {
						errCount.Add(1)
					}
				}

				// --- Savepoint flow ---
				t = time.Now()
				tx, err = pool.Begin(ctx)
				if err != nil {
					if !(cfg.PoolMode == "session" && IsCapacityError(err)) {
						errCount.Add(1)
					}
					ops.Add(1)
					continue
				}

				_, _ = tx.Exec(ctx, "SAVEPOINT sp1")
				spEmail := fmt.Sprintf("sp_%d_%d@test.com", workerID, iter)
				_, _ = tx.Exec(ctx, "INSERT INTO users (name, email, balance) VALUES ($1, $2, $3)",
					"savepoint_user", spEmail, 100.00)
				_, _ = tx.Exec(ctx, "ROLLBACK TO SAVEPOINT sp1")
				err = tx.Commit(ctx)
				d = time.Since(t)
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
		}(i)
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
