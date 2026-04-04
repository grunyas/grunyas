package scenarios

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// BasicCRUD exercises INSERT, SELECT, UPDATE, DELETE operations concurrently.
func BasicCRUD(ctx context.Context, cfg *Config) (*Result, error) {
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

	for i := 0; i < cfg.Concurrency; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()

			for iter := 0; iter < 20; iter++ {
				email := fmt.Sprintf("crud_%d_%d@test.com", workerID, iter)

				// INSERT
				t := time.Now()
				var userID int
				err := pool.QueryRow(ctx, "INSERT INTO users (name, email, balance) VALUES ($1, $2, $3) RETURNING id",
					fmt.Sprintf("crud_user_%d_%d", workerID, iter), email, 100.00).Scan(&userID)
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

				// SELECT
				t = time.Now()
				var name string
				err = pool.QueryRow(ctx, "SELECT name FROM users WHERE id = $1", userID).Scan(&name)
				d = time.Since(t)
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

				// UPDATE
				t = time.Now()
				_, err = pool.Exec(ctx, "UPDATE users SET balance = balance + 50 WHERE id = $1", userID)
				d = time.Since(t)
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

				// DELETE
				t = time.Now()
				_, err = pool.Exec(ctx, "DELETE FROM users WHERE id = $1", userID)
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
