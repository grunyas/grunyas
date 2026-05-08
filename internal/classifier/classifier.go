package classifier

import (
	"strings"
	"unicode"
)

type ClassType string

const (
	TypeRead    ClassType = "read"
	TypeWrite   ClassType = "write"
	TypeUnknown ClassType = "unknown"
)

var readKeywords = map[string]bool{
	"select":  true,
	"show":    true,
	"explain": true,
	"fetch":   true,
	"values":  true,
	"with":    true,
}

type PortClassifier struct {
	port string
}

func NewPortClassifier(port string) *PortClassifier {
	return &PortClassifier{port: port}
}

func (pc *PortClassifier) Classify(sql string) (ClassType, string) {
	switch pc.port {
	case "write":
		return TypeWrite, "port"
	case "read":
		stmt := firstKeyword(sql)
		if stmt == "" {
			return TypeRead, "port"
		}
		if readKeywords[stmt] {
			return TypeRead, "keyword"
		}
		return TypeWrite, "keyword"
	case "compat":
		stmt := firstKeyword(sql)
		if stmt == "" {
			return TypeWrite, "default"
		}
		if readKeywords[stmt] {
			return TypeRead, "keyword"
		}
		return TypeWrite, "keyword"
	default:
		return TypeWrite, "default"
	}
}

func firstKeyword(sql string) string {
	cleaned := stripComments(sql)
	cleaned = strings.TrimLeftFunc(cleaned, unicode.IsSpace)

	idx := strings.IndexFunc(cleaned, func(r rune) bool {
		return unicode.IsSpace(r) || r == '(' || r == ';'
	})
	if idx < 0 {
		idx = len(cleaned)
	}
	if idx == 0 {
		return ""
	}
	keyword := strings.ToLower(cleaned[:idx])
	return keyword
}

func stripComments(stmt string) string {
	for {
		s := strings.TrimLeftFunc(stmt, unicode.IsSpace)
		if strings.HasPrefix(s, "--") {
			idx := strings.Index(s, "\n")
			if idx >= 0 {
				stmt = s[idx+1:]
				continue
			}
			return ""
		}
		if strings.HasPrefix(s, "/*") {
			idx := strings.Index(s, "*/")
			if idx >= 0 {
				stmt = s[idx+2:]
				continue
			}
			return ""
		}
		return s
	}
}

func TruncateSQL(sql string, maxLen int) string {
	cleaned := strings.TrimSpace(sql)
	cleaned = strings.ReplaceAll(cleaned, "\n", " ")
	// collapse multiple spaces
	parts := strings.Fields(cleaned)
	cleaned = strings.Join(parts, " ")
	if len(cleaned) > maxLen {
		return cleaned[:maxLen]
	}
	return cleaned
}
