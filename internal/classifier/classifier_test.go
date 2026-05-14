package classifier

import (
	"testing"
)

func TestClassify_PortAssignment(t *testing.T) {
	// Write port is always write
	sess := &SessionState{Port: "write"}
	stmt := Statement{SQL: "SELECT 1"}
	result := Classify(stmt, sess)

	if result.Type != TypeWrite {
		t.Errorf("write port: expected TypeWrite, got %s", result.Type)
	}
	if result.Source != SourcePort {
		t.Errorf("write port: expected SourcePort, got %s", result.Source)
	}
}

func TestClassify_TransactionStateInheritance(t *testing.T) {
	// Once in a write transaction, all subsequent statements are write
	sess := &SessionState{Port: "read", TxState: TxInTransactionWrite}
	stmt := Statement{SQL: "SELECT 1"}
	result := Classify(stmt, sess)

	if result.Type != TypeWrite {
		t.Errorf("tx write state: expected TypeWrite, got %s", result.Type)
	}
	if result.Source != SourceTransactionState {
		t.Errorf("tx write state: expected SourceTransactionState, got %s", result.Source)
	}
}

func TestClassify_ReadKeywords(t *testing.T) {
	tests := []struct {
		sql      string
		expected Type
	}{
		{"SELECT 1", TypeRead},
		{"select * from foo", TypeRead},
		{"  SELECT 1", TypeRead},
		{"-- comment\nSELECT 1", TypeRead},
		{"/* block */ SELECT 1", TypeRead},
		{"SHOW max_connections", TypeRead},
		{"show tables", TypeRead},
		{"EXPLAIN SELECT 1", TypeRead},
		{"FETCH NEXT FROM c", TypeRead},
		{"VALUES (1, 2)", TypeRead},
	}

	sess := &SessionState{Port: "read", TxState: TxIdle}
	for _, tt := range tests {
		t.Run(tt.sql, func(t *testing.T) {
			stmt := Statement{SQL: tt.sql}
			result := Classify(stmt, sess)
			if result.Type != tt.expected {
				t.Errorf("expected %s, got %s", tt.expected, result.Type)
			}
			if result.Source != SourceKeyword {
				t.Errorf("expected SourceKeyword, got %s", result.Source)
			}
		})
	}
}

func TestClassify_WithIsAlwaysWrite(t *testing.T) {
	// M4: WITH is always write (no CTE body inspection in M4)
	sess := &SessionState{Port: "compat", TxState: TxIdle}
	stmt := Statement{SQL: "WITH foo AS (SELECT 1) SELECT * FROM foo"}
	result := Classify(stmt, sess)

	if result.Type != TypeWrite {
		t.Errorf("WITH should be write, got %s", result.Type)
	}
}

func TestClassify_ExplainAnalyzeIsWrite(t *testing.T) {
	// M4: EXPLAIN ANALYZE is always write
	sess := &SessionState{Port: "read", TxState: TxIdle}

	tests := []struct {
		sql      string
		expected Type
	}{
		{"EXPLAIN SELECT 1", TypeRead},
		{"EXPLAIN ANALYZE SELECT 1", TypeWrite},
		{"explain analyze select 1", TypeWrite},
		{"/* comment */ EXPLAIN ANALYZE SELECT 1", TypeWrite},
		// Parenthesized options
		{"EXPLAIN (ANALYZE) SELECT 1", TypeWrite},
		{"EXPLAIN (ANALYZE, VERBOSE) SELECT 1", TypeWrite},
		{"EXPLAIN (VERBOSE, ANALYZE) SELECT 1", TypeWrite},
		{"EXPLAIN (ANALYZE true, VERBOSE) SELECT 1", TypeWrite},
		{"EXPLAIN (COSTS false, BUFFERS, ANALYZE) SELECT 1", TypeWrite},
		// Bare flags before ANALYZE
		{"EXPLAIN VERBOSE ANALYZE SELECT 1", TypeWrite},
		{"EXPLAIN VERBOSE COSTS ANALYZE SELECT 1", TypeWrite},
		// Not analyze: should be read
		{"EXPLAIN (VERBOSE) SELECT 1", TypeRead},
		{"EXPLAIN (COSTS) SELECT 1", TypeRead},
		{"EXPLAIN (FORMAT JSON) SELECT 1", TypeRead},
	}

	for _, tt := range tests {
		t.Run(tt.sql, func(t *testing.T) {
			stmt := Statement{SQL: tt.sql}
			result := Classify(stmt, sess)
			if result.Type != tt.expected {
				t.Errorf("expected %s, got %s", tt.expected, result.Type)
			}
		})
	}
}

