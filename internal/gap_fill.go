package internal

import (
	"fmt"
	"strings"
)

// applyGapFillRewrite rewrites every `GAP_FILL(...)` invocation in
// `query` to its equivalent SQL using GENERATE_TIMESTAMP_ARRAY +
// LEFT JOIN + window-function gap fill. The upstream analyzer does
// not know GAP_FILL natively, so we lower it before
// AnalyzeStatement runs.
//
// Supported forms (per the BigQuery spec):
//
//	GAP_FILL(
//	  TABLE <name>,          | (<subquery>),
//	  ts_column => '<col>',
//	  bucket_width => INTERVAL <n> <part>,
//	  [partitioning_columns => ['c1', 'c2', ...]],
//	  [value_columns => [('col1', 'null'|'locf'|'linear'), ...]],
//	  [origin => <DATE|DATETIME|TIMESTAMP literal>],
//	  [ignore_null_values => TRUE|FALSE]
//	)
//
// String literals and backtick-quoted identifiers are honoured by
// the scanner, so embedded `GAP_FILL` substrings stay untouched.
func applyGapFillRewrite(query string) string {
	if !containsCaseInsensitive(query, "GAP_FILL") {
		return query
	}
	for {
		start, end, args, ok := findGapFillCall(query)
		if !ok {
			return query
		}
		replacement, err := rewriteGapFill(args)
		if err != nil {
			// On parse failure, leave the call untouched — the
			// analyzer will surface a clearer "Table-valued function
			// not found: GAP_FILL" error than we could here.
			return query
		}
		query = query[:start] + replacement + query[end:]
	}
}

func containsCaseInsensitive(s, needle string) bool {
	return strings.Contains(strings.ToUpper(s), strings.ToUpper(needle))
}

// findGapFillCall locates the next `GAP_FILL(...)` invocation,
// returning the byte range covering `GAP_FILL ( ... )` plus the raw
// inner argument string. Skips string literals and quoted
// identifiers; tracks paren depth.
func findGapFillCall(s string) (start, end int, args string, ok bool) {
	i := 0
	for i < len(s) {
		c := s[i]
		switch {
		case c == '\'' || c == '"':
			i = scanStringLiteral(s, i)
			continue
		case c == '`':
			j := i + 1
			for j < len(s) && s[j] != '`' {
				j++
			}
			if j < len(s) {
				j++
			}
			i = j
			continue
		case c == '-' && i+1 < len(s) && s[i+1] == '-':
			j := i
			for j < len(s) && s[j] != '\n' {
				j++
			}
			i = j
			continue
		}
		if isIdentStart(c) {
			j := i + 1
			for j < len(s) && isIdentCont(s[j]) {
				j++
			}
			word := s[i:j]
			if strings.EqualFold(word, "GAP_FILL") {
				// Skip whitespace before `(`.
				k := j
				for k < len(s) && (s[k] == ' ' || s[k] == '\t' || s[k] == '\n' || s[k] == '\r') {
					k++
				}
				if k < len(s) && s[k] == '(' {
					closeIdx := matchingCloseParen(s, k)
					if closeIdx > 0 {
						return i, closeIdx + 1, s[k+1 : closeIdx], true
					}
				}
			}
			i = j
			continue
		}
		i++
	}
	return 0, 0, "", false
}

// matchingCloseParen returns the offset of the `)` matching the
// `(` at `open`, honouring nested parens, string literals, and
// backtick-quoted identifiers. Returns -1 when unmatched.
func matchingCloseParen(s string, open int) int {
	depth := 0
	i := open
	for i < len(s) {
		c := s[i]
		switch {
		case c == '(':
			depth++
		case c == ')':
			depth--
			if depth == 0 {
				return i
			}
		case c == '\'' || c == '"':
			i = scanStringLiteral(s, i)
			continue
		case c == '`':
			j := i + 1
			for j < len(s) && s[j] != '`' {
				j++
			}
			if j < len(s) {
				j++
			}
			i = j
			continue
		}
		i++
	}
	return -1
}

