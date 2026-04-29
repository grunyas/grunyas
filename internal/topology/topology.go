package topology

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/grunyas/grunyas/config"
	pool "github.com/grunyas/grunyas/internal/pool/manager"
	"github.com/grunyas/grunyas/internal/probe"
	"go.uber.org/zap"
)

type nodeState struct {
	config config.NodeConfig

	mu           sync.RWMutex
	pool         pool.Manager
	probe        *probe.Probe
	observedRole Role
	liveness     Liveness
	systemID     SystemID
	lastProbeAt  time.Time
	lastProbeErr error
	permanentlyDown bool

	// M2: replication lag
	replicationLagMs    *int64
	replicationLagState LagState
	lastLagSampleAt     time.Time
}

type Topology struct {
	mu    sync.RWMutex
	nodes map[NodeID]*nodeState

	clusterID SystemID
	logger    *zap.Logger

	sysIDMu sync.Mutex
	sysIDErr error

	// Max-age configuration (derived from probe config at construction)
	livenessMaxAgeMs       int
	observedRoleMaxAgeMs   int
	replicationLagMaxAgeMs int
}

// NewEmpty creates a Topology with no nodes and no real pool/probe connections.
// Useful for unit-testing components (e.g. the admin API) that only read the
// topology surface and do not require actual connections.
func NewEmpty() *Topology {
	return &Topology{
		nodes:  make(map[NodeID]*nodeState),
		logger: zap.NewNop(),
	}
}

func New(ctx context.Context, cfg *config.Config, log *zap.Logger) (*Topology, error) {
	t := &Topology{
		nodes:                 make(map[NodeID]*nodeState),
		logger:                log,
		livenessMaxAgeMs:      maxAgeOrDefault(cfg.ProbeConfig.LivenessMaxAgeMs, cfg.ProbeConfig.IntervalMs*2),
		observedRoleMaxAgeMs:  maxAgeOrDefault(cfg.ProbeConfig.RoleMaxAgeMs, cfg.ProbeConfig.IntervalMs*5),
		replicationLagMaxAgeMs: maxAgeOrDefault(cfg.ProbeConfig.LagMaxAgeMs, cfg.ProbeConfig.IntervalMs*2),
	}

	for i := range cfg.Nodes {
		nc := cfg.Nodes[i]
		id := NodeID(nc.ID)

		pm, err := pool.New(ctx, pool.NodeSpec{
			ID:         string(id),
			Host:       nc.Host,
			Port:       uint16(nc.Port),
			Connection: nc.Connection,
			Pool:       nc.Pool,
			DiscardAll: true,
		}, log)
		if err != nil {
			return nil, fmt.Errorf("node %s pool: %w", nc.ID, err)
		}

		state := &nodeState{
			config:              nc,
			pool:                pm,
			observedRole:        RoleUnknown,
			liveness:            LivenessUnknown,
			replicationLagState: LagStateNotApplicable,
		}
		t.nodes[id] = state

		prb, err := probe.New(ctx, probe.NodeSpec{
			ID:         string(id),
			Host:       nc.Host,
			Port:       uint16(nc.Port),
			Connection: nc.Connection,
		}, t, probeConfig(cfg.ProbeConfig), log)
		if err != nil {
			pm.Close()
			return nil, fmt.Errorf("node %s probe: %w", nc.ID, err)
		}
		state.probe = prb
	}

	return t, nil
}

func (t *Topology) Nodes() []NodeView {
	t.mu.RLock()
	defer t.mu.RUnlock()

	views := make([]NodeView, 0, len(t.nodes))
	for id, ns := range t.nodes {
		views = append(views, t.nodeView(id, ns))
	}
	return views
}

func (t *Topology) Node(id NodeID) (NodeView, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	ns, ok := t.nodes[id]
	if !ok {
		return NodeView{}, false
	}
	return t.nodeView(id, ns), true
}

func (t *Topology) Primary() (NodeView, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	var primary *nodeState
	var primaryID NodeID
	for id, ns := range t.nodes {
		ns.mu.RLock()
		role := ns.observedRole
		pd := ns.permanentlyDown
		ns.mu.RUnlock()

		if pd {
			continue
		}
		if role == RolePrimary {
			if primary != nil {
				t.logger.Error("split-brain detected — multiple nodes observed as primary",
					zap.String("first", string(primaryID)),
					zap.String("second", string(id)),
				)
				return NodeView{}, false
			}
			primary = ns
			primaryID = id
		}
	}

	if primary == nil {
		return NodeView{}, false
	}
	return t.nodeView(primaryID, primary), true
}

func (t *Topology) Replicas() []NodeView {
	t.mu.RLock()
	defer t.mu.RUnlock()

	var views []NodeView
	for id, ns := range t.nodes {
		ns.mu.RLock()
		role := ns.observedRole
		pd := ns.permanentlyDown
		ns.mu.RUnlock()

		if pd {
			continue
		}
		if role == RoleReplica {
			views = append(views, t.nodeView(id, ns))
		}
	}
	return views
}

