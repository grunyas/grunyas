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
			defer conn.Close(ctx)

			rng := rand.New(rand.NewSource(int64(workerID)))

			for iter := 0; iter < 20; iter++ {
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
						errCount.Add(1)
						errs.Record(err)
					}
				} else {
					// 30% writes — transfer balance between two users
					otherID := rng.Intn(1000) + 1
					amount := rng.Float64() * 10

					t := time.Now()
					tx, err := conn.Begin(ctx)
					if err != nil {
						errCount.Add(1)
						errs.Record(err)
						ops.Add(1)
						continue
					}

					_, err = tx.Exec(ctx, "UPDATE users SET balance = balance - $1 WHERE id = $2", amount, userID)
					if err != nil {
						_ = tx.Rollback(ctx)
						errCount.Add(1)
						errs.Record(err)
						ops.Add(1)
						continue
					}

					_, err = tx.Exec(ctx, "UPDATE users SET balance = balance + $1 WHERE id = $2", amount, otherID)
					if err != nil {
						_ = tx.Rollback(ctx)
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