// gapFillArgs is the parsed argument set extracted from a
// GAP_FILL invocation.
type gapFillArgs struct {
	inputTable         string // bare table name, when used in `TABLE name` form
	inputSubquery      string // `( ... )` SQL, when used in subquery form
	tsColumn           string
	bucketWidth        string // raw SQL of INTERVAL expression
	partitioningCols   []string
	valueCols          []gapFillValueCol
	origin             string // raw SQL of origin expression, may be empty
	ignoreNullValues   bool
	ignoreNullExplicit bool
}

type gapFillValueCol struct {
	name   string
	method string // "null" | "locf" | "linear"
}

func parseGapFillArgs(raw string) (*gapFillArgs, error) {
	parts := splitTopLevelCommas(raw)
	if len(parts) < 1 {
		return nil, fmt.Errorf("GAP_FILL: empty argument list")
	}
	args := &gapFillArgs{ignoreNullValues: true}
	// First positional: TABLE name or (subquery).
	first := strings.TrimSpace(parts[0])
	if up := strings.ToUpper(first); strings.HasPrefix(up, "TABLE ") || strings.HasPrefix(up, "TABLE\t") || strings.HasPrefix(up, "TABLE\n") {
		args.inputTable = strings.TrimSpace(first[len("TABLE"):])
	} else if strings.HasPrefix(first, "(") {
		// Drop the outermost parens.
		args.inputSubquery = strings.TrimSpace(first[1 : len(first)-1])
	} else {
		// Some callers omit the TABLE keyword.
		args.inputTable = first
	}
	for _, p := range parts[1:] {
		p = strings.TrimSpace(p)
		eq := strings.Index(p, "=>")
		if eq < 0 {
			continue
		}
		name := strings.ToLower(strings.TrimSpace(p[:eq]))
		valStr := strings.TrimSpace(p[eq+2:])
		switch name {
		case "ts_column":
			args.tsColumn = stripQuotes(valStr)
		case "bucket_width":
			args.bucketWidth = valStr
		case "partitioning_columns":
			args.partitioningCols = parseStringArray(valStr)
		case "value_columns":
			args.valueCols = parseValueColumnsArray(valStr)
		case "origin":
			args.origin = valStr
		case "ignore_null_values":
			args.ignoreNullExplicit = true
			args.ignoreNullValues = strings.EqualFold(valStr, "TRUE")
		}
	}
	if args.tsColumn == "" {
		return nil, fmt.Errorf("GAP_FILL: ts_column is required")
	}
	if args.bucketWidth == "" {
		return nil, fmt.Errorf("GAP_FILL: bucket_width is required")
	}
	if args.inputTable == "" && args.inputSubquery == "" {
		return nil, fmt.Errorf("GAP_FILL: input table required")
	}
	return args, nil
}

func stripQuotes(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && (s[0] == '\'' || s[0] == '"') && s[0] == s[len(s)-1] {
		return s[1 : len(s)-1]
	}
	return s
}

// splitTopLevelCommas splits a raw argument list at commas that are
// not inside parens / brackets / string literals.
func splitTopLevelCommas(s string) []string {
	var out []string
	depth := 0
	last := 0
	i := 0
	for i < len(s) {
		c := s[i]
		switch {
		case c == '(' || c == '[':
			depth++
		case c == ')' || c == ']':
			depth--
		case c == '\'' || c == '"':
			i = scanStringLiteral(s, i)
			continue
		case c == ',' && depth == 0:
			out = append(out, s[last:i])
			last = i + 1
		}
		i++
	}
	if last < len(s) {
		out = append(out, s[last:])
	}
	return out
}

// parseStringArray accepts `['a', 'b', "c"]` style ARRAY literals.
func parseStringArray(s string) []string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]") {
		s = s[1 : len(s)-1]
	}
	parts := splitTopLevelCommas(s)
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		v := stripQuotes(strings.TrimSpace(p))
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

