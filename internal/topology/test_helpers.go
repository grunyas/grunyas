package topology

import (
	"time"

	pool "github.com/grunyas/grunyas/internal/pool/manager"
	"github.com/grunyas/grunyas/config"
	"go.uber.org/zap"
)

// NewTestTopology creates a Topology for testing with no real pool or probe
// connections. Use AddTestNode to populate it.
func NewTestTopology() *Topology {
	return &Topology{
		nodes:                  make(map[NodeID]*nodeState),
		logger:                 zap.NewNop(),
		livenessMaxAgeMs:       5000,
		observedRoleMaxAgeMs:   5000,
		replicationLagMaxAgeMs: 2000,
	}
}

// AddTestNode adds a node to the test topology. The node starts with a nil
// pool; call SetTestNodePool to attach a mock pool manager.
func (t *Topology) AddTestNode(id NodeID, declaredRole string, observedRole Role, liveness Liveness) {
	t.mu.Lock()
	defer t.mu.Unlock()
	roleStr := declaredRole
	if roleStr == "" {
		roleStr = "replica"
	}
		lastProbe := time.Now()
		if liveness == LivenessDown {
			// Set lastProbeAt far in the past so the staleness-detection
			// in nodeView() flips both Liveness and ObservedRole to Unknown.
			// Tests that assert on "no primary available" rely on this flip
			// producing role:unknown reasons for a down primary.
			lastProbe = time.Now().Add(-time.Hour)
		}
	t.nodes[id] = &nodeState{
		config: config.NodeConfig{
			ID:           string(id),
			Host:         "localhost",
			Port:         5432,
			DeclaredRole: roleStr,
		},
		observedRole:        observedRole,
		liveness:            liveness,
		replicationLagState: LagStateNotApplicable,
		lastProbeAt:         lastProbe,
	}
}

// SetTestNodePool attaches a pool manager to a test node.
func (t *Topology) SetTestNodePool(id NodeID, mgr pool.Manager) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if ns, ok := t.nodes[id]; ok {
		ns.pool = mgr
	}
}
