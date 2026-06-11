package internal

import "strings"

// applyTimestampDiffWeekRewrite works around a GoogleSQL analyzer gap in the
// wasm binding used by the DuckDB branch: TIMESTAMP_DIFF rejects WEEK-family
// parts for TIMESTAMP arguments before the formatter can lower the call. The
// repository's BigQuery spec expects the DATE_DIFF week-boundary behavior, so
// rewrite only the timestamp-looking unsupported shape before analysis.
func applyTimestampDiffWeekRewrite(query string) string {
	if !containsCaseInsensitive(query, "TIMESTAMP_DIFF") {
		return query
	}
	for {
		start, end, args, ok := findFunctionCall(query, "TIMESTAMP_DIFF")
		if !ok {
			return query
		}
		replacement, changed := rewriteTimestampDiffWeekCall(args)
		if !changed {
			next := query[end:]
			if !containsCaseInsensitive(next, "TIMESTAMP_DIFF") {
				return query
			}
			query = query[:end] + applyTimestampDiffWeekRewrite(next)
			return query
		}
		query = query[:start] + replacement + query[end:]
	}
}

func rewriteTimestampDiffWeekCall(rawArgs string) (string, bool) {
	args := splitTopLevelCommas(rawArgs)
	if len(args) != 3 {
		return "", false
	}
	part := strings.ToUpper(strings.TrimSpace(args[2]))
	if _, ok := weekPartOffset(part); !ok {
		return "", false
	}
	if !isTimestampDiffTimestampArg(args[0]) || !isTimestampDiffTimestampArg(args[1]) {
		return "", false
	}
	return "DATE_DIFF(DATE(" + strings.TrimSpace(args[0]) + "), DATE(" + strings.TrimSpace(args[1]) + "), " + strings.TrimSpace(args[2]) + ")", true
}

func isTimestampDiffTimestampArg(arg string) bool {
	arg = strings.TrimSpace(arg)
	upper := strings.ToUpper(arg)
	return strings.HasPrefix(upper, "TIMESTAMP ") ||
		strings.HasPrefix(upper, "TIMESTAMP\"") ||
		strings.HasPrefix(upper, "TIMESTAMP'") ||
		strings.HasPrefix(upper, "TIMESTAMP(")
}

func findFunctionCall(s, name string) (start, end int, args string, ok bool) {
	i := 0
	for i < len(s) {
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
		case '-':
			if i+1 < len(s) && s[i+1] == '-' {
				j := i
				for j < len(s) && s[j] != '\n' {
					j++
				}
				i = j
				continue
			}
		}
		if !isIdentStart(s[i]) {
			i++
			continue
		}
		j := i + 1
		for j < len(s) && isIdentCont(s[j]) {
			j++
		}
		if !strings.EqualFold(s[i:j], name) {
			i = j
			continue
		}
		k := j
		for k < len(s) && (s[k] == ' ' || s[k] == '\t' || s[k] == '\n' || s[k] == '\r') {
			k++
		}
		if k >= len(s) || s[k] != '(' {
			i = j
			continue
		}
		closeIdx := matchingCloseParen(s, k)
		if closeIdx < 0 {
			return 0, 0, "", false
		}
		return i, closeIdx + 1, s[k+1 : closeIdx], true
	}
	return 0, 0, "", false
}
