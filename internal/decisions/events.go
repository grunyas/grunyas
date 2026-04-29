// Package decisions defines the schema for routing decision events.
// The types and JSON shapes here are a rule-3 irreversible commitment:
// once shipped, field names and semantics cannot change.
//
// No events are emitted in M2; the schema is locked here so M3 (which
// wires emission and the SSE endpoint) does not discover a wrong shape
// under deadline pressure.
package decisions

import "time"

// SchemaVersion is the current event schema version.
const SchemaVersion = "1"

// Event is a structured record of a single routing decision.
type Event struct {
	SchemaVersion string         `json:"schema_version"`
	EventID       string         `json:"event_id"`       // ULID
	Timestamp     time.Time      `json:"timestamp"`      // RFC3339Nano, UTC
	Port          string         `json:"port"`           // "write" | "read" | "compat"
	PoolMode      string         `json:"pool_mode"`      // "session" | "transaction"
	LeaseType     string         `json:"lease_type"`     // "session" | "transaction" | "fallback"
	Classification Classification `json:"classification"`
	Candidates     []Candidate    `json:"candidates"`
	Outcome        Outcome        `json:"outcome"`
	Consistency    *Consistency   `json:"consistency,omitempty"`
	PoliciesActive []string       `json:"policies_active"`
	Source         string         `json:"source"` // "client" | "admin"
}

// Classification describes how the statement was classified.
type Classification struct {
	Type   string `json:"type"`             // "read" | "write" | "unknown"
	Source string `json:"source"`           // "port" | "transaction_state" | "keyword" | "default"
	SQL    string `json:"sql,omitempty"`    // truncated to 256 chars
}

// Candidate is a single node considered for routing.
type Candidate struct {
	NodeID   string   `json:"node_id"`
	Eligible bool     `json:"eligible"`
	Reasons  []string `json:"reasons"` // exclusion reasons when !Eligible
}

// Outcome describes the result of the routing decision.
type Outcome struct {
	Kind     string `json:"kind"`               // "leased" | "rejected" | "fallback"
	NodeID   string `json:"node_id,omitempty"`  // set when Kind=="leased" or "fallback"
	SQLState string `json:"sqlstate,omitempty"` // set when Kind=="rejected"
	Reason   string `json:"reason,omitempty"`
}

// Consistency describes the consistency guarantee applied.
type Consistency struct {
	Mode              string `json:"mode"`                         // "linearizable" | "bounded_staleness" | "post_write_window"
	LagThresholdMs    *int   `json:"lag_threshold_ms,omitempty"`
	WindowRemainingMs *int   `json:"window_remaining_ms,omitempty"`
}
