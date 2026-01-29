// Package proxy implements the main server logic for the PostgreSQL proxy.
// It handles accepting client connections, managing the connection pool to the backend,
// and orchestrating the lifecycle of client sessions.
package proxy

import (
	"context"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"
	"go.uber.org/zap"

	"crypto/tls"
	"strings"

	"github.com/grunyas/grunyas/config"
	"github.com/grunyas/grunyas/internal/auth"
	pool "github.com/grunyas/grunyas/internal/pool/manager"
	"github.com/grunyas/grunyas/internal/server/downstream_client"
	"github.com/grunyas/grunyas/internal/server/session"
	"github.com/grunyas/grunyas/internal/server/types"
)

// Proxy represents the main server instance.
// It tracks active sessions, manages the upstream connection pool,
// and handles the graceful shutdown of the service.
type Proxy struct {
	mu sync.Mutex

	ctx context.Context

	cfg    *config.Config
	logger *zap.Logger
	auth   *auth.Authenticator

	ln       net.Listener
	sessions map[*session.Session]struct{}
	poolMgr  types.PoolManagerInterface

	idle  *idleSweeper
	ready chan struct{}

	tlsConfig *tls.Config

	currentConnectionsCount  atomic.Int64
	lifetimeConnectionsCount atomic.Int64
}

// Initialize creates a new Proxy instance with the provided context, configuration, and logger.
// It initializes the backend connection pool and prepares the authentication mechanisms.
// It panics if config or logger are nil, or if initialization of sub-components fails.
func Initialize(ctx context.Context, cfg *config.Config, logger *zap.Logger) *Proxy {
	if cfg == nil {
		panic("config cannot be nil")
	}

	if ctx == nil {
		ctx = context.Background()
	}

	if logger == nil {
		panic("logger cannot be nil")
	}

	authn, err := auth.Initialize(cfg.Auth, logger)
	if err != nil {
		panic(fmt.Errorf("failed to initialize auth: %w", err))
	}

	idleTimeout := time.Duration(cfg.ServerConfig.ClientIdleTimeout) * time.Second

	var tlsConfig *tls.Config
	sslMode := strings.ToLower(cfg.ServerConfig.SSLMode)
	if sslMode == "optional" || sslMode == "mandatory" {
		cert, err := tls.LoadX509KeyPair(cfg.ServerConfig.SSLCert, cfg.ServerConfig.SSLKey)
		if err != nil {
			panic(fmt.Errorf("failed to load key pair: %w", err))
		}
		tlsConfig = &tls.Config{Certificates: []tls.Certificate{cert}}
	}

	return &Proxy{
		cfg:       cfg,
		ctx:       ctx,
		logger:    logger,
		auth:      authn,
		sessions:  make(map[*session.Session]struct{}),
		idle:      newIdleSweeper(idleTimeout),
		ready:     make(chan struct{}),
		tlsConfig: tlsConfig,
	}
}

// Run initializes the database connection pool and starts the TCP listener.
// It runs until the context is canceled or a fatal error occurs during listener startup.
// It also starts the idle connection sweeper background task.
func (prx *Proxy) Run() error {
	prx.poolMgr = pool.Initialize(prx)
	defer func() {
		prx.logger.Info("closing connection pool")
		prx.poolMgr.Close()
	}()

	ln, err := net.Listen("tcp", prx.cfg.ServerConfig.ListenAddr)
	if err != nil {
		return err
	}

	prx.ln = ln
	go func() {
		<-prx.ctx.Done()

		prx.logger.Info("shutting down proxy listener")

		if err := ln.Close(); err != nil {
			prx.logger.Warn("failed to close listener", zap.Error(err))
		}
	}()

	close(prx.ready)
	prx.logger.Info("proxy listening", zap.String("addr", ln.Addr().String()))

	go prx.idleSweeper()

	for {
		clientConn, err := ln.Accept()
		prx.logger.Debug("new client connection")

		if err != nil {
			if prx.ctx.Err() != nil { // context is closed
				return nil
			}

			prx.logger.Warn("client connection error", zap.Error(err))

			continue
		}

		go prx.handleNewIncomingConnection(clientConn)
	}
}

// GetContext returns the base context of the proxy.
func (prx *Proxy) GetContext() context.Context {
	return prx.ctx
}

// GetLogger returns the logger instance used by the proxy.
func (prx *Proxy) GetLogger() *zap.Logger {
	return prx.logger
}

