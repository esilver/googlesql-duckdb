package internal

import (
	"sort"
	"strings"
)

// applyBigQueryTvfStubs rewrites built-in BigQuery TVFs that have no
// natural local execution path into empty / minimal subqueries that
// the analyzer can resolve.
//
// The following call sites are handled by lowering the TVF to plain
// SQL BEFORE the analyzer runs (analogous to applyGapFillRewrite):
//
//	APPENDS(TABLE t, ...)              → (SELECT *, NULL AS _CHANGE_TYPE,
//	                                               NULL AS _CHANGE_TIMESTAMP
//	                                      FROM t WHERE FALSE)
//	CHANGES(TABLE t, ...)              → (SELECT *, NULL AS _CHANGE_TYPE,
//	                                               NULL AS _CHANGE_TIMESTAMP,
//	                                               NULL AS _CHANGE_SEQUENCE_NUMBER
//	                                      FROM t WHERE FALSE)
//	VECTOR_INDEX.STATISTICS(TABLE t)   → (SELECT CAST(NULL AS FLOAT64) AS drift
//	                                      WHERE FALSE)
//
// These functions report on time-travel snapshots and vector-index
// drift that googlesqlite does not maintain; the rewrite preserves
// the call-site shape so consumers can parse / analyze without
// crashing, returning an empty result set. EXTERNAL_QUERY,
// VECTOR_SEARCH, and EXTERNAL_OBJECT_TRANSFORM are not handled here
// because their output schema depends on caller-supplied information
// that no rewrite can derive.
func applyBigQueryTvfStubs(query string) string {
	upper := strings.ToUpper(query)
	if !strings.Contains(upper, "APPENDS") &&
		!strings.Contains(upper, "CHANGES") &&
		!strings.Contains(upper, "VECTOR_INDEX") {
		return query
	}
	for {
		changed := false
		query, changed = rewriteTvfCall(query, "APPENDS", buildAppendsBody)
		if changed {
			continue
		}
		query, changed = rewriteTvfCall(query, "CHANGES", buildChangesBody)
		if changed {
			continue
		}
		query, changed = rewriteTvfCall(query, "VECTOR_INDEX.STATISTICS", buildVectorIndexStatisticsBody)
		if changed {
			continue
		}
		break
	}
	return query
}

type userTVFRewriteCandidate struct {
	name string
	spec *TVFSpec
}

func applyUserTVFInlineRewrites(query string, tvfMap map[string]*TVFSpec) string {
	if len(tvfMap) == 0 {
		return query
	}
	var candidates []userTVFRewriteCandidate
	seen := map[string]struct{}{}
	for _, spec := range tvfMap {
		if spec == nil || strings.TrimSpace(spec.RawBody) == "" {
			continue
		}
		for _, name := range tvfSQLNames(spec.NamePath) {
			key := strings.ToLower(name)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			candidates = append(candidates, userTVFRewriteCandidate{name: name, spec: spec})
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		return len(candidates[i].name) > len(candidates[j].name)
	})
	for {
		changed := false
		for _, candidate := range candidates {
			var didRewrite bool
			query, didRewrite = rewriteUserTVFCall(query, candidate.name, candidate.spec.CallGoogleSQL)
			if didRewrite {
				changed = true
				break
			}
		}
		if !changed {
			return query
		}
	}
}

func tvfSQLNames(path []string) []string {
	var names []string
	for i := range path {
		names = append(names, strings.Join(path[i:], "."))
	}
	return names
}

func rewriteUserTVFCall(s, name string, bodyFn func(args []string) string) (string, bool) {
	for i := 0; i+len(name) <= len(s); i++ {
		switch s[i] {
		case '\'', '"':
			i = scanStringLiteral(s, i) - 1
			continue
		case '`':
			j := i + 1
			for j < len(s) && s[j] != '`' {
				j++
			}
			if j < len(s) {
				j++
			}
			i = j - 1
			continue
		case '-':
			if i+1 < len(s) && s[i+1] == '-' {
				j := i
				for j < len(s) && s[j] != '\n' {
					j++
				}
				i = j - 1
				continue
			}
		}
		if !strings.EqualFold(s[i:i+len(name)], name) {
			continue
		}
		if i > 0 {
			c := s[i-1]
			if isIdentCont(c) || c == '.' {
				continue
			}
		}
		if !isUserTVFTableContext(s, i) {
			continue
		}
		j := i + len(name)
		for j < len(s) && (s[j] == ' ' || s[j] == '\t' || s[j] == '\n' || s[j] == '\r') {
			j++
		}
		if j >= len(s) || s[j] != '(' {
			continue
		}
		end, args, ok := scanBalancedArgs(s, j)
		if !ok {
			continue
		}
		return s[:i] + bodyFn(args) + s[end:], true
	}
	return s, false
}

