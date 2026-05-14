package routing

import (
	"context"
	"testing"
	"time"

	"github.com/grunyas/grunyas/internal/classifier"
	"github.com/grunyas/grunyas/internal/decisions"
	"github.com/grunyas/grunyas/internal/policy"
	"github.com/grunyas/grunyas/internal/server/types"
	"github.com/grunyas/grunyas/internal/topology"
	"github.com/jackc/pgx/v5/pgproto3"
	"go.uber.org/zap"
)

// mockPoolMgr is a stub pool.Manager for routing tests.
type mockPoolMgr struct{}

func (m *mockPoolMgr) AcquireDbConnection() (types.UpstreamClientInterface, error) {
	return &mockUpstream{}, nil
}
func (m *mockPoolMgr) PoolStats() types.PoolStats { return types.PoolStats{} }
func (m *mockPoolMgr) Close()                     {}

// mockUpstream is a stub upstream client for routing tests.
type mockUpstream struct{}

func (m *mockUpstream) Send(...pgproto3.FrontendMessage) error { return nil }
func (m *mockUpstream) Flush() error                           { return nil }
func (m *mockUpstream) Receive(ctx context.Context) (pgproto3.BackendMessage, error) {
	return nil, nil
}
func (m *mockUpstream) TxStatus() byte    { return 'I' }
func (m *mockUpstream) Release() error    { return nil }
func (m *mockUpstream) Kill() error       { return nil }

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

func TestLeaseCompatPortReadRoutesToReplica(t *testing.T) {
	// M4: compat port with SELECT routes to a replica
	topo := topology.NewTestTopology()
	topo.AddTestNode("primary-1", "primary", topology.RolePrimary, topology.LivenessUp)
	topo.AddTestNode("replica-1", "replica", topology.RoleReplica, topology.LivenessUp)
	topo.SetTestNodePool("primary-1", &mockPoolMgr{})
	topo.SetTestNodePool("replica-1", &mockPoolMgr{})

	p := NewPipeline(topo, nil, nil, zap.NewNop())
	result, _ := p.Lease(LeaseRequest{Port: "compat", PoolMode: "transaction", LeaseType: "transaction", SQL: "SELECT 1"})

	if result.Error != nil {
		t.Fatalf("unexpected error: %v", result.Error)
	}
	if result.Decision.Outcome.Kind != "leased" {
		t.Fatalf("expected leased, got %s", result.Decision.Outcome.Kind)
	}
	if result.NodeID != "replica-1" {
		t.Fatalf("expected replica-1, got %s", result.NodeID)
	}
	if result.Decision.Classification.Type != "read" {
		t.Fatalf("expected read classification, got %s", result.Decision.Classification.Type)
	}
	if result.Decision.Consistency == nil || result.Decision.Consistency.Mode != "bounded_staleness" {
		t.Fatalf("expected bounded_staleness consistency, got %+v", result.Decision.Consistency)
	}
}

func TestLeaseCompatPortWriteRoutesToPrimary(t *testing.T) {
	// M4: compat port with INSERT routes to the primary
	topo := topology.NewTestTopology()
	topo.AddTestNode("primary-1", "primary", topology.RolePrimary, topology.LivenessUp)
	topo.AddTestNode("replica-1", "replica", topology.RoleReplica, topology.LivenessUp)
	topo.SetTestNodePool("primary-1", &mockPoolMgr{})
	topo.SetTestNodePool("replica-1", &mockPoolMgr{})

	p := NewPipeline(topo, nil, nil, zap.NewNop())
	result, _ := p.Lease(LeaseRequest{Port: "compat", PoolMode: "transaction", LeaseType: "transaction", SQL: "INSERT INTO foo VALUES (1)"})

	if result.Error != nil {
		t.Fatalf("unexpected error: %v", result.Error)
	}
	if result.Decision.Outcome.Kind != "leased" {
		t.Fatalf("expected leased, got %s", result.Decision.Outcome.Kind)
	}
	if result.NodeID != "primary-1" {
		t.Fatalf("expected primary-1, got %s", result.NodeID)
	}
	if result.Decision.Classification.Type != "write" {
		t.Fatalf("expected write classification, got %s", result.Decision.Classification.Type)
	}
	if result.Decision.Consistency == nil || result.Decision.Consistency.Mode != "linearizable" {
		t.Fatalf("expected linearizable consistency, got %+v", result.Decision.Consistency)
	}
}

