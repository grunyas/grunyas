package messaging

import "testing"

func TestQueryUsesSessionState(t *testing.T) {
	tests := []struct {
		name  string
		query string
		want  bool
	}{
		// Regular queries — no session state
		{"select", "SELECT 1", false},
		{"insert", "INSERT INTO t VALUES (1)", false},
		{"begin", "BEGIN", false},
		{"commit", "COMMIT", false},

		// SET creates session state
		{"set search_path", "SET search_path = public", true},
		{"set timezone", "SET timezone = 'UTC'", true},
		{"SET upper case", "SET client_encoding = 'UTF8'", true},

		// SET LOCAL does NOT create session state (transaction-scoped)
		{"set local", "SET LOCAL search_path = public", false},
		{"SET LOCAL upper", "SET LOCAL timezone = 'UTC'", false},

		// PREPARE creates session state
		{"prepare", "PREPARE stmt AS SELECT 1", true},
		{"PREPARE upper", "PREPARE foo(int) AS SELECT $1", true},

		// Multi-statement queries
		{"multi with set", "SELECT 1; SET foo = bar", true},
		{"multi without set", "SELECT 1; SELECT 2", false},
		{"multi set local only", "SET LOCAL foo = bar; SELECT 1", false},

		// Queries with leading comments
		{"comment then set", "-- comment\nSET foo = bar", true},
		{"block comment then set", "/* comment */ SET foo = bar", true},
		{"comment then select", "-- comment\nSELECT 1", false},

		// Empty and whitespace
		{"empty", "", false},
		{"whitespace", "   ", false},
		{"semicolons only", ";;;", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := queryUsesSessionState(tt.query)
			if got != tt.want {
				t.Fatalf("queryUsesSessionState(%q) = %v, want %v", tt.query, got, tt.want)
			}
		})
	}
}

func TestStripLeadingComments(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"no comment", "SELECT 1", "SELECT 1"},
		{"line comment", "-- foo\nSELECT 1", "SELECT 1"},
		{"block comment", "/* foo */ SELECT 1", "SELECT 1"},
		{"nested comments", "-- first\n/* second */ SELECT 1", "SELECT 1"},
		{"comment only (line)", "-- just a comment", ""},
		{"comment only (block)", "/* unclosed", ""},
		{"whitespace before comment", "  -- foo\nSELECT 1", "SELECT 1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stripLeadingComments(tt.input)
			if got != tt.want {
				t.Fatalf("stripLeadingComments(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