func TestClassify_WriteStatements(t *testing.T) {
	tests := []string{
		"INSERT INTO foo VALUES (1)",
		"UPDATE foo SET bar = 1",
		"DELETE FROM foo",
		"CREATE TABLE foo (id int)",
		"DROP TABLE foo",
		"ALTER TABLE foo",
		"BEGIN",
		"COMMIT",
		"ROLLBACK",
		"WITH foo AS (UPDATE bar SET x = 1) SELECT 1",
	}

	sess := &SessionState{Port: "read", TxState: TxIdle}
	for _, sql := range tests {
		t.Run(sql, func(t *testing.T) {
			stmt := Statement{SQL: sql}
			result := Classify(stmt, sess)
			if result.Type != TypeWrite {
				t.Errorf("expected TypeWrite, got %s", result.Type)
			}
		})
	}
}

func TestClassify_DefaultToWrite(t *testing.T) {
	// Empty or whitespace-only defaults to write
	sess := &SessionState{Port: "read", TxState: TxIdle}

	tests := []string{
		"",
		"   ",
		"-- just a comment",
		"/* block comment */",
	}

	for _, sql := range tests {
		t.Run(sql, func(t *testing.T) {
			stmt := Statement{SQL: sql}
			result := Classify(stmt, sess)
			if result.Type != TypeWrite {
				t.Errorf("expected TypeWrite for empty/comment, got %s", result.Type)
			}
			if result.Source != SourceDefault {
				t.Errorf("expected SourceDefault, got %s", result.Source)
			}
		})
	}
}

func TestClassify_PreparedStatementCache(t *testing.T) {
	prepared := make(map[string]Class)
	sess := &SessionState{Port: "read", TxState: TxIdle, Prepared: prepared}

	// First classification should cache with full metadata
	stmt1 := Statement{Name: "my_stmt", SQL: "SELECT 1"}
	result1 := Classify(stmt1, sess)
	if result1.Type != TypeRead {
		t.Fatalf("expected TypeRead, got %s", result1.Type)
	}
	if result1.Source != SourceKeyword {
		t.Fatalf("expected SourceKeyword on first classify, got %s", result1.Source)
	}

	// Verify it's cached
	if cached, ok := prepared["my_stmt"]; !ok {
		t.Fatal("prepared statement should be cached")
	} else {
		if cached.Type != TypeRead || cached.Source != SourceKeyword {
			t.Fatalf("cached value wrong: %+v", cached)
		}
	}

	// Second classification should use cache — same Source, not re-evaluated
	stmt2 := Statement{Name: "my_stmt", SQL: "UPDATE foo SET bar = 1"}
	result2 := Classify(stmt2, sess)
	if result2.Type != TypeRead {
		t.Errorf("cached classification should return read, got %s", result2.Type)
	}
	if result2.Source != SourceKeyword {
		// If the implementation re-ran the keyword sniffer, Source would still
		// be SourceKeyword by coincidence.  This assertion catches an
		// implementation that accidentally returns SourceDefault or
		// SourceTransactionState on cache hit.
		t.Errorf("cached classification should preserve SourceKeyword, got %s", result2.Source)
	}
}

func TestClassify_UnnamedPreparedStatement(t *testing.T) {
	// Unnamed ("") prepared statements re-classify each time
	prepared := make(map[string]Class)
	sess := &SessionState{Port: "read", TxState: TxIdle, Prepared: prepared}

	stmt1 := Statement{Name: "", SQL: "SELECT 1"}
	result1 := Classify(stmt1, sess)
	if result1.Type != TypeRead {
		t.Fatalf("expected TypeRead, got %s", result1.Type)
	}

	// Unnamed statements are not cached
	if _, ok := prepared[""]; ok {
		t.Error("unnamed prepared statement should not be cached")
	}
}

func TestFirstKeyword_64ByteCapActuallyCaps(t *testing.T) {
	// The cap happens BEFORE comment stripping, so a 1MB comment prolog
	// does not get fully scanned.  Verify that a keyword past the 64-byte
	// mark is NOT found, and that the cap truncates mid-token when needed.

	// 65 'x' chars + "SELECT 1" → capped to 64 chars → 64 'x' chars.
	// The first (and only) token is the 64 'x' chars; SELECT is past the cap.
	longProlog := makeString("x", 65)
	sql := longProlog + "SELECT 1"
	keyword, _ := firstKeyword(sql)
	if keyword == "" {
		t.Fatal("expected a token from the capped substring")
	}
	if keyword == "select" {
		t.Error("SELECT should NOT be found when it starts past the 64-byte cap")
	}

	// 58 'x' chars + " SELECT 1" = 67 chars total → capped to 64 chars.
	// After cap: 58 'x' + " SELE" (first token = 58 'x' chars, SELECT is truncated).
	sql2 := makeString("x", 58) + " SELECT 1"
	keyword2, _ := firstKeyword(sql2)
	if keyword2 == "select" {
		t.Error("SELECT should NOT be found when it is truncated by the 64-byte cap")
	}

	// SELECT at the start + padding that fits within 64 bytes → still found.
	// "SELECT " + 57 'x' = 64 chars exactly.
	sql3 := "SELECT " + makeString("x", 57)
	keyword3, _ := firstKeyword(sql3)
	if keyword3 != "select" {
		t.Errorf("expected 'select' when it fits within cap, got %q", keyword3)
	}
}

