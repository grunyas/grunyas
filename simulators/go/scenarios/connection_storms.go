package scenarios

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// ConnectionStorms simulates rapid connect/disconnect cycles — the worst case for a connection pooler.
// Each goroutine opens a fresh connection, runs one query, and closes immediately.
// The storm intentionally spawns 2× the configured concurrency to exceed the backend pool size,
// so capacity rejections (SQLSTATE 53300) are expected and filtered out.
//
// In duration mode each goroutine keeps storming (connect/query/close) until the deadline expires.
// In iteration mode each goroutine performs a single storm cycle (original behaviour).
func ConnectionStorms(ctx context.Context, cfg *Config) (*Result, error) {
	var (
		ops       atomic.Int64
		errCount  atomic.Int64
		mu        sync.Mutex
		latencies []time.Duration
	)
	errs := NewErrorSampler(10)

	start := time.Now()
	var wg sync.WaitGroup

	storms := cfg.Concurrency * 2
	for i := 0; i < storms; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			for iter := 0; cfg.Continue(ctx, iter, 1); iter++ {
				t := time.Now()
				conn, err := ConnectClient(ctx, cfg)
				if err != nil {
					// Capacity rejections are expected when exceeding the backend pool size.
					if !IsCapacityError(err) {
						if ctx.Err() != nil {
							return
						}
						errCount.Add(1)
						errs.Record(err)
					}
					ops.Add(1)
					continue
				}

				// conn.Exec with no parameters uses the simple protocol (single Query message),
				// bypassing the extended query protocol entirely — correct in both pool modes.
				_, err = conn.Exec(ctx, "SELECT 1")
				ops.Add(1)
				if err != nil {
					if !IsCapacityError(err) {
						if ctx.Err() == nil && !conn.IsClosed() {
							errCount.Add(1)
							errs.Record(err)
						}
					}
				}

				_ = conn.Close(context.Background())
				d := time.Since(t)
				mu.Lock()
				latencies = append(latencies, d)
				mu.Unlock()
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
