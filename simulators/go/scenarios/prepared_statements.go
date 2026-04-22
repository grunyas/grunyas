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
			defer conn.Close(context.Background())

			for outerIter := 0; cfg.Continue(ctx, outerIter, 1); outerIter++ {
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
						if ctx.Err() != nil || conn.IsClosed() {
							return
						}
						errCount.Add(1)
						errs.Record(err)
					}
				}

				// --- Named prepared statements ---
				// Wrapped in a transaction so the proxy pins the backend for the full
				// PREPARE → EXECUTE × 5 → DEALLOCATE sequence (required in transaction mode).
				// outerIter is included in stmtName to avoid conflicts across loop iterations.
				if _, err := conn.Exec(ctx, "BEGIN"); err != nil {
					if ctx.Err() != nil || conn.IsClosed() {
						return
					}
					errCount.Add(1)
					errs.Record(err)
					ops.Add(1)
					return
				}
				rollback := func() { _, _ = conn.Exec(context.Background(), "ROLLBACK") }

				stmtName := fmt.Sprintf("stmt_worker_%d_%d", workerID, outerIter)
				t := time.Now()
				_, err = conn.Prepare(ctx, stmtName, "SELECT id, name, balance FROM users WHERE id = $1")
				d := time.Since(t)
				mu.Lock()
				latencies = append(latencies, d)
				mu.Unlock()
				ops.Add(1)
				if err != nil {
					if ctx.Err() != nil || conn.IsClosed() {
						rollback()
						return
					}
					errCount.Add(1)
					errs.Record(err)
					rollback()
					return
				}

				// Reuse the named statement many times — all on the same pinned backend.
				// Larger reuse count shifts the measurement toward EXECUTE throughput (the
				// actual purpose of prepared statements), making it less sensitive to the
				// one-time PREPARE cost and to backend plan-cache state transitions that
				// otherwise produced bimodal trial results.
				const reuseCount = 20
				for iter := 0; iter < reuseCount; iter++ {
					t = time.Now()
					rows, err := conn.Query(ctx, stmtName, workerID*reuseCount+iter+1)
					d = time.Since(t)
					mu.Lock()
					latencies = append(latencies, d)
					mu.Unlock()
					ops.Add(1)
					if err != nil {
						if ctx.Err() != nil || conn.IsClosed() {
							rollback()
							return
						}
						errCount.Add(1)
						errs.Record(err)
						continue
					}
					rows.Close()
				}

				_, err = conn.Exec(ctx, "DEALLOCATE "+stmtName)
				ops.Add(1)
				if err != nil {
					if ctx.Err() != nil || conn.IsClosed() {
						return
					}
					errCount.Add(1)
					errs.Record(err)
				}

				if _, err := conn.Exec(ctx, "COMMIT"); err != nil {
					if ctx.Err() != nil || conn.IsClosed() {
						return
					}
					errCount.Add(1)
					errs.Record(err)
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
		Notes:     errs.Notes(),
	}, nil
}
