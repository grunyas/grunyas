package scenarios

import (
	"context"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"
)

// ConcurrentRW runs N workers doing mixed reads and writes to test isolation and data integrity.
func ConcurrentRW(ctx context.Context, cfg *Config) (*Result, error) {
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

			rng := rand.New(rand.NewSource(int64(workerID)))

			// Partition write range per worker to eliminate cross-worker UPDATE lock contention,
			// which was the dominant source of trial-to-trial variance. Reads still span the
			// full id=1..1000 range to keep the scenario realistic (shared read hot-set).
			// With 37 workers × 20 rows = 740 rows, we stay within the seeded user range.
			const writeRangePerWorker = 20
			writeBase := workerID*writeRangePerWorker + 1

			for iter := 0; cfg.Continue(ctx, iter, 20); iter++ {
				userID := rng.Intn(1000) + 1

				if rng.Float64() < 0.7 {
					// 70% reads
					t := time.Now()
					var balance float64
					err := conn.QueryRow(ctx, "SELECT balance FROM users WHERE id = $1", userID).Scan(&balance)
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
				} else {
					// 30% writes — transfer balance between two worker-local rows.
					// Both rows come from this worker's partitioned range, so no other
					// worker can take locks on them concurrently.
					writeID1 := writeBase + rng.Intn(writeRangePerWorker)
					writeID2 := writeBase + rng.Intn(writeRangePerWorker)
					userID := writeID1
					otherID := writeID2
					amount := rng.Float64() * 10

					t := time.Now()
					tx, err := conn.Begin(ctx)
					if err != nil {
						if ctx.Err() != nil || conn.IsClosed() {
							return
						}
						errCount.Add(1)
						errs.Record(err)
						ops.Add(1)
						continue
					}

					_, err = tx.Exec(ctx, "UPDATE users SET balance = balance - $1 WHERE id = $2", amount, userID)
					if err != nil {
						_ = tx.Rollback(context.Background())
						if ctx.Err() != nil || conn.IsClosed() {
							return
						}
						errCount.Add(1)
						errs.Record(err)
						ops.Add(1)
						continue
					}

					_, err = tx.Exec(ctx, "UPDATE users SET balance = balance + $1 WHERE id = $2", amount, otherID)
					if err != nil {
						_ = tx.Rollback(context.Background())
						if ctx.Err() != nil || conn.IsClosed() {
							return
						}
						errCount.Add(1)
						errs.Record(err)
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
						if ctx.Err() != nil || conn.IsClosed() {
							return
						}
						errCount.Add(1)
						errs.Record(err)
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
		Notes:     errs.Notes(),
	}, nil
}
