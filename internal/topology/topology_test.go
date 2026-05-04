package topology

import (
	"testing"
	"time"

	"github.com/grunyas/grunyas/config"
	"go.uber.org/zap"
)

// newTestTopology creates a Topology with a pre-populated nodes map and no real
// pool or probe connections — safe for unit testing state-machine logic.
func newTestTopology(t *testing.T, nodes map[NodeID]*nodeState, livenessMaxAgeMs int) *Topology {
	t.Helper()
	if nodes == nil {
		nodes = make(map[NodeID]*nodeState)
	}
	return &Topology{
		nodes:                  nodes,
		logger:                 zap.NewNop(),
		livenessMaxAgeMs:       livenessMaxAgeMs,
		observedRoleMaxAgeMs:   5000,
		replicationLagMaxAgeMs: 2000,
	}
}

func newNodeState(declaredRole string, observedRole Role, liveness Liveness, permanentlyDown bool) *nodeState {
	role := "replica"
	if declaredRole != "" {
		role = declaredRole
	}
	return &nodeState{
		config: config.NodeConfig{
			ID:           "test-node",
			Host:         "localhost",
			Port:         5432,
			DeclaredRole: role,
		},
		observedRole:        observedRole,
		liveness:            liveness,
		permanentlyDown:     permanentlyDown,
		replicationLagState: LagStateNotApplicable,
	}
}

// ---------------------------------------------------------------------------
// Stale-observation (§M2 §1)
// ---------------------------------------------------------------------------

func TestStaleLivenessPastMaxAge(t *testing.T) {
	ns := newNodeState("primary", RolePrimary, LivenessUp, false)
	ns.lastProbeAt = time.Now().Add(-10 * time.Second) // well past max age

	topo := newTestTopology(t, map[NodeID]*nodeState{"n1": ns}, 1000) // 1s max age
	nv := topo.nodeView("n1", ns)

	if nv.Liveness != LivenessUnknown {
		t.Fatalf("expected LivenessUnknown for stale probe, got %v", nv.Liveness)
	}
	if nv.ObservedRole != RoleUnknown {
		t.Fatalf("expected RoleUnknown for stale probe, got %v", nv.ObservedRole)
	}
}

func TestFreshLivenessWithinMaxAge(t *testing.T) {
	ns := newNodeState("primary", RolePrimary, LivenessUp, false)
	ns.lastProbeAt = time.Now().Add(-100 * time.Millisecond)

	topo := newTestTopology(t, map[NodeID]*nodeState{"n1": ns}, 5000)
	nv := topo.nodeView("n1", ns)

	if nv.Liveness != LivenessUp {
		t.Fatalf("expected LivenessUp for fresh probe, got %v", nv.Liveness)
	}
	if nv.ObservedRole != RolePrimary {
		t.Fatalf("expected RolePrimary for fresh probe, got %v", nv.ObservedRole)
	}
}

func TestNeverProbedLivenessUnknown(t *testing.T) {
	ns := newNodeState("primary", RoleUnknown, LivenessUnknown, false)
	// lastProbeAt is zero value

	topo := newTestTopology(t, map[NodeID]*nodeState{"n1": ns}, 5000)
	nv := topo.nodeView("n1", ns)

	// Never probed: liveness stays Unknown (not stale — not overwritten since
	// the stale check only fires when !LastProbeAt.IsZero()).
	if nv.Liveness != LivenessUnknown {
		t.Fatalf("expected LivenessUnknown for never-probed, got %v", nv.Liveness)
	}
}

// ---------------------------------------------------------------------------
// Split-brain (§M1 §2)
// ---------------------------------------------------------------------------

func TestSplitBrainReturnsFalse(t *testing.T) {
	n1 := newNodeState("primary", RolePrimary, LivenessUp, false)
	n2 := newNodeState("primary", RolePrimary, LivenessUp, false)

	topo := newTestTopology(t, map[NodeID]*nodeState{"n1": n1, "n2": n2}, 5000)
	_, ok := topo.Primary()

	if ok {
		t.Fatal("expected Primary() to return false on split-brain")
	}
}