func isUserTVFTableContext(s string, nameStart int) bool {
	j := nameStart - 1
	for j >= 0 && (s[j] == ' ' || s[j] == '\t' || s[j] == '\n' || s[j] == '\r') {
		j--
	}
	if j < 0 {
		return false
	}
	if s[j] == ',' {
		return true
	}
	end := j + 1
	for j >= 0 && isIdentCont(s[j]) {
		j--
	}
	word := s[j+1 : end]
	return strings.EqualFold(word, "FROM") ||
		strings.EqualFold(word, "JOIN") ||
		strings.EqualFold(word, "APPLY")
}

func replaceGoogleSQLIdentifiers(sql string, replacements map[string]string) string {
	if len(replacements) == 0 {
		return sql
	}
	var b strings.Builder
	b.Grow(len(sql))
	for i := 0; i < len(sql); {
		switch sql[i] {
		case '\'', '"':
			end := scanStringLiteral(sql, i)
			b.WriteString(sql[i:end])
			i = end
			continue
		case '`':
			j := i + 1
			for j < len(sql) && sql[j] != '`' {
				j++
			}
			if j < len(sql) {
				j++
			}
			b.WriteString(sql[i:j])
			i = j
			continue
		case '-':
			if i+1 < len(sql) && sql[i+1] == '-' {
				j := i
				for j < len(sql) && sql[j] != '\n' {
					j++
				}
				b.WriteString(sql[i:j])
				i = j
				continue
			}
		}
		if isIdentStart(sql[i]) && (i == 0 || !isIdentCont(sql[i-1])) {
			j := i + 1
			for j < len(sql) && isIdentCont(sql[j]) {
				j++
			}
			if repl, ok := replacements[strings.ToLower(sql[i:j])]; ok {
				b.WriteString("(")
				b.WriteString(repl)
				b.WriteString(")")
			} else {
				b.WriteString(sql[i:j])
			}
			i = j
			continue
		}
		b.WriteByte(sql[i])
		i++
	}
	return b.String()
}

func extractCreateTableFunctionBody(query string) string {
	for i := 0; i+2 <= len(query); i++ {
		switch query[i] {
		case '\'', '"':
			i = scanStringLiteral(query, i) - 1
			continue
		case '`':
			j := i + 1
			for j < len(query) && query[j] != '`' {
				j++
			}
			if j < len(query) {
				j++
			}
			i = j - 1
			continue
		case '-':
			if i+1 < len(query) && query[i+1] == '-' {
				j := i
				for j < len(query) && query[j] != '\n' {
					j++
				}
				i = j - 1
				continue
			}
		}
		if !strings.EqualFold(query[i:i+2], "AS") {
			continue
		}
		if i > 0 && isIdentCont(query[i-1]) {
			continue
		}
		j := i + 2
		if j < len(query) && isIdentCont(query[j]) {
			continue
		}
		for j < len(query) && (query[j] == ' ' || query[j] == '\t' || query[j] == '\n' || query[j] == '\r') {
			j++
		}
		if j >= len(query) || query[j] != '(' {
			continue
		}
		end, _, ok := scanBalancedArgs(query, j)
		if !ok || end <= j+1 {
			continue
		}
		return strings.TrimSpace(query[j+1 : end-1])
	}
	return ""
}

