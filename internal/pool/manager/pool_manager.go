package manager

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/tracelog"
	"github.com/grunyas/grunyas/config"
	"github.com/grunyas/grunyas/internal/pool/upstream_client"
	"github.com/grunyas/grunyas/internal/server/types"
	"github.com/grunyas/grunyas/internal/utils/pgx_log_adapter"
	"go.uber.org/zap"
)

type PoolManager struct {
	ctx context.Context

	logger *zap.Logger
	pool   *pgxpool.Pool
}

func Initialize(prx types.ProxyInterface) *PoolManager {
	ctx := prx.GetContext()
	cfg := prx.GetConfig().BackendConfig
	logger := prx.GetLogger()

	// Initialize connection pool
	poolConfig, err := pgxpool.ParseConfig(DatabaseDSN(cfg))
	if err != nil {
		panic(fmt.Errorf("failed to parse pool config: %w", err))
	}

	// Configure pool settings
	poolConfig.MinConns = int32(cfg.PoolMinConns)
	poolConfig.MaxConns = int32(cfg.PoolMaxConns)
	poolConfig.MaxConnLifetime = time.Duration(cfg.PoolMaxConnLifetime) * time.Second
	poolConfig.MaxConnIdleTime = time.Duration(cfg.PoolMaxConnIdleTime) * time.Second
	poolConfig.HealthCheckPeriod = time.Duration(cfg.PoolHealthCheckPeriod) * time.Second

	// Configure logging for background connection events
	poolConfig.ConnConfig.Tracer = &tracelog.TraceLog{
		Logger:   pgx_log_adapter.Initialize(logger),
		LogLevel: tracelog.LogLevelDebug, // Debug to see connection lifecycle events
	}

	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		panic(fmt.Errorf("failed to create connection pool: %w", err))
	}

	return &PoolManager{
		ctx:    ctx,
		logger: logger,
		pool:   pool,
	}
}

// AcquireDbConnection acquires a connection from the database pool.
func (pm *PoolManager) AcquireDbConnection() (types.UpstreamClientInterface, error) {
	acquireCtx, cancel := context.WithTimeout(pm.ctx, 10*time.Second) // TODO: make this configurable
	defer cancel()

	sessionClient, err := pm.pool.Acquire(acquireCtx)
	if err != nil {
		return nil, err
	}

	return upstream_client.Initialize(sessionClient), nil
}

// PoolStats returns the current statistics of the database connection pool.
func (pm *PoolManager) PoolStats() types.PoolStats {
	s := pm.pool.Stat()
	return types.PoolStats{
		TotalConns:    s.TotalConns(),
		AcquiredConns: s.AcquiredConns(),
		IdleConns:     s.IdleConns(),
		MaxConns:      s.MaxConns(),
	}
}

func (pm *PoolManager) Close() {
	pm.pool.Close()
}

func DatabaseDSN(cfg config.DatabasePoolConfig) string {
	u := &url.URL{
		Scheme: "postgres",
		Host:   fmt.Sprintf("%s:%d", cfg.DatabaseHost, cfg.DatabasePort),
		Path:   cfg.DatabaseName,
	}

	if cfg.DatabaseUser != "" {
		if cfg.DatabasePassword != "" {
			u.User = url.UserPassword(cfg.DatabaseUser, cfg.DatabasePassword)
		} else {
			u.User = url.User(cfg.DatabaseUser)
		}
	}

	q := u.Query()
	if cfg.DatabaseConnectTimeoutSeconds > 0 {
		q.Set("connect_timeout", strconv.Itoa(cfg.DatabaseConnectTimeoutSeconds))
	}

	u.RawQuery = q.Encode()

	return u.String()
}