func TestSinglePrimaryReturnsTrue(t *testing.T) {
	n1 := newNodeState("primary", RolePrimary, LivenessUp, false)
	n2 := newNodeState("replica", RoleReplica, LivenessUp, false)

	topo := newTestTopology(t, map[NodeID]*nodeState{"n1": n1, "n2": n2}, 5000)
	p, ok := topo.Primary()

	if !ok {
		t.Fatal("expected Primary() to return true")
	}
	if p.ID != "n1" {
		t.Fatalf("expected primary ID 'n1', got %q", p.ID)
	}
}

func TestNoPrimaryReturnsFalse(t *testing.T) {
	n1 := newNodeState("replica", RoleUnknown, LivenessUp, false)

	topo := newTestTopology(t, map[NodeID]*nodeState{"n1": n1}, 5000)
	_, ok := topo.Primary()

	if ok {
		t.Fatal("expected Primary() to return false when no primary observed")
	}
}

// ---------------------------------------------------------------------------
// Permanent-down (§M2 §1 — system_identifier mismatch)
// ---------------------------------------------------------------------------

func TestPermanentlyDownNodeSkippedByPrimary(t *testing.T) {
	n1 := newNodeState("primary", RolePrimary, LivenessDown, true)

	topo := newTestTopology(t, map[NodeID]*nodeState{"n1": n1}, 5000)
	_, ok := topo.Primary()

	if ok {
		t.Fatal("expected Primary() to skip permanently-down node")
	}
}

func TestPermanentlyDownNodeSkippedByReplicas(t *testing.T) {
	n1 := newNodeState("replica", RoleReplica, LivenessDown, true)

	topo := newTestTopology(t, map[NodeID]*nodeState{"n1": n1}, 5000)
	replicas := topo.Replicas()

	if len(replicas) != 0 {
		t.Fatalf("expected 0 replicas for permanently-down node, got %d", len(replicas))
	}
}

func TestPermanentlyDownNodeViewLivenessDown(t *testing.T) {
	ns := newNodeState("replica", RoleUnknown, LivenessUnknown, true)
	ns.lastProbeAt = time.Now()

	topo := newTestTopology(t, map[NodeID]*nodeState{"n1": ns}, 5000)
	nv := topo.nodeView("n1", ns)

	if nv.Liveness != LivenessDown {
		t.Fatalf("expected LivenessDown for permanently-down node, got %v", nv.Liveness)
	}
}

func TestPermanentlyDownUpdateLivenessUpIgnored(t *testing.T) {
	ns := newNodeState("primary", RolePrimary, LivenessDown, true)
	ns.lastProbeAt = time.Now()

	topo := newTestTopology(t, map[NodeID]*nodeState{"n1": ns}, 10000)

	topo.UpdateLiveness("n1", 0, nil) // probe.LivenessUp == 0

	nv := topo.nodeView("n1", ns)
	if nv.Liveness != LivenessDown {
		t.Fatalf("expected LivenessDown even after UpdateLiveness(Up) on permanently-down node, got %v", nv.Liveness)
	}
}

func TestUpdateSystemIDMismatchPermanentlyDown(t *testing.T) {
	n1 := newNodeState("replica", RoleUnknown, LivenessUnknown, false)

	topo := newTestTopology(t, map[NodeID]*nodeState{"n1": n1}, 5000)

	// First node sets cluster ID.
	if err := topo.UpdateSystemID("n1", "ABC123"); err != nil {
		t.Fatalf("first UpdateSystemID should succeed: %v", err)
	}

	// Second node with different ID.
	n2 := newNodeState("replica", RoleUnknown, LivenessUnknown, false)
	topo.nodes["n2"] = n2

	if err := topo.UpdateSystemID("n2", "DEF456"); err == nil {
		t.Fatal("expected error on system_identifier mismatch")
	}

	if !n2.permanentlyDown {
		t.Fatal("expected n2 to be permanentlyDown after mismatch")
	}
	nv2 := topo.nodeView("n2", n2)
	if nv2.Liveness != LivenessDown {
		t.Fatalf("expected LivenessDown for mismatched node, got %v", nv2.Liveness)
	}
}

// ---------------------------------------------------------------------------
// Replication lag — idle, unknown, fresh, NotApplicable
// ---------------------------------------------------------------------------