func TestFirstKeyword_64ByteCapWithLongComment(t *testing.T) {
	// A comment longer than 64 bytes should not prevent finding the keyword
	// because the cap bounds the total work, but if the comment fits within
	// 64 bytes the keyword after it should still be found.
	sql := "/* " + makeString("x", 50) + " */ SELECT 1"
	keyword, _ := firstKeyword(sql)
	if keyword != "select" {
		t.Errorf("expected 'select' after comment within cap, got %q", keyword)
	}

	// A comment that pushes the keyword past the 64-byte cap
	sql2 := "/* " + makeString("x", 60) + " */ SELECT 1"
	keyword2, _ := firstKeyword(sql2)
	if keyword2 != "" {
		t.Errorf("keyword should be empty when comment pushes keyword past cap, got %q", keyword2)
	}
}

func makeString(s string, n int) string {
	result := make([]byte, n)
	for i := range result {
		result[i] = s[0]
	}
	return string(result)
}

func TestTruncateSQL(t *testing.T) {
	// Long SQL should be truncated
	longSQL := "SELECT " + makeString("a", 300)
	truncated := TruncateSQL(longSQL, 256)
	if len(truncated) > 256 {
		t.Errorf("truncated SQL should be <= 256 chars, got %d", len(truncated))
	}

	// Newlines should be replaced with spaces
	sql := "SELECT\n  foo\nFROM\n  bar"
	truncated = TruncateSQL(sql, 256)
	if truncated != "SELECT foo FROM bar" {
		t.Errorf("newlines not handled correctly: %q", truncated)
	}
}

// Test unified classifier path for all ports (replaces the old PortClassifier tests).
func TestClassify_UnifiedPortPath(t *testing.T) {
	// Write port always returns write
	sess := &SessionState{Port: "write"}
	cls := Classify(Statement{SQL: "SELECT 1"}, sess)
	if cls.Type != TypeWrite || cls.Source != SourcePort {
		t.Errorf("write port: expected TypeWrite/SourcePort, got %+v", cls)
	}

	// Read port classifies by keyword
	sess.Port = "read"
	cls = Classify(Statement{SQL: "SELECT 1"}, sess)
	if cls.Type != TypeRead || cls.Source != SourceKeyword {
		t.Errorf("read port SELECT: expected TypeRead/SourceKeyword, got %+v", cls)
	}

	cls = Classify(Statement{SQL: "INSERT INTO foo VALUES (1)"}, sess)
	if cls.Type != TypeWrite || cls.Source != SourceKeyword {
		t.Errorf("read port INSERT: expected TypeWrite/SourceKeyword, got %+v", cls)
	}

	// EXPLAIN ANALYZE forms should be write even on read port
	for _, sql := range []string{
		"EXPLAIN ANALYZE SELECT 1",
		"EXPLAIN (ANALYZE) SELECT 1",
		"EXPLAIN VERBOSE ANALYZE SELECT 1",
	} {
		cls = Classify(Statement{SQL: sql}, sess)
		if cls.Type != TypeWrite || cls.Source != SourceKeyword {
			t.Errorf("read port %s: expected TypeWrite/SourceKeyword, got %+v", sql, cls)
		}
	}

	// Compat port classifies the same way as the unified path
	cls = Classify(Statement{SQL: "SELECT 1"}, &SessionState{Port: "compat"})
	if cls.Type != TypeRead || cls.Source != SourceKeyword {
		t.Errorf("compat port SELECT: expected TypeRead/SourceKeyword, got %+v", cls)
	}
}

func TestForget_RemovesCachedEntry(t *testing.T) {
	prepared := make(map[string]Class)
	sess := &SessionState{Port: "read", TxState: TxIdle, Prepared: prepared}

	stmt := Statement{Name: "my_stmt", SQL: "SELECT 1"}
	_ = Classify(stmt, sess)
	if _, ok := prepared["my_stmt"]; !ok {
		t.Fatal("prepared statement should be cached")
	}

	Forget(sess, "my_stmt")
	if _, ok := prepared["my_stmt"]; ok {
		t.Fatal("Forget should remove cached entry")
	}
}

func TestForgetAll_ClearsAllEntries(t *testing.T) {
	prepared := make(map[string]Class)
	sess := &SessionState{Port: "read", TxState: TxIdle, Prepared: prepared}

	_ = Classify(Statement{Name: "s1", SQL: "SELECT 1"}, sess)
	_ = Classify(Statement{Name: "s2", SQL: "SELECT 2"}, sess)
	if len(prepared) != 2 {
		t.Fatalf("expected 2 cached entries, got %d", len(prepared))
	}

	ForgetAll(sess)
	if len(prepared) != 0 {
		t.Fatalf("ForgetAll should clear all entries, got %d", len(prepared))
	}
}
