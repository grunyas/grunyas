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

	// Balanced worker split: modest stressor count exercises the "long queries
	// don't block quick queries" property; quick workers take a roughly equal
	// share of the concurrency budget to give a stable throughput measurement.
	// Cap each stressor group at 10 to avoid overwhelming postgres with too
	// many concurrent heavy queries.
	stressorsEach := min(cfg.Concurrency/4, 10)
	if stressorsEach < 1 {
		stressorsEach = 1
	}
	quickWorkers := cfg.Concurrency - 2*stressorsEach
	if quickWorkers < 1 {
		quickWorkers = 1
	}

	// pg_sleep workers — hold a connection open doing a single long sleep.
	// Purpose: verify their slow query does not block quick-query workers below.
	// In duration mode they issue one long sleep that spans the whole window; in
	// iteration mode they issue a single 1-second sleep.
	//
	// NOTE: sleep ops are intentionally excluded from the throughput measurement
	// (they run on isolated connections and would otherwise add a small, highly
	// variable op count that dominates trial-to-trial CV).
	sleepDuration := "1"
	if cfg.Duration > 0 {
		// One sleep spanning roughly the measurement window so we exercise a
		// long-running query without recording its completion as throughput.
		sleepDuration = "9"
	}
	for i := 0; i < stressorsEach; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			conn, err := ConnectClient(ctx, cfg)
			if err != nil {
				return
			}
			defer conn.Close(context.Background())

			_, _ = conn.Exec(ctx, "SELECT pg_sleep("+sleepDuration+")")
		}()
	}

	// Large result set workers — stream rows over a persistent connection as
	// background stressors. Their completions are NOT counted in ops: they're
	// large-variance units (scans can take 50–500ms) that would dominate
	// trial-to-trial CV. The scenario tests that they don't block quick-query
	// workers, which is what ops/s measures.
	for i := 0; i < stressorsEach; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			conn, err := ConnectClient(ctx, cfg)
			if err != nil {
				return
			}
			defer conn.Close(context.Background())

			for iter := 0; cfg.Continue(ctx, iter, 1); iter++ {
				rows, err := conn.Query(ctx, "SELECT generate_series(1, 10000)")
				if err != nil {
					if ctx.Err() != nil || conn.IsClosed() {
						return
					}
					continue
				}
				for rows.Next() {
				}
				rows.Close()
			}
		}()
	}

	// Quick query workers — should not be blocked by the long-running queries above.
	for i := 0; i < quickWorkers; i++ {
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
			defer conn.Close(context.Background())

			for iter := 0; cfg.Continue(ctx, iter, 5); iter++ {
				t := time.Now()
				var v int
				err := conn.QueryRow(ctx, "SELECT 1").Scan(&v)
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
