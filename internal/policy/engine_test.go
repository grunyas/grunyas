package policy

import (
	"testing"
	"time"

	"github.com/grunyas/grunyas/internal/topology"
)

func newTestEngine(instances []Instance) *Engine {
	templates := NewTemplateSet()
	return NewEngine(instances, templates, nil)
}

func TestHysteresisCleanToActive(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := &manualClock{t: start}

	instances := []Instance{
		{
			Name:     "test-lag",
			Template: TemplateLagFilter,
			Scope:    "cluster",
			Parameters: map[string]int{"threshold_ms": 50},
			Timing:   TemplateTiming{DwellMs: 100, ReleaseMs: 100},
		},
	}

	e := newTestEngine(instances)
	e.SetClock(clock.now)

	nv := topology.NodeView{
		ID:                  "replica-1",
		DeclaredRole:        topology.RoleReplica,
		ObservedRole:        topology.RoleReplica,
		Liveness:            topology.LivenessUp,
		ReplicationLagMs:    intPtr(60),
		ReplicationLagState: topology.LagStateFresh,
		LastProbeAt:         start,
		LastLagSampleAt:     start,
	}

	// Step 1: Above threshold → pending
	e.EvaluateNode(nv, false)
	state, _, cond := e.CandidateState("test-lag", "replica-1")
	if state != StatePending {
		t.Fatalf("expected pending after first violation, got %s (cond=%q)", state, cond)
	}

	// Step 2: Still above, dwell not elapsed → still pending
	e.EvaluateNode(nv, false)
	state, _, _ = e.CandidateState("test-lag", "replica-1")
	if state != StatePending {
		t.Fatalf("expected still pending before dwell, got %s", state)
	}

	// Step 3: Advance past dwell, evaluate → active
	clock.advance(101 * time.Millisecond)
	e.EvaluateNode(nv, false)
	state, _, _ = e.CandidateState("test-lag", "replica-1")
	if state != StateActive {
		t.Fatalf("expected active after dwell, got %s", state)
	}

	// Step 4: Recover (lag below threshold) → releasing
	nv.ReplicationLagMs = intPtr(10)
	e.EvaluateNode(nv, false)
	state, _, _ = e.CandidateState("test-lag", "replica-1")
	if state != StateReleasing {
		t.Fatalf("expected releasing after recovery, got %s", state)
	}

	// Step 5: Release not elapsed → still releasing
	e.EvaluateNode(nv, false)
	state, _, _ = e.CandidateState("test-lag", "replica-1")
	if state != StateReleasing {
		t.Fatalf("expected still releasing before release, got %s", state)
	}

	// Step 6: Advance past release → clean
	clock.advance(101 * time.Millisecond)
	e.EvaluateNode(nv, false)
	state, _, _ = e.CandidateState("test-lag", "replica-1")
	if state != StateClean {
		t.Fatalf("expected clean after release, got %s", state)
	}
}

func TestHysteresisFlapping(t *testing.T) {
	clock := &manualClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}

	instances := []Instance{
		{
			Name:    "test-health",
			Template: TemplateHealthFilter,
			Scope:   "cluster",
			Timing:  TemplateTiming{DwellMs: 500, ReleaseMs: 500},
		},
	}

	e := newTestEngine(instances)
	e.SetClock(clock.now)

	nv := topology.NodeView{
		ID: "node-1",
	}

	// Up → clean
	nv.Liveness = topology.LivenessUp
	e.EvaluateNode(nv, false)
	state, _, _ := e.CandidateState("test-health", "node-1")
	if state != StateClean {
		t.Fatalf("expected clean when up, got %s", state)
	}

	// Down → pending
	nv.Liveness = topology.LivenessDown
	e.EvaluateNode(nv, false)
	state, _, _ = e.CandidateState("test-health", "node-1")
	if state != StatePending {
		t.Fatalf("expected pending when first down, got %s", state)
	}

	// Back up within dwell → clean
	nv.Liveness = topology.LivenessUp
	e.EvaluateNode(nv, false)
	state, _, _ = e.CandidateState("test-health", "node-1")
	if state != StateClean {
		t.Fatalf("expected clean after recovery within dwell, got %s", state)
	}
}

