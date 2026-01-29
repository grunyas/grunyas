package messaging

import "strings"

func queryUsesSessionState(query string) bool {
	statements := strings.Split(query, ";")
	for _, stmt := range statements {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" {
			continue
		}
		normalized := strings.ToLower(stripLeadingComments(stmt))
		if strings.HasPrefix(normalized, "set ") {
			if strings.HasPrefix(normalized, "set local") {
				continue
			}
			return true
		}
		if strings.HasPrefix(normalized, "prepare ") {
			return true
		}
	}
	return false
}

func stripLeadingComments(stmt string) string {
	for {
		trimmed := strings.TrimSpace(stmt)
		if strings.HasPrefix(trimmed, "--") {
			if idx := strings.Index(trimmed, "\n"); idx >= 0 {
				stmt = trimmed[idx+1:]
				continue
			}
			return ""
		}
		if strings.HasPrefix(trimmed, "/*") {
			if idx := strings.Index(trimmed, "*/"); idx >= 0 {
				stmt = trimmed[idx+2:]
				continue
			}
			return ""
		}
		return trimmed
	}
}