// rewriteTvfCall locates the next case-insensitive `name (` call and,
// if it matches a TVF stub, replaces the entire `name(...)` span with
// `bodyFn(args)`. The args slice contains the comma-separated raw
// token sequences from the call (whitespace-trimmed), with TABLE
// keyword preserved on the leading position.
func rewriteTvfCall(s, name string, bodyFn func(args []string) string) (string, bool) {
	for i := 0; i+len(name) <= len(s); i++ {
		// Skip string literals / quoted identifiers / line comments to
		// avoid matching the keyword inside them.
		switch s[i] {
		case '\'', '"':
			i = scanStringLiteral(s, i) - 1
			continue
		case '`':
			j := i + 1
			for j < len(s) && s[j] != '`' {
				j++
			}
			if j < len(s) {
				j++
			}
			i = j - 1
			continue
		case '-':
			if i+1 < len(s) && s[i+1] == '-' {
				j := i
				for j < len(s) && s[j] != '\n' {
					j++
				}
				i = j - 1
				continue
			}
		}
		if !strings.EqualFold(s[i:i+len(name)], name) {
			continue
		}
		// Boundary check: previous char must not be alnum/_ (avoid
		// matching CHANGES inside MYCHANGES etc).
		if i > 0 {
			c := s[i-1]
			if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
				(c >= '0' && c <= '9') || c == '_' || c == '.' {
				continue
			}
		}
		// Skip whitespace.
		j := i + len(name)
		for j < len(s) && (s[j] == ' ' || s[j] == '\t' || s[j] == '\n' || s[j] == '\r') {
			j++
		}
		if j >= len(s) || s[j] != '(' {
			continue
		}
		// Locate matching close paren.
		end, args, ok := scanBalancedArgs(s, j)
		if !ok {
			continue
		}
		return s[:i] + bodyFn(args) + s[end:], true
	}
	return s, false
}

// scanBalancedArgs reads from `s[start]` (which must be '(') through
// the matching ')' and returns the index one past ')', the
// comma-split argument list (trimmed, paren-balanced), and ok=true on
// success. String literals and back-ticked identifiers are honoured.
func scanBalancedArgs(s string, start int) (int, []string, bool) {
	if start >= len(s) || s[start] != '(' {
		return 0, nil, false
	}
	depth := 0
	args := []string{}
	cur := strings.Builder{}
	for i := start; i < len(s); i++ {
		c := s[i]
		switch c {
		case '(':
			depth++
			if depth > 1 {
				cur.WriteByte(c)
			}
		case ')':
			depth--
			if depth == 0 {
				if cur.Len() > 0 || len(args) > 0 {
					args = append(args, strings.TrimSpace(cur.String()))
				}
				return i + 1, args, true
			}
			cur.WriteByte(c)
		case ',':
			if depth == 1 {
				args = append(args, strings.TrimSpace(cur.String()))
				cur.Reset()
			} else {
				cur.WriteByte(c)
			}
		case '\'', '"':
			endLit := scanStringLiteral(s, i)
			cur.WriteString(s[i:endLit])
			i = endLit - 1
		case '`':
			j := i + 1
			for j < len(s) && s[j] != '`' {
				j++
			}
			if j < len(s) {
				j++
			}
			cur.WriteString(s[i:j])
			i = j - 1
		default:
			cur.WriteByte(c)
		}
	}
	return 0, nil, false
}

func buildAppendsBody(args []string) string {
	tableRef := tvfTableRef(args)
	return "(SELECT *, CAST(NULL AS STRING) AS _CHANGE_TYPE, " +
		"CAST(NULL AS TIMESTAMP) AS _CHANGE_TIMESTAMP " +
		"FROM " + tableRef + " WHERE FALSE)"
}

func buildChangesBody(args []string) string {
	tableRef := tvfTableRef(args)
	return "(SELECT *, CAST(NULL AS STRING) AS _CHANGE_TYPE, " +
		"CAST(NULL AS TIMESTAMP) AS _CHANGE_TIMESTAMP, " +
		"CAST(NULL AS STRING) AS _CHANGE_SEQUENCE_NUMBER " +
		"FROM " + tableRef + " WHERE FALSE)"
}

func buildVectorIndexStatisticsBody(args []string) string {
	return "(SELECT CAST(NULL AS FLOAT64) AS drift FROM (SELECT 1) WHERE FALSE)"
}

// tvfTableRef pulls the table reference out of the first positional
// argument of an APPENDS / CHANGES call. The arg is expected to be
// "TABLE <name>"; we drop the leading TABLE keyword if present.
func tvfTableRef(args []string) string {
	if len(args) == 0 {
		return "(SELECT 1 WHERE FALSE)"
	}
	first := strings.TrimSpace(args[0])
	if strings.EqualFold(first[:min(len(first), 5)], "TABLE") {
		first = strings.TrimSpace(first[5:])
	}
	if first == "" {
		return "(SELECT 1 WHERE FALSE)"
	}
	return first
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