func TestLeaseCompatPortReadEmptyEligibleSetFallsBackToPrimary(t *testing.T) {
	// M4: compat port read with no eligible replica falls back to primary
	topo := topology.NewTestTopology()
	topo.AddTestNode("primary-1", "primary", topology.RolePrimary, topology.LivenessUp)
	// Replica is down — empty eligible set
	topo.AddTestNode("replica-1", "replica", topology.RoleReplica, topology.LivenessDown)
	topo.SetTestNodePool("primary-1", &mockPoolMgr{})
	topo.SetTestNodePool("replica-1", &mockPoolMgr{})

	p := NewPipeline(topo, nil, nil, zap.NewNop())
	result, _ := p.Lease(LeaseRequest{Port: "compat", PoolMode: "transaction", LeaseType: "transaction", SQL: "SELECT 1"})

	if result.Error != nil {
		t.Fatalf("unexpected error: %v", result.Error)
	}
	if result.Decision.Outcome.Kind != "fallback" {
		t.Fatalf("expected fallback, got %s", result.Decision.Outcome.Kind)
	}
	if result.NodeID != "primary-1" {
		t.Fatalf("expected primary-1 fallback, got %s", result.NodeID)
	}
	if result.Decision.Outcome.Reason != "empty_eligible_set" {
		t.Fatalf("expected empty_eligible_set reason, got %s", result.Decision.Outcome.Reason)
	}
	if result.Decision.Consistency == nil || result.Decision.Consistency.Mode != "linearizable" {
		t.Fatalf("expected linearizable consistency for fallback, got %+v", result.Decision.Consistency)
	}
}

func TestLeaseCompatPortWriteNoPrimary(t *testing.T) {
	// M4: compat port write with no primary returns no_primary, not no_eligible_replica
	topo := topology.NewTestTopology()
	// Primary is down
	topo.AddTestNode("primary-1", "primary", topology.RolePrimary, topology.LivenessDown)
	topo.AddTestNode("replica-1", "replica", topology.RoleReplica, topology.LivenessUp)
	topo.SetTestNodePool("primary-1", &mockPoolMgr{})
	topo.SetTestNodePool("replica-1", &mockPoolMgr{})

	p := NewPipeline(topo, nil, nil, zap.NewNop())
	result, _ := p.Lease(LeaseRequest{Port: "compat", PoolMode: "transaction", LeaseType: "transaction", SQL: "UPDATE foo SET bar = 1"})

	if result.Error == nil {
		t.Fatal("expected error when no primary is available")
	}
	if result.Decision.Outcome.Kind != "rejected" {
		t.Fatalf("expected rejected, got %s", result.Decision.Outcome.Kind)
	}
	if result.Decision.Outcome.Reason != "no_primary" {
		t.Fatalf("expected no_primary reason, got %s", result.Decision.Outcome.Reason)
	}
	if result.Decision.Outcome.SQLState != "57P03" {
		t.Fatalf("expected 57P03, got %s", result.Decision.Outcome.SQLState)
	}
	if result.Decision.Consistency == nil || result.Decision.Consistency.Mode != "linearizable" {
		t.Fatalf("expected linearizable consistency, got %+v", result.Decision.Consistency)
	}
}

func TestLeaseCompatPortReadNoNodes(t *testing.T) {
	// Empty topology: compat port read hits no_nodes early return
	topo := topology.NewEmpty()
	p := NewPipeline(topo, nil, nil, zap.NewNop())
	result, _ := p.Lease(LeaseRequest{Port: "compat", PoolMode: "transaction", LeaseType: "transaction", SQL: "SELECT 1"})
	if result.Error == nil {
		t.Fatal("expected error for empty topology")
	}
	if result.Decision.Outcome.Reason != "no_nodes" {
		t.Fatalf("expected no_nodes, got %s", result.Decision.Outcome.Reason)
	}
}

func TestLeaseDecisionsTotalIncremented(t *testing.T) {
	topo := topology.NewEmpty()
	p := NewPipeline(topo, nil, nil, zap.NewNop())
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

	candidates := p.filterCandidates(nodes, "read", "", false)
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
	eligible := eligibleNodes(candidates)
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
	eligible := eligibleNodes(candidates)
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
	chosen := p.selectNode(eligible, "write", topology.NodeView{}, false)
	if chosen != "alpha" {
		t.Fatalf("expected alpha (lowest), got %s", chosen)
	}
}

