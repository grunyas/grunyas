package policy

import "github.com/grunyas/grunyas/internal/topology"

// Reason vocabulary — rule-3 committed. Names are surfaced in decision events
// and must not silently change when enum values are renamed.
const (
	ReasonLivenessUp        = "liveness:up"
	ReasonLivenessDegraded  = "liveness:degraded"
	ReasonLivenessDown      = "liveness:down"
	ReasonLivenessUnknown   = "liveness:unknown"
	ReasonLagAboveThreshold = "lag:above_threshold"
	ReasonLagUnknown        = "lag:unknown"
	ReasonLagIdle           = "lag:idle"
	ReasonPoolSaturated     = "pool:saturated"
)

type healthEvaluator struct{}

var livenessReasons = map[topology.Liveness]string{
	topology.LivenessUp:        ReasonLivenessUp,
	topology.LivenessDegraded:  ReasonLivenessDegraded,
	topology.LivenessDown:      ReasonLivenessDown,
	topology.LivenessUnknown:   ReasonLivenessUnknown,
}

func (e *healthEvaluator) Evaluate(nv topology.NodeView, params map[string]int) (bool, string) {
	if nv.Liveness != topology.LivenessUp {
		if reason, ok := livenessReasons[nv.Liveness]; ok {
			return true, reason
		}
		return true, ReasonLivenessUnknown
	}
	return false, ""
}

type lagEvaluator struct{}

func (e *lagEvaluator) Evaluate(nv topology.NodeView, params map[string]int) (bool, string) {
	if nv.ObservedRole != topology.RoleReplica {
		return false, ""
	}
	thresholdMs := 100
	if v, ok := params["threshold_ms"]; ok {
		thresholdMs = v
	}

	switch nv.ReplicationLagState {
	case topology.LagStateFresh:
		if nv.ReplicationLagMs != nil && *nv.ReplicationLagMs > int64(thresholdMs) {
			return true, ReasonLagAboveThreshold
		}
		return false, ""
	case topology.LagStateUnknown, topology.LagStateStale:
		return true, ReasonLagUnknown
	case topology.LagStateIdle:
		return true, ReasonLagIdle
	default:
		return false, ""
	}
}

func NewTemplateSet() *templateSet {
	return &templateSet{
		health: &healthEvaluator{},
		lag:    &lagEvaluator{},
	}
}
