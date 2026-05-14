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

	"crypto/tls"
	"strings"

	"github.com/jackc/pgx/v5/pgproto3"
	"go.uber.org/zap"

	"github.com/grunyas/grunyas/config"
	"github.com/grunyas/grunyas/internal/auth"
	"github.com/grunyas/grunyas/internal/classifier"
	"github.com/grunyas/grunyas/internal/decisions"
	"github.com/grunyas/grunyas/internal/routing"
	"github.com/grunyas/grunyas/internal/server/downstream_client"
	"github.com/grunyas/grunyas/internal/server/session"
	"github.com/grunyas/grunyas/internal/server/types"
	"github.com/grunyas/grunyas/internal/topology"
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
	topo     *topology.Topology

	idle  *idleSweeper
	ready chan struct{}

	tlsConfig *tls.Config

	currentConnectionsCount  atomic.Int64
	lifetimeConnectionsCount atomic.Int64

	sessionsWg  sync.WaitGroup
	closeLnOnce sync.Once

	port            string
	routingPipeline *routing.Pipeline
	decisionsBus    *decisions.Bus
}

// Initialize creates a new Proxy instance with the provided context, configuration,
// logger, topology, and routing pipeline. The portID determines which listen port
// this proxy serves ("write", "read", or "compat").
func Initialize(ctx context.Context, cfg *config.Config, logger *zap.Logger, topo *topology.Topology, routingPipeline *routing.Pipeline, portID string) (*Proxy, error) {
	if cfg == nil {
		return nil, fmt.Errorf("config cannot be nil")
	}

	if ctx == nil {
		ctx = context.Background()
	}

	if logger == nil {
		return nil, fmt.Errorf("logger cannot be nil")
	}

	authn, err := auth.Initialize(cfg.Auth, logger)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize auth: %w", err)
	}

	idleTimeout := time.Duration(cfg.ServerConfig.ClientIdleTimeout) * time.Second

	var tlsConfig *tls.Config
	portCfg, portExists := cfg.ServerConfig.Ports[portID]
	if portExists {
		sslMode := strings.ToLower(portCfg.SSLMode)
		if sslMode == "optional" || sslMode == "mandatory" {
			cert, err := tls.LoadX509KeyPair(portCfg.SSLCert, portCfg.SSLKey)
			if err != nil {
				return nil, fmt.Errorf("failed to load key pair for port %s: %w", portID, err)
			}
			tlsConfig = &tls.Config{Certificates: []tls.Certificate{cert}}
		}
	}

	var bus *decisions.Bus
	if routingPipeline != nil {
		bus = routingPipeline.Bus()
	}

	return &Proxy{
		cfg:             cfg,
		ctx:             ctx,
		logger:          logger,
		auth:            authn,
		topo:            topo,
		sessions:        make(map[*session.Session]struct{}),
		idle:            newIdleSweeper(idleTimeout),
		ready:           make(chan struct{}),
		tlsConfig:       tlsConfig,
		port:            portID,
		routingPipeline: routingPipeline,
		decisionsBus:    bus,
	}, nil
}

// Port returns the listen port identifier this proxy serves on.
func (prx *Proxy) Port() string {
	return prx.port
}

// Run starts the TCP listener and serves client connections.
// It runs until the context is canceled or a fatal error occurs during listener startup.
// It also starts the idle connection sweeper background task.
func (prx *Proxy) Run() error {
	listenAddr, err := prx.listenAddr()
	if err != nil {
		return err
	}

	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return err
	}

	prx.ln = ln
	go func() {
		<-prx.ctx.Done()

		prx.logger.Info("shutting down proxy listener", zap.String("port", prx.port))

		prx.closeLnOnce.Do(func() {
			if err := prx.ln.Close(); err != nil {
				prx.logger.Warn("failed to close listener", zap.Error(err))
			}
		})
	}()

	close(prx.ready)
	prx.logger.Info("proxy listening", zap.String("port", prx.port), zap.String("addr", ln.Addr().String()))

	go prx.idleSweeper()

	for {
		clientConn, err := ln.Accept()
		prx.logger.Debug("new client connection", zap.String("port", prx.port))

		if err != nil {
			if prx.ctx.Err() != nil {
				return nil
			}

			prx.logger.Warn("client connection error", zap.Error(err))

			continue
		}

		prx.sessionsWg.Add(1)
		go func() {
			defer prx.sessionsWg.Done()
			prx.handleNewIncomingConnection(clientConn)
		}()
	}
}

