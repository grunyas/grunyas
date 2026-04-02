package scenarios

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
)

// PreparedStatements tests named and unnamed prepared statements, including reuse.
// In transaction pool mode, named statements may fail across transaction boundaries.
func PreparedStatements(ctx context.Context, cfg *Config) (*Result, error) {
	pool, err := NewPool(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}
	defer pool.Close()

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

	for i := 0; i < cfg.Concurrency; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()

			// --- Unnamed prepared statements (should work in all pool modes) ---
			for iter := 0; iter < 10; iter++ {
				t := time.Now()
				var count int
				err := pool.QueryRow(ctx, "SELECT count(*) FROM users WHERE balance > $1",
					float64(iter*100)).Scan(&count)
				d := time.Since(t)
				mu.Lock()
				latencies = append(latencies, d)
				mu.Unlock()
				ops.Add(1)
				if err != nil {
					errCount.Add(1)
				}
			}

			// --- Named prepared statements via connection ---
			conn, err := pool.Acquire(ctx)
			if err != nil {
				errCount.Add(1)
				ops.Add(1)
				return
			}

			stmtName := fmt.Sprintf("stmt_worker_%d", workerID)
			t := time.Now()
			_, err = conn.Conn().Prepare(ctx, stmtName, "SELECT id, name, balance FROM users WHERE id = $1")
			d := time.Since(t)
			mu.Lock()
			latencies = append(latencies, d)
			mu.Unlock()
			ops.Add(1)
			if err != nil {
				errCount.Add(1)
				notesMu.Lock()
				if len(notes) == 0 {
					notes = append(notes, fmt.Sprintf("named prepare failed: %v", err))
				}
				notesMu.Unlock()
				conn.Release()
				return
			}

			// Reuse the named statement multiple times
			for iter := 0; iter < 5; iter++ {
				t = time.Now()
				rows, err := conn.Query(ctx, stmtName, pgx.NamedArgs{"": workerID*5 + iter + 1})
				if err != nil {
					// Try positional args instead
					rows, err = conn.Query(ctx, stmtName, workerID*5+iter+1)
				}
				if err != nil {
					errCount.Add(1)
					ops.Add(1)
					continue
				}
				rows.Close()
				d = time.Since(t)
				mu.Lock()
				latencies = append(latencies, d)
				mu.Unlock()
				ops.Add(1)
			}

			// Deallocate
			_, err = conn.Exec(ctx, "DEALLOCATE "+stmtName)
			ops.Add(1)
			if err != nil {
				errCount.Add(1)
			}

			conn.Release()
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
