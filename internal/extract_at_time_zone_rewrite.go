package internal

import "strings"

// applyExtractAtTimeZoneRewrite preserves GoogleSQL's civil-time extraction
// semantics before the analyzer lowers EXTRACT and drops the explicit time zone
// from the resolved function arguments.
func applyExtractAtTimeZoneRewrite(query string) string {
	if !containsCaseInsensitive(query, "EXTRACT") || !containsCaseInsensitive(query, "TIME ZONE") {
		return query
	}
	var out strings.Builder
	for {
		start, end, args, ok := findFunctionCall(query, "EXTRACT")
		if !ok {
			out.WriteString(query)
			return out.String()
		}
		out.WriteString(query[:start])
		replacement, changed := rewriteExtractAtTimeZoneCall(args)
		if changed {
			out.WriteString(replacement)
			query = query[end:]
			continue
		}

		open := end - len(args) - 2
		rewrittenArgs := applyExtractAtTimeZoneRewrite(args)
		out.WriteString(query[start : open+1])
		out.WriteString(rewrittenArgs)
		out.WriteByte(')')
		query = query[end:]
	}
}

func rewriteExtractAtTimeZoneCall(raw string) (string, bool) {
	fromIdx := findTopLevelKeyword(raw, "FROM")
	if fromIdx < 0 {
		return "", false
	}
	part := strings.TrimSpace(raw[:fromIdx])
	rest := strings.TrimSpace(raw[fromIdx+len("FROM"):])
	atIdx, zoneStart := findTopLevelAtTimeZone(rest)
	if atIdx < 0 {
		return "", false
	}
	expr := strings.TrimSpace(rest[:atIdx])
	zone := strings.TrimSpace(rest[zoneStart:])
	if part == "" || expr == "" || zone == "" {
		return "", false
	}
	switch strings.ToUpper(part) {
	case "DATE":
		return "DATE(" + expr + ", " + zone + ")", true
	case "TIME":
		return "TIME(" + expr + ", " + zone + ")", true
	case "DATETIME":
		return "DATETIME(" + expr + ", " + zone + ")", true
	}
	return "EXTRACT(" + part + " FROM DATETIME(" + expr + ", " + zone + "))", true
}

func findTopLevelKeyword(s, keyword string) int {
	depth := 0
	for i := 0; i < len(s); {
		switch s[i] {
		case '\'', '"':
			i = scanStringLiteral(s, i)
			continue
		case '`':
			j := i + 1
			for j < len(s) && s[j] != '`' {
				j++
			}
			if j < len(s) {
				j++
			}
			i = j
			continue
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
		}
		if depth == 0 && isKeywordAt(s, i, keyword) {
			return i
		}
		i++
	}
	return -1
}

func findTopLevelAtTimeZone(s string) (atIdx, zoneStart int) {
	depth := 0
	for i := 0; i < len(s); {
		switch s[i] {
		case '\'', '"':
			i = scanStringLiteral(s, i)
			continue
		case '`':
			j := i + 1
			for j < len(s) && s[j] != '`' {
				j++
			}
			if j < len(s) {
				j++
			}
			i = j
			continue
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
		}
		if depth == 0 && isKeywordAt(s, i, "AT") {
			j := skipSQLSpaces(s, i+len("AT"))
			if isKeywordAt(s, j, "TIME") {
				k := skipSQLSpaces(s, j+len("TIME"))
				if isKeywordAt(s, k, "ZONE") {
					return i, skipSQLSpaces(s, k+len("ZONE"))
				}
			}
		}
		i++
	}
	return -1, -1
}

func isKeywordAt(s string, i int, keyword string) bool {
	if i < 0 || i+len(keyword) > len(s) || !strings.EqualFold(s[i:i+len(keyword)], keyword) {
		return false
	}
	if i > 0 && isIdentCont(s[i-1]) {
		return false
	}
	if i+len(keyword) < len(s) && isIdentCont(s[i+len(keyword)]) {
		return false
	}
	return true
}

func skipSQLSpaces(s string, i int) int {
	for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r') {
		i++
	}
	return i
}
