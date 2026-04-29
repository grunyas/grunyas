// Package topology provides a cluster-topology view of declared Postgres nodes,
// their observed state, and per-node pool managers.
package topology

import (
	"time"
)

type NodeID string

type Role int

const (
	RolePrimary Role = iota
	RoleReplica
	RoleUnknown
)

func (r Role) String() string {
	switch r {
	case RolePrimary:
		return "primary"
	case RoleReplica:
		return "replica"
	default:
		return "unknown"
	}
}

type Liveness int

const (
	LivenessUp       Liveness = iota
	LivenessDegraded
	LivenessDown
	LivenessUnknown
)

func (l Liveness) String() string {
	switch l {
	case LivenessUp:
		return "up"
	case LivenessDegraded:
		return "degraded"
	case LivenessDown:
		return "down"
	default:
		return "unknown"
	}
}

type SystemID string

type LagState int

const (
	LagStateFresh        LagState = iota
	LagStateIdle
	LagStateStale
	LagStateUnknown
	LagStateNotApplicable
)

func (ls LagState) String() string {
	switch ls {
	case LagStateFresh:
		return "fresh"
	case LagStateIdle:
		return "idle"
	case LagStateStale:
		return "stale"
	case LagStateUnknown:
		return "unknown"
	default:
		return "not_applicable"
	}
}

type NodeView struct {
	ID           NodeID
	Host         string
	Port         uint16
	DeclaredRole Role
	ObservedRole Role
	Liveness     Liveness
	SystemID     SystemID
	LastProbeAt  time.Time
	LastProbeErr error

	// M2: replication lag
	ReplicationLagMs    *int64
	ReplicationLagState LagState
	LastLagSampleAt     time.Time

	// M2: max-age configuration for stale-observation detection
	LivenessMaxAgeMs       int
	ObservedRoleMaxAgeMs   int
	ReplicationLagMaxAgeMs int
}

var ErrNoPrimaryAvailable = &NodeError{Code: "57P03", Message: "[grunyas] no primary available"}

type NodeError struct {
	Code    string
	Message string
}

func (e *NodeError) Error() string {
	return e.Message
}
