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

// Manager defines the interface for a per-node pool manager.
type Manager interface {
	AcquireDbConnection() (types.UpstreamClientInterface, error)
	PoolStats() types.PoolStats
	Close()
}

// NodeSpec describes a backend node for pool construction.
type NodeSpec struct {
	ID         string
	Host       string
	Port       uint16
	Connection config.NodeConnectionConfig
	Pool       config.NodePoolConfig
	DiscardAll bool
}

type PoolManager struct {
	ctx context.Context

	logger     *zap.Logger
	pool       *pgxpool.Pool
	discardAll bool // true only in session mode
}

// New creates a pool manager for a single node from a NodeSpec.
func New(ctx context.Context, spec NodeSpec, log *zap.Logger) (*PoolManager, error) {
	dsn := databaseDSN(spec.Host, int(spec.Port), spec.Connection, spec.Pool)
	poolConfig, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		panic(fmt.Errorf("failed to parse pool config: %w", err))
	}

	poolConfig.MinConns = int32(spec.Pool.MinConns)
	poolConfig.MaxConns = int32(spec.Pool.MaxConns)
	poolConfig.MaxConnLifetime = time.Duration(spec.Pool.MaxConnLifetime) * time.Second
	poolConfig.MaxConnIdleTime = time.Duration(spec.Pool.MaxConnIdleTime) * time.Second
	poolConfig.HealthCheckPeriod = time.Duration(spec.Pool.HealthCheckPeriod) * time.Second

	poolConfig.ConnConfig.Tracer = &tracelog.TraceLog{
		Logger:   pgx_log_adapter.Initialize(log),
		LogLevel: tracelog.LogLevelDebug,
	}

	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		panic(fmt.Errorf("failed to create connection pool: %w", err))
	}

	return &PoolManager{
		ctx:        ctx,
		logger:     log,
		pool:       pool,
		discardAll: spec.DiscardAll,
	}, nil
}

// AcquireDbConnection acquires a connection from the database pool.
func (pm *PoolManager) AcquireDbConnection() (types.UpstreamClientInterface, error) {
	acquireCtx, cancel := context.WithTimeout(pm.ctx, 10*time.Second) // TODO: make this configurable
	defer cancel()

	sessionClient, err := pm.pool.Acquire(acquireCtx)
	if err != nil {
		s := pm.pool.Stat()
		pm.logger.Warn("pool acquire failed",
			zap.Error(err),
			zap.Int32("total_conns", s.TotalConns()),
			zap.Int32("acquired_conns", s.AcquiredConns()),
			zap.Int32("idle_conns", s.IdleConns()),
			zap.Int32("max_conns", s.MaxConns()),
		)
		return nil, err
	}

	if ce := pm.logger.Check(zap.DebugLevel, "pool acquire succeeded"); ce != nil {
		s := pm.pool.Stat()
		ce.Write(
			zap.Int32("total_conns", s.TotalConns()),
			zap.Int32("acquired_conns", s.AcquiredConns()),
			zap.Int32("idle_conns", s.IdleConns()),
			zap.Int32("max_conns", s.MaxConns()),
		)
	}

	return upstream_client.Initialize(sessionClient, pm.discardAll), nil
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

func databaseDSN(host string, port int, connCfg config.NodeConnectionConfig, _ config.NodePoolConfig) string {
	u := &url.URL{
		Scheme: "postgres",
		Host:   fmt.Sprintf("%s:%d", host, port),
		Path:   connCfg.Database,
	}

	if connCfg.User != "" {
		if connCfg.Password != "" {
			u.User = url.UserPassword(connCfg.User, connCfg.Password)
		} else {
			u.User = url.User(connCfg.User)
		}
	}

	q := u.Query()
	if connCfg.ConnectTimeoutSeconds > 0 {
		q.Set("connect_timeout", strconv.Itoa(connCfg.ConnectTimeoutSeconds))
	}

	u.RawQuery = q.Encode()

	return u.String()
}