// GetConfig returns the configuration used by the proxy.
func (prx *Proxy) GetConfig() *config.Config {
	return prx.cfg
}

// PoolStats returns the current statistics of the database connection pool.
func (prx *Proxy) PoolStats() types.PoolStats {
	if prx.poolMgr == nil {
		return types.PoolStats{}
	}
	return prx.poolMgr.PoolStats()
}

// Authenticate validates the provided user credentials and returns an upstream connection if successful.
func (prx *Proxy) AuthenticateUser(user, password string) error {
	if err := prx.auth.AuthenticateUser(user, password); err != nil {
		return &types.ProxyError{Code: "28P01", Message: err.Error()}
	}
	return nil
}

// AcquireUpstream acquires a connection from the database pool.
func (prx *Proxy) AcquireUpstream() (types.UpstreamClientInterface, error) {
	if prx.poolMgr == nil {
		return nil, fmt.Errorf("connection pool not initialized")
	}
	return prx.poolMgr.AcquireDbConnection()
}

// Ready returns a channel that is closed when the proxy is successfully listening
// and ready to accept connections.
func (prx *Proxy) Ready() <-chan struct{} {
	return prx.ready
}

func (prx *Proxy) handleNewIncomingConnection(conn net.Conn) {
	requiredSSL := strings.ToLower(prx.cfg.ServerConfig.SSLMode) == "mandatory"
	downstream := downstream_client.Initialize(conn, prx.tlsConfig, requiredSSL, prx.logger)

	// Configure keep-alives
	if tc, ok := conn.(*net.TCPConn); ok {
		if err := tc.SetKeepAliveConfig(net.KeepAliveConfig{
			Enable:   true,
			Idle:     time.Duration(prx.cfg.ServerConfig.KeepAliveTimeout) * time.Second,
			Interval: time.Duration(prx.cfg.ServerConfig.KeepAliveInterval) * time.Second,
			Count:    prx.cfg.ServerConfig.KeepAliveCount,
		}); err != nil {
			prx.logger.Warn("failed to set keepalive config", zap.Error(err))
		}
	} else {
		prx.logger.Warn("unexpected connection type", zap.String("remote", downstream.RemoteAddr().String()), zap.String("type", fmt.Sprintf("%T", conn)))

		downstream.Close() //nolint:errcheck

		return
	}

	if !prx.canAcceptNewConnection() {
		if err := downstream.Send(&pgproto3.ErrorResponse{
			Severity: "FATAL",
			Code:     "53300", // too_many_connections
			Message:  "connection pool exhausted, please try again later",
		}); err != nil {
			prx.logger.Warn("failed to send error response", zap.Error(err))
		}

		downstream.Close() //nolint:errcheck
		return
	}

	prx.logger.Debug("initializing new session", zap.String("remote", downstream.RemoteAddr().String()))
	sess := session.Initialize(prx, downstream)

	prx.idle.Track(sess)

	prx.mu.Lock()
	prx.sessions[sess] = struct{}{}
	prx.mu.Unlock()

	prx.lifetimeConnectionsCount.Add(1)

	go func() {
		defer func() {
			prx.mu.Lock()
			delete(prx.sessions, sess)
			prx.mu.Unlock()

			prx.idle.Untrack(sess)
			prx.subtractCurrentSessionsCount()
		}()

		sess.Run()
	}()
}

func (prx *Proxy) idleSweeper() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-prx.ctx.Done():
			return
		case <-ticker.C:
			for _, sess := range prx.idle.Expire() {
				go func(s types.Expirable) {
					prx.logger.Info("idle timeout, closing session", zap.Uint64("session_id", s.ID()))
					if err := s.CloseWithError("FATAL", "57P01", "terminating connection due to idle timeout"); err != nil {
						prx.logger.Warn("failed to close session with error", zap.Uint64("session_id", s.ID()), zap.Error(err))
					}
				}(sess)
			}
		}
	}
}

func (prx *Proxy) canAcceptNewConnection() bool {
	if prx.cfg.ServerConfig.MaxSessions <= 0 {
		return true
	}

	for {
		cur := prx.currentConnectionsCount.Load()

		if int(cur) >= prx.cfg.ServerConfig.MaxSessions {
			return false
		}

		if prx.currentConnectionsCount.CompareAndSwap(cur, cur+1) {
			return true
		}
	}
}

func (prx *Proxy) subtractCurrentSessionsCount() {
	prx.currentConnectionsCount.Add(-1)
}
