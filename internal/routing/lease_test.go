package routing

import (
	"context"
	"testing"
	"time"

	"github.com/grunyas/grunyas/internal/decisions"
	"github.com/grunyas/grunyas/internal/policy"
	"github.com/grunyas/grunyas/internal/topology"
	"go.uber.org/zap"
)

func TestLeaseNoNodes(t *testing.T) {
	topo := topology.NewEmpty()
	p := NewPipeline(topo, nil, nil, zap.NewNop())

	result, _ := p.Lease(LeaseRequest{Port: "write", PoolMode: "session", LeaseType: "session"})
	if result.Error == nil {
		t.Fatal("expected error for empty topology")
	}

	// Check the decision event
	if result.Decision.Outcome.Kind != "rejected" {
		t.Fatalf("expected rejected outcome, got %s", result.Decision.Outcome.Kind)
	}
	if result.Decision.Outcome.SQLState != "57P03" {
		t.Fatalf("expected 57P03, got %s", result.Decision.Outcome.SQLState)
	}
	if result.Decision.Outcome.Reason != "no_nodes" {
		t.Fatalf("expected no_nodes reason, got %s", result.Decision.Outcome.Reason)
	}
}

func TestLeaseCompatPortStub(t *testing.T) {
	p := NewPipeline(nil, nil, nil, zap.NewNop())
	result, _ := p.Lease(LeaseRequest{Port: "compat", PoolMode: "transaction", LeaseType: "transaction"})
	if result.Error == nil {
		t.Fatal("expected error for compat port in M3")
	}
	if result.Decision.Outcome.Kind != "rejected" {
		t.Fatalf("expected rejected, got %s", result.Decision.Outcome.Kind)
	}
	if result.Decision.Outcome.SQLState != "0A000" {
		t.Fatalf("expected 0A000, got %s", result.Decision.Outcome.SQLState)
	}
	if result.Decision.Outcome.Reason != "compat_port:m4_stub" {
		t.Fatalf("expected compat_port:m4_stub reason, got %s", result.Decision.Outcome.Reason)
	}
}

func TestLeaseDecisionsTotalIncremented(t *testing.T) {
	p := NewPipeline(nil, nil, nil, zap.NewNop())
	before := p.DecisionsTotal.Load()
	_, _ = p.Lease(LeaseRequest{Port: "compat", PoolMode: "transaction", LeaseType: "transaction"})
	if p.DecisionsTotal.Load() != before+1 {
		t.Fatalf("expected DecisionsTotal to increment, before=%d after=%d", before, p.DecisionsTotal.Load())
	}
}

func TestFilterCandidatesReasonsInOrder(t *testing.T) {
	p := NewPipeline(nil, nil, nil, zap.NewNop())

	nodes := []topology.NodeView{
		{
			ID:           "bad-role",
			DeclaredRole: topology.RolePrimary,
			ObservedRole: topology.RolePrimary,
			Liveness:     topology.LivenessUp,
		},
		{
			ID:           "bad-system-id",
			DeclaredRole: topology.RoleReplica,
			ObservedRole: topology.RoleReplica,
			Liveness:     topology.LivenessUp,
			SystemID:     "mismatch",
		},
	}

	candidates := p.filterCandidates(nodes, "read")
	if len(candidates) != 2 {
		t.Fatalf("expected 2 candidates, got %d", len(candidates))
	}

	// bad-role: role:not_replica
	if candidates[0].Eligible {
		t.Fatal("bad-role should be ineligible (observed primary on read port)")
	}
	if len(candidates[0].Reasons) != 1 || candidates[0].Reasons[0] != "role:not_replica" {
		t.Fatalf("bad-role reasons: %v", candidates[0].Reasons)
	}

	// bad-system-id: system_id:mismatch + role:passed (replica) → only mismatch
	if candidates[1].Eligible {
		t.Fatal("bad-system-id should be ineligible")
	}
	if len(candidates[1].Reasons) != 1 || candidates[1].Reasons[0] != "system_id:mismatch" {
		t.Fatalf("bad-system-id reasons: %v", candidates[1].Reasons)
	}
}

func TestEligibleNodesEmpty(t *testing.T) {
	candidates := []decisions.Candidate{
		{NodeID: "a", Eligible: false, Reasons: []string{"role:not_replica"}},
		{NodeID: "b", Eligible: false, Reasons: []string{"liveness:down"}},
	}
	eligible := eligibleNodes(candidates, "read")
	if len(eligible) != 0 {
		t.Fatalf("expected 0 eligible, got %d", len(eligible))
	}
}

func TestEligibleNodesMixed(t *testing.T) {
	candidates := []decisions.Candidate{
		{NodeID: "a", Eligible: false, Reasons: []string{"role:not_replica"}},
		{NodeID: "b", Eligible: true},
		{NodeID: "c", Eligible: false, Reasons: []string{"liveness:down"}},
	}
	eligible := eligibleNodes(candidates, "read")
	if len(eligible) != 1 || eligible[0] != "b" {
		t.Fatalf("expected [b], got %v", eligible)
	}
}

func TestObservationLoopStartsAndStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	p := NewPipeline(nil, nil, nil, zap.NewNop())
	p.StartObservationLoop(ctx, 100)
	cancel()
	// Should not panic; the goroutine exits on ctx.Done()
	time.Sleep(50 * time.Millisecond)
}

func TestSelectNodeWriteFallback(t *testing.T) {
	// selectNode with an empty topology: Primary() returns false, falls through to
	// deterministic lowest-NodeID pick as a safety net.
	p := NewPipeline(topology.NewEmpty(), nil, nil, zap.NewNop())
	eligible := []topology.NodeID{"beta", "alpha"}
	chosen := p.selectNode(eligible, "write")
	if chosen != "alpha" {
		t.Fatalf("expected alpha (lowest), got %s", chosen)
	}
}

func TestSelectNodeReadRoundRobin(t *testing.T) {
	p := NewPipeline(nil, nil, nil, zap.NewNop())
	eligible := []topology.NodeID{"r1", "r2", "r3", "r4"}
	seen := map[topology.NodeID]int{}
	for i := 0; i < 40; i++ {
		chosen := p.selectNode(eligible, "read")
		seen[chosen]++
	}
	for _, id := range eligible {
		if seen[id] != 10 {
			t.Fatalf("round-robin: expected 10 for %s, got %d", id, seen[id])
		}
	}
}

func TestFilterCandidatesReadPortRoleRejection(t *testing.T) {
	// On read port, nodes with observed role != replica are filtered out.
	p := NewPipeline(nil, nil, nil, zap.NewNop())
	nodes := []topology.NodeView{
		{
			ID:           "primary-1",
			DeclaredRole: topology.RolePrimary,
			ObservedRole: topology.RolePrimary,
			Liveness:     topology.LivenessUp,
		},
	}
	candidates := p.filterCandidates(nodes, "read")
	if len(candidates) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(candidates))
	}
	if candidates[0].Eligible {
		t.Fatal("primary should be ineligible on read port")
	}
	if len(candidates[0].Reasons) < 1 || candidates[0].Reasons[0] != "role:not_replica" {
		t.Fatalf("expected role:not_replica, got %v", candidates[0].Reasons)
	}
}

func TestPipelineHysteresisPendingEligible(t *testing.T) {
	// Verify that during pending (within dwell), the routing pipeline
	// marks the node as eligible.
	clock := &manualClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	templates := policy.NewTemplateSet()
	instances := []policy.Instance{
		{
			Name:     "default-lag-filter",
			Template: policy.TemplateLagFilter,
			Scope:    "cluster",
			Parameters: map[string]int{"threshold_ms": 50},
			Timing:   policy.TemplateTiming{DwellMs: 5000, ReleaseMs: 5000},
		},
	}
	eng := policy.NewEngine(instances, templates, nil, nil)
	eng.SetClock(clock.now)

	p := NewPipeline(topology.NewEmpty(), eng, nil, zap.NewNop())

	nv := topology.NodeView{
		ID:                  "replica-1",
		DeclaredRole:        topology.RoleReplica,
		ObservedRole:        topology.RoleReplica,
		Liveness:            topology.LivenessUp,
		ReplicationLagMs:    intPtr2(60),
		ReplicationLagState: topology.LagStateFresh,
	}

	nodes := []topology.NodeView{nv}
	// Evaluate: lag above threshold → policy goes pending
	eng.EvaluateNode(nv, false)

	candidates := p.filterCandidates(nodes, "read")
	if len(candidates) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(candidates))
	}
	// During pending, node is eligible
	if !candidates[0].Eligible {
		t.Fatalf("expected eligible during pending, got reasons: %v", candidates[0].Reasons)
	}

	// Advance past dwell → policy goes active
	clock.advance(6 * time.Second)
	eng.EvaluateNode(nv, false)
	candidates = p.filterCandidates(nodes, "read")
	if candidates[0].Eligible {
		t.Fatalf("expected ineligible during active, got reasons: %v", candidates[0].Reasons)
	}

	// Recover → releasing (still ineligible)
	nv.ReplicationLagMs = intPtr2(10)
	eng.EvaluateNode(nv, false)
	candidates = p.filterCandidates(nodes, "read")
	if candidates[0].Eligible {
		t.Fatalf("expected ineligible during releasing, got reasons: %v", candidates[0].Reasons)
	}

	// Advance past release → clean (eligible again)
	clock.advance(6 * time.Second)
	eng.EvaluateNode(nv, false)
	candidates = p.filterCandidates(nodes, "read")
	if !candidates[0].Eligible {
		t.Fatalf("expected eligible after release, got reasons: %v", candidates[0].Reasons)
	}
}

type manualClock struct {
	t time.Time
}

func (c *manualClock) now() time.Time {
	return c.t
}

func (c *manualClock) advance(d time.Duration) {
	c.t = c.t.Add(d)
}

func intPtr2(v int64) *int64 {
	return &v
}