func TestHysteresisActiveToReleasingToClean(t *testing.T) {
	clock := &manualClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}

	instances := []Instance{
		{
			Name:     "test-lag",
			Template: TemplateLagFilter,
			Scope:    "cluster",
			Parameters: map[string]int{"threshold_ms": 50},
			Timing:   TemplateTiming{DwellMs: 0, ReleaseMs: 100},
		},
	}

	e := newTestEngine(instances)
	e.SetClock(clock.now)

	nv := topology.NodeView{
		ID:                  "replica-1",
		DeclaredRole:        topology.RoleReplica,
		ObservedRole:        topology.RoleReplica,
		Liveness:            topology.LivenessUp,
		ReplicationLagMs:    intPtr(60),
		ReplicationLagState: topology.LagStateFresh,
		LastProbeAt:         clock.t,
		LastLagSampleAt:     clock.t,
	}

	// Dwell=0: still takes two calls — first goes to pending
	e.EvaluateNode(nv, false)
	state, _, _ := e.CandidateState("test-lag", "replica-1")
	if state != StatePending {
		t.Fatalf("expected pending on first eval, got %s", state)
	}
	// Second call: transition pending→active (dwell=0, any time elapsed satisfies it)
	e.EvaluateNode(nv, false)
	state, _, _ = e.CandidateState("test-lag", "replica-1")
	if state != StateActive {
		t.Fatalf("expected active on second eval with dwell=0, got %s", state)
	}

	// Recover → releasing
	nv.ReplicationLagMs = intPtr(10)
	e.EvaluateNode(nv, false)
	state, _, _ = e.CandidateState("test-lag", "replica-1")
	if state != StateReleasing {
		t.Fatalf("expected releasing after recovery, got %s", state)
	}

	// Advance past release → clean
	clock.advance(101 * time.Millisecond)
	e.EvaluateNode(nv, false)
	state, _, _ = e.CandidateState("test-lag", "replica-1")
	if state != StateClean {
		t.Fatalf("expected clean after release, got %s", state)
	}
}

func TestPendingEligibility(t *testing.T) {
	// During pending, the node is still eligible for routing.
	clock := &manualClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}

	instances := []Instance{
		{
			Name:     "test-lag",
			Template: TemplateLagFilter,
			Scope:    "cluster",
			Parameters: map[string]int{"threshold_ms": 50},
			Timing:   TemplateTiming{DwellMs: 5000, ReleaseMs: 5000},
		},
	}

	e := newTestEngine(instances)
	e.SetClock(clock.now)

	nv := topology.NodeView{
		ID:                  "replica-1",
		DeclaredRole:        topology.RoleReplica,
		ObservedRole:        topology.RoleReplica,
		Liveness:            topology.LivenessUp,
		ReplicationLagMs:    intPtr(60),
		ReplicationLagState: topology.LagStateFresh,
		LastProbeAt:         clock.t,
		LastLagSampleAt:     clock.t,
	}

	// First violation → pending
	e.EvaluateNode(nv, false)
	eligible, reason := e.EvaluatePolicyState("test-lag", "replica-1")
	if !eligible {
		t.Fatalf("expected eligible during pending, got reason=%q", reason)
	}

	// Advance past dwell while condition persists → active
	clock.advance(5 * time.Second)
	e.EvaluateNode(nv, false)
	eligible, reason = e.EvaluatePolicyState("test-lag", "replica-1")
	if eligible {
		t.Fatalf("expected ineligible during active, got reason=%q", reason)
	}

	// Recover → releasing (still ineligible, lastCondition cleared)
	nv.ReplicationLagMs = intPtr(10)
	e.EvaluateNode(nv, false)
	eligible, reason = e.EvaluatePolicyState("test-lag", "replica-1")
	if eligible {
		t.Fatalf("expected ineligible during releasing, got reason=%q", reason)
	}

	// Advance past release → clean (eligible again)
	clock.advance(5 * time.Second)
	e.EvaluateNode(nv, false)
	eligible, _ = e.EvaluatePolicyState("test-lag", "replica-1")
	if !eligible {
		t.Fatal("expected eligible after release")
	}
}

func intPtr(v int64) *int64 {
	return &v
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
