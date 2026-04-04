package scenarios

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// BatchOperations tests bulk INSERT operations and multi-row queries.
func BatchOperations(ctx context.Context, cfg *Config) (*Result, error) {
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

			// --- Bulk INSERT using a transaction ---
			t := time.Now()
			tx, err := pool.Begin(ctx)
			if err != nil {
				if !(cfg.PoolMode == "session" && IsCapacityError(err)) {
					errCount.Add(1)
				}
				ops.Add(1)
				return
			}

			batchSize := 100
			for j := 0; j < batchSize; j++ {
				_, err = tx.Exec(ctx,
					"INSERT INTO events (type, payload) VALUES ($1, $2)",
					fmt.Sprintf("batch_event_%d", workerID),
					fmt.Sprintf(`{"worker":%d,"iter":%d}`, workerID, j))
				if err != nil {
					errCount.Add(1)
					break
				}
			}

			if err != nil {
				_ = tx.Rollback(ctx)
			} else {
				err = tx.Commit(ctx)
			}
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

			// --- Multi-row INSERT using VALUES ---
			t = time.Now()
			_, err = pool.Exec(ctx,
				`INSERT INTO events (type, payload) VALUES
				 ('multi_1', '{"source":"batch"}'),
				 ('multi_2', '{"source":"batch"}'),
				 ('multi_3', '{"source":"batch"}'),
				 ('multi_4', '{"source":"batch"}'),
				 ('multi_5', '{"source":"batch"}')`)
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

			// --- Bulk read ---
			t = time.Now()
			rows, err := pool.Query(ctx,
				"SELECT id, type, payload FROM events WHERE type = $1 LIMIT 100",
				fmt.Sprintf("batch_event_%d", workerID))
			if err != nil {
				if !(cfg.PoolMode == "session" && IsCapacityError(err)) {
					errCount.Add(1)
				}
				ops.Add(1)
				return
			}
			count := 0
			for rows.Next() {
				count++
			}
			rows.Close()
			d = time.Since(t)
			mu.Lock()
			latencies = append(latencies, d)
			mu.Unlock()
			ops.Add(1)
			if rows.Err() != nil {
				if !(cfg.PoolMode == "session" && IsCapacityError(rows.Err())) {
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
