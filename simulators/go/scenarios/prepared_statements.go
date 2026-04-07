package scenarios

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// PreparedStatements tests named and unnamed prepared statements, including reuse.
//
// Unnamed/parameterized queries (conn.QueryRow) work in all pool modes because they
// use the extended query protocol with an unnamed prepared statement that is re-parsed
// on every round-trip.
//
// Named prepared statements (conn.Prepare) create server-side state scoped to a specific
// backend. In transaction pool mode, the PREPARE, all subsequent EXECUTEs, and the final
// DEALLOCATE must run within a single BEGIN...COMMIT so that the proxy keeps the same
// backend pinned for the entire sequence. Outside a transaction the backend is released
// after each ReadyForQuery, and the next statement would land on a different backend that
// has no knowledge of the prepared statement.
func PreparedStatements(ctx context.Context, cfg *Config) (*Result, error) {
	var (
		ops       atomic.Int64
		errCount  atomic.Int64
		mu        sync.Mutex
		latencies []time.Duration
	)
	errs := NewErrorSampler(10)

	start := time.Now()
	var wg sync.WaitGroup

	for i := 0; i < cfg.Concurrency; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()

			conn, err := ConnectClient(ctx, cfg)
			if err != nil {
				ops.Add(1)
				errCount.Add(1)
				errs.Record(err)
				return
			}
			defer conn.Close(ctx)

			// --- Unnamed prepared statements (work in all pool modes) ---
			for iter := 0; iter < 10; iter++ {
				t := time.Now()
				var count int
				err := conn.QueryRow(ctx, "SELECT count(*) FROM users WHERE balance > $1",
					float64(iter*100)).Scan(&count)
				d := time.Since(t)
				mu.Lock()
				latencies = append(latencies, d)
				mu.Unlock()
				ops.Add(1)
				if err != nil {
					errCount.Add(1)
					errs.Record(err)
				}
			}

			// --- Named prepared statements ---
			// Wrapped in a transaction so the proxy pins the backend for the full
			// PREPARE → EXECUTE × 5 → DEALLOCATE sequence (required in transaction mode).
			if _, err := conn.Exec(ctx, "BEGIN"); err != nil {
				errCount.Add(1)
				errs.Record(err)
				ops.Add(1)
				return
			}
			rollback := func() { _, _ = conn.Exec(ctx, "ROLLBACK") }

			stmtName := fmt.Sprintf("stmt_worker_%d", workerID)
			t := time.Now()
			_, err = conn.Prepare(ctx, stmtName, "SELECT id, name, balance FROM users WHERE id = $1")
			d := time.Since(t)
			mu.Lock()
			latencies = append(latencies, d)
			mu.Unlock()
			ops.Add(1)
			if err != nil {
				errCount.Add(1)
				errs.Record(err)
				rollback()
				return
			}

			// Reuse the named statement — all on the same pinned backend.
			for iter := 0; iter < 5; iter++ {
				t = time.Now()
				rows, err := conn.Query(ctx, stmtName, workerID*5+iter+1)
				d = time.Since(t)
				mu.Lock()
				latencies = append(latencies, d)
				mu.Unlock()
				ops.Add(1)
				if err != nil {
					errCount.Add(1)
					errs.Record(err)
					continue
				}
				rows.Close()
			}

			_, err = conn.Exec(ctx, "DEALLOCATE "+stmtName)
			ops.Add(1)
			if err != nil {
				errCount.Add(1)
				errs.Record(err)
			}

			if _, err := conn.Exec(ctx, "COMMIT"); err != nil {
				errCount.Add(1)
				errs.Record(err)
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
		Notes:     errs.Notes(),
	}, nil
}
