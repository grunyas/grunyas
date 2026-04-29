// Package probe provides a dedicated observation connection per node for
// liveness, role detection, system_identifier verification, and replication
// lag measurement. Each node gets exactly one long-lived probe connection,
// separate from the traffic pool.
package probe

import (
	"context"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/grunyas/grunyas/config"
	"go.uber.org/zap"
)

type Liveness int

const (
	LivenessUp       Liveness = iota
	LivenessDegraded
	LivenessDown
	LivenessUnknown
)

type Role int

const (
	RolePrimary Role = iota
	RoleReplica
	RoleUnknown
)

// Sink is the interface through which probe results are delivered.
// Implemented by the topology object.
type Sink interface {
	UpdateLiveness(id string, liveness Liveness, err error)
	UpdateObservedRole(id string, role Role)
	UpdateSystemID(id string, sid string) error

	// M2: replication lag
	UpdateLag(id string, lagMs int64, sampledAt time.Time)
	MarkLagIdle(id string, sampledAt time.Time)
	MarkLagUnknown(id string, reason error)
}

type NodeSpec struct {
	ID         string
	Host       string
	Port       uint16
	Connection config.NodeConnectionConfig
}

type ProbeConfig struct {
	IntervalMs            int
	LivenessFailureCount  int
	LivenessMaxAgeMs      int
	RoleMaxAgeMs          int
}

type Probe struct {
	spec   NodeSpec
	cfg    ProbeConfig
	sink   Sink
	logger *zap.Logger

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu   sync.Mutex
	conn *pgx.Conn

	failureCount int
}

func New(ctx context.Context, spec NodeSpec, sink Sink, cfg ProbeConfig, log *zap.Logger) (*Probe, error) {
	ctx, cancel := context.WithCancel(ctx)
	p := &Probe{
		spec:   spec,
		cfg:    cfg,
		sink:   sink,
		logger: log.With(zap.String("node_id", spec.ID)),
		ctx:    ctx,
		cancel: cancel,
	}

	p.wg.Add(1)
	go p.loop()

	return p, nil
}

func (p *Probe) Close() {
	p.cancel()
	p.wg.Wait()

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.conn != nil {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := p.conn.Close(closeCtx); err != nil {
			p.logger.Warn("error closing probe connection", zap.Error(err))
		}
		p.conn = nil
	}
}

func (p *Probe) loop() {
	defer p.wg.Done()

	interval := time.Duration(p.cfg.IntervalMs) * time.Millisecond
	if interval < 100*time.Millisecond {
		interval = 100 * time.Millisecond
	}

	backoff := 100 * time.Millisecond
	backoffCap := 5 * time.Second

	timer := time.NewTimer(0)
	defer timer.Stop()

	for {
		select {
		case <-p.ctx.Done():
			return
		case <-timer.C:
			p.probeOnce()
		}

		select {
		case <-p.ctx.Done():
			return
		default:
		}

		p.mu.Lock()
		connAlive := p.conn != nil
		p.mu.Unlock()

		if connAlive {
			timer.Reset(interval)
			backoff = 100 * time.Millisecond
		} else {
			timer.Reset(backoff)
			backoff = time.Duration(math.Min(float64(backoff*2), float64(backoffCap)))
		}
	}
}

func (p *Probe) probeOnce() {
	log := p.logger

	conn, err := p.getOrOpenConn()
	if err != nil {
		p.recordFailure(err)
		return
	}

	// 1. Liveness ping
	if err := p.ping(conn); err != nil {
		log.Debug("probe ping failed", zap.Error(err))
		p.recordFailure(err)
		return
	}

	// 2. Observed role
	role, err := p.fetchRole(conn)
	if err != nil {
		log.Debug("probe role query failed", zap.Error(err))
		p.recordFailure(err)
		return
	}
	p.sink.UpdateObservedRole(p.spec.ID, role)

	// 3. System identifier
	sid, err := p.fetchSystemID(conn)
	if err != nil {
		log.Debug("probe system_id query failed", zap.Error(err))
		p.recordFailure(err)
		return
	}
	if err := p.sink.UpdateSystemID(p.spec.ID, sid); err != nil {
		log.Error("system_identifier mismatch — marking node permanently down", zap.Error(err))
		p.recordFailure(err)
		return
	}

	// 4. Replication lag (replicas only; primary is N/A)
	if role == RoleReplica {
		lagMs, idle, lagErr := p.fetchLag(conn)
		if lagErr != nil {
			log.Debug("probe lag query failed", zap.Error(lagErr))
			p.sink.MarkLagUnknown(p.spec.ID, lagErr)
			// Lag failure does not affect liveness; continue.
		} else if idle {
			p.sink.MarkLagIdle(p.spec.ID, time.Now())
		} else {
			p.sink.UpdateLag(p.spec.ID, lagMs, time.Now())
		}
	}

	// Success
	p.mu.Lock()
	p.failureCount = 0
	p.mu.Unlock()
	p.sink.UpdateLiveness(p.spec.ID, LivenessUp, nil)
}

