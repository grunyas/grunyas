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

// Manager is the per-node connection pool interface.
type Manager interface {
	Acquire(ctx context.Context) (types.UpstreamClientInterface, error)
	PoolStats() types.PoolStats
	Close()
}

// NodeSpec holds the configuration needed to build a pool for one node.
type NodeSpec struct {
	ID         string
	Host       string
	Port       uint16
	Connection config.NodeConnectionConfig
	Pool       config.NodePoolConfig
	// DiscardAll is true in session mode (connections need DISCARD ALL
	// before returning to the pool). In M1 this is always true since
	// only the write port (session mode) is wired.
	DiscardAll bool
}

// poolManager is the concrete per-node pool. It wraps a pgxpool.Pool.
type poolManager struct {
	ctx        context.Context
	logger     *zap.Logger
	pool       *pgxpool.Pool
	discardAll bool
}

// New constructs a Manager for one node, with its connection and pool config.
func New(ctx context.Context, spec NodeSpec, log *zap.Logger) (Manager, error) {
	poolConfig, err := pgxpool.ParseConfig(NodeDSN(spec))
	if err != nil {
		return nil, fmt.Errorf("failed to parse pool config: %w", err)
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
		return nil, fmt.Errorf("failed to create connection pool: %w", err)
	}

	return &poolManager{
		ctx:        ctx,
		logger:     log.With(zap.String("pool_node", spec.ID)),
		pool:       pool,
		discardAll: spec.DiscardAll,
	}, nil
}

func (pm *poolManager) Acquire(ctx context.Context) (types.UpstreamClientInterface, error) {
	acquireCtx, cancel := context.WithTimeout(pm.ctx, 10*time.Second)
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

func (pm *poolManager) PoolStats() types.PoolStats {
	s := pm.pool.Stat()
	return types.PoolStats{
		TotalConns:    s.TotalConns(),
		AcquiredConns: s.AcquiredConns(),
		IdleConns:     s.IdleConns(),
		MaxConns:      s.MaxConns(),
	}
}

func (pm *poolManager) Close() {
	pm.pool.Close()
}

// NodeDSN builds a postgres:// DSN from a NodeSpec. The spec's connection
// fields supply the user, password, host, port, database, and connect_timeout.
func NodeDSN(spec NodeSpec) string {
	u := &url.URL{
		Scheme: "postgres",
		Host:   fmt.Sprintf("%s:%d", spec.Host, spec.Port),
		Path:   spec.Connection.Database,
	}

	if spec.Connection.User != "" {
		if spec.Connection.Password != "" {
			u.User = url.UserPassword(spec.Connection.User, spec.Connection.Password)
		} else {
			u.User = url.User(spec.Connection.User)
		}
	}

	q := u.Query()
	if spec.Connection.ConnectTimeoutSeconds > 0 {
		q.Set("connect_timeout", strconv.Itoa(spec.Connection.ConnectTimeoutSeconds))
	}
	u.RawQuery = q.Encode()

	return u.String()
}