func (t *Topology) PoolFor(id NodeID) (pool.Manager, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	ns, ok := t.nodes[id]
	if !ok {
		return nil, fmt.Errorf("node %s not found", id)
	}
	return ns.pool, nil
}

// ClusterID returns the cluster's verified system_identifier, or empty string
// if no node has been probed yet (M2 §1 — "Once verified, never expires").
func (t *Topology) ClusterID() SystemID {
	t.sysIDMu.Lock()
	defer t.sysIDMu.Unlock()
	return t.clusterID
}

func (t *Topology) Close() {
	t.mu.RLock()
	defer t.mu.RUnlock()

	for id, ns := range t.nodes {
		ns.probe.Close()
		ns.pool.Close()
		t.logger.Debug("topology node closed", zap.String("node", string(id)))
	}
}

// ---------------------------------------------------------------------------
// probe.Sink implementation
// ---------------------------------------------------------------------------

func (t *Topology) UpdateLiveness(id string, l probe.Liveness, err error) {
	nid := NodeID(id)
	t.mu.RLock()
	ns, ok := t.nodes[nid]
	t.mu.RUnlock()
	if !ok {
		return
	}

	ns.mu.Lock()
	defer ns.mu.Unlock()

	if ns.permanentlyDown {
		if l == probe.LivenessUp {
			return
		}
		ns.lastProbeAt = time.Now()
		ns.lastProbeErr = err
		return
	}

	var newLiveness Liveness
	switch l {
	case probe.LivenessUp:
		newLiveness = LivenessUp
	case probe.LivenessDegraded:
		newLiveness = LivenessDegraded
	case probe.LivenessDown:
		newLiveness = LivenessDown
	default:
		newLiveness = LivenessUnknown
	}

	if ns.liveness != newLiveness {
		if newLiveness == LivenessDown {
			t.logger.Warn("node marked down by probe",
				zap.String("node", id),
				zap.String("from", ns.liveness.String()),
				zap.Error(err),
			)
		} else if ns.liveness == LivenessDown && newLiveness == LivenessUp {
			t.logger.Info("node recovered",
				zap.String("node", id),
				zap.String("from", ns.liveness.String()),
			)
		} else if ns.liveness == LivenessUnknown && newLiveness == LivenessUp {
			t.logger.Info("node became reachable",
				zap.String("node", id),
			)
		} else if ns.liveness != newLiveness {
			t.logger.Info("node liveness changed",
				zap.String("node", id),
				zap.String("from", ns.liveness.String()),
				zap.String("to", newLiveness.String()),
			)
		}
	}

	ns.liveness = newLiveness
	ns.lastProbeAt = time.Now()
	ns.lastProbeErr = err
}

func (t *Topology) UpdateObservedRole(id string, role probe.Role) {
	nid := NodeID(id)
	t.mu.RLock()
	ns, ok := t.nodes[nid]
	t.mu.RUnlock()
	if !ok {
		return
	}

	ns.mu.Lock()
	defer ns.mu.Unlock()

	var r Role
	switch role {
	case probe.RolePrimary:
		r = RolePrimary
	case probe.RoleReplica:
		r = RoleReplica
	default:
		r = RoleUnknown
	}

	if ns.observedRole != r {
		t.logger.Info("node observed role changed",
			zap.String("node", id),
			zap.String("from", ns.observedRole.String()),
			zap.String("to", r.String()),
		)
	}
	ns.observedRole = r
}

func (t *Topology) UpdateSystemID(id string, sid string) error {
	nid := NodeID(id)
	t.mu.RLock()
	ns, ok := t.nodes[nid]
	t.mu.RUnlock()
	if !ok {
		return fmt.Errorf("node %s not found", id)
	}

	t.sysIDMu.Lock()
	firstTime := t.clusterID == ""
	if firstTime {
		t.clusterID = SystemID(sid)
	}
	clusterID := t.clusterID
	t.sysIDMu.Unlock()

	ns.mu.Lock()
	defer ns.mu.Unlock()

	if firstTime {
		ns.systemID = clusterID
		return nil
	}

	if SystemID(sid) != clusterID {
		err := fmt.Errorf("system_identifier mismatch for node %s: got %s, cluster has %s",
			id, sid, clusterID)
		ns.permanentlyDown = true
		ns.liveness = LivenessDown
		t.sysIDMu.Lock()
		t.sysIDErr = err
		t.sysIDMu.Unlock()
		t.logger.Error("system_identifier mismatch — node permanently excluded",
			zap.String("node", id),
			zap.Error(err))
		return err
	}

	if ns.systemID != "" && ns.systemID != SystemID(sid) {
		err := fmt.Errorf("system_identifier changed for node %s: was %s, now %s",
			id, string(ns.systemID), sid)
		ns.permanentlyDown = true
		ns.liveness = LivenessDown
		t.logger.Error("system_identifier changed — node permanently excluded",
			zap.String("node", id),
			zap.Error(err))
		return err
	}

	ns.systemID = SystemID(sid)
	return nil
}

