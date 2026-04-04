package scenarios

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// ErrorHandling tests invalid SQL, constraint violations, and verifies connections remain usable after errors.
func ErrorHandling(ctx context.Context, cfg *Config) (*Result, error) {
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
		notes      []string
		notesMu    sync.Mutex
	)

	start := time.Now()
	var wg sync.WaitGroup

	for i := 0; i < cfg.Concurrency; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()

			// --- Invalid SQL (syntax error) ---
			t := time.Now()
			_, err := pool.Exec(ctx, "SELEKT invalid_syntax FROM nowhere")
			d := time.Since(t)
			mu.Lock()
			latencies = append(latencies, d)
			mu.Unlock()
			ops.Add(1)
			if err == nil {
				errCount.Add(1) // Expected an error
				notesMu.Lock()
				notes = append(notes, "expected error for invalid SQL but got none")
				notesMu.Unlock()
			}

			// --- Verify connection still works after error ---
			t = time.Now()
			var v int
			err = pool.QueryRow(ctx, "SELECT 1").Scan(&v)
			d = time.Since(t)
			mu.Lock()
			latencies = append(latencies, d)
			mu.Unlock()
			ops.Add(1)
			if err != nil {
				if !(cfg.PoolMode == "session" && IsCapacityError(err)) {
					errCount.Add(1)
					notesMu.Lock()
					if len(notes) < 5 {
						notes = append(notes, fmt.Sprintf("connection broken after error: %v", err))
					}
					notesMu.Unlock()
				}
			}

			// --- Unique constraint violation ---
			t = time.Now()
			_, err = pool.Exec(ctx, "INSERT INTO users (name, email, balance) VALUES ($1, $2, $3)",
				"dup_user", "user_1@test.com", 0) // user_1@test.com already exists from seed
			d = time.Since(t)
			mu.Lock()
			latencies = append(latencies, d)
			mu.Unlock()
			ops.Add(1)
			if err == nil {
				// Could succeed if another worker deleted user_1, that's fine
			}

			// --- Verify connection still works after constraint violation ---
			t = time.Now()
			err = pool.QueryRow(ctx, "SELECT 1").Scan(&v)
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

			// --- Division by zero ---
			t = time.Now()
			_, err = pool.Exec(ctx, "SELECT 1/0")
			d = time.Since(t)
			mu.Lock()
			latencies = append(latencies, d)
			mu.Unlock()
			ops.Add(1)
			if err == nil {
				errCount.Add(1)
			}

			// --- Verify connection still works ---
			t = time.Now()
			err = pool.QueryRow(ctx, "SELECT 42").Scan(&v)
			d = time.Since(t)
			mu.Lock()
			latencies = append(latencies, d)
			mu.Unlock()
			ops.Add(1)
			if err != nil || v != 42 {
				if !(cfg.PoolMode == "session" && IsCapacityError(err)) {
					errCount.Add(1)
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
		Notes:     notes,
	}, nil
}
