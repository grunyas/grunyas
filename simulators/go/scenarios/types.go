package scenarios

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Config holds the connection and concurrency settings for scenario execution.
type Config struct {
	ConnStr     string
	Concurrency int
	PoolMode    string
	DBHost      string
	DBPort      string
	DBUser      string
	DBPass      string
	DBName      string
}

// Result captures the output of a single scenario run.
type Result struct {
	TotalOps  int
	Errors    int
	Duration  time.Duration
	Latencies []time.Duration
	Notes     []string
}

// NewPool creates a new connection pool using the scenario config.
// In transaction pool mode, we use QueryExecModeCacheDescribe which sends unnamed
// prepared statements, compatible with connection pool proxies like PgBouncer/Grunyas.
func NewPool(ctx context.Context, cfg *Config) (*pgxpool.Pool, error) {
	connStr := fmt.Sprintf("postgres://%s:%s@%s:%s/%s?pool_max_conns=%d",
		cfg.DBUser, cfg.DBPass, cfg.DBHost, cfg.DBPort, cfg.DBName, cfg.Concurrency)
	poolCfg, err := pgxpool.ParseConfig(connStr)
	if err != nil {
		return nil, fmt.Errorf("parse pool config: %w", err)
	}
	if cfg.PoolMode == "transaction" {
		// Named prepared statements don't survive backend switches in transaction
		// pool mode. CacheDescribe uses unnamed statements, which are safe.
		poolCfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeCacheDescribe
	}
	return pgxpool.NewWithConfig(ctx, poolCfg)
}