// parseValueColumnsArray accepts the
// `[('col1','locf'), ('col2','linear')]` shape.
func parseValueColumnsArray(s string) []gapFillValueCol {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]") {
		s = s[1 : len(s)-1]
	}
	parts := splitTopLevelCommas(s)
	var out []gapFillValueCol
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if !strings.HasPrefix(p, "(") || !strings.HasSuffix(p, ")") {
			continue
		}
		inner := p[1 : len(p)-1]
		fields := splitTopLevelCommas(inner)
		if len(fields) < 2 {
			continue
		}
		col := stripQuotes(strings.TrimSpace(fields[0]))
		method := strings.ToLower(stripQuotes(strings.TrimSpace(fields[1])))
		if col == "" {
			continue
		}
		out = append(out, gapFillValueCol{name: col, method: method})
	}
	return out
}

// rewriteGapFill produces a SQL subquery that has the same shape as
// `GAP_FILL(...)` and can be referenced directly. The result is
// wrapped in parens so callers can embed it in FROM positions.
//
// The lowered SQL is structured as a chain of nested subqueries:
//
//	__gf_src       — input rows tagged with their bucket
//	__gf_ticks     — generated bucket boundaries (native timestamps,
//	                 via a recursive CTE; see tick-generation note)
//	__gf_joined    — LEFT JOIN of ticks vs aligned
//	__gf_grouped   — COUNT(col) OVER as a per-column LOCF group id
//	                 (the standard SQL idiom for IGNORE NULLS without
//	                 engine support)
//	final SELECT   — null / locf / linear projection
//
// Tick-generation note: bucket boundaries are produced by a
// WITH RECURSIVE CTE that steps TIMESTAMP_ADD(prev, bucket_width)
// from the floor bucket to the ceiling bucket, rather than by
// GENERATE_TIMESTAMP_ARRAY. The bucket column (TIMESTAMP_BUCKET) and
// the source time column both lower to native DuckDB timestamps,
// while GENERATE_TIMESTAMP_ARRAY returns the value-layer base64
// envelope; joining the two representations is what made the
// envelope ticks uncomparable to the native bucket column. Stepping
// with TIMESTAMP_ADD keeps every tick in the same native domain as
// the join key.
func rewriteGapFill(rawArgs string) (string, error) {
	args, err := parseGapFillArgs(rawArgs)
	if err != nil {
		return "", err
	}
	inputRef := args.inputTable
	if inputRef == "" {
		inputRef = "(" + args.inputSubquery + ")"
	}
	ts := args.tsColumn
	bw := args.bucketWidth
	origin := args.origin
	if origin == "" {
		origin = "TIMESTAMP '1950-01-01 00:00:00+00'"
	}

	// The recursive step advances one bucket at a time. TIMESTAMP_ADD
	// needs an INTERVAL <count> <part> literal, so split the raw
	// bucket_width text back into its count and part.
	stepInterval, err := normalizeIntervalLiteral(bw)
	if err != nil {
		return "", err
	}

	var partitionExprs []string
	for _, c := range args.partitioningCols {
		partitionExprs = append(partitionExprs, "`"+c+"`")
	}
	partitionList := strings.Join(partitionExprs, ", ")

	// Aligned source: every input row tagged with the bucket
	// boundary that contains its timestamp.
	alignedSelect := fmt.Sprintf(
		"SELECT TIMESTAMP_BUCKET(`%s`, %s, %s) AS `%s`, * EXCEPT(`%s`) FROM %s",
		ts, bw, origin, ts, ts, inputRef,
	)

	// Bucket-tick generation via a recursive CTE. The anchor reads
	// the floor / ceiling bucket boundaries (already bucket-aligned
	// because TIMESTAMP_BUCKET is monotonic, so MIN/MAX of the
	// bucketed source equals the bucket of MIN/MAX); each recursive
	// step adds one bucket_width and stops once it passes the
	// ceiling. The ceiling is carried as a column so the recursive
	// term can reference it. Native throughout: the tick column has
	// the same TIMESTAMP type as the join key.
	var ticksCTE string
	if partitionList == "" {
		ticksCTE = fmt.Sprintf(
			"`__gf_ticks` AS ("+
				"SELECT `__gf_lo` AS `%s`, `__gf_hi` AS `__gf_hi` FROM (SELECT MIN(`%s`) AS `__gf_lo`, MAX(`%s`) AS `__gf_hi` FROM (%s)) "+
				"WHERE `__gf_lo` IS NOT NULL "+
				"UNION ALL "+
				"SELECT TIMESTAMP_ADD(`%s`, %s) AS `%s`, `__gf_hi` AS `__gf_hi` FROM `__gf_ticks` WHERE TIMESTAMP_ADD(`%s`, %s) <= `__gf_hi`"+
				")",
			ts, ts, ts, alignedSelect, ts, stepInterval, ts, ts, stepInterval,
		)
	} else {
		ticksCTE = fmt.Sprintf(
			"`__gf_ticks` AS ("+
				"SELECT `__gf_lo` AS `%s`, `__gf_hi` AS `__gf_hi`, %s FROM (SELECT %s, MIN(`%s`) AS `__gf_lo`, MAX(`%s`) AS `__gf_hi` FROM (%s) GROUP BY %s) "+
				"WHERE `__gf_lo` IS NOT NULL "+
				"UNION ALL "+
				"SELECT TIMESTAMP_ADD(`%s`, %s) AS `%s`, `__gf_hi` AS `__gf_hi`, %s FROM `__gf_ticks` WHERE TIMESTAMP_ADD(`%s`, %s) <= `__gf_hi`"+
				")",
			ts, partitionList, partitionList, ts, ts, alignedSelect, partitionList,
			ts, stepInterval, ts, partitionList, ts, stepInterval,
		)
	}

	// LEFT JOIN ticks ↔ aligned. We hand-craft the join key list so
	// that USING() picks up the partitioning_columns plus ts.
	joinUsing := "`" + ts + "`"
	for _, c := range args.partitioningCols {
		joinUsing += ", `" + c + "`"
	}
	ticksProject := "`" + ts + "`"
	for _, c := range args.partitioningCols {
		ticksProject += ", `" + c + "`"
	}
	joined := fmt.Sprintf(
		"SELECT * FROM (SELECT %s FROM `__gf_ticks`) AS `__gf_t` LEFT JOIN (%s) AS `__gf_src` USING (%s)",
		ticksProject, alignedSelect, joinUsing,
	)

	// For LOCF / linear we need a per-column "group id" computed as
	// COUNT(col) OVER (ORDER BY ts). Rows that share the same group
	// id all sit downstream of the same last-non-null value.
	partClause := ""
	if partitionList != "" {
		partClause = "PARTITION BY " + partitionList + " "
	}
	var groupedCols []string
	groupedCols = append(groupedCols, "*")
	needGroups := false
	hasLinear := false
	for _, vc := range args.valueCols {
		if vc.method == "locf" || vc.method == "linear" {
			needGroups = true
			groupedCols = append(groupedCols, fmt.Sprintf(
				"COUNT(`%s`) OVER (%sORDER BY `%s` ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW) AS `__gf_grp_%s`",
				vc.name, partClause, ts, vc.name,
			))
		}
		if vc.method == "linear" {
			hasLinear = true
			// Also need the forward-looking group id, for the
			// "next non-null" lookup.
			groupedCols = append(groupedCols, fmt.Sprintf(
				"COUNT(`%s`) OVER (%sORDER BY `%s` ROWS BETWEEN CURRENT ROW AND UNBOUNDED FOLLOWING) AS `__gf_fgrp_%s`",
				vc.name, partClause, ts, vc.name,
			))
		}
	}
	// Linear interpolation works in epoch seconds rather than on the
	// timestamps directly: MIN/MAX of a TIMESTAMP resolves to the
	// value-layer envelope (a VARCHAR), which TIMESTAMP_DIFF then
	// cannot bind natively. UNIX_SECONDS lowers to a native BIGINT, so
	// carrying the tick as __gf_secs and aggregating that keeps every
	// anchor and span in the native integer domain.
	if hasLinear {
		groupedCols = append(groupedCols, fmt.Sprintf("UNIX_SECONDS(`%s`) AS `__gf_secs`", ts))
	}

	grouped := joined
	if needGroups {
		grouped = fmt.Sprintf("SELECT %s FROM (%s) AS `__gf_j`", strings.Join(groupedCols, ", "), joined)
	}

	// Linear interpolation needs the bracketing non-null anchors
	// (value + timestamp on each side) materialized as plain columns
	// before the arithmetic runs. Computing them as window functions
	// inline in the final projection made the analyzer resolve some
	// TIMESTAMP_DIFF instances to the value-layer envelope and others
	// natively (the asymmetry depended on whether an argument was a
	// bare column or a window call), leaving the division with a
	// VARCHAR operand. Emitting them here as their own columns makes
	// every downstream reference a uniform native column.
	anchored := grouped
	var anchorCols []string
	anchorCols = append(anchorCols, "*")
	needAnchors := false
	for _, vc := range args.valueCols {
		if vc.method != "linear" {
			continue
		}
		needAnchors = true
		prevPart := "PARTITION BY `__gf_grp_" + vc.name + "`"
		nextPart := "PARTITION BY `__gf_fgrp_" + vc.name + "`"
		if partitionList != "" {
			prevPart = "PARTITION BY " + partitionList + ", `__gf_grp_" + vc.name + "`"
			nextPart = "PARTITION BY " + partitionList + ", `__gf_fgrp_" + vc.name + "`"
		}
		// __gf_grp increments at each non-null, so a group's earliest
		// row is its non-null anchor (MIN epoch seconds); __gf_fgrp
		// decrements going forward, so a group's latest row is the
		// next non-null anchor (MAX epoch seconds). MAX(value) over
		// each group surfaces the single non-null value.
		anchorCols = append(anchorCols,
			fmt.Sprintf("MAX(`%s`) OVER (%s) AS `__gf_prevv_%s`", vc.name, prevPart, vc.name),
			fmt.Sprintf("MAX(`%s`) OVER (%s) AS `__gf_nextv_%s`", vc.name, nextPart, vc.name),
			fmt.Sprintf("MIN(`__gf_secs`) OVER (%s) AS `__gf_prevt_%s`", prevPart, vc.name),
			fmt.Sprintf("MAX(`__gf_secs`) OVER (%s) AS `__gf_nextt_%s`", nextPart, vc.name),
		)
	}
	if needAnchors {
		anchored = fmt.Sprintf("SELECT %s FROM (%s) AS `__gf_a`", strings.Join(anchorCols, ", "), grouped)
	}

	// Final projection.
	var projCols []string
	projCols = append(projCols, "`"+ts+"`")
	for _, p := range args.partitioningCols {
		projCols = append(projCols, "`"+p+"`")
	}
	for _, vc := range args.valueCols {
		switch vc.method {
		case "locf":
			// LOCF: take MAX of the non-null value within the
			// LOCF group. The window partition includes the
			// per-row group id so each group sees a single
			// non-null value (or NULL if no preceding value).
			locfPart := "PARTITION BY `__gf_grp_" + vc.name + "`"
			if partitionList != "" {
				locfPart = "PARTITION BY " + partitionList + ", `__gf_grp_" + vc.name + "`"
			}
			projCols = append(projCols, fmt.Sprintf(
				"MAX(`%s`) OVER (%s) AS `%s`", vc.name, locfPart, vc.name,
			))
		case "linear":
			// Linear interpolation between the bracketing non-null
			// anchors, all read from the materialized __gf_*_<col>
			// columns produced by the anchored layer (epoch seconds for
			// the timestamps).
			prevV := fmt.Sprintf("`__gf_prevv_%s`", vc.name)
			nextV := fmt.Sprintf("`__gf_nextv_%s`", vc.name)
			prevT := fmt.Sprintf("`__gf_prevt_%s`", vc.name)
			nextT := fmt.Sprintf("`__gf_nextt_%s`", vc.name)
			// IEEE_DIVIDE rather than `/`: its lowering casts both operands
			// to a native DOUBLE, whereas `/` coerces them through the
			// value-layer envelope and then cannot bind the native `* 1.0`
			// lift against a VARCHAR. The span-zero guard already excludes
			// the single-point segment, so the IEEE +/-Inf-on-zero
			// behaviour is never reached.
			frac := fmt.Sprintf("IEEE_DIVIDE(UNIX_SECONDS(`%s`) - %s, %s - %s)", ts, prevT, nextT, prevT)
			// The interpolated ELSE branch is a native DOUBLE (IEEE_DIVIDE
			// forces a native cast), but a value passed straight through is
			// value-layer INT64 (a VARCHAR envelope), and DuckDB's CASE
			// refuses to mix DOUBLE with VARCHAR. Adding a native DOUBLE
			// zero (IEEE_DIVIDE(0, 1)) lifts each passthrough branch into
			// the native DOUBLE domain without changing its value, so every
			// branch unifies on DOUBLE.
			nativeZero := "IEEE_DIVIDE(0, 1)"
			// The no-anchor branch is a typed FLOAT64 NULL rather than a
			// bare NULL: an untyped NULL arm leaves the analyzer unable to
			// resolve the CASE type, so it defaults the output column to
			// STRING and then cannot accept the native DOUBLE the other
			// arms produce.
			interp := fmt.Sprintf(
				"CASE WHEN `%s` IS NOT NULL THEN (`%s` + %s) "+
					"WHEN %s IS NULL OR %s IS NULL THEN CAST(NULL AS FLOAT64) "+
					"WHEN (%s - %s) = 0 THEN (%s + %s) "+
					"ELSE %s + %s * (%s - %s) END",
				vc.name, vc.name, nativeZero,
				prevV, nextV,
				nextT, prevT, prevV, nativeZero,
				prevV, frac, nextV, prevV,
			)
			projCols = append(projCols, fmt.Sprintf("%s AS `%s`", interp, vc.name))
		case "null", "":
			projCols = append(projCols, fmt.Sprintf("`%s`", vc.name))
		default:
			return "", fmt.Errorf("GAP_FILL: unknown method %q for column %q", vc.method, vc.name)
		}
	}

	out := fmt.Sprintf(
		"(WITH RECURSIVE %s SELECT %s FROM (%s) AS `__gf_g` ORDER BY `%s`)",
		ticksCTE,
		strings.Join(projCols, ", "),
		anchored, ts,
	)
	return out, nil
}

// normalizeIntervalLiteral takes the raw bucket_width text (e.g.
// "INTERVAL 1 MINUTE") and returns a canonical single-part
// "INTERVAL <count> <part>" literal usable as the second argument of
// TIMESTAMP_ADD. Multi-part intervals (e.g. "INTERVAL '1:30' HOUR TO
// MINUTE") are not supported by the recursive single-step generator
// and are rejected so the caller leaves the GAP_FILL call untouched.
func normalizeIntervalLiteral(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	up := strings.ToUpper(s)
	if !strings.HasPrefix(up, "INTERVAL") {
		return "", fmt.Errorf("GAP_FILL: bucket_width must be an INTERVAL literal, got %q", raw)
	}
	rest := strings.TrimSpace(s[len("INTERVAL"):])
	fields := strings.Fields(rest)
	if len(fields) != 2 {
		return "", fmt.Errorf("GAP_FILL: unsupported bucket_width %q", raw)
	}
	count := fields[0]
	part := strings.ToUpper(fields[1])
	return fmt.Sprintf("INTERVAL %s %s", count, part), nil
}