func (prx *Proxy) listenAddr() (string, error) {
	portCfg, ok := prx.cfg.ServerConfig.Ports[prx.port]
	if !ok {
		return "", fmt.Errorf("no config for port %q", prx.port)
	}
	return portCfg.ListenAddr, nil
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

// PoolStats returns the current statistics of the primary's database connection pool.
// Returns zero-value PoolStats if no primary is currently observed.
func (prx *Proxy) PoolStats() types.PoolStats {
	if prx.topo == nil {
		return types.PoolStats{}
	}
	primary, ok := prx.topo.Primary()
	if !ok {
		return types.PoolStats{}
	}
	mgr, err := prx.topo.PoolFor(primary.ID)
	if err != nil {
		return types.PoolStats{}
	}
	return mgr.PoolStats()
}

// GetAuthMethod returns the configured authentication method.
func (prx *Proxy) GetAuthMethod() types.AuthMethod {
	return prx.auth.Method()
}

// Authenticate validates cleartext credentials.
func (prx *Proxy) Authenticate(user, password string) error {
	if err := prx.auth.Authenticate(user, password); err != nil {
		return &types.ProxyError{Code: "28P01", Message: err.Error()}
	}
	return nil
}

// AuthenticateMD5 validates MD5-hashed credentials.
func (prx *Proxy) AuthenticateMD5(user, clientHash string, salt [4]byte) error {
	if err := prx.auth.AuthenticateMD5(user, clientHash, salt); err != nil {
		return &types.ProxyError{Code: "28P01", Message: err.Error()}
	}
	return nil
}

// NewSCRAMSession creates a new SCRAM-SHA-256 server conversation.
func (prx *Proxy) NewSCRAMSession() (types.SCRAMSession, error) {
	return prx.auth.NewSCRAMSession()
}

// PublishDecisionEvent emits a decision event to the bus.
// If the proxy does not have a decisions bus, it is a no-op.
func (prx *Proxy) PublishDecisionEvent(event interface{}) {
	if prx.decisionsBus == nil {
		return
	}
	prx.decisionsBus.Publish(event)
}

// RejectReadPortWrite routes a read-port write rejection through the
// pipeline so metrics stay consistent.  If the pipeline is not available
// (e.g. in tests) it returns a correctly-shaped event without publishing.
func (prx *Proxy) RejectReadPortWrite(sql, poolMode string) decisions.Event {
	if prx.routingPipeline == nil {
		sess := &classifier.SessionState{Port: "read", TxState: classifier.TxIdle}
		cls := classifier.Classify(classifier.Statement{SQL: sql}, sess)
		return decisions.Event{
			Port:      "read",
			PoolMode:  poolMode,
			LeaseType: "transaction",
			Source:    "client",
			Classification: decisions.Classification{
				Type:   string(cls.Type),
				Source: string(cls.Source),
				SQL:    classifier.TruncateSQL(sql, 256),
			},
			Outcome: decisions.Outcome{
				Kind:     "rejected",
				SQLState: "25006",
				Reason:   "read_port:write_attempted",
			},
		}
	}
	return prx.routingPipeline.RejectReadPortWrite(sql, poolMode)
}

// RejectCompatReclassification routes a compat-port mid-transaction
// reclassification rejection through the pipeline so metrics stay consistent.
func (prx *Proxy) RejectCompatReclassification(sql, poolMode string) decisions.Event {
	if prx.routingPipeline == nil {
		sess := &classifier.SessionState{Port: "compat", TxState: classifier.TxIdle}
		cls := classifier.Classify(classifier.Statement{SQL: sql}, sess)
		return decisions.Event{
			Port:      "compat",
			PoolMode:  poolMode,
			LeaseType: "transaction",
			Source:    "client",
			Classification: decisions.Classification{
				Type:   string(cls.Type),
				Source: string(cls.Source),
				SQL:    classifier.TruncateSQL(sql, 256),
			},
			Outcome: decisions.Outcome{
				Kind:     "rejected",
				SQLState: "25006",
				Reason:   "compat:reclassification",
			},
			Consistency: &decisions.Consistency{Mode: "bounded_staleness"},
		}
	}
	return prx.routingPipeline.RejectCompatReclassification(sql, poolMode)
}

// AcquireUpstream acquires a connection using the routing pipeline.
// The routing pipeline evaluates all candidates, applies policies,
// and returns a connection to the chosen node along with a decision event.
func (prx *Proxy) AcquireUpstream() (types.UpstreamClientInterface, error) {
	return prx.acquireUpstreamWithSQL("")
}

// AcquireUpstreamForSQL acquires a connection using the routing pipeline
// with SQL classification for compat-port routing decisions.
func (prx *Proxy) AcquireUpstreamForSQL(sql string) (types.UpstreamClientInterface, error) {
	return prx.acquireUpstreamWithSQL(sql)
}

func (prx *Proxy) acquireUpstreamWithSQL(sql string) (types.UpstreamClientInterface, error) {
	lease, err := prx.acquireUpstreamLeaseWithSQL(sql, false, 0)
	if err != nil {
		return nil, err
	}
	return lease.Client, nil
}

// AcquireUpstreamLease acquires a connection and returns routing metadata.
func (prx *Proxy) AcquireUpstreamLease(sql string, postWriteWindowActive bool, postWriteWindowRemainingMs int) (*types.UpstreamLease, error) {
	return prx.acquireUpstreamLeaseWithSQL(sql, postWriteWindowActive, postWriteWindowRemainingMs)
}

func (prx *Proxy) acquireUpstreamLeaseWithSQL(sql string, postWriteWindowActive bool, postWriteWindowRemainingMs int) (*types.UpstreamLease, error) {
	if prx.routingPipeline == nil {
		// Fallback for tests: use old direct-primary path
		if prx.topo == nil {
			return nil, &types.ProxyError{Code: "57P03", Message: "[grunyas] no primary available"}
		}
		primary, ok := prx.topo.Primary()
		if !ok {
			return nil, &types.ProxyError{Code: "57P03", Message: "[grunyas] no primary available"}
		}
		mgr, err := prx.topo.PoolFor(primary.ID)
		if err != nil {
			return nil, &types.ProxyError{Code: "57P03", Message: "[grunyas] no primary available", Cause: err}
		}
		upstream, err := mgr.AcquireDbConnection()
		if err != nil {
			return nil, &types.ProxyError{Code: "53300", Message: "[grunyas] pool saturated", Cause: err}
		}
		return &types.UpstreamLease{
			Client: upstream,
			NodeID: string(primary.ID),
			Role:   primary.ObservedRole.String(),
		}, nil
	}

	poolMode := prx.poolMode()
	req := routing.LeaseRequest{
		Port:                       prx.port,
		PoolMode:                   poolMode,
		SQL:                        sql,
		PostWriteWindowActive:      postWriteWindowActive,
		PostWriteWindowRemainingMs: postWriteWindowRemainingMs,
	}
	switch prx.port {
	case "write":
		req.LeaseType = string(poolMode)
	case "read":
		if poolMode == "session" {
			req.LeaseType = "session"
		} else {
			req.LeaseType = "transaction"
		}
	case "compat":
		req.LeaseType = "transaction"
	}

	result, err := prx.routingPipeline.Lease(req)
	if err != nil {
		return nil, err
	}
	if result.Error != nil {
		return nil, result.Error
	}

	role := "unknown"
	if nv, ok := prx.topo.Node(result.NodeID); ok {
		role = nv.ObservedRole.String()
	}

	return &types.UpstreamLease{
		Client: result.Upstream,
		NodeID: string(result.NodeID),
		Role:   role,
	}, nil
}

func (prx *Proxy) poolMode() string {
	portCfg, ok := prx.cfg.ServerConfig.Ports[prx.port]
	if !ok {
		return "session"
	}
	return portCfg.PoolMode
}

// Ready returns a channel that is closed when the proxy is successfully listening
// and ready to accept connections.
func (prx *Proxy) Ready() <-chan struct{} {
	return prx.ready
}

func (prx *Proxy) handleNewIncomingConnection(conn net.Conn) {
	portCfg, portExists := prx.cfg.ServerConfig.Ports[prx.port]
	requiredSSL := portExists && strings.ToLower(portCfg.SSLMode) == "mandatory"
	downstream := downstream_client.Initialize(conn, prx.tlsConfig, requiredSSL, prx.logger)

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
			Code:     "53300",
			Message:  "connection pool exhausted, please try again later",
		}); err != nil {
			prx.logger.Warn("failed to buffer error response", zap.Error(err))
		}
		if err := downstream.Flush(); err != nil {
			prx.logger.Warn("failed to flush error response", zap.Error(err))
		}

		downstream.Close() //nolint:errcheck
		return
	}

	prx.logger.Debug("initializing new session", zap.String("remote", downstream.RemoteAddr().String()), zap.String("port", prx.port))
	sess := session.Initialize(prx, downstream)

	prx.idle.Track(sess)

	prx.mu.Lock()
	prx.sessions[sess] = struct{}{}
	prx.mu.Unlock()

	prx.lifetimeConnectionsCount.Add(1)

	defer func() {
		prx.mu.Lock()
		delete(prx.sessions, sess)
		prx.mu.Unlock()

		prx.idle.Untrack(sess)
		prx.currentConnectionsCount.Add(-1)
	}()

	sess.Run()
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

// Shutdown gracefully stops the proxy by closing all active sessions and waiting
// for them to drain. Returns when all sessions are closed or ctx expires.
func (prx *Proxy) Shutdown(ctx context.Context) error {
	select {
	case <-prx.ready:
	case <-ctx.Done():
		return ctx.Err()
	}

	prx.logger.Info("shutting down proxy, draining active sessions", zap.String("port", prx.port))

	prx.closeLnOnce.Do(func() {
		_ = prx.ln.Close()
	})

	prx.mu.Lock()
	sessions := make([]*session.Session, 0, len(prx.sessions))
	for s := range prx.sessions {
		sessions = append(sessions, s)
	}
	prx.mu.Unlock()

	for _, s := range sessions {
		go s.Close()
	}

	done := make(chan struct{})
	go func() {
		prx.sessionsWg.Wait()
		close(done)
	}()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-done:
		return nil
	}
}
