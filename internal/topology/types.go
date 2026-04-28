// Package topology provides a cluster-topology view of declared Postgres nodes,
// their observed state, and per-node pool managers. It is the canonical answer
// to "what is the cluster doing right now."
package topology

import (
	"time"
)

// NodeID is a stable operator-assigned identifier for a Postgres node.
// It must be non-empty and URL-safe ([a-z0-9-]+).
type NodeID string

// Role represents a node's role in the cluster.
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

// Liveness represents the health state of a node from the probe's perspective.
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

// SystemID is the Postgres system_identifier from pg_control_system().
type SystemID string

// NodeView is a read-only snapshot of a node's current state.
// Reading from it does not require a lock.
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
}

// ErrNoPrimaryAvailable is returned when no node is currently observed as primary.
var ErrNoPrimaryAvailable = &NodeError{Code: "57P03", Message: "[grunyas] no primary available"}

// NodeError is a node-level error with a PostgreSQL-compatible code.
type NodeError struct {
	Code    string
	Message string
}

func (e *NodeError) Error() string {
	return e.Message
}