func TestLagIdle(t *testing.T) {
	ns := newNodeState("replica", RoleReplica, LivenessUp, false)
	now := time.Now()

	topo := newTestTopology(t, map[NodeID]*nodeState{"n1": ns}, 5000)
	topo.MarkLagIdle("n1", now)

	nv := topo.nodeView("n1", ns)
	if nv.ReplicationLagState != LagStateIdle {
		t.Fatalf("expected LagStateIdle, got %v", nv.ReplicationLagState)
	}
	if nv.ReplicationLagMs != nil {
		t.Fatalf("expected nil LagMs for idle, got %d", *nv.ReplicationLagMs)
	}
}

func TestLagUnknown(t *testing.T) {
	ns := newNodeState("replica", RoleReplica, LivenessUp, false)

	topo := newTestTopology(t, map[NodeID]*nodeState{"n1": ns}, 5000)
	topo.MarkLagUnknown("n1", nil, time.Now())

	nv := topo.nodeView("n1", ns)
	if nv.ReplicationLagState != LagStateUnknown {
		t.Fatalf("expected LagStateUnknown, got %v", nv.ReplicationLagState)
	}
	if nv.ReplicationLagMs != nil {
		t.Fatalf("expected nil LagMs for unknown, got %d", *nv.ReplicationLagMs)
	}
}

func TestLagFresh(t *testing.T) {
	ns := newNodeState("replica", RoleReplica, LivenessUp, false)
	now := time.Now()

	topo := newTestTopology(t, map[NodeID]*nodeState{"n1": ns}, 5000)
	topo.UpdateLag("n1", 42, now)

	nv := topo.nodeView("n1", ns)
	if nv.ReplicationLagState != LagStateFresh {
		t.Fatalf("expected LagStateFresh, got %v", nv.ReplicationLagState)
	}
	if nv.ReplicationLagMs == nil || *nv.ReplicationLagMs != 42 {
		t.Fatalf("expected LagMs=42, got %v", nv.ReplicationLagMs)
	}
}

func TestLagNotApplicableOnPrimary(t *testing.T) {
	ns := newNodeState("primary", RolePrimary, LivenessUp, false)

	topo := newTestTopology(t, map[NodeID]*nodeState{"n1": ns}, 5000)
	nv := topo.nodeView("n1", ns)

	// Primary starts with NotApplicable and never gets overwritten since
	// the probe only calls UpdateLag/MarkLagIdle/MarkLagUnknown for replicas.
	if nv.ReplicationLagState != LagStateNotApplicable {
		t.Fatalf("expected LagStateNotApplicable for primary, got %v", nv.ReplicationLagState)
	}
}

func TestLagStaleAfterMaxAge(t *testing.T) {
	ns := newNodeState("replica", RoleReplica, LivenessUp, false)
	now := time.Now()

	topo := newTestTopology(t, map[NodeID]*nodeState{"n1": ns}, 5000)
	topo.UpdateLag("n1", 10, now.Add(-10*time.Second)) // sample is old

	nv := topo.nodeView("n1", ns)
	if nv.ReplicationLagState != LagStateStale {
		t.Fatalf("expected LagStateStale after max age, got %v", nv.ReplicationLagState)
	}
	if nv.ReplicationLagMs == nil {
		t.Fatalf("expected non-nil LagMs for stale (last known value), got nil")
	}
	if *nv.ReplicationLagMs != 10 {
		t.Fatalf("expected LagMs=10 for stale, got %d", *nv.ReplicationLagMs)
	}
}

// ---------------------------------------------------------------------------
// Node
// ---------------------------------------------------------------------------

func TestNodeNotFound(t *testing.T) {
	topo := newTestTopology(t, nil, 5000)
	_, ok := topo.Node("nonexistent")
	if ok {
		t.Fatal("expected Node() to return false for unknown ID")
	}
}

func TestPoolForNotFound(t *testing.T) {
	topo := newTestTopology(t, nil, 5000)
	_, err := topo.PoolFor("nonexistent")
	if err == nil {
		t.Fatal("expected error from PoolFor for unknown ID")
	}
}
