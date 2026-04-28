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

// nodeState is the mutable state for a single node, protected by both the
// topology-level RWMutex (for structural changes) and its own per-node mutex
// (for observation updates from the probe loop).
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

	// permanentlyDown is set once when system_identifier mismatch is detected.
	// While true, the node is never eligible for routing and UpdateLiveness
	// ignores any subsequent Up signals.
	permanentlyDown bool
}

// Topology is the in-process representation of the cluster. It tracks each
// declared node, its observed state, its pool manager, and its probe goroutine.
type Topology struct {
	mu    sync.RWMutex
	nodes map[NodeID]*nodeState

	clusterID SystemID // first system_identifier ever observed
	logger    *zap.Logger

	sysIDMu sync.Mutex
	sysIDErr error // set when any node reports a mismatched system_identifier
}

// New constructs a Topology from the validated config, creating one pool
// manager and one probe goroutine per node.
func New(ctx context.Context, cfg *config.Config, log *zap.Logger) (*Topology, error) {
	t := &Topology{
		nodes:  make(map[NodeID]*nodeState),
		logger: log,
	}

	for i := range cfg.Nodes {
		nc := cfg.Nodes[i]
		id := NodeID(nc.ID)

		// Per-node pool manager (M1: always session-mode discardAll,
		// write-port only).
		pm, err := pool.New(ctx, pool.NodeSpec{
			ID:         string(id),
			Host:       nc.Host,
			Port:       uint16(nc.Port),
			Connection: nc.Connection,
			Pool:       nc.Pool,
			DiscardAll: true, // M1 only serves the write port (session mode)
		}, log)
		if err != nil {
			return nil, fmt.Errorf("node %s pool: %w", nc.ID, err)
		}

		state := &nodeState{
			config:       nc,
			pool:         pm,
			observedRole: RoleUnknown,
			liveness:     LivenessUnknown,
		}
		t.nodes[id] = state

		// Start probe goroutine for this node.
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

// Nodes returns a snapshot of every node's current state. The returned slice
// is a copy; callers may retain it.
func (t *Topology) Nodes() []NodeView {
	t.mu.RLock()
	defer t.mu.RUnlock()

	views := make([]NodeView, 0, len(t.nodes))
	for id, ns := range t.nodes {
		views = append(views, t.nodeView(id, ns))
	}
	return views
}

// Node returns the current state of a single node by ID.
func (t *Topology) Node(id NodeID) (NodeView, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	ns, ok := t.nodes[id]
	if !ok {
		return NodeView{}, false
	}
	return t.nodeView(id, ns), true
}

// Primary returns the node currently considered primary based on observed role.
// Returns (NodeView{}, false) if no node is currently observed-primary, or if
// more than one node claims primary (split-brain — refuses to choose).
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
				// Split-brain: two nodes claim primary. Refuse.
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

// Replicas returns the nodes currently observed as replicas.
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

// PoolFor returns the pool manager for a node, used to acquire upstream
// connections.
func (t *Topology) PoolFor(id NodeID) (pool.Manager, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	ns, ok := t.nodes[id]
	if !ok {
		return nil, fmt.Errorf("node %s not found", id)
	}
	return ns.pool, nil
}

// Close stops every probe goroutine and closes every pool.
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

	// A permanently-down node (system_identifier mismatch) stays down forever.
	// Ignore any probe success.
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

	// Only log on state transitions.
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

	// Lock ordering: sysIDMu (topology-level) → ns.mu (per-node).
	// sysIDMu protects t.clusterID and t.sysIDErr (both topology-scoped).

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

	// Subsequent nodes: compare against the cluster ID we already have.
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

	// Previously-valid node with a changed system_identifier (should never happen).
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

	return NodeView{
		ID:           id,
		Host:         ns.config.Host,
		Port:         uint16(ns.config.Port),
		DeclaredRole: role,
		ObservedRole: ns.observedRole,
		Liveness:     liveness,
		SystemID:     ns.systemID,
		LastProbeAt:  ns.lastProbeAt,
		LastProbeErr: ns.lastProbeErr,
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

// WaitForInitialProbes blocks until every node has completed at least one
// probe cycle, or the context is cancelled / timeout expires. Nodes that
// do not respond within the deadline keep their initial LivenessUnknown
// state and will be probed again on the next cycle.
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

// SystemIDError returns the first system_identifier mismatch error detected
// during probing, or nil if no mismatch has been observed.
func (t *Topology) SystemIDError() error {
	t.sysIDMu.Lock()
	defer t.sysIDMu.Unlock()
	return t.sysIDErr
}

func probeConfig(cfg config.ProbeConfig) probe.ProbeConfig {
	return probe.ProbeConfig{
		IntervalMs:            cfg.IntervalMs,
		LivenessFailureCount:  cfg.LivenessFailureCount,
		LivenessMaxAgeMs:      cfg.LivenessMaxAgeMs,
		RoleMaxAgeMs:          cfg.RoleMaxAgeMs,
	}
}
