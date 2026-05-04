package routing

import (
	"testing"
	"time"

	"github.com/grunyas/grunyas/internal/decisions"
	"github.com/grunyas/grunyas/internal/policy"
	"github.com/grunyas/grunyas/internal/topology"
	"go.uber.org/zap"
)

func TestRoundRobinDistributesFairly(t *testing.T) {
	p := &Pipeline{
		logger: zap.NewNop(),
	}
	eligible := []topology.NodeID{"r1", "r2", "r3"}

	counts := map[topology.NodeID]int{}
	for i := 0; i < 99; i++ {
		chosen := p.selectNode(eligible, "read", topology.NodeView{}, false)
		counts[chosen]++
	}

	for _, id := range eligible {
		c := counts[id]
		if c < 30 || c > 36 {
			t.Fatalf("round-robin unfair: node %s got %d out of 99", id, c)
		}
	}
}

func TestSelectNodeWriteFallsThroughWhenPrimaryNotFound(t *testing.T) {
	topo := topology.NewEmpty()
	p := NewPipeline(topo, nil, nil, zap.NewNop())

	eligible := []topology.NodeID{"n1", "n2"}
	chosen := p.selectNode(eligible, "write", topology.NodeView{}, false)
	if chosen != "n1" {
		t.Fatalf("selectNode fallback when Primary() not found: expected n1, got %s", chosen)
	}
}

func TestRoundRobinModuloShrink(t *testing.T) {
	p := &Pipeline{
		logger: zap.NewNop(),
	}

	// Start with 3 eligible, select 5 times
	eligible3 := []topology.NodeID{"r1", "r2", "r3"}
	last := topology.NodeID("")
	for i := 0; i < 5; i++ {
		last = p.selectNode(eligible3, "read", topology.NodeView{}, false)
	}
	// After 5 calls: counter values are 1,2,3,4,5 → selections: r2, r3, r1, r2, r3
	// Counter is now 5

	// Shrink to 2: counter becomes 6, 6%2 = 0 → picks rA
	eligible2 := []topology.NodeID{"rA", "rB"}
	chosen := p.selectNode(eligible2, "read", topology.NodeView{}, false)
	if chosen != "rA" {
		t.Fatalf("modulo on shrink expected rA (6%%2=0), got %s", chosen)
	}
	_ = last
}

func TestPipelineEmitEventPubishedCountIncremented(t *testing.T) {
	p := NewPipeline(topology.NewEmpty(), nil, decisions.NewBus(1, 1), zap.NewNop())
	before := p.PublishedTotal.Load()
	p.emitEvent(decisions.Event{
		Port:     "write",
		PoolMode: "session",
		Source:   "client",
	})
	if p.PublishedTotal.Load() != before+1 {
		t.Fatalf("expected PublishedTotal to increment when bus is non-nil, before=%d after=%d", before, p.PublishedTotal.Load())
	}
}

func TestPipelineEmitEventNilBusIsNoOp(t *testing.T) {
	// This is a no-op test; emitEvent with nil bus returns early
	p := &Pipeline{
		logger: zap.NewNop(),
	}
	p.emitEvent(decisions.Event{})
	// Should not panic
}

func TestPipelineEligibleSetSizeUpdate(t *testing.T) {
	p := &Pipeline{
		logger: zap.NewNop(),
	}
	if p.EligibleSetRead.Load() != 0 {
		t.Fatal("expected EligibleSetRead to start at 0")
	}
	p.EligibleSetRead.Store(3)
	if p.EligibleSetRead.Load() != 3 {
		t.Fatal("expected EligibleSetRead to be 3 after update")
	}
}

func TestPolicyNameHelpers(t *testing.T) {
	templates := policy.NewTemplateSet()
	eng := policy.NewEngine(nil, templates, nil)
	n := policyDefaultHealthNameOrFirst(eng)
	if n != "default-health-filter" {
		t.Fatalf("expected default-health-filter, got %s", n)
	}
	n = policyDefaultLagNameOrFirst(eng)
	if n != "default-lag-filter" {
		t.Fatalf("expected default-lag-filter, got %s", n)
	}
	n = policyDefaultSatNameOrFirst(eng)
	if n != "default-pool-saturation-rejection" {
		t.Fatalf("expected default-pool-saturation-rejection, got %s", n)
	}
}

// TestBusPublishPreservesFields verifies the bus sets the ULID, timestamp, and schema version.
func TestBusPublishMetadataFieldsSet(t *testing.T) {
	bus := decisions.NewBus(10, 10)
	defer bus.Close()

	sub, ok := bus.Subscribe()
	if !ok {
		t.Fatal("failed to subscribe")
	}
	defer sub.Unsub()

	bus.Publish(decisions.Event{
		Port:     "write",
		PoolMode: "session",
		Source:   "client",
	})

	select {
	case msg := <-sub.Ch:
		event := msg.(decisions.Event)
		if event.EventID == "" {
			t.Fatal("expected EventID to be set")
		}
		if len(event.EventID) != 26 {
			t.Fatalf("expected 26-char ULID EventID, got %q (len=%d)", event.EventID, len(event.EventID))
		}
		if event.Timestamp.IsZero() {
			t.Fatal("expected Timestamp to be set")
		}
		if event.SchemaVersion != decisions.SchemaVersion {
			t.Fatalf("expected SchemaVersion %q, got %q", decisions.SchemaVersion, event.SchemaVersion)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("timeout waiting for event")
	}
}
