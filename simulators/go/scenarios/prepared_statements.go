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
// Unnamed/parameterized queries (pool.QueryRow) work in all pool modes because
// they use the extended query protocol with an unnamed prepared statement that is
// re-parsed on every round-trip.
//
// Named prepared statements (conn.Conn().Prepare) create server-side state that is
// scoped to a specific backend. In transaction pool mode, the PREPARE, all
// subsequent EXECUTEs, and the final DEALLOCATE must run within a single
// BEGIN...COMMIT so that Grunyas keeps the same backend pinned for the entire
// sequence. Running them across separate transactions would release the backend
// after PREPARE, and the next EXECUTE would land on a different backend that has
// no knowledge of the statement.
func PreparedStatements(ctx context.Context, cfg *Config) (*Result, error) {
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

			// --- Unnamed prepared statements (work in all pool modes) ---
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
					if !(cfg.PoolMode == "session" && IsCapacityError(err)) {
						errCount.Add(1)
					}
				}
			}

			// --- Named prepared statements via connection ---
			// Must be wrapped in a transaction so Grunyas pins the backend for
			// the full PREPARE → EXECUTE → DEALLOCATE sequence.
			conn, err := pool.Acquire(ctx)
			if err != nil {
				if !(cfg.PoolMode == "session" && IsCapacityError(err)) {
					errCount.Add(1)
				}
				ops.Add(1)
				return
			}
			defer conn.Release()

			if _, err := conn.Exec(ctx, "BEGIN"); err != nil {
				if !(cfg.PoolMode == "session" && IsCapacityError(err)) {
					errCount.Add(1)
				}
				ops.Add(1)
				return
			}
			rollback := func() { _, _ = conn.Exec(ctx, "ROLLBACK") }

			stmtName := fmt.Sprintf("stmt_worker_%d", workerID)
			t := time.Now()
			_, err = conn.Conn().Prepare(ctx, stmtName, "SELECT id, name, balance FROM users WHERE id = $1")
			d := time.Since(t)
			mu.Lock()
			latencies = append(latencies, d)
			mu.Unlock()
			ops.Add(1)
			if err != nil {
				if !(cfg.PoolMode == "session" && IsCapacityError(err)) {
					errCount.Add(1)
				}
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
					if !(cfg.PoolMode == "session" && IsCapacityError(err)) {
						errCount.Add(1)
					}
					continue
				}
				rows.Close()
			}

			_, err = conn.Exec(ctx, "DEALLOCATE "+stmtName)
			ops.Add(1)
			if err != nil {
				if !(cfg.PoolMode == "session" && IsCapacityError(err)) {
					errCount.Add(1)
				}
			}

			if _, err := conn.Exec(ctx, "COMMIT"); err != nil {
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
	}, nil
}