func TestSelectNodeReadRoundRobin(t *testing.T) {
	p := NewPipeline(nil, nil, nil, zap.NewNop())
	eligible := []topology.NodeID{"r1", "r2", "r3", "r4"}
	seen := map[topology.NodeID]int{}
	for i := 0; i < 40; i++ {
		chosen := p.selectNode(eligible, "read", topology.NodeView{}, false)
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
	candidates := p.filterCandidates(nodes, "read", "", false)
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

	candidates := p.filterCandidates(nodes, "read", "", false)
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
	candidates = p.filterCandidates(nodes, "read", "", false)
	if candidates[0].Eligible {
		t.Fatalf("expected ineligible during active, got reasons: %v", candidates[0].Reasons)
	}

	// Recover → releasing (still ineligible)
	nv.ReplicationLagMs = intPtr2(10)
	eng.EvaluateNode(nv, false)
	candidates = p.filterCandidates(nodes, "read", "", false)
	if candidates[0].Eligible {
		t.Fatalf("expected ineligible during releasing, got reasons: %v", candidates[0].Reasons)
	}

	// Advance past release → clean (eligible again)
	clock.advance(6 * time.Second)
	eng.EvaluateNode(nv, false)
	candidates = p.filterCandidates(nodes, "read", "", false)
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

func TestFilterCandidatesCompatPortReadFiltersReplicas(t *testing.T) {
	p := NewPipeline(nil, nil, nil, zap.NewNop())
	nodes := []topology.NodeView{
		{
			ID:           "primary-1",
			DeclaredRole: topology.RolePrimary,
			ObservedRole: topology.RolePrimary,
			Liveness:     topology.LivenessUp,
		},
		{
			ID:           "replica-1",
			DeclaredRole: topology.RoleReplica,
			ObservedRole: topology.RoleReplica,
			Liveness:     topology.LivenessUp,
		},
	}
	// Compat port + read classification → replica should be eligible, primary should not
	candidates := p.filterCandidates(nodes, "compat", classifier.TypeRead, false)
	if len(candidates) != 2 {
		t.Fatalf("expected 2 candidates, got %d", len(candidates))
	}
	if candidates[0].Eligible {
		t.Fatal("primary should be ineligible for compat+read")
	}
	if candidates[0].Reasons[0] != "role:not_replica" {
		t.Fatalf("expected role:not_replica, got %v", candidates[0].Reasons)
	}
	if !candidates[1].Eligible {
		t.Fatalf("replica should be eligible for compat+read, got %v", candidates[1].Reasons)
	}
}

func TestFilterCandidatesCompatPortWriteFiltersPrimary(t *testing.T) {
	p := NewPipeline(nil, nil, nil, zap.NewNop())
	nodes := []topology.NodeView{
		{
			ID:           "primary-1",
			DeclaredRole: topology.RolePrimary,
			ObservedRole: topology.RolePrimary,
			Liveness:     topology.LivenessUp,
		},
		{
			ID:           "replica-1",
			DeclaredRole: topology.RoleReplica,
			ObservedRole: topology.RoleReplica,
			Liveness:     topology.LivenessUp,
		},
	}
	// Compat port + write classification → primary should be eligible, replica should not
	candidates := p.filterCandidates(nodes, "compat", classifier.TypeWrite, false)
	if len(candidates) != 2 {
		t.Fatalf("expected 2 candidates, got %d", len(candidates))
	}
	if !candidates[0].Eligible {
		t.Fatalf("primary should be eligible for compat+write, got %v", candidates[0].Reasons)
	}
	if candidates[1].Eligible {
		t.Fatal("replica should be ineligible for compat+write")
	}
	if candidates[1].Reasons[0] != "role:not_primary" {
		t.Fatalf("expected role:not_primary, got %v", candidates[1].Reasons)
	}
}

func TestLeaseCompatPortClassifiesAndRoutes(t *testing.T) {
	// M4: compat port classifies SQL and routes accordingly.
	topo := topology.NewTestTopology()
	topo.AddTestNode("primary-1", "primary", topology.RolePrimary, topology.LivenessUp)
	topo.AddTestNode("replica-1", "replica", topology.RoleReplica, topology.LivenessUp)
	topo.SetTestNodePool("primary-1", &mockPoolMgr{})
	topo.SetTestNodePool("replica-1", &mockPoolMgr{})

	p := NewPipeline(topo, nil, nil, zap.NewNop())

	// SELECT → read → routes to replica
	result, _ := p.Lease(LeaseRequest{Port: "compat", PoolMode: "transaction", LeaseType: "transaction", SQL: "SELECT 1"})
	if result.Decision.Classification.Type != "read" {
		t.Fatalf("expected read classification, got %s", result.Decision.Classification.Type)
	}
	if result.Decision.Outcome.Kind != "leased" {
		t.Fatalf("expected leased outcome, got %s", result.Decision.Outcome.Kind)
	}
	if result.NodeID != "replica-1" {
		t.Fatalf("expected replica-1, got %s", result.NodeID)
	}
	if result.Decision.Consistency == nil || result.Decision.Consistency.Mode != "bounded_staleness" {
		t.Fatalf("expected bounded_staleness, got %+v", result.Decision.Consistency)
	}

	// INSERT → write → routes to primary
	result, _ = p.Lease(LeaseRequest{Port: "compat", PoolMode: "transaction", LeaseType: "transaction", SQL: "INSERT INTO foo VALUES (1)"})
	if result.Decision.Classification.Type != "write" {
		t.Fatalf("expected write classification, got %s", result.Decision.Classification.Type)
	}
	if result.Decision.Outcome.Kind != "leased" {
		t.Fatalf("expected leased outcome, got %s", result.Decision.Outcome.Kind)
	}
	if result.NodeID != "primary-1" {
		t.Fatalf("expected primary-1, got %s", result.NodeID)
	}
	if result.Decision.Consistency == nil || result.Decision.Consistency.Mode != "linearizable" {
		t.Fatalf("expected linearizable, got %+v", result.Decision.Consistency)
	}

	// WITH → write → routes to primary
	result, _ = p.Lease(LeaseRequest{Port: "compat", PoolMode: "transaction", LeaseType: "transaction", SQL: "WITH foo AS (SELECT 1) SELECT * FROM foo"})
	if result.Decision.Classification.Type != "write" {
		t.Fatalf("expected write classification for WITH, got %s", result.Decision.Classification.Type)
	}
	if result.NodeID != "primary-1" {
		t.Fatalf("expected primary-1 for WITH, got %s", result.NodeID)
	}
}

func intPtr2(v int64) *int64 {
	return &v
}

func TestLeaseCompatPortPostWriteWindowNarrowsToPrimary(t *testing.T) {
	// M4 §2/§4: when PostWriteWindowActive is true, a read-classified
	// statement must narrow the eligible set to the primary alone.
	topo := topology.NewTestTopology()
	topo.AddTestNode("primary-1", "primary", topology.RolePrimary, topology.LivenessUp)
	topo.AddTestNode("replica-1", "replica", topology.RoleReplica, topology.LivenessUp)
	topo.SetTestNodePool("primary-1", &mockPoolMgr{})
	topo.SetTestNodePool("replica-1", &mockPoolMgr{})

	p := NewPipeline(topo, nil, nil, zap.NewNop())
	result, _ := p.Lease(LeaseRequest{
		Port:                  "compat",
		PoolMode:              "transaction",
		LeaseType:             "transaction",
		SQL:                   "SELECT 1",
		PostWriteWindowActive: true,
	})

	if result.Error != nil {
		t.Fatalf("unexpected error: %v", result.Error)
	}
	if result.Decision.Outcome.Kind != "leased" {
		t.Fatalf("expected leased, got %s", result.Decision.Outcome.Kind)
	}
	if result.NodeID != "primary-1" {
		t.Fatalf("expected primary-1 (window narrowed to primary), got %s", result.NodeID)
	}
	if result.Decision.Consistency == nil || result.Decision.Consistency.Mode != "post_write_window" {
		t.Fatalf("expected post_write_window consistency, got %+v", result.Decision.Consistency)
	}
}

// TestRejectCompatReclassificationCounters verifies that
// RejectCompatReclassification increments DecisionsTotal, DecisionsRejected,
// CompatReclassificationRejections, and PublishedTotal, and emits an event
// with the correct outcome shape.
func TestRejectCompatReclassificationCounters(t *testing.T) {
	bus := decisions.NewBus(10, 10)
	p := NewPipeline(topology.NewEmpty(), nil, bus, zap.NewNop())

	prevTotal := p.DecisionsTotal.Load()
	prevRejected := p.DecisionsRejected.Load()
	prevCompat := p.CompatReclassificationRejections.Load()
	prevPublished := p.PublishedTotal.Load()

	event := p.RejectCompatReclassification("INSERT INTO foo VALUES (1)", "transaction")

	if n := p.DecisionsTotal.Load(); n != prevTotal+1 {
		t.Fatalf("DecisionsTotal: expected %d, got %d", prevTotal+1, n)
	}
	if n := p.DecisionsRejected.Load(); n != prevRejected+1 {
		t.Fatalf("DecisionsRejected: expected %d, got %d", prevRejected+1, n)
	}
	if n := p.CompatReclassificationRejections.Load(); n != prevCompat+1 {
		t.Fatalf("CompatReclassificationRejections: expected %d, got %d", prevCompat+1, n)
	}
	if n := p.PublishedTotal.Load(); n != prevPublished+1 {
		t.Fatalf("PublishedTotal: expected %d, got %d", prevPublished+1, n)
	}

	if event.Outcome.Kind != "rejected" {
		t.Fatalf("expected outcome.kind=rejected, got %q", event.Outcome.Kind)
	}
	if event.Outcome.SQLState != "25006" {
		t.Fatalf("expected outcome.sqlstate=25006, got %q", event.Outcome.SQLState)
	}
	if event.Outcome.Reason != "compat:reclassification" {
		t.Fatalf("expected outcome.reason=compat:reclassification, got %q", event.Outcome.Reason)
	}
}
