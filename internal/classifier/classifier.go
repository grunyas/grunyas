package classifier

import (
	"strings"
	"unicode"
)

// Type represents the traffic type classification.
type Type string

const (
	TypeRead    Type = "read"
	TypeWrite   Type = "write"
	TypeUnknown Type = "unknown"
)

// Source represents how the classification was derived.
type Source string

const (
	SourcePort             Source = "port"
	SourceTransactionState Source = "transaction_state"
	SourceKeyword          Source = "keyword"
	SourceDefault          Source = "default"
)

// Class is the result of classifying a statement.
type Class struct {
	Type   Type   // read | write | unknown
	Source Source // port | transaction_state | keyword | default
}

// TxState represents the transaction state of a session.
type TxState int

const (
	TxIdle TxState = iota
	TxInTransactionRead   // in a transaction, no write seen yet
	TxInTransactionWrite  // in a transaction, write seen
	TxInFailedTransaction // in a failed transaction
)

// SessionState carries the state needed for classification.
type SessionState struct {
	Port     string           // "write" | "read" | "compat"
	TxState  TxState          // current transaction state
	Prepared map[string]Class // cached classifications for prepared statements
}

// Statement represents a SQL statement to classify.
// For simple query protocol, Name is empty and SQL contains the query.
// For extended protocol, Name is the prepared statement name.
type Statement struct {
	Name string // prepared statement name (empty for simple query)
	SQL  string // the SQL text
}

// The conservative read keyword set from M4.md §1.
// Evaluated against the first non-comment, non-whitespace, non-CTE token.
var readKeywords = map[string]bool{
	"select":  true,
	"show":    true,
	"explain": true,
	"fetch":   true,
	"values":  true,
	// Note: "with" is NOT in this list - WITH is always write per M4
}

// Classify determines the traffic type for a statement.
// It follows the priority order from M4.md §1:
// 1. Port assignment (write port = always write)
// 2. Transaction state (once in write transaction, all statements are write)
// 3. Prepared statement cache (for extended protocol)
// 4. Statement keyword inspection
// 5. Default to write (conservative)
//
// sess is a pointer because the function mutates sess.Prepared when caching
// named prepared-statement classifications.
func Classify(stmt Statement, sess *SessionState) Class {
	// 1. Port assignment - write port is always write
	if sess.Port == "write" {
		return Class{Type: TypeWrite, Source: SourcePort}
	}

	// 2. Transaction state inheritance
	// Once in a write transaction, every subsequent statement is write
	if sess.TxState == TxInTransactionWrite {
		return Class{Type: TypeWrite, Source: SourceTransactionState}
	}

	// 3. Prepared statement cache lookup (extended protocol)
	if stmt.Name != "" && sess.Prepared != nil {
		if cached, ok := sess.Prepared[stmt.Name]; ok {
			return cached
		}
	}

	// 4. Statement keyword inspection
	result := classifyByKeyword(stmt.SQL, sess.TxState)

	// Cache the result for named prepared statements
	if stmt.Name != "" && sess.Prepared != nil {
		sess.Prepared[stmt.Name] = result
	}

	return result
}

// classifyByKeyword inspects the first keyword of the SQL statement.
// It reads at most the first 64 bytes, skipping comments and whitespace.
func classifyByKeyword(sql string, txState TxState) Class {
	keyword, isExplainAnalyze := firstKeyword(sql)

	// Empty statement defaults to write (conservative)
	if keyword == "" {
		return Class{Type: TypeWrite, Source: SourceDefault}
	}

	// EXPLAIN ANALYZE is always write (it executes the underlying statement)
	if isExplainAnalyze {
		return Class{Type: TypeWrite, Source: SourceKeyword}
	}

	// Check against read keyword set
	if readKeywords[keyword] {
		return Class{Type: TypeRead, Source: SourceKeyword}
	}

	// Everything else is write (conservative)
	return Class{Type: TypeWrite, Source: SourceKeyword}
}

// firstKeyword extracts the first keyword from a SQL statement.
// It caps the input to 64 bytes first (bounding work on pathological input),
// then skips leading comments and whitespace, and returns the lowercase keyword.
// It also returns true if this is "EXPLAIN ANALYZE" (which is always write).
func firstKeyword(sql string) (keyword string, isExplainAnalyze bool) {
	// Cap first to bound work on pathological input (e.g. 1MB comment prolog).
	// M4.md §1: "the scanner reads at most the first 64 bytes of the statement".
	if len(sql) > 64 {
		sql = sql[:64]
	}

	s := stripCommentsAndWhitespace(sql)

	// Find the end of the first token
	idx := strings.IndexFunc(s, func(r rune) bool {
		return unicode.IsSpace(r) || r == '(' || r == ';'
	})
	if idx < 0 {
		idx = len(s)
	}
	if idx == 0 {
		return "", false
	}

	first := strings.ToLower(s[:idx])

	// Check for EXPLAIN ANALYZE
	if first == "explain" {
		return "explain", explainHasAnalyze(s[idx:])
	}

	return first, false
}

