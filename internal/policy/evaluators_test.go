package policy

import (
	"testing"

	"github.com/grunyas/grunyas/internal/topology"
)

func TestReasonVocabularyIsHardcoded(t *testing.T) {
	templates := NewTemplateSet()

	nvUp := topology.NodeView{Liveness: topology.LivenessUp}
	nvDown := topology.NodeView{Liveness: topology.LivenessDown}
	nvDegraded := topology.NodeView{Liveness: topology.LivenessDegraded}
	nvUnknown := topology.NodeView{Liveness: topology.LivenessUnknown}

	_, r := templates.health.Evaluate(nvUp, nil)
	if r != "" {
		t.Fatalf("health up should return empty reason, got %q", r)
	}
	_, r = templates.health.Evaluate(nvDown, nil)
	if r != ReasonLivenessDown {
		t.Fatalf("health down reason mismatch: got %q, want %q", r, ReasonLivenessDown)
	}
	_, r = templates.health.Evaluate(nvDegraded, nil)
	if r != ReasonLivenessDegraded {
		t.Fatalf("health degraded reason mismatch: got %q, want %q", r, ReasonLivenessDegraded)
	}
	_, r = templates.health.Evaluate(nvUnknown, nil)
	if r != ReasonLivenessUnknown {
		t.Fatalf("health unknown reason mismatch: got %q, want %q", r, ReasonLivenessUnknown)
	}
}

func TestLagReasonVocabulary(t *testing.T) {
	templates := NewTemplateSet()

	nvAboveThreshold := topology.NodeView{
		DeclaredRole:        topology.RoleReplica,
		ObservedRole:        topology.RoleReplica,
		ReplicationLagMs:    intPtr(200),
		ReplicationLagState: topology.LagStateFresh,
	}
	nvUnknown := topology.NodeView{
		DeclaredRole:        topology.RoleReplica,
		ObservedRole:        topology.RoleReplica,
		ReplicationLagState: topology.LagStateUnknown,
	}
	nvIdle := topology.NodeView{
		DeclaredRole:        topology.RoleReplica,
		ObservedRole:        topology.RoleReplica,
		ReplicationLagState: topology.LagStateIdle,
	}

	_, r := templates.lag.Evaluate(nvAboveThreshold, map[string]int{"threshold_ms": 100})
	if r != ReasonLagAboveThreshold {
		t.Fatalf("lag above threshold reason mismatch: got %q, want %q", r, ReasonLagAboveThreshold)
	}
	_, r = templates.lag.Evaluate(nvUnknown, nil)
	if r != ReasonLagUnknown {
		t.Fatalf("lag unknown reason mismatch: got %q, want %q", r, ReasonLagUnknown)
	}
	_, r = templates.lag.Evaluate(nvIdle, nil)
	if r != ReasonLagIdle {
		t.Fatalf("lag idle reason mismatch: got %q, want %q", r, ReasonLagIdle)
	}
}