func (p *Probe) fetchLag(conn *pgx.Conn) (lagMs int64, idle bool, err error) {
	ctx, cancel := context.WithTimeout(p.ctx, 5*time.Second)
	defer cancel()

	var lagMsFloat *float64
	var replayAt *time.Time
	err = conn.QueryRow(ctx,
		`SELECT EXTRACT(EPOCH FROM (now() - pg_last_xact_replay_timestamp())) * 1000 AS lag_ms,
		        pg_last_xact_replay_timestamp() AS last_replay_at`,
	).Scan(&lagMsFloat, &replayAt)
	if err != nil {
		return 0, false, fmt.Errorf("replication lag: %w", err)
	}

	return classifyLagResult(lagMsFloat, replayAt)
}

// classifyLagResult converts raw SQL scan values into the classified (lagMs, idle, err) tuple.
// Exported for testing in the same package.
func classifyLagResult(lagMsFloat *float64, replayAt *time.Time) (lagMs int64, idle bool, err error) {
	if lagMsFloat == nil || replayAt == nil {
		return 0, true, nil
	}
	return int64(*lagMsFloat), false, nil
}

func (p *Probe) getOrOpenConn() (*pgx.Conn, error) {
	p.mu.Lock()
	existing := p.conn
	p.mu.Unlock()

	if existing != nil {
		if err := p.ping(existing); err == nil {
			return existing, nil
		}
		p.logger.Debug("probe connection lost, reconnecting")
		_ = existing.Close(p.ctx)
		p.mu.Lock()
		if p.conn == existing {
			p.conn = nil
		}
		p.mu.Unlock()
	}

	dsn := probeDSN(p.spec)
	conn, err := pgx.Connect(p.ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("probe connect: %w", err)
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.conn != nil {
		_ = conn.Close(p.ctx)
		return p.conn, nil
	}
	p.conn = conn
	p.failureCount = 0
	return conn, nil
}

func (p *Probe) ping(conn *pgx.Conn) error {
	ctx, cancel := context.WithTimeout(p.ctx, 5*time.Second)
	defer cancel()

	var result int
	return conn.QueryRow(ctx, "SELECT 1").Scan(&result)
}

func (p *Probe) fetchRole(conn *pgx.Conn) (Role, error) {
	ctx, cancel := context.WithTimeout(p.ctx, 5*time.Second)
	defer cancel()

	var inRecovery bool
	if err := conn.QueryRow(ctx, "SELECT pg_is_in_recovery()").Scan(&inRecovery); err != nil {
		return RoleUnknown, fmt.Errorf("pg_is_in_recovery: %w", err)
	}
	if inRecovery {
		return RoleReplica, nil
	}
	return RolePrimary, nil
}

func (p *Probe) fetchSystemID(conn *pgx.Conn) (string, error) {
	ctx, cancel := context.WithTimeout(p.ctx, 5*time.Second)
	defer cancel()

	var systemID string
	if err := conn.QueryRow(ctx, "SELECT system_identifier::text FROM pg_control_system()").Scan(&systemID); err != nil {
		return "", fmt.Errorf("pg_control_system: %w", err)
	}
	return systemID, nil
}

func (p *Probe) recordFailure(err error) {
	p.mu.Lock()
	p.failureCount++
	fc := p.failureCount
	threshold := p.cfg.LivenessFailureCount
	if threshold <= 0 {
		threshold = 3
	}
	p.mu.Unlock()

	if fc >= threshold {
		p.sink.UpdateLiveness(p.spec.ID, LivenessDown, err)
	}
}

func probeDSN(spec NodeSpec) string {
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
