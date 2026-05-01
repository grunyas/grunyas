package decisions

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

func TestEventGoldenJSON(t *testing.T) {
	lagThreshold := 50
	windowRemaining := 200
	ts := time.Date(2026, 4, 28, 12, 0, 0, 123456789, time.UTC)

	e := Event{
		SchemaVersion: SchemaVersion,
		EventID:       "01JZ5X1A2B3C4D5E6F7G8H9I0",
		Timestamp:     ts,
		Port:          "write",
		PoolMode:      "session",
		LeaseType:     "session",
		Classification: Classification{
			Type:   "read",
			Source: "keyword",
			SQL:    "SELECT 1",
		},
		Candidates: []Candidate{
			{NodeID: "primary-1", Eligible: true, Reasons: nil},
			{NodeID: "replica-1", Eligible: false, Reasons: []string{"liveness:down"}},
		},
		Outcome: Outcome{
			Kind:   "leased",
			NodeID: "primary-1",
		},
		Consistency: &Consistency{
			Mode:              "linearizable",
			LagThresholdMs:    &lagThreshold,
			WindowRemainingMs: &windowRemaining,
		},
		PoliciesActive: []string{"default"},
		Source:         "client",
	}

	got, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		t.Fatalf("failed to marshal Event: %v", err)
	}

	want := `{
  "schema_version": "1",
  "event_id": "01JZ5X1A2B3C4D5E6F7G8H9I0",
  "timestamp": "2026-04-28T12:00:00.123456789Z",
  "port": "write",
  "pool_mode": "session",
  "lease_type": "session",
  "classification": {
    "type": "read",
    "source": "keyword",
    "sql": "SELECT 1"
  },
  "candidates": [
    {
      "node_id": "primary-1",
      "eligible": true,
      "reasons": null
    },
    {
      "node_id": "replica-1",
      "eligible": false,
      "reasons": [
        "liveness:down"
      ]
    }
  ],
  "outcome": {
    "kind": "leased",
    "node_id": "primary-1"
  },
  "consistency": {
    "mode": "linearizable",
    "lag_threshold_ms": 50,
    "window_remaining_ms": 200
  },
  "policies_active": [
    "default"
  ],
  "source": "client"
}`

	if string(got) != want {
		t.Fatalf("JSON mismatch:\n--- got:\n%s\n--- want:\n%s", got, want)
	}
}

func TestEventRoundTrip(t *testing.T) {
	ts := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	original := Event{
		SchemaVersion: "1",
		EventID:       "01JZ5X1A2B3C4D5E6F7G8H9I0",
		Timestamp:     ts,
		Port:          "read",
		PoolMode:      "transaction",
		LeaseType:     "fallback",
		Classification: Classification{
			Type:   "write",
			Source: "transaction_state",
			SQL:    "INSERT INTO t VALUES (1)",
		},
		Candidates: []Candidate{
			{NodeID: "primary-1", Eligible: true, Reasons: nil},
		},
		Outcome: Outcome{
			Kind:     "rejected",
			SQLState: "57P03",
			Reason:   "no primary available",
		},
		PoliciesActive: []string{},
		Source:         "client",
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded Event
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Compare key fields
	if decoded.SchemaVersion != original.SchemaVersion {
		t.Errorf("SchemaVersion: got %q, want %q", decoded.SchemaVersion, original.SchemaVersion)
	}
	if decoded.Port != original.Port {
		t.Errorf("Port: got %q, want %q", decoded.Port, original.Port)
	}
	if decoded.Outcome.Kind != original.Outcome.Kind {
		t.Errorf("Outcome.Kind: got %q, want %q", decoded.Outcome.Kind, original.Outcome.Kind)
	}
	if decoded.Classification.Type != original.Classification.Type {
		t.Errorf("Classification.Type: got %q, want %q", decoded.Classification.Type, original.Classification.Type)
	}
	if len(decoded.Candidates) != len(original.Candidates) {
		t.Errorf("Candidates: got %d, want %d", len(decoded.Candidates), len(original.Candidates))
	}
	if decoded.Consistency != nil {
		t.Errorf("expected nil Consistency, got %+v", decoded.Consistency)
	}
}

func TestSchemaVersionConstant(t *testing.T) {
	if SchemaVersion != "1" {
		t.Fatalf("SchemaVersion must be \"1\", got %q", SchemaVersion)
	}
}

func init() {
	// Suppress unused import lint — the os import is available for golden-file mode.
	_ = os.ReadFile
}

func TestRingBufferOverflowCounter(t *testing.T) {
	bus := NewBus(10, 10)
	defer bus.Close()

	// Fill ring to capacity (1024) + 5 more → 5 overwrites
	for i := 0; i < ringCapacity+5; i++ {
		bus.Publish(Event{Port: "write", PoolMode: "session", Source: "client"})
	}

	if bus.DroppedBusOverflow() != 5 {
		t.Fatalf("expected 5 bus overflows after %d publishes, got %d", ringCapacity+5, bus.DroppedBusOverflow())
	}

	// Another publish → another overwrite
	bus.Publish(Event{Port: "write", PoolMode: "session", Source: "client"})
	if bus.DroppedBusOverflow() != 6 {
		t.Fatalf("expected 6 after one more, got %d", bus.DroppedBusOverflow())
	}
}

func TestRingBufferOverflowOnSecondWrap(t *testing.T) {
	bus := NewBus(10, 10)
	defer bus.Close()

	// First wrap: ringCapacity writes
	for i := 0; i < ringCapacity; i++ {
		bus.Publish(Event{Port: "write", PoolMode: "session", Source: "client"})
	}
	if bus.DroppedBusOverflow() != 0 {
		t.Fatalf("expected 0 before first wrap, got %d", bus.DroppedBusOverflow())
	}

	// Second wrap: ringCapacity more writes → every one overwrites
	for i := 0; i < ringCapacity; i++ {
		bus.Publish(Event{Port: "write", PoolMode: "session", Source: "client"})
	}
	if bus.DroppedBusOverflow() != ringCapacity {
		t.Fatalf("expected %d after second wrap, got %d", ringCapacity, bus.DroppedBusOverflow())
	}
}

func TestMarkOTelDroppedTakesCount(t *testing.T) {
	bus := NewBus(10, 10)
	defer bus.Close()

	bus.MarkOTelDropped(50)
	bus.MarkOTelDropped(7)
	if bus.DroppedOTelOverflow() != 57 {
		t.Fatalf("expected 57, got %d", bus.DroppedOTelOverflow())
	}
}
