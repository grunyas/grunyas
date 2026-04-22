package scenarios

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// ErrorHandling tests invalid SQL, constraint violations, and verifies the connection
// remains usable after each error — on the same persistent connection.
func ErrorHandling(ctx context.Context, cfg *Config) (*Result, error) {
	var (
		ops       atomic.Int64
		errCount  atomic.Int64
		mu        sync.Mutex
		latencies []time.Duration
		notes     []string
		notesMu   sync.Mutex
	)

	start := time.Now()
	var wg sync.WaitGroup

	recordNote := func(msg string) {
		notesMu.Lock()
		if len(notes) < 5 {
			notes = append(notes, msg)
		}
		notesMu.Unlock()
	}

	for i := 0; i < cfg.Concurrency; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()

			conn, err := ConnectClient(ctx, cfg)
			if err != nil {
				ops.Add(1)
				errCount.Add(1)
				return
			}
			defer conn.Close(context.Background())

			for iter := 0; cfg.Continue(ctx, iter, 1); iter++ {
				// --- Invalid SQL (syntax error) ---
				t := time.Now()
				_, err = conn.Exec(ctx, "SELEKT invalid_syntax FROM nowhere")
				d := time.Since(t)
				mu.Lock()
				latencies = append(latencies, d)
				mu.Unlock()
				ops.Add(1)
				if err == nil {
					errCount.Add(1) // Expected an error
					recordNote("expected error for invalid SQL but got none")
				} else if ctx.Err() != nil || conn.IsClosed() {
					return
				}

				// --- Verify connection still works after error ---
				t = time.Now()
				var v int
				err = conn.QueryRow(ctx, "SELECT 1").Scan(&v)
				d = time.Since(t)
				mu.Lock()
				latencies = append(latencies, d)
				mu.Unlock()
				ops.Add(1)
				if err != nil {
					if ctx.Err() != nil || conn.IsClosed() {
						return
					}
					errCount.Add(1)
					recordNote(fmt.Sprintf("connection broken after syntax error: %v", err))
				}

				// --- Unique constraint violation ---
				t = time.Now()
				_, err = conn.Exec(ctx, "INSERT INTO users (name, email, balance) VALUES ($1, $2, $3)",
					"dup_user", "user_1@test.com", 0) // user_1@test.com already exists from seed
				d = time.Since(t)
				mu.Lock()
				latencies = append(latencies, d)
				mu.Unlock()
				ops.Add(1)
				// Not counting as error — may succeed if another worker deleted user_1
				if err != nil && (ctx.Err() != nil || conn.IsClosed()) {
					return
				}

				// --- Verify connection still works after constraint violation ---
				t = time.Now()
				err = conn.QueryRow(ctx, "SELECT 1").Scan(&v)
				d = time.Since(t)
				mu.Lock()
				latencies = append(latencies, d)
				mu.Unlock()
				ops.Add(1)
				if err != nil {
					if ctx.Err() != nil || conn.IsClosed() {
						return
					}
					errCount.Add(1)
					recordNote(fmt.Sprintf("connection broken after constraint violation: %v", err))
				}

				// --- Division by zero ---
				t = time.Now()
				_, err = conn.Exec(ctx, "SELECT 1/0")
				d = time.Since(t)
				mu.Lock()
				latencies = append(latencies, d)
				mu.Unlock()
				ops.Add(1)
				if err == nil {
					errCount.Add(1)
				} else if ctx.Err() != nil || conn.IsClosed() {
					return
				}

				// --- Verify connection still works ---
				t = time.Now()
				err = conn.QueryRow(ctx, "SELECT 42").Scan(&v)
				d = time.Since(t)
				mu.Lock()
				latencies = append(latencies, d)
				mu.Unlock()
				ops.Add(1)
				if err != nil || v != 42 {
					if ctx.Err() != nil || conn.IsClosed() {
						return
					}
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
