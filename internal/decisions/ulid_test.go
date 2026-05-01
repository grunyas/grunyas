package decisions

import (
	"testing"
	"time"
)

func TestNewULID_TimestampOrdering(t *testing.T) {
	id1 := NewULID()
	time.Sleep(10 * time.Millisecond)
	id2 := NewULID()

	if id1 >= id2 {
		t.Fatalf("expected ULID1 < ULID2 (time-ordered), got %s >= %s", id1, id2)
	}
}

func TestNewULID_Length(t *testing.T) {
	id := NewULID()
	if len(id) != 26 {
		t.Fatalf("expected 26-character ULID, got %d: %s", len(id), id)
	}
}

func TestNewULID_Alphabet(t *testing.T) {
	valid := map[byte]bool{}
	for _, c := range []byte("0123456789ABCDEFGHJKMNPQRSTVWXYZ") {
		valid[c] = true
	}
	id := NewULID()
	for i := 0; i < len(id); i++ {
		if !valid[id[i]] {
			t.Fatalf("invalid character %q at position %d in ULID %s", id[i], i, id)
		}
	}
}

func TestNewULID_Uniqueness(t *testing.T) {
	seen := make(map[string]bool, 1000)
	for i := 0; i < 1000; i++ {
		id := NewULID()
		if seen[id] {
			t.Fatalf("duplicate ULID after %d iterations: %s", i, id)
		}
		seen[id] = true
	}
}