// explainHasAnalyze scans the tokens after EXPLAIN looking for a top-level
// ANALYZE keyword. It handles:
//   - bare ANALYZE after EXPLAIN:                    EXPLAIN ANALYZE SELECT 1
//   - parenthesized option lists:                    EXPLAIN (ANALYZE) SELECT 1
//   - bare flags before ANALYZE:                     EXPLAIN VERBOSE ANALYZE SELECT 1
//   - mixed parenthesized and bare flags:            EXPLAIN (VERBOSE, ANALYZE) SELECT 1
//
// Any of these forms should return true.
func explainHasAnalyze(afterExplain string) bool {
	s := stripCommentsAndWhitespace(afterExplain)

	// If the next non-whitespace char is '(', scan inside the parens for ANALYZE.
	if strings.HasPrefix(s, "(") {
		depth := 1
		i := 1
		for i < len(s) && depth > 0 {
			switch s[i] {
			case '(':
				depth++
			case ')':
				depth--
			}
			i++
		}
		if depth == 0 {
			// Scan the parenthesized content for an ANALYZE token.
			inner := s[1 : i-1]
			if tokenListContains(inner, "analyze") {
				return true
			}
			// ANALYZE was not inside the parens — continue scanning after them.
			s = stripCommentsAndWhitespace(s[i:])
		}
	}

	// Scan tokens after any parenthesized list, skipping known bare flag
	// keywords, looking for ANALYZE.
	for {
		s = stripCommentsAndWhitespace(s)
		if s == "" {
			return false
		}

		idx := strings.IndexFunc(s, func(r rune) bool {
			return unicode.IsSpace(r) || r == '(' || r == ';'
		})
		if idx < 0 {
			idx = len(s)
		}
		if idx == 0 {
			return false
		}

		tok := strings.ToLower(s[:idx])

		if tok == "analyze" {
			return true
		}

		// Known bare flag keywords that can appear before ANALYZE
		if tok == "verbose" || tok == "costs" || tok == "buffers" ||
			tok == "timing" || tok == "summary" || tok == "format" ||
			tok == "settings" || tok == "wal" {
			s = s[idx:]
			continue
		}

		return false
	}
}

// tokenListContains scans a comma-separated token list (like the inside of
// EXPLAIN's parenthesized options) for a token equal to target.
func tokenListContains(list, target string) bool {
	s := stripCommentsAndWhitespace(list)
	for {
		if s == "" {
			return false
		}

		// Skip leading comma if present
		if strings.HasPrefix(s, ",") {
			s = stripCommentsAndWhitespace(s[1:])
			continue
		}

		// Find end of current token (comma, space, or paren)
		idx := strings.IndexFunc(s, func(r rune) bool {
			return unicode.IsSpace(r) || r == ',' || r == '(' || r == ')'
		})
		if idx < 0 {
			idx = len(s)
		}
		if idx == 0 {
			return false
		}

		tok := strings.ToLower(s[:idx])
		if tok == target {
			return true
		}

		// Skip to next comma
		commaIdx := strings.Index(s, ",")
		if commaIdx < 0 {
			return false
		}
		s = stripCommentsAndWhitespace(s[commaIdx+1:])
	}
}

// stripCommentsAndWhitespace removes leading comments and whitespace.
// Handles both line comments (-- ... \n) and block comments (/* ... */).
func stripCommentsAndWhitespace(s string) string {
	for {
		// Skip whitespace
		s = strings.TrimLeftFunc(s, unicode.IsSpace)

		// Line comment
		if strings.HasPrefix(s, "--") {
			idx := strings.Index(s, "\n")
			if idx >= 0 {
				s = s[idx+1:]
				continue
			}
			return "" // Comment to end of string
		}

		// Block comment
		if strings.HasPrefix(s, "/*") {
			idx := strings.Index(s, "*/")
			if idx >= 0 {
				s = s[idx+2:]
				continue
			}
			return "" // Unclosed block comment
		}

		return s
	}
}

// TruncateSQL truncates SQL to maxLen for logging/decision events.
func TruncateSQL(sql string, maxLen int) string {
	cleaned := strings.TrimSpace(sql)
	cleaned = strings.ReplaceAll(cleaned, "\n", " ")
	// Collapse multiple spaces
	parts := strings.Fields(cleaned)
	cleaned = strings.Join(parts, " ")
	if len(cleaned) > maxLen {
		return cleaned[:maxLen]
	}
	return cleaned
}

// Forget removes a cached prepared-statement classification.
// Called when the client sends a Close message for a named prepared statement.
func Forget(sess *SessionState, name string) {
	if sess.Prepared != nil {
		delete(sess.Prepared, name)
	}
}

// ForgetAll clears every cached prepared-statement classification.
// Called on session end or when the client sends DISCARD ALL.
func ForgetAll(sess *SessionState) {
	if sess.Prepared != nil {
		for k := range sess.Prepared {
			delete(sess.Prepared, k)
		}
	}
}
