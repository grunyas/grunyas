package scenarios

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// LongRunning tests pg_sleep and large result set queries to ensure they don't block other clients.
func LongRunning(ctx context.Context, cfg *Config) (*Result, error) {
	var (
		ops       atomic.Int64
		errCount  atomic.Int64
		mu        sync.Mutex
		latencies []time.Duration
	)
	errs := NewErrorSampler(10)

	start := time.Now()
	var wg sync.WaitGroup

	// Limit workers to avoid too many concurrent long queries.
	workers := min(cfg.Concurrency, 20)

	// pg_sleep workers — each holds its connection open for the full sleep duration.
	for i := 0; i < workers/2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			conn, err := ConnectClient(ctx, cfg)
			if err != nil {
				ops.Add(1)
				errCount.Add(1)
				errs.Record(err)
				return
			}
			defer conn.Close(ctx)

			t := time.Now()
			_, err = conn.Exec(ctx, "SELECT pg_sleep(1)")
			d := time.Since(t)
			mu.Lock()
			latencies = append(latencies, d)
			mu.Unlock()
			ops.Add(1)
			if err != nil {
				errCount.Add(1)
				errs.Record(err)
			}
		}()
	}

	// Large result set workers — stream 10k rows over a persistent connection.
	for i := 0; i < workers/2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			conn, err := ConnectClient(ctx, cfg)
			if err != nil {
				ops.Add(1)
				errCount.Add(1)
				errs.Record(err)
				return
			}
			defer conn.Close(ctx)

			t := time.Now()
			rows, err := conn.Query(ctx, "SELECT generate_series(1, 10000)")
			if err != nil {
				errCount.Add(1)
				errs.Record(err)
				ops.Add(1)
				return
			}
			count := 0
			for rows.Next() {
				count++
			}
			rows.Close()
			d := time.Since(t)
			mu.Lock()
			latencies = append(latencies, d)
			mu.Unlock()
			ops.Add(1)
			if err := rows.Err(); err != nil {
				errCount.Add(1)
				errs.Record(err)
			}
		}()
	}

	// Quick query workers — should not be blocked by the long-running queries above.
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			conn, err := ConnectClient(ctx, cfg)
			if err != nil {
				ops.Add(1)
				errCount.Add(1)
				errs.Record(err)
				return
			}
			defer conn.Close(ctx)

			for j := 0; j < 5; j++ {
				t := time.Now()
				var v int
				err := conn.QueryRow(ctx, "SELECT 1").Scan(&v)
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
		}()
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