// M2 sink methods

func (t *Topology) UpdateLag(id string, lagMs int64, sampledAt time.Time) {
	nid := NodeID(id)
	t.mu.RLock()
	ns, ok := t.nodes[nid]
	t.mu.RUnlock()
	if !ok {
		return
	}

	ns.mu.Lock()
	defer ns.mu.Unlock()

	ns.replicationLagMs = &lagMs
	ns.replicationLagState = LagStateFresh
	ns.lastLagSampleAt = sampledAt
}

func (t *Topology) MarkLagIdle(id string, sampledAt time.Time) {
	nid := NodeID(id)
	t.mu.RLock()
	ns, ok := t.nodes[nid]
	t.mu.RUnlock()
	if !ok {
		return
	}

	ns.mu.Lock()
	defer ns.mu.Unlock()

	ns.replicationLagMs = nil
	ns.replicationLagState = LagStateIdle
	ns.lastLagSampleAt = sampledAt
}

func (t *Topology) MarkLagUnknown(id string, reason error) {
	nid := NodeID(id)
	t.mu.RLock()
	ns, ok := t.nodes[nid]
	t.mu.RUnlock()
	if !ok {
		return
	}

	ns.mu.Lock()
	defer ns.mu.Unlock()

	ns.replicationLagMs = nil
	ns.replicationLagState = LagStateUnknown
	ns.lastLagSampleAt = time.Now()
	if reason != nil {
		t.logger.Debug("replication lag unknown", zap.String("node", id), zap.Error(reason))
	}
}

// ---------------------------------------------------------------------------
// internal helpers
// ---------------------------------------------------------------------------

func (t *Topology) nodeView(id NodeID, ns *nodeState) NodeView {
	ns.mu.RLock()
	defer ns.mu.RUnlock()

	role, _ := roleFromString(ns.config.DeclaredRole)
	liveness := ns.liveness
	if ns.permanentlyDown {
		liveness = LivenessDown
	}

	// Stale-observation detection
	now := time.Now()
	if !ns.lastProbeAt.IsZero() && now.Sub(ns.lastProbeAt).Milliseconds() > int64(t.livenessMaxAgeMs) {
		liveness = LivenessUnknown
	}

	observedRole := ns.observedRole
	if !ns.lastProbeAt.IsZero() && now.Sub(ns.lastProbeAt).Milliseconds() > int64(t.observedRoleMaxAgeMs) {
		observedRole = RoleUnknown
	}

	lagMs := ns.replicationLagMs
	lagState := ns.replicationLagState
	if lagState == LagStateFresh && !ns.lastLagSampleAt.IsZero() &&
		now.Sub(ns.lastLagSampleAt).Milliseconds() > int64(t.replicationLagMaxAgeMs) {
		lagMs = nil
		lagState = LagStateStale
	}

	return NodeView{
		ID:           id,
		Host:         ns.config.Host,
		Port:         uint16(ns.config.Port),
		DeclaredRole: role,
		ObservedRole: observedRole,
		Liveness:     liveness,
		SystemID:     ns.systemID,
		LastProbeAt:  ns.lastProbeAt,
		LastProbeErr: ns.lastProbeErr,

		ReplicationLagMs:      lagMs,
		ReplicationLagState:   lagState,
		LastLagSampleAt:       ns.lastLagSampleAt,

		LivenessMaxAgeMs:       t.livenessMaxAgeMs,
		ObservedRoleMaxAgeMs:   t.observedRoleMaxAgeMs,
		ReplicationLagMaxAgeMs: t.replicationLagMaxAgeMs,
	}
}

func roleFromString(s string) (Role, bool) {
	switch s {
	case "primary":
		return RolePrimary, true
	case "replica":
		return RoleReplica, true
	default:
		return RoleUnknown, false
	}
}

func (t *Topology) WaitForInitialProbes(ctx context.Context) {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	for {
		allDone := true
		for _, nv := range t.Nodes() {
			if nv.LastProbeAt.IsZero() {
				allDone = false
				break
			}
		}
		if allDone {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (t *Topology) SystemIDError() error {
	t.sysIDMu.Lock()
	defer t.sysIDMu.Unlock()
	return t.sysIDErr
}

func maxAgeOrDefault(val, fallback int) int {
	if val > 0 {
		return val
	}
	return fallback
}

func probeConfig(cfg config.ProbeConfig) probe.ProbeConfig {
	return probe.ProbeConfig{
		IntervalMs:            cfg.IntervalMs,
		LivenessFailureCount:  cfg.LivenessFailureCount,
		LivenessMaxAgeMs:      cfg.LivenessMaxAgeMs,
		RoleMaxAgeMs:          cfg.RoleMaxAgeMs,
	}
}
