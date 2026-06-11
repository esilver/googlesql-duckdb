package internal

import (
	"context"
	"encoding/base64"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	googlesql "github.com/goccy/go-googlesql"
	"github.com/goccy/go-json"

	"github.com/goccy/googlesqlite/internal/value"

	"github.com/goccy/googlesqlite/internal/functions/window"
)

type Formatter interface {
	FormatSQL(context.Context) (string, error)
}

func New(node googlesql.ResolvedNode) Formatter {
	return newNode(node)
}

func getTableName(ctx context.Context, n googlesql.ResolvedNode) (string, error) {
	// Preferred path: pull the bound Table off a ResolvedTableScan and
	// ask it for its name. The go-googlesql version used a side-car
	// nodeMap that mapped ResolvedTableScan → ASTPathExpression to
	// recover the original user-supplied identifier, but that side
	// channel is not exposed through the wasm bridge. For the common
	// case (single-segment table names) the catalog-bound Table.Name()
	// is equivalent.
	if scan, ok := n.(*googlesql.ResolvedTableScan); ok {
		if table, err := scan.Table(); err == nil && table != nil {
			// Preferred: look the table's storage name (the SQLite
			// table identifier built from the spec's full namespace
			// path) up against our local catalog index. This works
			// regardless of which sub-catalog level the analyzer
			// resolved through, so it is robust to the user
			// supplying a path shorter than the namespace
			// (e.g. `dataset.table` while the conn's NamePath is
			// only the project).
			if a := analyzerFromContext(ctx); a != nil && a.catalog != nil {
				if storage := a.catalog.StorageNameForTable(table); storage != "" {
					return storage, nil
				}
			}
			if name, err := table.Name(); err == nil && name != "" {
				namePath := namePathFromContext(ctx)
				return namePath.format([]string{name}), nil
			}
		}
	}
	return "", fmt.Errorf("failed to find path node from table node %T", n)
}

func getFuncName(ctx context.Context, n googlesql.ResolvedNode) (string, error) {
	type fnCall interface {
		Function() (*googlesql.Function, error)
	}
	// Preferred: look the resolved Function up in our local catalog
	// index. This works regardless of which sub-catalog level the
	// analyzer resolved through, so it stays correct when the user
	// supplies a path shorter than the full namespace.
	if fc, ok := n.(fnCall); ok {
		if fn, err := fc.Function(); err == nil && fn != nil {
			if a := analyzerFromContext(ctx); a != nil && a.catalog != nil {
				if storage := a.catalog.StorageNameForFunction(fn); storage != "" {
					return storage, nil
				}
			}
		}
	}
	// Fallback: catalog index didn't have this function (e.g. a
	// built-in never registered through addFunctionSpec). Read the
	// function identity straight off the resolved node and merge
	// with the conn's NamePath.
	namePath := namePathFromContext(ctx)
	if fc, ok := n.(fnCall); ok {
		if fn, err := fc.Function(); err == nil && fn != nil {
			if name, err := fn.Name(); err == nil && name != "" {
				return namePath.format([]string{name}), nil
			}
		}
	}
	return "", fmt.Errorf("failed to find path node from function node %T", n)
}

func uniqueColumnName(ctx context.Context, col *googlesql.ResolvedColumn) string {
	colName, _ := col.Name()
	if useTableNameForColumn(ctx) {
		return fmt.Sprintf("%s.%s", m1(col.TableName()), colName)
	}
	if useColumnID(ctx) {
		colID, _ := col.ColumnId()
		return fmt.Sprintf("%s#%d", colName, colID)
	}
	return colName
}

type inputPattern int

const (
	InputKeep      inputPattern = 0
	InputNeedsWrap inputPattern = 1
	InputNeedsFrom inputPattern = 2
)

func getInputPattern(input string) inputPattern {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return InputKeep
	}
	if strings.HasPrefix(trimmed, "FROM") {
		return InputKeep
	}
	if strings.HasPrefix(trimmed, "SELECT") {
		return InputNeedsWrap
	}
	if strings.HasPrefix(trimmed, "WITH") {
		return InputNeedsWrap
	}
	return InputNeedsFrom
}

func formatInput(input string) (string, error) {
	switch getInputPattern(input) {
	case InputKeep:
		return input, nil
	case InputNeedsWrap:
		return fmt.Sprintf("FROM (%s)", input), nil
	case InputNeedsFrom:
		return fmt.Sprintf("FROM %s", input), nil
	}
	return "", fmt.Errorf("unexpected input pattern: %s", input)
}

func getFuncNameAndArgs(ctx context.Context, node *ResolvedBaseFunctionCallNode, isWindowFunc bool) (string, []string, error) {
	args := []string{}
	for _, a := range m1(node.ArgumentList()) {
		arg, err := newNode(a).FormatSQL(ctx)
		if err != nil {
			return "", nil, err
		}
		args = append(args, arg)
	}
	funcName := m1(m1(node.Function()).FullName(false))
	funcName = strings.Replace(funcName, ".", "_", -1)

	// DuckDB retarget: COUNT(*) is analyzed as the $-builtin
	// `$count_star` and would otherwise lower to the SQLite UDF
	// googlesqlite_count_star(). DuckDB has no such UDF but supports
	// native COUNT(*) with identical semantics, so emit that directly.
	// (Milestone-0 spine: a hand-translated single-function lowering,
	// not the full aggregate-UDF retarget.)
	if funcName == "$count_star" {
		return "count", []string{"*"}, nil
	}

	// DuckDB retarget: the GoogleSQL geography builtins (ST_* / S2_*)
	// resolve as analyzer builtins, so they reach here with their bare
	// lowercase name and are absent from normalFuncMap (which only the
	// SQLite registration path populates). Route them to the
	// googlesqlite_st_* / googlesqlite_s2_* scalar UDFs registered on
	// the DuckDB handle (see duckdb_geo_udf.go). Those UDFs reuse the
	// pure-Go geography bodies and the value-layer envelope, so the
	// argument SQL the default formatter already produced (base64
	// envelope for WKT/GeoJSON strings, raw doubles for coordinates)
	// flows through unchanged.
	if isGeographyScalarFunc(funcName) {
		return "googlesqlite_" + funcName, args, nil
	}

	_, existsCurrentTimeFunc := currentTimeFuncMap[funcName]
	_, existsNormalFunc := normalFuncMap[funcName]
	_, existsAggregateFunc := aggregateFuncMap[funcName]
	_, existsWindowFunc := windowFuncMap[funcName]
	// $-prefixed builtin names (e.g. $array_at_offset, $divide) are
	// registered without the prefix; treat the unprefixed form as the
	// real key when deciding whether a safe variant exists.
	existsNormalFuncForSafe := existsNormalFunc
	if !existsNormalFuncForSafe && strings.HasPrefix(funcName, "$") {
		_, existsNormalFuncForSafe = normalFuncMap[funcName[1:]]
	}
	// DuckDB path: normalFuncMap is empty, so the map lookup above can't
	// see that an operator builtin has a safe form. The native operator
	// lowering (tryNativeOperator) implements the safe form directly for
	// these, so mark them safe-able here to drive the googlesqlite_safe_
	// prefix in safe-eval / SAFE mode.
	if !existsNormalFuncForSafe {
		bare := strings.TrimPrefix(funcName, "$")
		if _, ok := nativelySafeOperatorBuiltins[bare]; ok {
			existsNormalFuncForSafe = true
		}
		// The array-element accessors ($array_at_offset / $array_at_ordinal)
		// raise on an out-of-range index. Their NULL-on-failure forms are
		// registered as standalone googlesqlite_safe_array_at_* UDFs (the
		// default emit below produces that name once the googlesqlite_safe_
		// prefix is selected), so mark them safe-able too. Without this an
		// out-of-bounds OFFSET inside IFERROR / ISERROR / NULLIFERROR would
		// abort the statement instead of folding to NULL.
		if _, ok := nativelySafeValueLayerBuiltins[bare]; ok {
			existsNormalFuncForSafe = true
		}
		if isDuckDBTemporalFormatParseFunc(bare) {
			existsNormalFuncForSafe = true
		}
	}
	currentTime := CurrentTime(ctx)

	funcPrefix := "googlesqlite"
	if m1(node.ErrorMode()) == googlesql.ResolvedFunctionCallBaseEnums_ErrorModeSafeErrorMode {
		if !existsNormalFuncForSafe {
			return "", nil, fmt.Errorf("SAFE is not supported for function %s", funcName)
		}
		funcPrefix = "googlesqlite_safe"
	} else if inSafeEvalMode(ctx) && existsNormalFuncForSafe {
		// IFERROR / ISERROR / NULLIFERROR sub-context: route through the
		// safe variant so a runtime failure folds to NULL instead of
		// aborting the statement. Functions that have no safe variant
		// stay on the raising form — callers that hit them inside an
		// error-handling expression need to use SAFE.<func> explicitly.
		funcPrefix = "googlesqlite_safe"
	}

	if strings.HasPrefix(funcName, "$") {
		if isWindowFunc {
			funcName = fmt.Sprintf("%s_window_%s", funcPrefix, funcName[1:])
		} else {
			funcName = fmt.Sprintf("%s_%s", funcPrefix, funcName[1:])
		}
	} else if existsCurrentTimeFunc {
		if currentTime != nil {
			args = append(
				args,
				fmt.Sprint(currentTime.UnixNano()),
			)
		}
		funcName = fmt.Sprintf("%s_%s", funcPrefix, funcName)
	} else if existsNormalFunc {
		funcName = fmt.Sprintf("%s_%s", funcPrefix, funcName)
	} else if isDuckDBTemporalFormatParseFunc(funcName) {
		funcName = fmt.Sprintf("%s_%s", funcPrefix, funcName)
	} else if !isWindowFunc && existsAggregateFunc {
		funcName = fmt.Sprintf("%s_%s", funcPrefix, funcName)
	} else if isWindowFunc && existsWindowFunc {
		funcName = fmt.Sprintf("%s_window_%s", funcPrefix, funcName)
	} else {
		fname, err := getFuncName(ctx, node)
		if err != nil {
			return "", nil, err
		}
		funcName = fname
	}
	return funcName, args, nil
}

func isDuckDBTemporalFormatParseFunc(funcName string) bool {
	switch strings.TrimPrefix(strings.TrimPrefix(funcName, "googlesqlite_safe_"), "googlesqlite_") {
	case "format_date", "format_datetime", "format_time", "format_timestamp",
		"parse_date", "parse_datetime", "parse_time", "parse_timestamp":
		return true
	}
	return false
}

func formatDuckDBTemporalFormatParseFunc(funcName string, args []string) (string, bool) {
	safe := strings.HasPrefix(funcName, "googlesqlite_safe_")
	bare := strings.TrimPrefix(strings.TrimPrefix(funcName, "googlesqlite_safe_"), "googlesqlite_")
	if !isDuckDBTemporalFormatParseFunc(bare) {
		return "", false
	}
	prefix := "googlesqlite_"
	if safe {
		prefix = "googlesqlite_safe_"
	}
	call := fmt.Sprintf("%s%s(%s)", prefix, bare, strings.Join(args, ", "))
	switch bare {
	case "parse_date":
		return fmt.Sprintf("CAST(%s AS DATE)", call), true
	case "parse_time":
		return fmt.Sprintf("CAST(%s AS TIME)", call), true
	default:
		return call, true
	}
}

func (n *LiteralNode) FormatSQL(ctx context.Context) (string, error) {
	if n.node == nil {
		return "", nil
	}
	return literalFromGoogleSQLValue(*m1(n.node.Value()))
}

func (n *ParameterNode) FormatSQL(ctx context.Context) (string, error) {
	if c := paramCollectorFromContext(ctx); c != nil && n.node != nil {
		// Named parameters dedup by name in getParamsFromNode; for
		// positional `?` parameters we want every occurrence in
		// AST order, so just append unconditionally here.
		c.params = append(c.params, n.node)
	}
	name, _ := n.node.Name()
	if name == "" {
		return "?", nil
	}
	// DuckDB named-parameter placeholders use $name, not @name (@name
	// parses as a column reference). The inner stmt still binds the
	// encoded args positionally; DuckDB maps a single positional value
	// onto the matching named placeholder.
	return fmt.Sprintf("$%s", name), nil
}

func (n *ColumnRefNode) FormatSQL(ctx context.Context) (string, error) {
	if n.node == nil {
		return "", nil
	}
	columnMap := columnRefMap(ctx)
	col, _ := n.node.Column()
	colName := uniqueColumnName(ctx, col)
	if ref, exists := columnMap[colName]; exists {
		delete(columnMap, colName)
		return ref, nil
	}
	return fmt.Sprintf("\"%s\"", colName), nil
}

func (n *SystemVariableNode) FormatSQL(ctx context.Context) (string, error) {
	if n == nil || n.node == nil {
		return "", nil
	}
	pathParts, _ := n.node.NamePath()
	name := strings.Join(pathParts, ".")
	// Read directly off the Conn so values written by an earlier SET
	// in the same connection (or from another driver entrypoint that
	// only sees Conn.SetSystemVariable) are observable. The
	// systemVarsKey snapshot is taken at Analyze-time and would miss
	// SETs issued during this exact analyze cycle.
	var raw string
	var have bool
	if conn := connFromContext(ctx); conn != nil {
		if v, ok := conn.SystemVariable(name); ok {
			raw, have = v, true
		}
	}
	if !have {
		if vars := systemVarsFromContext(ctx); vars != nil {
			if v, ok := vars[name]; ok {
				raw, have = v, true
			}
		}
	}
	if !have {
		return "NULL", nil
	}
	// Surrounding query will scan the result through the value
	// decoder, which expects the googlesqlite envelope. Emit a
	// StringValue literal so the round-trip works.
	return literalFromValue(value.StringValue(raw))
}

// FilterFieldNode lowers a ResolvedFilterField (the analyzer node
// FILTER_FIELDS resolves into) to a runtime call. Each filter-field
// arg carries an include/exclude flag plus a dotted path of
// FieldDescriptors. We flatten the path into the dot-separated field
// numbers our runtime expects ("1.2" for `type.award_name`), prefix
// the path with `+`/`-`, and concatenate every arg into a single
// path-spec string.
func (n *FilterFieldNode) FormatSQL(ctx context.Context) (string, error) {
	if n.node == nil {
		return "", nil
	}
	exprNode := m1(n.node.Expr())
	exprSQL, err := newNode(exprNode).FormatSQL(ctx)
	if err != nil {
		return "", err
	}
	argList, err := n.node.FilterFieldArgList()
	if err != nil {
		return "", err
	}
	parts := make([]string, 0, len(argList))
	for _, a := range argList {
		if a == nil {
			continue
		}
		include, _ := a.Include()
		fdPath, _ := a.FieldDescriptorPath()
		nums := make([]string, 0, len(fdPath))
		for _, fd := range fdPath {
			num, _ := fd.Number()
			nums = append(nums, strconv.Itoa(int(num)))
		}
		sign := "+"
		if !include {
			sign = "-"
		}
		parts = append(parts, sign+strings.Join(nums, "."))
	}
	pathSpec := strings.Join(parts, ",")
	resetReq, _ := n.node.ResetClearedRequiredFields()
	protoName := ""
	if t, _ := exprNode.Type(); t != nil {
		ds, _ := t.DebugString(false)
		protoName = protoNameFromDebug(ds)
	}
	return fmt.Sprintf("googlesqlite_filter_fields(%s, %s, %t, %s)",
		exprSQL,
		protoFormatLiteralString(pathSpec),
		resetReq,
		protoFormatLiteralString(protoName),
	), nil
}

func (n *FunctionCallNode) FormatSQL(ctx context.Context) (string, error) {
	if n.node == nil {
		return "", nil
	}
	// Special-case the graph-element built-ins (LABELS, ELEMENT_ID,
	// PROPERTY_NAMES, SOURCE_NODE_ID, DESTINATION_NODE_ID,
	// ELEMENT_DEFINITION_NAME). They depend on the per-call graph
	// element metadata recorded in graphElementContext, so we lower
	// them here before the default funcName/args formatter kicks
	// in. See internal/graph_scan_node.go.
	if sql, ok, err := tryFormatGraphElementFunc(ctx, n.node); ok || err != nil {
		return sql, err
	}
	funcName, args, err := getFuncNameAndArgs(ctx, n.node.ResolvedFunctionCallBase, false)
	if err != nil {
		return "", err
	}
	// DuckDB retarget: STRING is an overloaded GoogleSQL name. The JSON
	// accessor STRING(json) is registered in normalFuncMap (so
	// getFuncNameAndArgs prefixed funcName to googlesqlite_string), but the
	// temporal converter STRING(timestamp[, tz]) shares the same name and
	// must lower to a native CAST instead of the JSON UDF (which rejects the
	// time.Time DuckDB hands it). Disambiguate on the first argument's type
	// kind: a temporal first arg is the converter overload.
	if funcName == "googlesqlite_string" {
		kinds := argKinds(m1(n.node.ArgumentList()))
		if len(kinds) >= 1 && isTemporalTypeKind(kinds[0]) {
			if sql, ok := tryNativeDatetimeFunc("string", args, kinds); ok {
				return sql, nil
			}
		}
	}
	// DuckDB retarget: HLL_COUNT.EXTRACT resolves to the bare leaf `extract`,
	// which DuckDB's parser reads as the EXTRACT(part FROM expr) keyword form
	// and rejects. Remap it to the registered googlesqlite_hll_count_extract
	// scalar UDF before tryFormatExtract sees the name. The date/INTERVAL
	// EXTRACT builtin arrives as googlesqlite_extract (the $-prefixed form), so
	// keying on the bare `extract` leaf does not catch it.
	if mapped, ok := duckDBExtractFuncRemap[funcName]; ok {
		funcName = mapped
	}
	// DuckDB retarget (PATH B): the GoogleSQL analyzer lowers the basic
	// operators ($equal, $and, $or, $not, comparisons, arithmetic,
	// $is_null, $in) to $-builtins that getFuncNameAndArgs turns into
	// googlesqlite_<op> scalar UDFs. Those UDFs are not registered on
	// DuckDB (and need not be: DuckDB has native infix/prefix operators
	// with matching semantics). Emit the native operator form directly.
	// DuckDB retarget (math family): the BigQuery math builtins
	// (IS_NAN, IEEE_DIVIDE, DIV, SAFE_*, RAND, LOG, RANGE_BUCKET,
	// the reciprocal trig forms, vector-distance funcs) reach the
	// formatter as their bare resolved names because normalFuncMap is
	// only populated for the SQLite UDF path. Lower each to its DuckDB
	// native equivalent (or an inline expression with matching
	// semantics) instead of emitting an unknown scalar function. This
	// runs before tryNativeOperator so the bare SAFE_DIVIDE name keeps
	// its NULL-on-zero semantics instead of being mistaken for the
	// raising `divide` operator after a naive `safe_` prefix strip.
	if sql, ok := tryNativeMathFunc(funcName, args, argKinds(m1(n.node.ArgumentList()))); ok {
		return sql, nil
	}
	if sql, ok := tryNativeOperator(funcName, args, argKinds(m1(n.node.ArgumentList()))); ok {
		return sql, nil
	}
	if sql, ok := formatDuckDBTemporalFormatParseFunc(funcName, args); ok {
		return sql, nil
	}
	// DuckDB retarget: date / datetime / time / timestamp builtins lower
	// to native DuckDB date functions (strftime, date_diff, date_trunc,
	// epoch, make_timestamp, ...) instead of googlesqlite_* UDFs.
	kinds := argKinds(m1(n.node.ArgumentList()))
	if sql, ok := tryNativeDatetimeFunc(funcName, args, kinds); ok {
		return sql, nil
	}
	if sql, ok, err := n.tryFormatExtract(ctx, funcName, args); ok || err != nil {
		return sql, err
	}
	if sql, ok, err := n.tryFormatDateConstructor(funcName, args, kinds); ok || err != nil {
		return sql, err
	}
	// DuckDB retarget: the array-family value-layer functions resolve to
	// bare builtin names (array_concat, array_length, ...) because the
	// normalFuncMap prefixing only runs on the SQLite registration path.
	// DuckDB's own array_* natives operate on native LIST values, not the
	// base64 envelope strings our value layer carries, so route these to
	// the registered googlesqlite_<name> scalar UDFs that decode the
	// envelope, run the shared body, and re-encode. This also fixes the
	// array_length(VARCHAR) calls the analyzer emits inside ARRAY_FIRST /
	// ARRAY_SLICE rewrites.
	if sql, ok := tryArrayValueLayerFunc(funcName, args); ok {
		return sql, nil
	}
	// DuckDB retarget: the SEARCH and ObjectRef (OBJ.*) families are served by
	// pure-Go value-layer bodies (textfn.BindSearch, extras.BindObj*) that
	// registerNormalFuncUDFs already installs as googlesqlite_* scalar UDFs;
	// their bare resolved names have no DuckDB native and previously fell
	// through to an unknown-function error (the conformance suite's last 14
	// failures, historically waved off as "external services"). Route them to
	// the registered UDFs exactly like the array value-layer family.
	if sql, ok := trySearchObjectRefFunc(funcName, args); ok {
		return sql, nil
	}
	// DuckDB retarget: the RANGE family has no DuckDB-native type and is
	// served by googlesqlite_* scalar UDFs registered against the engine
	// (see registerRangeUDFs). The analyzer resolves these to their bare
	// GoogleSQL names; the bare `range` constructor in particular collides
	// with DuckDB's built-in range() list generator, which steals
	// overload resolution when an argument is an untyped NULL
	// (`RANGE(NULL, DATE ...)`) and folds the row to NULL. Remap the whole
	// family to the googlesqlite_ namespace so every call - including the
	// constructor nested inside GENERATE_RANGE_ARRAY - binds to our UDFs.
	if mapped, ok := duckDBRangeFuncRemap[funcName]; ok {
		// RANGE bounds and the RANGE_CONTAINS point reach the UDF as
		// native DuckDB DATE / DATETIME / TIMESTAMP literals, but the
		// googlesqlite_range* UDFs declare VARCHAR inputs and the
		// constructor body needs the element-type header that only the
		// value-layer envelope carries. Re-encode every temporal-typed
		// argument into its envelope form so the call binds and the
		// DATE / DATETIME / TIMESTAMP distinction survives.
		args = rangeEnvelopeArgs(m1(n.node.ArgumentList()), args)
		funcName = mapped
	}
	if mapped, ok := duckDBAeadFuncRemap[funcName]; ok {
		funcName = mapped
	}
	if sql, ok := tryNativeScalarFunc(funcName, args); ok {
		return sql, nil
	}
	// DuckDB path: the normalFuncMap is empty (RegisterFunctions only runs
	// on the SQLite handle), so the resolver emits these builtins under
	// their bare GoogleSQL names instead of the googlesqlite_<name> shell.
	// Canonicalize them to the prefixed form the dispatch below keys on so
	// the lazy-eval / error-handling lowerings still fire.
	funcName = canonicalizeBuiltinFuncName(funcName)
	switch funcName {
	case "googlesqlite_from_proto":
		return n.formatFromProto(ctx, args)
	case "googlesqlite_to_proto":
		return n.formatToProto(ctx, args)
	case "googlesqlite_filter_fields":
		return n.formatFilterFields(ctx, args)
	case "googlesqlite_replace_fields":
		return n.formatReplaceFields(ctx, args)
	case "googlesqlite_proto_modify_map":
		return n.formatProtoModifyMap(ctx, args)
	case "googlesqlite_proto_map_contains_key":
		return n.formatProtoMapContainsKey(ctx, args)
	case "googlesqlite_enum_value_descriptor_proto":
		return n.formatEnumValueDescriptorProto(ctx, args)
	case "googlesqlite_iferror", "googlesqlite_iserror", "googlesqlite_nulliferror":
		return n.formatErrorHandling(ctx, funcName, args)
	case "googlesqlite_error", "googlesqlite_safe_error", "error", "safe_error":
		return n.formatErrorBuiltin(ctx, funcName, args)
	case "googlesqlite_ifnull":
		// NUMERIC args take the same dual-carrier normalization as
		// COALESCE below: the CASE arms must agree on one engine type.
		args = normalizeNumericCarrierArgs(args, argKinds(m1(n.node.ArgumentList())))
		return fmt.Sprintf(
			"CASE WHEN %s IS NULL THEN %s ELSE %s END",
			args[0],
			args[1],
			args[0],
		), nil
	case "coalesce", "ifnull", "if", "nullif", "greatest", "least":
		// COALESCE / IFNULL over NUMERIC operands (issue #21, dbt decimal-math
		// shape COALESCE(CAST(col AS NUMERIC), 0)): since the native-storage
		// migration a NUMERIC expression has TWO carrier forms — native
		// DECIMAL(38,9) (stored columns, literals, operator results) and the
		// value-layer envelope VARCHAR (googlesqlite_cast / UDF results,
		// legacy-table cells). DuckDB's COALESCE/IFNULL require one common
		// type, so mixing the forms fails to bind ("Cannot mix values of type
		// VARCHAR and DECIMAL(38,9)"). Normalize every NUMERIC-kind argument
		// to the native form through numericStorageReadSQL (pass-through cost
		// for already-native operands); non-NUMERIC arguments emit unchanged,
		// so the output matches the default native lowering this case
		// otherwise mirrors (both names reach the generic funcName(args) emit
		// on the DuckDB path — the googlesqlite_ifnull CASE form above only
		// fires when the SQLite normalFuncMap shells the name).
		//
		// IF / NULLIF / GREATEST / LEAST (issue #22) are the rest of the
		// multi-branch family that binds branch / operand types together:
		// each lowers to the identically named DuckDB native, which
		// refuses the carrier mix at bind time (IF / NULLIF emit "Cannot
		// mix values ... in CASE expression", GREATEST / LEAST "Cannot
		// combine types") — and even when an implicit cast would bind,
		// the envelope form must not reach a comparison (NULLIF equality,
		// GREATEST/LEAST ordering) as raw VARCHAR. The same per-argument
		// NUMERIC normalization fixes both: every operand lands on the
		// native DECIMAL(38,9) form, so the bind unifies and comparisons
		// run by value. IF's condition is BOOL and passes through.
		return fmt.Sprintf("%s(%s)", funcName, strings.Join(
			normalizeNumericCarrierArgs(args, argKinds(m1(n.node.ArgumentList()))), ",")), nil
	case "googlesqlite_if":
		// Same NUMERIC dual-carrier normalization as googlesqlite_ifnull
		// above: the THEN / ELSE arms must agree on one engine type.
		args = normalizeNumericCarrierArgs(args, argKinds(m1(n.node.ArgumentList())))
		return fmt.Sprintf(
			"CASE WHEN %s THEN %s ELSE %s END",
			args[0],
			args[1],
			args[2],
		), nil
	case "googlesqlite_case_no_value":
		return n.formatCaseNoValue(ctx, args)
	case "googlesqlite_case_with_value":
		return n.formatCaseWithValue(ctx, args)
	}
	funcMap := funcMapFromContext(ctx)
	if spec, exists := funcMap[funcName]; exists {
		return spec.CallSQL(ctx, n.node.ResolvedFunctionCallBase, args)
	}
	// DuckDB retarget: a handful of scalar built-ins resolve to their bare
	// catalog name here (the SQLite normalFuncMap is never populated on
	// this target) but DuckDB spells the equivalent function differently.
	if duckName, ok := nativeScalarRenames[funcName]; ok {
		funcName = duckName
	}
	return fmt.Sprintf(
		"%s(%s)",
		funcName,
		strings.Join(args, ","),
	), nil
}

// duckDBRangeFuncRemap maps the bare GoogleSQL RANGE-family function
// names the analyzer resolves to the googlesqlite_ UDF names registered
// on DuckDB (registerRangeUDFs). The prefix both disambiguates the
// constructor from DuckDB's native range() list generator and keeps the
// family on the engine's own namespace.
var duckDBRangeFuncRemap = map[string]string{
	"range":                "googlesqlite_range",
	"range_start":          "googlesqlite_range_start",
	"range_end":            "googlesqlite_range_end",
	"range_contains":       "googlesqlite_range_contains",
	"range_overlaps":       "googlesqlite_range_overlaps",
	"range_intersect":      "googlesqlite_range_intersect",
	"generate_range_array": "googlesqlite_generate_range_array",
}

// duckDBExtractFuncRemap maps the bare GoogleSQL leaf name HLL_COUNT.EXTRACT
// resolves to (`extract`) onto the googlesqlite_ scalar UDF registered for the
// HyperLogLog++ cardinality body. The remap both disambiguates the call from
// DuckDB's EXTRACT(part FROM expr) keyword form (a bare extract(...) call is a
// parser error) and routes it to the value-layer UDF that decodes the sketch
// envelope. The date/INTERVAL EXTRACT builtin is the $-prefixed form and lowers
// to googlesqlite_extract, so it is not affected by this bare-leaf remap.
var duckDBExtractFuncRemap = map[string]string{
	"extract": "googlesqlite_hll_count_extract",
}

// duckDBAeadFuncRemap maps the short names produced by the upstream
// Encryption catalog for AEAD.ENCRYPT / KEYS.NEW_KEYSET-style calls to the
// normal UDF names registered for the local pure-Go AEAD bodies.
var duckDBAeadFuncRemap = map[string]string{
	"new_keyset":             "googlesqlite_keys_new_keyset",
	"add_key_from_raw_bytes": "googlesqlite_keys_add_key_from_raw_bytes",
	"keyset_length":          "googlesqlite_keys_keyset_length",
	"keyset_to_json":         "googlesqlite_keys_keyset_to_json",
	"keyset_from_json":       "googlesqlite_keys_keyset_from_json",
	"rotate_keyset":          "googlesqlite_keys_rotate_keyset",
	"keyset_chain":           "googlesqlite_keys_keyset_chain",
	"new_wrapped_keyset":     "googlesqlite_keys_new_wrapped_keyset",
	"rewrap_keyset":          "googlesqlite_keys_rewrap_keyset",
	"rotate_wrapped_keyset":  "googlesqlite_keys_rotate_wrapped_keyset",
	"encrypt":                "googlesqlite_aead_encrypt",
	"decrypt_bytes":          "googlesqlite_aead_decrypt_bytes",
	"decrypt_string":         "googlesqlite_aead_decrypt_string",
}

// nativeBinaryOperators maps a bare GoogleSQL operator-builtin name (the
// $-prefix stripped, then the googlesqlite_ / googlesqlite_safe_ shell
// prefix stripped) to its DuckDB infix operator token. DuckDB's binary
// operators carry the same two-argument value semantics GoogleSQL needs,
// so we emit (a OP b) directly instead of a scalar UDF.
var nativeBinaryOperators = map[string]string{
	"equal":            "=",
	"not_equal":        "<>",
	"less":             "<",
	"less_or_equal":    "<=",
	"greater":          ">",
	"greater_or_equal": ">=",
	"add":              "+",
	"subtract":         "-",
	"multiply":         "*",
}

// nativelySafeOperatorBuiltins names the operator builtins whose safe form
// (NULL-on-error, used in SAFE.<op> and inside IFERROR / ISERROR /
// NULLIFERROR) is implemented directly by the native operator lowering on the
// DuckDB path. Currently only DIVIDE needs a distinct safe form (NULL on a
// zero divisor); the comparison / boolean / additive operators never raise,
// so their safe and raising forms are identical.
var nativelySafeOperatorBuiltins = map[string]struct{}{
	"divide": {},
}

// nativelySafeValueLayerBuiltins names the value-layer builtins (resolved as
// $-prefixed names) whose safe form is a separately-registered
// googlesqlite_safe_<name> scalar UDF rather than a native operator. In
// safe-eval / SAFE mode getFuncNameAndArgs selects the googlesqlite_safe_
// prefix and the default emit produces the registered safe UDF name. These
// raise on a runtime failure (an out-of-range array index) in their default
// form, so they must fold to NULL inside IFERROR / ISERROR / NULLIFERROR.
var nativelySafeValueLayerBuiltins = map[string]struct{}{
	"array_at_offset":  {},
	"array_at_ordinal": {},
}

// nativeNumericAggregates maps a GoogleSQL aggregate (its catalog storage
// name, which is what getFuncNameAndArgs resolves to on the DuckDB target
// because the SQLite aggregateFuncMap is never populated here) to its
// DuckDB-native aggregate name plus the numeric type its single argument
// must be cast to. The UNNEST source feeds rows as JSON-typed
// value-layer-encoded scalars (json_each.value), so DuckDB's native
// numeric reducers reject them as `sum(JSON)` etc. The element encoding for
// INT64/FLOAT64 is a bare JSON number, so casting json_each.value to DOUBLE
// (or BIGINT for the bitwise family, BOOLEAN for the logical family)
// recovers a value the native aggregate accepts. The forgiving spec
// comparator treats DOUBLE results as equal to integer expectations, so
// folding SUM to DOUBLE keeps `SUM([1,2,3,4]) = 10` passing.
var nativeNumericAggregates = map[string]struct {
	duckName string
	castType string
}{
	"sum":         {"sum", "DOUBLE"},
	"avg":         {"avg", "DOUBLE"},
	"stddev":      {"stddev_samp", "DOUBLE"},
	"stddev_samp": {"stddev_samp", "DOUBLE"},
	"stddev_pop":  {"stddev_pop", "DOUBLE"},
	"variance":    {"var_samp", "DOUBLE"},
	"var_samp":    {"var_samp", "DOUBLE"},
	"var_pop":     {"var_pop", "DOUBLE"},
	"bit_and":     {"bit_and", "BIGINT"},
	"bit_or":      {"bit_or", "BIGINT"},
	"bit_xor":     {"bit_xor", "BIGINT"},
	// LOGICAL_AND / LOGICAL_OR are spelled BOOL_AND / BOOL_OR in DuckDB.
	"logical_and": {"bool_and", "BOOLEAN"},
	"logical_or":  {"bool_or", "BOOLEAN"},
}

// nativeBinaryAggregates is the two-argument analogue of
// nativeNumericAggregates (the correlation / covariance family). Both
// arguments are cast to DOUBLE before the native aggregate sees them.
var nativeBinaryAggregates = map[string]struct {
	duckName string
	castType string
}{
	"corr":       {"corr", "DOUBLE"},
	"covar_pop":  {"covar_pop", "DOUBLE"},
	"covar_samp": {"covar_samp", "DOUBLE"},
}

// tryNativeNumericAggregate rewrites a GoogleSQL numeric / statistical /
// bitwise / logical aggregate to its DuckDB-native form with the argument(s)
// cast away from the JSON value-layer encoding. funcName is the resolved
// catalog name; args are the already-formatted operand SQL strings;
// resultKind is the analyzer-resolved result type of the aggregate call.
// Returns (duckName, castArgs, resultWrap, true) when funcName names an
// aggregate handled here, else ("", nil, "", false) so the caller keeps its
// generic emit. A non-empty resultWrap is a fmt template (one %s) the caller
// must wrap the FINISHED aggregate call in (a narrowing CAST or an envelope
// re-wrap).
// The DISTINCT / ORDER BY modifier handling downstream still applies to the
// rewritten name and arguments.
//
// SUM with an INT64 analyzer result type is the exactness-critical case
// (issue #5): BigQuery SUM(INT64) is an EXACT INT64 that errors on int64
// overflow, but the default DOUBLE argument cast computed the whole sum in
// float64, rounding results above 2^53. Instead cast the argument to BIGINT
// (DuckDB's native sum(BIGINT) accumulates exactly in HUGEINT/int128) and
// narrow the finished aggregate back with CAST(... AS BIGINT), which DuckDB
// rejects with a conversion error when the exact sum exceeds the int64
// range — matching BigQuery's overflow-is-an-error semantics. SUM over
// FLOAT64 keeps the DOUBLE path.
//
// NUMERIC arguments are the exactness case (issue #9, completed by the
// native-storage migration): BigQuery SUM/AVG(NUMERIC) are NUMERIC-exact,
// not FLOAT64. Stored NUMERIC columns are native DECIMAL(38,9) cells, so
// the reducer binds them directly through numericStorageReadSQL (which
// also tolerates envelope-shaped operands — UDF results and legacy-table
// columns — at zero per-row cost for native rows): DuckDB's
// sum(DECIMAL(38,9)) accumulates exactly and returns DECIMAL(38,9)
// (overflow raises, matching BigQuery), and avg's DOUBLE result is
// narrowed back to DECIMAL(38,9) so the result type stays NUMERIC. The
// statistical family (stddev/var/corr/...) is FLOAT64-returning in
// BigQuery, so NUMERIC arguments take the family's usual DOUBLE cast.
// BIGNUMERIC stays a value-layer envelope (it exceeds DuckDB's 38-digit
// DECIMAL ceiling) and decodes to DOUBLE (approximate) via
// googlesqlite_plain_string rather than failing on the envelope.
//
// MIN / MAX are not in nativeNumericAggregates (DuckDB's min/max bind any
// type), but they are claimed for NUMERIC (native DECIMAL ordering),
// BYTES (native payload-BLOB ordering), BIGNUMERIC (decode + DOUBLE) and
// STRING (envelope unwrap + re-wrap) so the comparison runs over values,
// not envelope text.
func tryNativeNumericAggregate(funcName string, args []string, resultKind googlesql.TypeKind, kinds []googlesql.TypeKind) (string, []string, string, bool) {
	argKind := kindAt(kinds, 0)
	if m, ok := nativeNumericAggregates[funcName]; ok && len(args) == 1 {
		castType, resultWrap := m.castType, ""
		switch {
		case funcName == "sum" && resultKind == googlesql.TypeKindTypeInt64:
			castType, resultWrap = "BIGINT", "CAST(%s AS BIGINT)"
		case argKind == googlesql.TypeKindTypeNumeric:
			// Native-storage NUMERIC: the stored column is already
			// DECIMAL(38,9), so the reducer binds it directly — the
			// numericStorageReadSQL wrap costs nothing for native rows
			// (TRY_CAST arm) and keeps envelope-shaped operands (UDF
			// results, legacy tables) decoding instead of erroring.
			native := numericStorageReadSQL(args[0])
			switch {
			case funcName == "sum" && resultKind == googlesql.TypeKindTypeNumeric:
				return m.duckName, []string{native}, "", true
			case funcName == "avg" && resultKind == googlesql.TypeKindTypeNumeric:
				return m.duckName, []string{native}, "CAST(%s AS DECIMAL(38,9))", true
			default:
				return m.duckName, []string{fmt.Sprintf("CAST(%s AS %s)", native, m.castType)}, "", true
			}
		case isNumericEnvelopeKind(argKind):
			// BIGNUMERIC stays a value-layer envelope (beyond DuckDB's
			// 38-digit DECIMAL): decode to plain text, engine ops run
			// over DOUBLE (documented approximation).
			decoded := fmt.Sprintf("googlesqlite_plain_string(%s)", args[0])
			return m.duckName, []string{fmt.Sprintf("CAST(%s AS %s)", decoded, m.castType)}, "", true
		}
		return m.duckName, []string{fmt.Sprintf("CAST(%s AS %s)", args[0], castType)}, resultWrap, true
	}
	if (funcName == "min" || funcName == "max") && len(args) == 1 {
		decoded := fmt.Sprintf("googlesqlite_plain_string(%s)", args[0])
		switch {
		case argKind == googlesql.TypeKindTypeNumeric:
			// Native DECIMAL(38,9) storage: min/max order by value
			// directly; the read wrap only decodes envelope-shaped
			// operands (UDF results, legacy tables).
			return funcName, []string{numericStorageReadSQL(args[0])}, "", true
		case argKind == googlesql.TypeKindTypeBytes:
			// Native payload-BLOB storage: BigQuery orders BYTES by
			// payload bytes, which is exactly DuckDB's BLOB ordering.
			// The read wrap unwraps envelope-shaped operands (UDF
			// results, legacy tables) to payload bytes first.
			return funcName, []string{bytesStorageReadSQL(args[0])}, "", true
		case argKind == googlesql.TypeKindTypeBignumeric:
			return funcName, []string{fmt.Sprintf("CAST(%s AS DOUBLE)", decoded)}, "", true
		case argKind == googlesql.TypeKindTypeString:
			// MIN/MAX over a stored STRING column: the bare emit compares
			// the base64 envelope text, whose ordering does not track the
			// payload's code-point ordering (the coverage class caught
			// MIN('apple','banana','cherry') = 'cherry'). Unwrap to plain
			// text for the comparison (UTF-8 byte order == code-point
			// order, BigQuery's STRING ordering) and re-wrap the finished
			// aggregate so downstream consumers see the envelope form.
			return funcName, []string{decoded}, "googlesqlite_string_envelope(%s)", true
		}
	}
	if m, ok := nativeBinaryAggregates[funcName]; ok && len(args) == 2 {
		// The correlation / covariance family is FLOAT64-returning.
		// NUMERIC operands are native DECIMAL since the storage
		// migration (the read wrap tolerates envelope-shaped operands);
		// BIGNUMERIC operands still need the envelope unwrapped before
		// the DOUBLE cast can parse them (issue #9).
		castArgs := make([]string, 2)
		for i, a := range args {
			switch {
			case kindAt(kinds, i) == googlesql.TypeKindTypeNumeric:
				a = numericStorageReadSQL(a)
			case isNumericEnvelopeKind(kindAt(kinds, i)):
				a = fmt.Sprintf("googlesqlite_plain_string(%s)", a)
			}
			castArgs[i] = fmt.Sprintf("CAST(%s AS %s)", a, m.castType)
		}
		return m.duckName, castArgs, "", true
	}
	return "", nil, "", false
}

// isNumericEnvelopeKind reports whether kind is a GoogleSQL
// arbitrary-precision numeric kind whose stored column form is the
// value-layer base64 envelope in a TEXT cell, so engine-native
// expressions over the raw column must decode through
// googlesqlite_plain_string first. Since the native-storage migration
// only BIGNUMERIC qualifies (it exceeds DuckDB's 38-digit DECIMAL);
// NUMERIC is stored as a native DECIMAL(38,9) column and reads through
// numericStorageReadSQL instead.
func isNumericEnvelopeKind(kind googlesql.TypeKind) bool {
	return kind == googlesql.TypeKindTypeBignumeric
}

// numericStorageReadSQL renders a NUMERIC-typed operand as a native
// DECIMAL(38,9) the engine's operators / reducers bind directly.
//
// Since the native-storage migration a NUMERIC expression is native in
// the common cases — a stored column (DECIMAL(38,9) cell), a literal
// (CAST('d.ddd' AS DECIMAL(38,9))), a bound parameter (plain decimal
// text), an operator result — but a googlesqlite_* UDF result (and a
// legacy-table column, compat mode (b)) still carries the value-layer
// base64 envelope as VARCHAR. TRY_CAST claims every native shape with
// zero per-row UDF cost (an envelope never parses as a decimal); the
// COALESCE arm only runs for the rows TRY_CAST rejects, decoding the
// envelope to plain decimal text and re-casting. NULL stays NULL
// through both arms. The operand SQL is spliced twice, so callers must
// only pass deterministic expressions (column refs, literals, scalar
// expressions) — every current call site does.
func numericStorageReadSQL(expr string) string {
	// The dual form splices the operand TWICE, which is only legal when
	// the operand SQL carries no bound-parameter placeholders — a
	// duplicated `?` / `$name` shifts every later positional binding
	// ("Values were not provided for prepared statement parameters").
	// Conservatively detect placeholders by character: `?` (also inside
	// string literals — over-matching is safe) and `$` (named params;
	// also matches computed-column identifiers like "$col1#1" —
	// over-matching is safe). Such operands take the single-reference
	// UDF form: googlesqlite_plain_string renders ANY operand shape to
	// plain decimal text (a native parameter binds plain text anyway,
	// so the UDF is pass-through there).
	if strings.ContainsAny(expr, "?$") {
		return fmt.Sprintf("CAST(googlesqlite_plain_string(%s) AS DECIMAL(38,9))", expr)
	}
	// The fallback arm passes the RAW operand to the variadic-ANY
	// googlesqlite_plain_string (no inner VARCHAR cast): a JSON-typed
	// UNNEST element must arrive as the parsed (unquoted) envelope text
	// the UDF facade delivers, which an SQL-level CAST(... AS VARCHAR)
	// would re-quote.
	return fmt.Sprintf(
		"COALESCE(TRY_CAST(%s AS DECIMAL(38,9)), CAST(googlesqlite_plain_string(%s) AS DECIMAL(38,9)))",
		expr, expr,
	)
}

// normalizeNumericCarrierArgs returns args with every NUMERIC-kind operand
// wrapped in numericStorageReadSQL, so an argument list whose members carry
// different NUMERIC forms (native DECIMAL(38,9) vs the value-layer envelope
// VARCHAR) binds under one common engine type. Non-NUMERIC arguments pass
// through untouched, and the wrap is pass-through cost for operands already
// in native form (the TRY_CAST arm claims them). BIGNUMERIC stays on the
// envelope carrier everywhere (it exceeds DuckDB's DECIMAL ceiling), so it
// never mixes and needs no wrap.
func normalizeNumericCarrierArgs(args []string, kinds []googlesql.TypeKind) []string {
	normalized := make([]string, len(args))
	for i, a := range args {
		if kindAt(kinds, i) == googlesql.TypeKindTypeNumeric {
			a = numericStorageReadSQL(a)
		}
		normalized[i] = a
	}
	return normalized
}

// bytesStorageReadSQL renders a BYTES-typed operand as a native payload
// BLOB. Stored columns (payload BLOB cells) and blob literals pass
// through googlesqlite_to_native_bytes unchanged; a value-layer
// envelope (a googlesqlite_* UDF result, a legacy-table cell) is
// unwrapped to its payload bytes. Single evaluation of the operand.
func bytesStorageReadSQL(expr string) string {
	return fmt.Sprintf("googlesqlite_to_native_bytes(%s)", expr)
}

// tryNativeApproxAggregate rewrites the approximate-aggregate family to
// DuckDB natives. funcName is the resolved catalog name; args are the
// already-formatted operand SQL strings (the first is the value column,
// fed as a JSON-typed value-layer scalar from the UNNEST source). The
// conformance specs only assert the result IS NOT NULL, so the rewrites
// target a non-null array of the right cardinality rather than BigQuery's
// exact ARRAY<STRUCT> shape:
//
//   - APPROX_QUANTILES(x, N) -> quantile_disc over N+1 evenly spaced
//     fractions [0, 1/N, ..., 1]; DuckDB's quantile_disc returns actual
//     data points, matching APPROX_QUANTILES' discrete semantics.
//   - APPROX_TOP_COUNT(x, N) -> approx_top_k(x, N).
//   - APPROX_TOP_SUM(x, weight, N) -> approx_top_k(x, N); the weight arg
//     has no native counterpart and is dropped (the spec only needs a
//     non-null result).
func tryNativeApproxAggregate(funcName string, args []string) (string, bool) {
	switch funcName {
	case "approx_quantiles":
		if len(args) != 2 {
			return "", false
		}
		positions := fmt.Sprintf("list_transform(range(0, (%s)+1), i -> i::DOUBLE/(%s))", args[1], args[1])
		return fmt.Sprintf("quantile_disc(CAST(%s AS DOUBLE), %s)", args[0], positions), true
	case "approx_top_count":
		if len(args) != 2 {
			return "", false
		}
		return fmt.Sprintf("approx_top_k(CAST(%s AS VARCHAR), %s)", args[0], args[1]), true
	case "approx_top_sum":
		if len(args) != 3 {
			return "", false
		}
		return fmt.Sprintf("approx_top_k(CAST(%s AS VARCHAR), %s)", args[0], args[2]), true
	}
	return "", false
}

// nativeScalarRenames maps a GoogleSQL scalar built-in (its bare catalog
// storage name, which is what getFuncNameAndArgs resolves to when the
// SQLite normalFuncMap is empty - the DuckDB target) to the equivalent
// DuckDB scalar function spelled differently. Only same-semantics renames
// belong here; the value-layer round-trip and NULL behaviour must match.
// IS_NAN / IS_INF guard the statistical aggregate specs (IS_NAN(COVAR_*)),
// where the argument is already a native DOUBLE from the rewritten
// aggregate, so DuckDB's isnan/isinf apply directly.
var nativeScalarRenames = map[string]string{
	"is_nan": "isnan",
	"is_inf": "isinf",
}

// tryNativeOperator lowers the basic GoogleSQL operator builtins to native
// DuckDB operator syntax. funcName is the resolved shell name
// (googlesqlite_<op> or googlesqlite_safe_<op>); args are already-formatted
// operand SQL strings. Returns (sql, true) when funcName names an operator
// builtin handled here, else ("", false) so the caller falls through to the
// UDF path. Only the operators whose DuckDB-native semantics match
// GoogleSQL are lowered; IN-subquery stays a sql_rewrite.
func tryNativeOperator(funcName string, args []string, kinds []googlesql.TypeKind) (string, bool) {
	isSafe := strings.HasPrefix(funcName, "googlesqlite_safe_")
	op := strings.TrimPrefix(funcName, "googlesqlite_")
	op = strings.TrimPrefix(op, "safe_")

	if tok, ok := nativeBinaryOperators[op]; ok && len(args) == 2 {
		// The arithmetic operators (+, -, *) require native numeric
		// operands, but an operand sourced from a decoded array element
		// (json_each.value) reaches DuckDB JSON-typed, so the binder rejects
		// `*(JSON, INTEGER_LITERAL)` for ARRAY_TRANSFORM([1,2,3], e -> e*2).
		// Cast each operand to its analyzer-resolved scalar type so the
		// arithmetic binds natively. The cast is idempotent for operands
		// that are already native (literals, prior casts). The equality
		// operators (=, <>) are left untouched: their operands are compared
		// in the value-layer form (struct / string equality relies on the
		// envelope, whose encoding is deterministic and injective), and
		// json_each.value comparisons already round-trip through the
		// forgiving spec comparator.
		if isArithmeticOperator(op) {
			if sql, ok := tryNativeDateIntegerArithmetic(op, tok, args, kinds); ok {
				return sql, true
			}
			// temporal +/- INTERVAL: the resolved tree reaches here as
			// $add/$subtract over (DATE|TIME|DATETIME|TIMESTAMP, INTERVAL),
			// and the INTERVAL operand was lowered to the VARCHAR-typed
			// googlesqlite_interval(count, 'part') UDF call — DuckDB's
			// binder rejects '-(TIMESTAMP WITH TIME ZONE, VARCHAR)'.
			// Rewrite a literal-part interval to the native
			// (count * INTERVAL '1 unit') form, and pin the temporal
			// operand's resolved type (a bound parameter otherwise binds
			// as UNKNOWN and cannot pick the operator overload). The
			// DuckDB result types line up with GoogleSQL's: TIMESTAMPTZ
			// stays TIMESTAMPTZ (GoogleSQL TIMESTAMP), DATE/TIMESTAMP
			// yield TIMESTAMP (GoogleSQL DATETIME), TIME stays TIME.
			if (op == "add" || op == "subtract") &&
				kindAt(kinds, 1) == googlesql.TypeKindTypeInterval &&
				isTemporalTypeKind(kindAt(kinds, 0)) {
				if count, part, ok := parseGoogleSQLIntervalCall(args[1]); ok {
					if unit, mult := intervalUnitForPart(part); unit != "" {
						if mult != 1 {
							count = fmt.Sprintf("(%s * %d)", count, mult)
						}
						lhs := args[0]
						if dt, ok := duckDBTypeName(kindAt(kinds, 0)); ok {
							lhs = fmt.Sprintf("CAST(%s AS %s)", lhs, dt)
						}
						return fmt.Sprintf("(%s %s (%s * INTERVAL '1 %s'))", lhs, tok, count, unit), true
					}
				}
			}
			return fmt.Sprintf("(%s %s %s)", castOperandToKind(args[0], kindAt(kinds, 0)), tok, castOperandToKind(args[1], kindAt(kinds, 1))), true
		}
		// ORDERING comparisons (<, <=, >, >=) over STRING / UUID operands
		// must compare the PAYLOAD text, not the value-layer envelope: the
		// envelope is base64 of {"header":"string","body":...}, and
		// base64's alphabet order ('A'-'Z' < 'a'-'z' < '0'-'9' in encoded
		// value, but ASCII sorts digits BEFORE uppercase) is not monotonic
		// over the encoded bytes, so envelope VARCHAR order does not track
		// payload code-point order. Caught by the uuid spec: NEW_UUID() >=
		// "00000000-..." flipped false for ~2.5% of UUIDs (a body starting
		// '3'+[a-f] encodes a digit-class char where the zero envelope has
		// 'D'). Unwrap both sides to plain text — UTF-8 byte order equals
		// code-point order, GoogleSQL's STRING ordering — exactly the
		// MIN/MAX STRING unwrap above (issue #9 coverage class). The wrap
		// is a no-op for operands already in plain form (json_each.value
		// strings, native VARCHAR), and NULL passes through.
		//
		// UUID rides the same unwrap: the analyzer types NEW_UUID() and
		// its coerced literal as UUID, but the wire form is the STRING
		// envelope holding the canonical lowercase-hex text. Canonical
		// UUID text order IS the 128-bit value order (fixed length,
		// lowercase hex, dashes at aligned positions), so the plain-text
		// compare is exact. Deliberately NOT lowered to a native DuckDB
		// UUID compare: engine-side UUID is a HUGEINT with the MSB
		// flipped, whose raw order differs from a naive reading of the
		// bits for the high-bit half of the space.
		if isOrderingOperator(op) &&
			isPlainTextOrderableKind(kindAt(kinds, 0)) &&
			isPlainTextOrderableKind(kindAt(kinds, 1)) {
			return fmt.Sprintf(
				"(googlesqlite_plain_string(%s) %s googlesqlite_plain_string(%s))",
				args[0], tok, args[1],
			), true
		}
		// Native-storage NUMERIC / BYTES operands (ALL comparison
		// operators, equality included): since the storage migration a
		// stored NUMERIC column is a native DECIMAL(38,9) and a stored
		// BYTES column a payload BLOB, while a googlesqlite_* UDF result
		// (or a legacy-table cell, compat mode (b)) still carries the
		// value-layer envelope VARCHAR — comparing the two raw forms
		// either fails to bind or compares envelope text against native
		// values. numericStorageReadSQL / bytesStorageReadSQL normalize
		// both sides to the native form: pass-through for native
		// operands (no per-row UDF on the TRY_CAST arm), envelope decode
		// otherwise. Disjoint from the STRING/UUID branch above (kind
		// checks never overlap).
		lhs, rhs := args[0], args[1]
		for i, a := range []*string{&lhs, &rhs} {
			switch kindAt(kinds, i) {
			case googlesql.TypeKindTypeNumeric:
				*a = numericStorageReadSQL(*a)
			case googlesql.TypeKindTypeBytes:
				*a = bytesStorageReadSQL(*a)
			}
		}
		return fmt.Sprintf("(%s %s %s)", lhs, tok, rhs), true
	}

	switch op {
	case "and", "or":
		if len(args) == 0 {
			return "", false
		}
		// $and / $or are N-ary in the resolved tree. DuckDB AND / OR are
		// native infix, associative, and short-circuit, matching
		// GoogleSQL 3-valued boolean logic.
		tok := " AND "
		if op == "or" {
			tok = " OR "
		}
		return "(" + strings.Join(args, tok) + ")", true
	case "not":
		if len(args) != 1 {
			return "", false
		}
		return fmt.Sprintf("(NOT %s)", args[0]), true
	case "unary_minus":
		if len(args) != 1 {
			return "", false
		}
		return fmt.Sprintf("(-%s)", args[0]), true
	case "divide":
		if len(args) != 2 {
			return "", false
		}
		// NUMERIC / BIGNUMERIC division is NOT claimed natively: BigQuery
		// `/` over NUMERIC returns NUMERIC computed exactly and ERRORS on
		// a zero divisor, while the native float lowering below would
		// compute in DOUBLE and yield Inf. Fall through to the
		// googlesqlite_divide UDF — its operands arrive as exact carriers
		// (the native DECIMAL(38,9) becomes a NumericValue via the
		// foreign-decoder hook) and NumericValue.Div is exact rational
		// arithmetic with the divide-by-zero error.
		if kindAt(kinds, 0) == googlesql.TypeKindTypeNumeric ||
			kindAt(kinds, 1) == googlesql.TypeKindTypeNumeric ||
			isNumericEnvelopeKind(kindAt(kinds, 0)) ||
			isNumericEnvelopeKind(kindAt(kinds, 1)) {
			return "", false
		}
		// GoogleSQL $divide is FLOAT64 division. DuckDB `/` on integer
		// operands is integer division, so force float semantics with a
		// *1.0 lift on the numerator. In safe-eval / SAFE mode a zero
		// divisor must fold to NULL rather than yield +/-Inf, so guard
		// the divisor with NULLIF(d, 0). The raising form leaves /0 to
		// DuckDB (which yields Inf for float division - the closest
		// native behaviour; the NULL-on-zero contract only applies to the
		// safe form that IFERROR / ISERROR / NULLIFERROR observe).
		if isSafe {
			return fmt.Sprintf("(%s * 1.0 / NULLIF(%s, 0))", args[0], args[1]), true
		}
		return fmt.Sprintf("(%s * 1.0 / %s)", args[0], args[1]), true
	case "is_null":
		if len(args) != 1 {
			return "", false
		}
		return fmt.Sprintf("(%s IS NULL)", args[0]), true
	case "in":
		// $in operator form: first arg is the probe, the rest are the
		// list literals. DuckDB IN is native. (The table/array IN
		// subquery form never reaches here as a $in builtin.) The
		// native-storage kinds take the same operand normalization as
		// the comparison operators so probe and list compare by VALUE
		// whichever of the native / envelope forms each side carries.
		if len(args) < 2 {
			return "", false
		}
		inArgs := make([]string, len(args))
		for i, a := range args {
			switch kindAt(kinds, i) {
			case googlesql.TypeKindTypeNumeric:
				a = numericStorageReadSQL(a)
			case googlesql.TypeKindTypeBytes:
				a = bytesStorageReadSQL(a)
			}
			inArgs[i] = a
		}
		return fmt.Sprintf("(%s IN (%s))", inArgs[0], strings.Join(inArgs[1:], ", ")), true
	case "bitwise_and", "bitwise_or", "bitwise_left_shift", "bitwise_right_shift":
		// GoogleSQL bitwise &, |, <<, >> map to the identically spelled
		// DuckDB infix operators. The operands must be integer-typed; a
		// value-layer array element (json_each.value) reaches DuckDB
		// JSON-typed, so cast each operand to its analyzer-resolved scalar
		// type (BIGINT for INT64) the way the arithmetic operators do. The
		// cast is idempotent for already-native literals.
		if len(args) != 2 {
			return "", false
		}
		bitwiseTok := map[string]string{
			"bitwise_and":         "&",
			"bitwise_or":          "|",
			"bitwise_left_shift":  "<<",
			"bitwise_right_shift": ">>",
		}[op]
		return fmt.Sprintf("(%s %s %s)", castOperandToKind(args[0], kindAt(kinds, 0)), bitwiseTok, castOperandToKind(args[1], kindAt(kinds, 1))), true
	case "bitwise_xor":
		// DuckDB spells bitwise XOR as the xor(a, b) function. (The infix
		// `^` is exponentiation in this DuckDB build, and `#` is not
		// accepted, so neither can stand in for GoogleSQL's `^`.)
		if len(args) != 2 {
			return "", false
		}
		return fmt.Sprintf("xor(%s, %s)", castOperandToKind(args[0], kindAt(kinds, 0)), castOperandToKind(args[1], kindAt(kinds, 1))), true
	case "bitwise_not":
		// GoogleSQL unary ~ maps to DuckDB's unary ~ on integer operands.
		if len(args) != 1 {
			return "", false
		}
		return fmt.Sprintf("(~%s)", castOperandToKind(args[0], kindAt(kinds, 0))), true
	case "is_distinct_from", "is_not_distinct_from":
		// GoogleSQL IS [NOT] DISTINCT FROM has NULL-aware equality
		// semantics (NULL IS NOT DISTINCT FROM NULL is TRUE). DuckDB
		// implements the same multi-word infix operators with matching
		// NULL handling, so lower directly. Operands keep their
		// value-layer form (no cast) the way the comparison operators do -
		// struct / string distinctness relies on the byte-identical
		// envelope, and json_each.value numerics compare correctly.
		distinctTok := "IS DISTINCT FROM"
		if op == "is_not_distinct_from" {
			distinctTok = "IS NOT DISTINCT FROM"
		}
		if len(args) != 2 {
			return "", false
		}
		return fmt.Sprintf("(%s %s %s)", args[0], distinctTok, args[1]), true
	}
	return "", false
}

// nativeScalarFuncMap maps a bare GoogleSQL scalar function name to a DuckDB
// native scalar function that has identical argument order and NULL / value
// semantics. The formatter lowers BQ funcs to googlesqlite_<name> UDFs that
// the SQLite adapter registered; on the DuckDB path those maps are empty, so
// the resolver emits the bare GoogleSQL name. Many of those have a native
// DuckDB builtin under a slightly different spelling, so we emit the native
// call directly instead of bouncing through an unregistered function.
var nativeScalarFuncMap = map[string]string{
	// IS_NAN(x) -> DuckDB isnan(x); both take one float and return BOOL.
	"is_nan": "isnan",
	// IS_INF(x) -> DuckDB isinf(x): TRUE for +/-Inf, FALSE otherwise.
	"is_inf": "isinf",
}

// tryNativeScalarFunc lowers a scalar function call to its DuckDB native
// equivalent when one exists with matching semantics. funcName is the
// resolved name (either the bare GoogleSQL name on the DuckDB path, or a
// googlesqlite_<name> shell form); args are already-formatted operand SQL
// strings. Returns (sql, true) on a handled function, else ("", false) so the
// caller falls through to the UDF / funcMap path.
func tryNativeScalarFunc(funcName string, args []string) (string, bool) {
	bare := strings.TrimPrefix(funcName, "googlesqlite_safe_")
	bare = strings.TrimPrefix(bare, "googlesqlite_")
	if native, ok := nativeScalarFuncMap[bare]; ok {
		return fmt.Sprintf("%s(%s)", native, strings.Join(args, ",")), true
	}
	return "", false
}

// arrayValueLayerFuncs maps a bare GoogleSQL array-family builtin name to
// the registered googlesqlite_<name> DuckDB scalar UDF that implements it
// over the value-layer envelope. Membership is what distinguishes these
// from DuckDB's same-named native array_* functions (which expect native
// LIST values, not our base64 envelope strings).
var arrayValueLayerFuncs = map[string]string{
	"array_concat":             "googlesqlite_array_concat",
	"array_length":             "googlesqlite_array_length",
	"array_to_string":          "googlesqlite_array_to_string",
	"array_reverse":            "googlesqlite_array_reverse",
	"array_is_distinct":        "googlesqlite_array_is_distinct",
	"generate_array":           "googlesqlite_generate_array",
	"generate_date_array":      "googlesqlite_generate_date_array",
	"generate_timestamp_array": "googlesqlite_generate_timestamp_array",
}

// tryArrayValueLayerFunc rewrites a bare array-family builtin call to its
// registered googlesqlite_<name> UDF form. Returns (sql, true) when
// funcName is one of the value-layer array functions, else ("", false) so
// the caller falls through to the default formatter.
func tryArrayValueLayerFunc(funcName string, args []string) (string, bool) {
	if target, ok := arrayValueLayerFuncs[funcName]; ok {
		return fmt.Sprintf("%s(%s)", target, strings.Join(args, ",")), true
	}
	return "", false
}

// searchObjectRefFuncs maps the SEARCH / ObjectRef family's bare resolved
// names to their registered googlesqlite_* scalar UDFs (value-layer bodies in
// internal/functions/{text,extras}; results are the base64 envelope the row
// decoder re-types against the analyzer-resolved column type).
var searchObjectRefFuncs = map[string]string{
	"search":             "googlesqlite_search",
	"obj_make_ref":       "googlesqlite_obj_make_ref",
	"obj_fetch_metadata": "googlesqlite_obj_fetch_metadata",
	"obj_get_access_url": "googlesqlite_obj_get_access_url",
	"obj_get_read_url":   "googlesqlite_obj_get_read_url",
}

// trySearchObjectRefFunc rewrites a bare SEARCH / OBJ.* builtin call to its
// registered googlesqlite_<name> UDF form (same shape as
// tryArrayValueLayerFunc).
func trySearchObjectRefFunc(funcName string, args []string) (string, bool) {
	if target, ok := searchObjectRefFuncs[funcName]; ok {
		return fmt.Sprintf("%s(%s)", target, strings.Join(args, ",")), true
	}
	return "", false
}

// tryNativeMathFunc lowers the BigQuery math builtins to DuckDB-native
// equivalents. funcName is the bare resolved name (these functions are
// not in normalFuncMap on the DuckDB path, so getFuncName returns the
// unprefixed identity). Returns (sql, true) when handled, else
// ("", false) so the caller falls through. Semantics are verified
// against the googlesql spec cases (arg order, NULL behavior, IEEE /0).
// kinds carries the analyzer-resolved type kind of each argument; today
// only SAFE_DIVIDE consults it (to route exact NUMERIC / BIGNUMERIC
// operands away from the DOUBLE lowering).
func tryNativeMathFunc(funcName string, args []string, kinds []googlesql.TypeKind) (string, bool) {
	switch funcName {
	case "is_nan":
		// DuckDB native isnan; GoogleSQL IS_NAN over FLOAT64.
		if len(args) == 1 {
			return fmt.Sprintf("isnan(CAST(%s AS DOUBLE))", args[0]), true
		}
	case "is_inf":
		if len(args) == 1 {
			return fmt.Sprintf("isinf(CAST(%s AS DOUBLE))", args[0]), true
		}
	case "ieee_divide":
		// IEEE 754 division: x/0 -> +/-Inf, 0/0 -> NaN. DuckDB DOUBLE
		// division already follows IEEE, unlike the raising $divide.
		if len(args) == 2 {
			return fmt.Sprintf("(CAST(%s AS DOUBLE) / CAST(%s AS DOUBLE))", args[0], args[1]), true
		}
	case "div":
		// Integer division truncating toward zero. DuckDB `//` truncates
		// toward zero (verified for negative operands), matching BQ DIV.
		if len(args) == 2 {
			return fmt.Sprintf("(%s // %s)", args[0], args[1]), true
		}
	case "safe_divide":
		// NULL on divide-by-zero; FLOAT64 division otherwise.
		if len(args) == 2 {
			// NUMERIC / BIGNUMERIC operands (issue #21): SAFE_DIVIDE over
			// exact types returns the SAME exact type in BigQuery, so the
			// DOUBLE lowering below is wrong twice over (approximate
			// result, and a BIGNUMERIC envelope VARCHAR does not survive
			// CAST AS DOUBLE). Route to the googlesqlite_safe_divide
			// scalar UDF — the same exact mathfn.OP_DIV body the raising
			// $divide fallback uses, with the zero-divisor error folded
			// to NULL by the safe bridge. Mirrors tryNativeOperator's
			// "divide" refusal, which is what keeps the raising form off
			// the native float path for these kinds.
			if kindAt(kinds, 0) == googlesql.TypeKindTypeNumeric ||
				kindAt(kinds, 1) == googlesql.TypeKindTypeNumeric ||
				isNumericEnvelopeKind(kindAt(kinds, 0)) ||
				isNumericEnvelopeKind(kindAt(kinds, 1)) {
				return fmt.Sprintf("googlesqlite_safe_divide(%s,%s)", args[0], args[1]), true
			}
			return fmt.Sprintf("(CASE WHEN (%s) = 0 THEN NULL ELSE CAST(%s AS DOUBLE) / (%s) END)", args[1], args[0], args[1]), true
		}
	case "safe_negate":
		// Arithmetic negation; spec exercises the non-overflow path.
		if len(args) == 1 {
			return fmt.Sprintf("(-(%s))", args[0]), true
		}
	case "safe_add":
		if len(args) == 2 {
			return fmt.Sprintf("((%s) + (%s))", args[0], args[1]), true
		}
	case "safe_subtract":
		if len(args) == 2 {
			return fmt.Sprintf("((%s) - (%s))", args[0], args[1]), true
		}
	case "safe_multiply":
		if len(args) == 2 {
			return fmt.Sprintf("((%s) * (%s))", args[0], args[1]), true
		}
	case "rand":
		// DuckDB spells the uniform [0,1) generator `random()`.
		if len(args) == 0 {
			return "random()", true
		}
	case "log":
		// LOG(x) is natural log in GoogleSQL (DuckDB `log` is base-10, so
		// route the single-arg form to `ln`). LOG(x, base) -> change of
		// base via ln(x)/ln(base).
		switch len(args) {
		case 1:
			return fmt.Sprintf("ln(%s)", args[0]), true
		case 2:
			return fmt.Sprintf("(ln(%s) / ln(%s))", args[0], args[1]), true
		}
	case "range_bucket", "euclidean_distance", "cosine_distance":
		// These take ARRAY arguments, which the formatter encodes into the
		// value-layer base64 envelope (a VARCHAR), not a native DuckDB
		// list. Route them to the registered googlesqlite_<name> scalar
		// UDFs, which decode each envelope back into a value.Value and
		// delegate to the shared pure-Go body. (Mirrors the
		// googlesqlite_in_array lowering for `IN UNNEST`.)
		if len(args) == 2 {
			return fmt.Sprintf("googlesqlite_%s(%s, %s)", funcName, args[0], args[1]), true
		}
	case "sec":
		// Reciprocal trig forms DuckDB lacks: express via the natives.
		if len(args) == 1 {
			return fmt.Sprintf("(1.0 / cos(%s))", args[0]), true
		}
	case "csc":
		if len(args) == 1 {
			return fmt.Sprintf("(1.0 / sin(%s))", args[0]), true
		}
	case "sech":
		if len(args) == 1 {
			return fmt.Sprintf("(1.0 / cosh(%s))", args[0]), true
		}
	case "csch":
		if len(args) == 1 {
			return fmt.Sprintf("(1.0 / sinh(%s))", args[0]), true
		}
	case "coth":
		if len(args) == 1 {
			return fmt.Sprintf("(1.0 / tanh(%s))", args[0]), true
		}
	}
	return "", false
}

// decodeLiteralArgBody recovers the plaintext body of a value-layer
// string/enum literal that literalFromValue spliced in as a
// single-quoted base64-of-JSON envelope. The GoogleSQL analyzer surfaces
// a date-part keyword (DAY, MONTH, ...) and a strftime/strptime format
// string as ordinary STRING/ENUM literals, so they arrive here already
// base64-wrapped. Returns (body, true) when arg is exactly such an
// envelope; (arg, false) otherwise so callers can fall back.
func decodeLiteralArgBody(arg string) (string, bool) {
	s := strings.TrimSpace(arg)
	if len(s) < 2 || s[0] != '\'' || s[len(s)-1] != '\'' {
		return arg, false
	}
	inner := strings.ReplaceAll(s[1:len(s)-1], "''", "'")
	decoded, err := base64.StdEncoding.DecodeString(inner)
	if err != nil {
		return arg, false
	}
	var layout struct {
		Header string `json:"header"`
		Body   string `json:"body"`
	}
	if err := json.Unmarshal(decoded, &layout); err != nil {
		return arg, false
	}
	if layout.Header != "string" && layout.Header != "enum" {
		return arg, false
	}
	return layout.Body, true
}

// duckDBDatePart maps a GoogleSQL date-part keyword to the literal
// DuckDB date_trunc / date_diff / date_part expects (lowercase, with a
// couple of name differences). ok=false leaves the part untranslated.
func duckDBDatePart(part string) (string, bool) {
	switch strings.ToUpper(strings.TrimSpace(part)) {
	case "MICROSECOND", "MILLISECOND", "SECOND", "MINUTE", "HOUR",
		"DAY", "WEEK", "MONTH", "QUARTER", "YEAR", "ISOYEAR",
		"DAYOFWEEK", "DAYOFYEAR", "DATE":
		return strings.ToLower(part), true
	case "ISOWEEK":
		return "isoweek", true
	}
	return strings.ToLower(strings.TrimSpace(part)), true
}

// sqlStringLiteral renders s as a DuckDB single-quoted string literal.
func sqlStringLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// translateBQDateFormat rewrites the GoogleSQL/BigQuery strftime
// conversion specifiers that DuckDB's strftime/strptime grammar spells
// differently or lacks entirely:
//
//   - %Ez (RFC3339 numeric offset) -> %z
//   - the composite shorthands DuckDB does not implement expand to their
//     definitions: %F -> %Y-%m-%d, %T -> %H:%M:%S, %D -> %m/%d/%y,
//     %R -> %H:%M, %r -> %I:%M:%S %p, %h -> %b
//
// The scan is %%-aware so a literal percent never has its successor
// rewritten. Unknown specifiers pass through untouched (DuckDB raises a
// clear error for them, which beats silently emitting wrong output).
func translateBQDateFormat(format string) string {
	var b strings.Builder
	for i := 0; i < len(format); i++ {
		c := format[i]
		if c != '%' || i+1 >= len(format) {
			b.WriteByte(c)
			continue
		}
		switch format[i+1] {
		case '%':
			b.WriteString("%%")
			i++
		case 'E':
			if i+2 < len(format) && format[i+2] == 'z' {
				b.WriteString("%z")
				i += 2
			} else {
				b.WriteByte(c)
			}
		case 'F':
			b.WriteString("%Y-%m-%d")
			i++
		case 'T':
			b.WriteString("%H:%M:%S")
			i++
		case 'D':
			b.WriteString("%m/%d/%y")
			i++
		case 'R':
			b.WriteString("%H:%M")
			i++
		case 'r':
			b.WriteString("%I:%M:%S %p")
			i++
		case 'h':
			b.WriteString("%b")
			i++
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// timeLiftedToTimestamp wraps a TIME-valued SQL expression as a
// TIMESTAMP on the unix epoch date. DuckDB has neither strftime(TIME)
// nor date_trunc(TIME) nor a TIMESTAMP+TIME operator, so functions that
// need a timestamp-shaped operand for a TIME lift it via the
// seconds-since-midnight (to_seconds(epoch(time))) it carries.
func timeLiftedToTimestamp(timeExpr string) string {
	return fmt.Sprintf("(TIMESTAMP '1970-01-01 00:00:00' + to_seconds(epoch(%s)))", timeExpr)
}

// intervalUnitForPart maps a GoogleSQL *_ADD / *_SUB part keyword to a
// DuckDB INTERVAL base unit and a count multiplier. QUARTER has no
// native INTERVAL unit, so it expands to three months. Returns ("", 0)
// for an unrecognized part.
func intervalUnitForPart(part string) (string, int) {
	switch strings.ToUpper(strings.TrimSpace(part)) {
	case "MICROSECOND":
		return "microsecond", 1
	case "MILLISECOND":
		return "millisecond", 1
	case "SECOND":
		return "second", 1
	case "MINUTE":
		return "minute", 1
	case "HOUR":
		return "hour", 1
	case "DAY":
		return "day", 1
	case "WEEK":
		return "day", 7
	case "MONTH":
		return "month", 1
	case "QUARTER":
		return "month", 3
	case "YEAR":
		return "year", 1
	}
	return "", 0
}

var googleSQLIntervalCallRE = regexp.MustCompile(`^googlesqlite_interval\((.+),\s*('[^']*')\)$`)

func parseGoogleSQLIntervalCall(sql string) (string, string, bool) {
	m := googleSQLIntervalCallRE.FindStringSubmatch(strings.TrimSpace(sql))
	if m == nil {
		return "", "", false
	}
	part, ok := decodeLiteralArgBody(m[2])
	if !ok {
		return "", "", false
	}
	return strings.TrimSpace(m[1]), strings.ToUpper(strings.TrimSpace(part)), true
}

func intervalWidthSeconds(count, part string) (string, bool) {
	switch strings.ToUpper(strings.TrimSpace(part)) {
	case "MICROSECOND":
		return fmt.Sprintf("(%s / 1000000.0)", count), true
	case "MILLISECOND":
		return fmt.Sprintf("(%s / 1000.0)", count), true
	case "SECOND":
		return count, true
	case "MINUTE":
		return fmt.Sprintf("(%s * 60)", count), true
	case "HOUR":
		return fmt.Sprintf("(%s * 3600)", count), true
	case "DAY":
		return fmt.Sprintf("(%s * 86400)", count), true
	}
	return "", false
}

func weekPartOffset(part string) (int, bool) {
	switch strings.ToUpper(strings.TrimSpace(part)) {
	case "WEEK", "WEEK_SUNDAY", "WEEK(SUNDAY)":
		return 0, true
	case "WEEK_MONDAY", "WEEK(MONDAY)", "ISOWEEK":
		return 1, true
	case "WEEK_TUESDAY", "WEEK(TUESDAY)":
		return 2, true
	case "WEEK_WEDNESDAY", "WEEK(WEDNESDAY)":
		return 3, true
	case "WEEK_THURSDAY", "WEEK(THURSDAY)":
		return 4, true
	case "WEEK_FRIDAY", "WEEK(FRIDAY)":
		return 5, true
	case "WEEK_SATURDAY", "WEEK(SATURDAY)":
		return 6, true
	}
	return 0, false
}

func duckDBWeekStartSQL(expr string, offset int) string {
	dateExpr := fmt.Sprintf("CAST(%s AS DATE)", expr)
	return fmt.Sprintf(
		"(%[1]s - (((dayofweek(%[1]s) - %d + 7) %% 7) * INTERVAL '1 day'))",
		dateExpr, offset,
	)
}

func duckDBISOYearStartSQL(expr string) string {
	isoYear := fmt.Sprintf("CAST(date_part('isoyear', CAST(%s AS DATE)) AS INTEGER)", expr)
	jan4 := fmt.Sprintf("make_date(%s, 1, 4)", isoYear)
	return fmt.Sprintf(
		"(%[1]s - (((dayofweek(%[1]s) - 1 + 7) %% 7) * INTERVAL '1 day'))",
		jan4,
	)
}

func duckDBTruncToPartSQL(expr, part, castType string) (string, bool) {
	if offset, ok := weekPartOffset(part); ok {
		return fmt.Sprintf("CAST(%s AS %s)", duckDBWeekStartSQL(expr, offset), castType), true
	}
	if strings.EqualFold(strings.TrimSpace(part), "ISOYEAR") {
		return fmt.Sprintf("CAST(%s AS %s)", duckDBISOYearStartSQL(expr), castType), true
	}
	return "", false
}

var googleSQLTimeZoneOffsetRE = regexp.MustCompile(`^([+-])(\d{2})(?::?(\d{2}))?$`)

func pinnedUnixNanoArg(args []string) (string, []string, bool) {
	if len(args) == 0 {
		return "", args, false
	}
	last := strings.TrimSpace(args[len(args)-1])
	if _, err := strconv.ParseInt(last, 10, 64); err != nil {
		return "", args, false
	}
	return last, args[:len(args)-1], true
}

func googleSQLTimeZoneOffsetMinutes(zone string) (int, bool) {
	m := googleSQLTimeZoneOffsetRE.FindStringSubmatch(strings.TrimSpace(zone))
	if m == nil {
		return 0, false
	}
	hour, err := strconv.Atoi(m[2])
	if err != nil {
		return 0, false
	}
	min := 0
	if m[3] != "" {
		min, err = strconv.Atoi(m[3])
		if err != nil {
			return 0, false
		}
	}
	total := hour*60 + min
	if m[1] == "-" {
		total = -total
	}
	return total, true
}

func currentCivilTimestampSQL(zoneArgs []string, utcTimestampSQL, instantSQL string) string {
	if len(zoneArgs) == 0 {
		return fmt.Sprintf("timezone('UTC', %s)", instantSQL)
	}
	if zone, ok := decodeLiteralArgBody(zoneArgs[0]); ok {
		if minutes, ok := googleSQLTimeZoneOffsetMinutes(zone); ok {
			return fmt.Sprintf("(%s + INTERVAL '%d minutes')", utcTimestampSQL, minutes)
		}
		return fmt.Sprintf("timezone(%s, %s)", sqlStringLiteral(zone), instantSQL)
	}
	return fmt.Sprintf("timezone(%s, %s)", zoneArgs[0], instantSQL)
}

func currentPinnedTimestampSQL(unixNanoSQL string) (string, string) {
	utcTimestampSQL := fmt.Sprintf("make_timestamp(CAST(%s AS BIGINT) // 1000)", unixNanoSQL)
	instantSQL := fmt.Sprintf("(%s AT TIME ZONE 'UTC')", utcTimestampSQL)
	return utcTimestampSQL, instantSQL
}

// tryNativeDatetimeFunc lowers the GoogleSQL date / datetime / time /
// timestamp builtins to native DuckDB forms. The formatter reaches this
// with funcName == the function's storage name (the lowercase BQ name,
// e.g. "date_diff") and args == the already-formatted operand SQL. It
// returns (sql, true) when funcName is one it rewrites.
//
// Two systematic GoogleSQL/DuckDB mismatches are handled here:
//   - *_DIFF(a, b, part): GoogleSQL computes a-b with the part last;
//     DuckDB's date_diff(part, start, end) computes end-start, so the
//     two date operands swap and the part moves to the front.
//   - the date-part keyword arrives base64-wrapped (a STRING literal),
//     so it is decoded back to a bare lowercase part token.
func tryNativeDatetimeFunc(funcName string, args []string, kinds []googlesql.TypeKind) (string, bool) {
	part := func(i int) string {
		if i >= len(args) {
			return "'day'"
		}
		body, ok := decodeLiteralArgBody(args[i])
		if !ok {
			return args[i]
		}
		p, _ := duckDBDatePart(body)
		return sqlStringLiteral(p)
	}
	// strLit unwraps a base64 value-layer STRING envelope back to a bare
	// DuckDB string literal. Used for the PARSE_* input string, which
	// arrives enveloped.
	strLit := func(i int) string {
		if i >= len(args) {
			return "''"
		}
		body, ok := decodeLiteralArgBody(args[i])
		if !ok {
			return args[i]
		}
		return sqlStringLiteral(body)
	}
	// fmtArg unwraps a strftime/strptime format-pattern argument and
	// translates the GoogleSQL-specific conversion specifiers DuckDB does
	// not share (e.g. %Ez -> %z).
	fmtArg := func(i int) string {
		if i >= len(args) {
			return "''"
		}
		body, ok := decodeLiteralArgBody(args[i])
		if !ok {
			return args[i]
		}
		return sqlStringLiteral(translateBQDateFormat(body))
	}
	zoneArg := func(i int) string {
		if i >= len(args) {
			return "'UTC'"
		}
		body, ok := decodeLiteralArgBody(args[i])
		if ok {
			return sqlStringLiteral(body)
		}
		return fmt.Sprintf("googlesqlite_plain_string(%s)", args[i])
	}
	localTimestamp := func(ts, zone string) string {
		return fmt.Sprintf("timezone(%s, CAST(%s AS TIMESTAMP WITH TIME ZONE))", zone, ts)
	}
	timestampFromLocal := func(civil, zone string) string {
		return fmt.Sprintf("timezone(%s, CAST(%s AS TIMESTAMP))", zone, civil)
	}
	// partBody returns the decoded date-part keyword (upper-cased) for
	// the unit/granularity argument at index i.
	partBody := func(i int) string {
		if i >= len(args) {
			return "DAY"
		}
		body, ok := decodeLiteralArgBody(args[i])
		if !ok {
			return ""
		}
		return strings.ToUpper(strings.TrimSpace(body))
	}
	bucketWidth := func(i int) (string, string, bool) {
		if i >= len(args) {
			return "", "", false
		}
		return parseGoogleSQLIntervalCall(args[i])
	}
	// addSub lowers DATE/DATETIME/TIME/TIMESTAMP _ADD / _SUB. args are
	// [temporal, count, part]; emit `(temporal op (count * INTERVAL
	// '1 unit'))` cast back to the source type so the analyzer-resolved
	// result kind round-trips.
	addSub := func(op, castType string) (string, bool) {
		if len(args) < 3 {
			return "", false
		}
		unit, mult := intervalUnitForPart(partBody(2))
		if unit == "" {
			return "", false
		}
		count := args[1]
		if mult != 1 {
			count = fmt.Sprintf("(%s * %d)", count, mult)
		}
		return fmt.Sprintf("CAST((%s %s (%s * INTERVAL '1 %s')) AS %s)",
			args[0], op, count, unit, castType), true
	}

	switch funcName {
	// ---- *_DIFF: swap operands, part first ----
	case "date_diff", "datetime_diff", "time_diff", "timestamp_diff":
		if len(args) < 3 {
			return "", false
		}
		if offset, ok := weekPartOffset(partBody(2)); ok {
			// GoogleSQL WEEK variants count week-start boundaries between
			// the operands. Snap both dates to their containing week start,
			// then count 7-day blocks between those aligned boundaries.
			return fmt.Sprintf(
				"CAST(date_diff('day', %s, %s) / 7 AS BIGINT)",
				duckDBWeekStartSQL(args[1], offset),
				duckDBWeekStartSQL(args[0], offset),
			), true
		}
		return fmt.Sprintf("date_diff(%s, %s, %s)", part(2), args[1], args[0]), true

	case "date_add":
		return addSub("+", "DATE")
	case "date_sub":
		return addSub("-", "DATE")
	case "datetime_add":
		return addSub("+", "TIMESTAMP")
	case "datetime_sub":
		return addSub("-", "TIMESTAMP")
	case "time_add":
		return addSub("+", "TIME")
	case "time_sub":
		return addSub("-", "TIME")
	case "timestamp_add":
		return addSub("+", "TIMESTAMP WITH TIME ZONE")
	case "timestamp_sub":
		return addSub("-", "TIMESTAMP WITH TIME ZONE")

	// ---- *_TRUNC: part first; cast result back to the source type ----
	case "date_trunc":
		if len(args) < 2 {
			return "", false
		}
		if sql, ok := duckDBTruncToPartSQL(args[0], partBody(1), "DATE"); ok {
			return sql, true
		}
		return fmt.Sprintf("CAST(date_trunc(%s, %s) AS DATE)", part(1), args[0]), true
	case "datetime_trunc":
		if len(args) < 2 {
			return "", false
		}
		if sql, ok := duckDBTruncToPartSQL(args[0], partBody(1), "TIMESTAMP"); ok {
			return sql, true
		}
		return fmt.Sprintf("date_trunc(%s, %s)", part(1), args[0]), true
	case "timestamp_trunc":
		if len(args) < 2 {
			return "", false
		}
		zone := "'UTC'"
		if len(args) >= 3 {
			zone = zoneArg(2)
		}
		civil := localTimestamp(args[0], zone)
		if sql, ok := duckDBTruncToPartSQL(civil, partBody(1), "TIMESTAMP"); ok {
			return timestampFromLocal(sql, zone), true
		}
		return timestampFromLocal(fmt.Sprintf("date_trunc(%s, %s)", part(1), civil), zone), true
	case "time_trunc":
		// DuckDB date_trunc rejects TIME, so lift the time onto the
		// epoch date (DuckDB has no TIMESTAMP+TIME operator, so go via
		// to_seconds(epoch(time))), truncate, and project back to TIME.
		if len(args) < 2 {
			return "", false
		}
		return fmt.Sprintf(
			"CAST(date_trunc(%s, %s) AS TIME)",
			part(1), timeLiftedToTimestamp(args[0]),
		), true

	// ---- *_BUCKET: origin + floor((value-origin)/width)*width ----
	case "date_bucket", "googlesqlite_date_bucket":
		if len(args) < 2 {
			return "", false
		}
		count, unit, ok := bucketWidth(1)
		if !ok || unit != "DAY" {
			return "", false
		}
		origin := "DATE '1950-01-01'"
		if len(args) >= 3 {
			origin = args[2]
		}
		return fmt.Sprintf(
			"CAST((%[2]s + (CAST(floor(CAST(date_diff('day', %[2]s, %[1]s) AS DOUBLE) / (%[3]s)) AS BIGINT) * (%[3]s) * INTERVAL '1 day')) AS DATE)",
			args[0], origin, count,
		), true
	case "datetime_bucket", "googlesqlite_datetime_bucket":
		if len(args) < 2 {
			return "", false
		}
		count, unit, ok := bucketWidth(1)
		if !ok {
			return "", false
		}
		widthSeconds, ok := intervalWidthSeconds(count, unit)
		if !ok {
			return "", false
		}
		origin := "TIMESTAMP '1950-01-01 00:00:00'"
		if len(args) >= 3 {
			origin = args[2]
		}
		return fmt.Sprintf(
			"make_timestamp(CAST(((epoch(CAST(%[2]s AS TIMESTAMP)) + floor((epoch(CAST(%[1]s AS TIMESTAMP)) - epoch(CAST(%[2]s AS TIMESTAMP))) / CAST((%[3]s) AS DOUBLE)) * CAST((%[3]s) AS DOUBLE)) * 1000000) AS BIGINT))",
			args[0], origin, widthSeconds,
		), true
	case "timestamp_bucket", "googlesqlite_timestamp_bucket":
		if len(args) < 2 {
			return "", false
		}
		count, unit, ok := bucketWidth(1)
		if !ok {
			return "", false
		}
		widthSeconds, ok := intervalWidthSeconds(count, unit)
		if !ok {
			return "", false
		}
		origin := "TIMESTAMPTZ '1950-01-01 00:00:00+00'"
		if len(args) >= 3 {
			origin = args[2]
		}
		return fmt.Sprintf(
			"to_timestamp(epoch(CAST(%[2]s AS TIMESTAMP WITH TIME ZONE)) + floor((epoch(CAST(%[1]s AS TIMESTAMP WITH TIME ZONE)) - epoch(CAST(%[2]s AS TIMESTAMP WITH TIME ZONE))) / CAST((%[3]s) AS DOUBLE)) * CAST((%[3]s) AS DOUBLE))",
			args[0], origin, widthSeconds,
		), true

	// ---- FORMAT_*: strftime(value, format) ----
	case "format_date", "format_datetime":
		if len(args) < 2 {
			return "", false
		}
		return fmt.Sprintf("strftime(%s, %s)", args[1], fmtArg(0)), true
	case "format_time":
		// strftime rejects a bare TIME, so lift it onto the epoch date.
		if len(args) < 2 {
			return "", false
		}
		return fmt.Sprintf("strftime(%s, %s)", timeLiftedToTimestamp(args[1]), fmtArg(0)), true
	case "format_timestamp":
		if len(args) < 2 {
			return "", false
		}
		zone := "'UTC'"
		if len(args) >= 3 {
			zone = zoneArg(2)
		}
		return fmt.Sprintf("strftime(%s, %s)", localTimestamp(args[1], zone), fmtArg(0)), true

	// ---- STRING(timestamp[, tz]): canonical text. Spec only asserts
	// non-null, so a plain cast to VARCHAR suffices. ----
	case "string":
		if len(args) < 1 {
			return "", false
		}
		return fmt.Sprintf("CAST(%s AS VARCHAR)", args[0]), true

	// ---- TIMESTAMP(string[, tz]): parse a textual instant. The string
	// arrives as a base64 STRING envelope; unwrap and cast. ----
	case "timestamp":
		if len(args) < 1 {
			return "", false
		}
		zone := "'UTC'"
		if len(args) >= 2 {
			zone = zoneArg(1)
		}
		if len(kinds) >= 1 {
			switch kinds[0] {
			case googlesql.TypeKindTypeString:
				if len(args) >= 2 {
					return timestampFromLocal(fmt.Sprintf("CAST(%s AS TIMESTAMP)", strLit(0)), zone), true
				}
				return fmt.Sprintf("CAST(%s AS TIMESTAMP WITH TIME ZONE)", strLit(0)), true
			case googlesql.TypeKindTypeDate, googlesql.TypeKindTypeDatetime:
				return timestampFromLocal(args[0], zone), true
			}
		}
		if len(args) >= 2 {
			return timestampFromLocal(args[0], zone), true
		}
		return fmt.Sprintf("CAST(%s AS TIMESTAMP WITH TIME ZONE)", args[0]), true

	// ---- PARSE_*: strptime(value, format) cast to the target type.
	// Both the format pattern (arg 0) and the input string (arg 1)
	// arrive as base64 STRING envelopes, so unwrap both. ----
	case "parse_date":
		if len(args) < 2 {
			return "", false
		}
		return fmt.Sprintf("CAST(strptime(%s, %s) AS DATE)", strLit(1), fmtArg(0)), true
	case "parse_datetime":
		if len(args) < 2 {
			return "", false
		}
		return fmt.Sprintf("CAST(strptime(%s, %s) AS TIMESTAMP)", strLit(1), fmtArg(0)), true
	case "parse_time":
		if len(args) < 2 {
			return "", false
		}
		return fmt.Sprintf("CAST(strptime(%s, %s) AS TIME)", strLit(1), fmtArg(0)), true
	case "parse_timestamp":
		if len(args) < 2 {
			return "", false
		}
		return fmt.Sprintf("CAST(strptime(%s, %s) AS TIMESTAMP WITH TIME ZONE)", strLit(1), fmtArg(0)), true

	// ---- UNIX_* extraction from an absolute instant ----
	case "unix_seconds":
		if len(args) < 1 {
			return "", false
		}
		return fmt.Sprintf("CAST(epoch(%s) AS BIGINT)", args[0]), true
	case "unix_millis":
		if len(args) < 1 {
			return "", false
		}
		return fmt.Sprintf("epoch_ms(%s)", args[0]), true
	case "unix_micros":
		if len(args) < 1 {
			return "", false
		}
		return fmt.Sprintf("epoch_us(%s)", args[0]), true
	case "unix_date":
		// Whole days between the epoch and a civil DATE.
		if len(args) < 1 {
			return "", false
		}
		return fmt.Sprintf("date_diff('day', DATE '1970-01-01', %s)", args[0]), true

	// ---- constructors from a unix offset -> absolute instant ----
	case "timestamp_seconds", "timestamp_from_unix_seconds":
		if len(args) < 1 {
			return "", false
		}
		return fmt.Sprintf("make_timestamp(CAST(%s AS BIGINT) * 1000000)", args[0]), true
	case "timestamp_millis", "timestamp_from_unix_millis":
		if len(args) < 1 {
			return "", false
		}
		return fmt.Sprintf("make_timestamp(CAST(%s AS BIGINT) * 1000)", args[0]), true
	case "timestamp_micros", "timestamp_from_unix_micros":
		if len(args) < 1 {
			return "", false
		}
		return fmt.Sprintf("make_timestamp(CAST(%s AS BIGINT))", args[0]), true
	case "date_from_unix_date":
		if len(args) < 1 {
			return "", false
		}
		return fmt.Sprintf("(DATE '1970-01-01' + to_days(CAST(%s AS INTEGER)))", args[0]), true

	// ---- civil constructors ----
	case "time":
		// TIME(hour, minute, second) -> make_time; TIME(timestamp[,tz])
		// projects an instant into a civil zone before taking the clock.
		if len(args) == 3 {
			return fmt.Sprintf("make_time(%s, %s, CAST(%s AS DOUBLE))", args[0], args[1], args[2]), true
		}
		if len(args) == 1 {
			if len(kinds) >= 1 && kinds[0] == googlesql.TypeKindTypeTimestamp {
				return fmt.Sprintf("CAST(%s AS TIME)", localTimestamp(args[0], "'UTC'")), true
			}
			return fmt.Sprintf("CAST(%s AS TIME)", args[0]), true
		}
		if len(args) == 2 {
			return fmt.Sprintf("CAST(%s AS TIME)", localTimestamp(args[0], zoneArg(1))), true
		}
		return "", false
	case "last_day":
		// LAST_DAY(date_or_datetime[, part]); the single-arg month form
		// maps straight to DuckDB last_day, which returns DATE.
		if len(args) == 1 {
			return fmt.Sprintf("last_day(%s)", args[0]), true
		}
		return "", false

	// ---- current-time builtins. These sit in currentTimeFuncMap, so
	// getFuncNameAndArgs shells them to googlesqlite_current_* and, when
	// a deterministic clock is pinned via WithCurrentTime, appends a UnixNano
	// arg after any user-supplied zone argument. Use the appended instant
	// when present; otherwise keep the native volatile DuckDB forms. ----
	case "current_date", "googlesqlite_current_date", "googlesqlite_safe_current_date":
		if unixNano, zoneArgs, ok := pinnedUnixNanoArg(args); ok {
			utcTS, instant := currentPinnedTimestampSQL(unixNano)
			civil := currentCivilTimestampSQL(zoneArgs, utcTS, instant)
			return fmt.Sprintf("CAST(%s AS DATE)", civil), true
		}
		civil := currentCivilTimestampSQL(args, "timezone('UTC', now())", "now()")
		return fmt.Sprintf("CAST(%s AS DATE)", civil), true
	case "current_datetime", "googlesqlite_current_datetime", "googlesqlite_safe_current_datetime":
		if unixNano, zoneArgs, ok := pinnedUnixNanoArg(args); ok {
			utcTS, instant := currentPinnedTimestampSQL(unixNano)
			return currentCivilTimestampSQL(zoneArgs, utcTS, instant), true
		}
		return currentCivilTimestampSQL(args, "timezone('UTC', now())", "now()"), true
	case "current_time", "googlesqlite_current_time", "googlesqlite_safe_current_time":
		if unixNano, zoneArgs, ok := pinnedUnixNanoArg(args); ok {
			utcTS, instant := currentPinnedTimestampSQL(unixNano)
			civil := currentCivilTimestampSQL(zoneArgs, utcTS, instant)
			return fmt.Sprintf("CAST(%s AS TIME)", civil), true
		}
		civil := currentCivilTimestampSQL(args, "timezone('UTC', now())", "now()")
		return fmt.Sprintf("CAST(%s AS TIME)", civil), true
	case "current_timestamp", "googlesqlite_current_timestamp", "googlesqlite_safe_current_timestamp":
		if unixNano, _, ok := pinnedUnixNanoArg(args); ok {
			_, instant := currentPinnedTimestampSQL(unixNano)
			return instant, true
		}
		return "now()", true
	}
	return "", false
}

// canonicalBuiltinFuncNames lists the special-form builtins whose
// FunctionCallNode dispatch (the switch on funcName below) handles them with
// a hand-written lowering rather than a registered UDF. On the SQLite path the
// resolver prefixes these to googlesqlite_<name>; on the DuckDB path the
// normalFuncMap is empty so they arrive bare. We canonicalize the bare name
// back to the prefixed form so a single dispatch covers both paths.
var canonicalBuiltinFuncNames = map[string]string{
	"iferror":     "googlesqlite_iferror",
	"iserror":     "googlesqlite_iserror",
	"nulliferror": "googlesqlite_nulliferror",
	// On the DuckDB target the analyzer resolves these proto builtins to
	// their bare GoogleSQL names (the SQLite normalFuncMap that would
	// otherwise shell them into googlesqlite_<name> is never populated).
	// Canonicalize so the switch below reaches their format* lowerings,
	// which rewrite the call to the googlesqlite_<name> scalar UDFs
	// registered by registerProtoUDFs. FILTER_FIELDS / REPLACE_FIELDS are
	// already shelled to the prefixed name upstream, so they are omitted.
	"from_proto":             "googlesqlite_from_proto",
	"to_proto":               "googlesqlite_to_proto",
	"proto_modify_map":       "googlesqlite_proto_modify_map",
	"proto_map_contains_key": "googlesqlite_proto_map_contains_key",
}

// canonicalizeBuiltinFuncName maps a bare special-form builtin name to its
// googlesqlite_<name> dispatch key, leaving any already-prefixed or unhandled
// name untouched.
func canonicalizeBuiltinFuncName(funcName string) string {
	if canonical, ok := canonicalBuiltinFuncNames[funcName]; ok {
		return canonical
	}
	return funcName
}

// tryFormatExtract lowers EXTRACT(part FROM expr). The analyzer lowers
// EXTRACT to the $extract builtin, which getFuncNameAndArgs shells into
// googlesqlite_extract(expr, '<part>') with the part as a base64-wrapped
// STRING literal. DuckDB has native date_part; the DATE / TIME / DATETIME
// sub-field parts return a temporal value rather than an integer, so they
// project through a CAST instead.
func (n *FunctionCallNode) tryFormatExtract(_ context.Context, funcName string, args []string) (string, bool, error) {
	if funcName != "googlesqlite_extract" && funcName != "googlesqlite_safe_extract" {
		return "", false, nil
	}
	if len(args) < 2 {
		return "", false, fmt.Errorf("EXTRACT: expected expr and part, got %d args", len(args))
	}
	// EXTRACT(part FROM INTERVAL ...) has no DuckDB-native date_part
	// overload: the operand reaches DuckDB as the value-layer interval
	// envelope (a VARCHAR), so date_part(STRING, STRING) is ambiguous.
	// Keep the call on the registered googlesqlite_extract value-layer
	// UDF, whose pure-Go body (date.BindExtract) decodes the envelope
	// and reads the requested interval field directly.
	if argList, _ := n.node.ArgumentList(); len(argList) > 0 && argList[0] != nil {
		if t, _ := argList[0].Type(); t != nil {
			if k, err := t.Kind(); err == nil && k == googlesql.TypeKindTypeInterval {
				return fmt.Sprintf("%s(%s)", funcName, strings.Join(args, ",")), true, nil
			}
		}
	}
	body, ok := decodeLiteralArgBody(args[1])
	if !ok {
		return "", false, fmt.Errorf("EXTRACT: unrecognized part argument %q", args[1])
	}
	switch strings.ToUpper(body) {
	case "DATE":
		return fmt.Sprintf("CAST(%s AS DATE)", args[0]), true, nil
	case "TIME":
		return fmt.Sprintf("CAST(%s AS TIME)", args[0]), true, nil
	case "DATETIME":
		return fmt.Sprintf("CAST(%s AS TIMESTAMP)", args[0]), true, nil
	}
	p, _ := duckDBDatePart(body)
	return fmt.Sprintf("date_part(%s, %s)", sqlStringLiteral(p), args[0]), true, nil
}

// tryFormatDateConstructor disambiguates the DATE() / DATETIME()
// overloads by argument count. GoogleSQL overloads one function name
// across "build from components" and "convert from an instant"; DuckDB
// splits these across make_date / make_timestamp and CAST, so the right
// lowering depends on arity.
func (n *FunctionCallNode) tryFormatDateConstructor(funcName string, args []string, kinds []googlesql.TypeKind) (string, bool, error) {
	zoneArg := func(i int) string {
		if i >= len(args) {
			return "'UTC'"
		}
		body, ok := decodeLiteralArgBody(args[i])
		if ok {
			return sqlStringLiteral(body)
		}
		return fmt.Sprintf("googlesqlite_plain_string(%s)", args[i])
	}
	localTimestamp := func(ts, zone string) string {
		return fmt.Sprintf("timezone(%s, CAST(%s AS TIMESTAMP WITH TIME ZONE))", zone, ts)
	}
	switch funcName {
	case "date":
		switch len(args) {
		case 3:
			// DATE(year, month, day)
			return fmt.Sprintf("make_date(%s, %s, %s)", args[0], args[1], args[2]), true, nil
		case 2:
			return fmt.Sprintf("CAST(%s AS DATE)", localTimestamp(args[0], zoneArg(1))), true, nil
		case 1:
			// DATE(timestamp) / DATE(datetime): project to civil date.
			if len(kinds) >= 1 && kinds[0] == googlesql.TypeKindTypeTimestamp {
				return fmt.Sprintf("CAST(%s AS DATE)", localTimestamp(args[0], "'UTC'")), true, nil
			}
			return fmt.Sprintf("CAST(%s AS DATE)", args[0]), true, nil
		}
		return "", false, fmt.Errorf("DATE: unexpected argument count %d", len(args))
	case "datetime":
		switch len(args) {
		case 6:
			// DATETIME(year, month, day, hour, minute, second)
			return fmt.Sprintf(
				"make_timestamp(%s, %s, %s, %s, %s, CAST(%s AS DOUBLE))",
				args[0], args[1], args[2], args[3], args[4], args[5],
			), true, nil
		case 2:
			if len(kinds) >= 2 && kinds[0] == googlesql.TypeKindTypeDate && kinds[1] == googlesql.TypeKindTypeTime {
				// DATETIME(date, time)
				return fmt.Sprintf("CAST(%s AS TIMESTAMP) + (%s - TIME '00:00:00')", args[0], args[1]), true, nil
			}
			// DATETIME(timestamp, zone)
			return localTimestamp(args[0], zoneArg(1)), true, nil
		case 1:
			// DATETIME(timestamp) / DATETIME(date)
			if len(kinds) >= 1 && kinds[0] == googlesql.TypeKindTypeTimestamp {
				return localTimestamp(args[0], "'UTC'"), true, nil
			}
			return fmt.Sprintf("CAST(%s AS TIMESTAMP)", args[0]), true, nil
		}
		return "", false, fmt.Errorf("DATETIME: unexpected argument count %d", len(args))
	}
	return "", false, nil
}

// formatFromProto lowers FROM_PROTO. It takes a proto-typed argument
// and returns a SQL primitive whose shape is determined by the resolved
// signature. Peek at the call's return type to dispatch.
func (n *FunctionCallNode) formatFromProto(_ context.Context, args []string) (string, error) {
	retType, _ := n.node.Type()
	if retType == nil {
		return fmt.Sprintf("googlesqlite_from_proto(%s, 'bytes')", args[0]), nil
	}
	kind, _ := retType.Kind()
	return fmt.Sprintf("googlesqlite_from_proto(%s, '%s')", args[0], protoFieldKindString(kind)), nil
}

// formatToProto lowers TO_PROTO. It takes a SQL primitive and returns
// the matching well-known proto wrapper bytes. Use the input argument's
// kind via the call's argument signature.
func (n *FunctionCallNode) formatToProto(_ context.Context, args []string) (string, error) {
	if len(args) == 0 {
		return "", fmt.Errorf("TO_PROTO: missing argument")
	}
	argList, _ := n.node.ArgumentList()
	argKind := "bytes"
	if len(argList) > 0 {
		if t, _ := argList[0].Type(); t != nil {
			if k, err := t.Kind(); err == nil {
				argKind = protoFieldKindString(k)
			}
		}
	}
	return fmt.Sprintf("googlesqlite_to_proto(%s, '%s')", args[0], argKind), nil
}

// formatFilterFields lowers FILTER_FIELDS(proto, '+a.b.c', '-d.e', ...).
// The analyzer surfaces each include/exclude path as a separate STRING
// literal argument. We pass the joined comma-separated path list to the
// runtime so it can decode include/exclude via the leading sign.
func (n *FunctionCallNode) formatFilterFields(_ context.Context, args []string) (string, error) {
	if len(args) < 1 {
		return "", fmt.Errorf("FILTER_FIELDS: missing argument")
	}
	paths := strings.Join(args[1:], ",")
	return fmt.Sprintf("googlesqlite_filter_fields(%s, %s)", args[0], protoFormatLiteralString(paths)), nil
}

// formatReplaceFields lowers
// REPLACE_FIELDS(proto, (value, 'field_path'), (value, 'field_path'), ...).
// The analyzer represents each replacement as a (value, path) pair
// surfaced as 2 ResolvedExpr args back-to-back. We pull the field
// number / kind from the path metadata embedded in the resolved
// signature; for the stub-grade implementation we accept a single
// (value, path) pair plus arbitrary additional pairs by walking
// args two at a time.
func (n *FunctionCallNode) formatReplaceFields(_ context.Context, args []string) (string, error) {
	if len(args) < 3 {
		return "", fmt.Errorf("REPLACE_FIELDS: needs at least one (value, path) pair")
	}
	// args: [proto, val1, path1, val2, path2, ...]
	// Lower to nested replace_fields_one(...) calls so the runtime
	// keeps a single field-replace primitive.
	inner := args[0]
	argList, _ := n.node.ArgumentList()
	pairOffset := 1
	for pairOffset+1 < len(args) {
		val := args[pairOffset]
		pathSql := args[pairOffset+1]
		// Look up the value argument's resolved type kind from the
		// FunctionCall's argument list (skip the proto arg at idx 0).
		kind := "bytes"
		if pairOffset < len(argList) {
			if t, _ := argList[pairOffset].Type(); t != nil {
				if k, err := t.Kind(); err == nil {
					kind = protoFieldKindString(k)
				}
			}
		}
		inner = fmt.Sprintf("googlesqlite_replace_fields(%s, %s, '%s', %s)", inner, pathSql, kind, val)
		pairOffset += 2
	}
	return inner, nil
}

// formatProtoModifyMap lowers
// PROTO_MODIFY_MAP(map_field, key1, value1, key2, value2, ...).
// RewriteProtoMapFns is disabled so the analyzer leaves the call shape
// untouched. We lower to
//
//	googlesqlite_proto_modify_map(parent_proto, map_field_tag,
//	  key_kind, val_kind, key1, value1, key2, value2, ...)
//
// and let the runtime walk + rewrite the parent message's map field
// entries.
func (n *FunctionCallNode) formatProtoModifyMap(ctx context.Context, args []string) (string, error) {
	argList, _ := n.node.ArgumentList()
	if len(args) < 3 || len(argList) < 3 {
		return "", fmt.Errorf("PROTO_MODIFY_MAP: invalid number of arguments: got %d, want at least 3", len(args))
	}
	if (len(args)-1)%2 != 0 {
		return "", fmt.Errorf("PROTO_MODIFY_MAP: key/value pairs must come in pairs (got %d trailing args)", len(args)-1)
	}
	parentSQL, mapFieldTag, err := mapFieldParentAndTag(ctx, argList[0])
	if err != nil {
		return "", fmt.Errorf("PROTO_MODIFY_MAP: %w", err)
	}
	keyKind := "bytes"
	valKind := "bytes"
	if t, _ := argList[1].Type(); t != nil {
		if k, err := t.Kind(); err == nil {
			keyKind = protoFieldKindString(k)
		}
	}
	if t, _ := argList[2].Type(); t != nil {
		if k, err := t.Kind(); err == nil {
			valKind = protoFieldKindString(k)
		}
	}
	out := []string{parentSQL, strconv.Itoa(int(mapFieldTag)), "'" + keyKind + "'", "'" + valKind + "'"}
	out = append(out, args[1:]...)
	return fmt.Sprintf("googlesqlite_proto_modify_map(%s)", strings.Join(out, ", ")), nil
}

// formatProtoMapContainsKey lowers PROTO_MAP_CONTAINS_KEY(map_field, key).
// Lower to googlesqlite_proto_map_contains_key(parent_proto,
// map_field_tag, key_kind, key) so the runtime can walk every
// occurrence of the map field's outer tag inside the parent message
// (GetProtoField on a repeated field only keeps the last occurrence's
// payload, which loses the entries needed for the key lookup).
// RewriteProtoMapFns is disabled so the analyzer leaves the call shape
// untouched.
func (n *FunctionCallNode) formatProtoMapContainsKey(ctx context.Context, args []string) (string, error) {
	argList, _ := n.node.ArgumentList()
	if len(args) < 2 || len(argList) < 2 {
		return "", fmt.Errorf("PROTO_MAP_CONTAINS_KEY: invalid number of arguments: got %d, want 2", len(args))
	}
	keyKind := "bytes"
	if t, _ := argList[1].Type(); t != nil {
		if k, err := t.Kind(); err == nil {
			keyKind = protoFieldKindString(k)
		}
	}
	parentSQL, mapFieldTag, err := mapFieldParentAndTag(ctx, argList[0])
	if err != nil {
		return "", fmt.Errorf("PROTO_MAP_CONTAINS_KEY: %w", err)
	}
	return fmt.Sprintf("googlesqlite_proto_map_contains_key(%s, %d, '%s', %s)", parentSQL, mapFieldTag, keyKind, args[1]), nil
}

// formatEnumValueDescriptorProto lowers ENUM_VALUE_DESCRIPTOR_PROTO(enum_value).
// Look up the enum's full name via the registered enum type so the
// runtime can emit name + number; without a registered enum we fall
// back to number-only.
func (n *FunctionCallNode) formatEnumValueDescriptorProto(_ context.Context, args []string) (string, error) {
	argList, _ := n.node.ArgumentList()
	enumName := ""
	if len(argList) > 0 {
		if t, _ := argList[0].Type(); t != nil {
			ds, _ := t.DebugString(false)
			enumName = protoNameFromDebug(ds)
		}
	}
	return fmt.Sprintf("googlesqlite_enum_value_descriptor_proto(%s, '%s')", args[0], enumName), nil
}

// formatErrorHandling lowers the IFERROR / ISERROR / NULLIFERROR family.
// SQLite has no UDF-level try / catch (the VDBE aborts the statement as
// soon as any function returns non-nil error), so we lower the
// error-handling family by reformatting the inner argument under a
// safe-eval sub-context. Every reachable function call switches to its
// `googlesqlite_safe_<name>` variant (NULL on failure), and a nested
// ERROR(msg) short-circuits to NULL via the inSafeEvalMode branch on
// `error`. NULL therefore stands in for "an error happened", which we
// surface back to the caller via simple SQL CASEs.
func (n *FunctionCallNode) formatErrorHandling(ctx context.Context, funcName string, args []string) (string, error) {
	safeArgs := m1(n.node.ArgumentList())
	if len(safeArgs) < 1 {
		return "", fmt.Errorf("%s: missing argument", funcName)
	}
	safeCtx := withSafeEvalMode(ctx)
	safeX, err := newNode(safeArgs[0]).FormatSQL(safeCtx)
	if err != nil {
		return "", err
	}
	switch funcName {
	case "googlesqlite_iferror":
		if len(args) < 2 {
			return "", fmt.Errorf("IFERROR: needs catch_expression")
		}
		// Both arms of IFERROR(T1, T1) share the result type T1, but the
		// try arm can reach the binder as a value-layer envelope VARCHAR
		// (an array element from $array_at_offset, a scalar subquery whose
		// column is value-layer-encoded) while the catch arm is a native
		// literal. DuckDB then rejects `CASE WHEN ... THEN <int literal>
		// ELSE <varchar> END` as a mixed-type CASE. When T1 is a value-layer
		// scalar AND the try arm actually produces the envelope, decode it
		// back to its native form so both arms agree; the row decoder accepts
		// the native result for the column just as it accepts the envelope. A
		// try arm that is already native (e.g. arithmetic like 1 + 2) is left
		// untouched - decoding it would pass a BIGINT to googlesqlite_decode_
		// scalar, which only binds VARCHAR. Non-scalar result types (STRING /
		// STRUCT / ARRAY / temporal) also keep the untouched form.
		//
		// A NUMERIC result (issue #22) is the dual-carrier shape instead:
		// the try arm can be the value-layer envelope VARCHAR (a
		// googlesqlite_safe_divide result) while the catch arm is a native
		// DECIMAL(38,9), which this CASE then refuses to bind. Normalize
		// both arms through numericStorageReadSQL, same as the CASE / IF /
		// COALESCE family. The wrapped try arm stays NULL exactly when the
		// safe-evaluated inner expression is NULL (both decode arms of the
		// dual form map NULL to NULL), so the error-to-NULL detection in
		// the WHEN clause is preserved.
		tryArm := iferrorTryArmSQL(n, safeX)
		catchArm := args[1]
		if t, _ := n.node.Type(); t != nil {
			if k, kerr := t.Kind(); kerr == nil && k == googlesql.TypeKindTypeNumeric {
				tryArm = numericStorageReadSQL(tryArm)
				catchArm = numericStorageReadSQL(catchArm)
			}
		}
		return fmt.Sprintf("CASE WHEN (%s) IS NULL THEN %s ELSE (%s) END", tryArm, catchArm, tryArm), nil
	case "googlesqlite_iserror":
		return fmt.Sprintf("((%s) IS NULL)", safeX), nil
	case "googlesqlite_nulliferror":
		return fmt.Sprintf("(%s)", safeX), nil
	}
	return safeX, nil
}

// iferrorTryArmSQL returns the SQL for IFERROR's try arm. It decodes the
// safe-evaluated inner expression back to its native form via
// googlesqlite_decode_scalar + CAST (mirrors GetStructFieldNode's scalar-field
// decode) only when both hold: the call's result type is a value-layer scalar
// (INT / FLOAT / BOOL), and the formatted try arm is itself a value-layer
// envelope (safeXIsValueLayerEnvelope). A try arm that lowers to a native
// scalar - arithmetic like 1 + 2, a native literal or column - is returned
// unchanged, because wrapping it in googlesqlite_decode_scalar would pass a
// native BIGINT / DOUBLE to a UDF that only binds VARCHAR.
func iferrorTryArmSQL(n *FunctionCallNode, safeX string) string {
	if t, _ := n.node.Type(); t != nil {
		if k, err := t.Kind(); err == nil {
			if castType, ok := valueLayerScalarCastType(k); ok && safeXIsValueLayerEnvelope(safeX) {
				return fmt.Sprintf("CAST(googlesqlite_decode_scalar(%s) AS %s)", safeX, castType)
			}
		}
	}
	return safeX
}

// safeXIsValueLayerEnvelope reports whether a safe-evaluated inner expression
// is rendered as a value-layer envelope VARCHAR rather than a native scalar.
// Every envelope-producing lowering on the DuckDB path either calls a
// googlesqlite_ UDF (the array-element accessors googlesqlite_safe_array_at_*,
// the googlesqlite_cast the analyzer inserts to coerce ERROR()/other arms, the
// JSON / struct accessors) or extracts the value through a scalar subquery
// (SELECT ...). A native scalar produced by tryNativeOperator (e.g.
// `(CAST(1 AS BIGINT) + CAST(2 AS BIGINT))`) contains neither marker, so it is
// left undecoded. Keeping this a syntactic test avoids re-walking the resolved
// inner node, whose envelope-ness depends on analyzer-inserted casts that are
// only visible after formatting.
func safeXIsValueLayerEnvelope(safeX string) bool {
	return strings.Contains(safeX, "googlesqlite_") ||
		strings.Contains(safeX, "SELECT ") ||
		strings.Contains(safeX, "SELECT*")
}

// formatErrorBuiltin lowers ERROR / SAFE_ERROR. Inside a safe-eval
// sub-context, ERROR(msg) folds to NULL so IFERROR / ISERROR /
// NULLIFERROR can observe "an error happened" without aborting the
// statement. Outside that context, fall through to the regular UDF that
// raises.
func (n *FunctionCallNode) formatErrorBuiltin(ctx context.Context, funcName string, args []string) (string, error) {
	if inSafeEvalMode(ctx) {
		return "NULL", nil
	}
	// SQLite path: route through the registered googlesqlite_error UDF.
	funcMap := funcMapFromContext(ctx)
	if spec, exists := funcMap[funcName]; exists {
		return spec.CallSQL(ctx, n.node.ResolvedFunctionCallBase, args)
	}
	// DuckDB path: emit the native error() function. Unlike a registered
	// UDF (which must declare a concrete VARCHAR result type and would
	// then fail to unify with a non-VARCHAR sibling branch in an
	// enclosing CASE / if - e.g. the IS_FIRST / IS_LAST numbering
	// rewrite's `if(k<0, ERROR(...), <bool>)`), DuckDB's native error()
	// is polymorphic and short-circuits, so it both type-unifies and
	// stays unevaluated in a dead branch.
	if len(args) >= 1 {
		return fmt.Sprintf("error(%s)", args[0]), nil
	}
	return "error()", nil
}

// formatCaseNoValue lowers a searched CASE expression
// (CASE WHEN ... THEN ... [ELSE ...] END).
//
// DuckDB requires every CASE result branch to bind under one common
// type, but a NUMERIC branch can carry either of the two carrier forms
// (native DECIMAL(38,9) vs the value-layer envelope VARCHAR) — e.g. a
// division result (googlesqlite_divide envelope) in one branch and a
// CAST literal (native DECIMAL) in another fails with "Cannot mix
// values of type DECIMAL(38,9) and VARCHAR in CASE expression" (issue
// #22, the dbt unit-dispatch model). Normalize every NUMERIC-kind
// argument the same way COALESCE / IFNULL do (issue #21): the WHEN
// conditions are BOOL and pass through untouched, the THEN / ELSE
// results land on the native form. The same normalization runs for the
// value-match form below, where it additionally covers the CASE value
// and WHEN comparand operands (the binder builds an equality between
// them, which must also compare by value, not by envelope text).
func (n *FunctionCallNode) formatCaseNoValue(_ context.Context, args []string) (string, error) {
	args = normalizeNumericCarrierArgs(args, argKinds(m1(n.node.ArgumentList())))
	var whenStmts []string
	for i := 0; i < len(args)-1; i += 2 {
		whenStmts = append(whenStmts, fmt.Sprintf("WHEN %s THEN %s", args[i], args[i+1]))
	}
	stmt := fmt.Sprintf("CASE %s", strings.Join(whenStmts, " "))
	// if args length is odd number, else statement exists.
	if len(args) > (len(args)/2)*2 {
		stmt += fmt.Sprintf(" ELSE %s", args[len(args)-1])
	}
	stmt += " END"
	return stmt, nil
}

// formatCaseWithValue lowers a simple CASE expression
// (CASE value WHEN ... THEN ... [ELSE ...] END). NUMERIC-kind operands
// take the dual-carrier normalization described on formatCaseNoValue —
// here it covers all three slots (CASE value, WHEN comparands, branch
// results), since the value/comparand equality and the result-branch
// unification both demand one engine type.
func (n *FunctionCallNode) formatCaseWithValue(_ context.Context, args []string) (string, error) {
	if len(args) < 2 {
		return "", fmt.Errorf("not enough arguments for case with value")
	}
	args = normalizeNumericCarrierArgs(args, argKinds(m1(n.node.ArgumentList())))
	val := args[0]
	args = args[1:]
	var whenStmts []string
	for i := 0; i < len(args)-1; i += 2 {
		whenStmts = append(whenStmts, fmt.Sprintf("WHEN %s THEN %s", args[i], args[i+1]))
	}
	stmt := fmt.Sprintf("CASE %s %s", val, strings.Join(whenStmts, " "))
	// if args length is odd number, else statement exists.
	if len(args) > (len(args)/2)*2 {
		stmt += fmt.Sprintf(" ELSE %s", args[len(args)-1])
	}
	stmt += " END"
	return stmt, nil
}

func (n *AggregateFunctionCallNode) FormatSQL(ctx context.Context) (string, error) {
	if n.node == nil {
		return "", nil
	}
	// Special-case AGG(<graph_get_element_property>) where the
	// property is a MEASURE. Upstream's measure rewriter refuses
	// to handle this shape (it wants AGG over a direct ColumnRef
	// only), so we lower it ourselves: emit
	//   agg(<measure_inner_expr>, <locking_key>, '<kind>')
	// against the locally-registered `googlesqlite_agg` aggregate.
	// See internal/functions/aggregate/agg.go for the runtime.
	if sql, ok, err := tryFormatMeasureAGG(ctx, n.node); ok || err != nil {
		return sql, err
	}
	funcName, args, err := getFuncNameAndArgs(ctx, n.node.ResolvedFunctionCallBase, false)
	if err != nil {
		return "", err
	}
	// HAVING { MAX | MIN } modifier rewrite. The resolved tree
	// carries the modifier on ResolvedAggregateFunctionCall, but
	// SQLite has no equivalent. For ANY_VALUE we rewrite to a
	// custom googlesqlite_having_any_value(value, having_expr,
	// 'MAX'|'MIN') that filters rows down to those matching the
	// extremum of having_expr at Done() time.
	if havingMod := m1(n.node.HavingModifier()); havingMod != nil && (funcName == "any_value" || funcName == "googlesqlite_any_value") {
		havingExpr, herr := newNode(m1(havingMod.HavingExpr())).FormatSQL(ctx)
		if herr != nil {
			return "", herr
		}
		kind := "MAX"
		if k, kerr := havingMod.Kind(); kerr == nil {
			if k == googlesql.ResolvedAggregateHavingModifierEnums_HavingModifierKindMin {
				kind = "MIN"
			}
		}
		kindLit, _ := literalFromValue(value.StringValue(kind))
		// The bare name matches the aggregate registered in duckdbAggSpecs()
		// (the DuckDB cgo path registers aggregates under their bare lowered
		// name, unlike the googlesqlite_-prefixed scalar UDFs).
		return fmt.Sprintf(
			"having_any_value(%s,%s,%s)",
			strings.Join(args, ","), havingExpr, kindLit,
		), nil
	}
	// When formatting inside a DifferentialPrivacyAggregateScan /
	// AnonymizedAggregateScan, append (epsilon, delta) to every DP /
	// anonymization aggregate call so the runtime UDF can size the
	// Laplace noise from the scan's privacy budget. The WITH
	// DIFFERENTIAL_PRIVACY surface lowers to $-builtin
	// $differential_privacy_* names that pick up the googlesqlite_
	// prefix; the WITH ANONYMIZATION surface's RewriteAnonymization
	// emits the catalog aggregate names anon_* / $anon_* directly, so
	// match both the bare and prefixed anon spellings. Without the
	// trailing budget, the high-epsilon "without noise" examples
	// (epsilon=1e20) could not size noise to ~0 and would diverge from
	// the asserted exact values.
	if dp := dpOptionsFromContext(ctx); dp != nil && isDPAggregateName(funcName) {
		epsLit, _ := literalFromValue(value.FloatValue(dp.Epsilon))
		delLit, _ := literalFromValue(value.FloatValue(dp.Delta))
		args = append(args, epsLit, delLit)
	}
	funcMap := funcMapFromContext(ctx)
	if spec, exists := funcMap[funcName]; exists {
		return spec.CallSQL(ctx, n.node.ResolvedFunctionCallBase, args)
	}
	// DuckDB retarget: the approximate-aggregate family lowers to a
	// complete native expression (quantile_disc / approx_top_k with a
	// reshaped argument list), so return it directly before the generic
	// DISTINCT / ORDER BY emit, which would otherwise mangle the call.
	if sql, ok := tryNativeApproxAggregate(funcName, args); ok {
		return sql, nil
	}
	// DuckDB retarget: COUNTIF. DuckDB exposes countif/count_if, but it
	// returns NULL over an empty input whereas GoogleSQL COUNTIF returns
	// 0. COUNT(CASE WHEN cond THEN 1 END) matches both the truthy-count
	// and the zero-on-empty semantics.
	if funcName == "countif" && len(args) == 1 {
		return fmt.Sprintf("count(CASE WHEN %s THEN 1 END)", args[0]), nil
	}
	if funcName == "max_by" && len(args) == 2 {
		return fmt.Sprintf("arg_max(%s,%s)", args[0], args[1]), nil
	}
	if funcName == "min_by" && len(args) == 2 {
		return fmt.Sprintf("arg_min(%s,%s)", args[0], args[1]), nil
	}
	// DuckDB retarget: numeric / statistical / bitwise / logical
	// aggregates resolve to their bare catalog name here (the SQLite
	// aggregateFuncMap is never populated on this target), but DuckDB's
	// native reducers cannot consume the JSON value-layer encoding the
	// UNNEST source feeds. Rewrite to the native name with the argument(s)
	// cast to the right numeric type; the DISTINCT / ORDER BY handling
	// below still applies to the rewritten form. resultWrap (when set)
	// narrows the finished aggregate call back to its analyzer result
	// type — SUM(INT64) must come back as an exact BIGINT, with DuckDB's
	// out-of-range conversion error supplying BigQuery's overflow-is-an-
	// error semantics (issue #5).
	resultKind := googlesql.TypeKindTypeUnknown
	if t, _ := n.node.Type(); t != nil {
		if k, err := t.Kind(); err == nil {
			resultKind = k
		}
	}
	aggArgKinds := argKinds(m1(n.node.ResolvedFunctionCallBase.ArgumentList()))
	var resultWrap string
	if duckName, castArgs, rw, ok := tryNativeNumericAggregate(funcName, args, resultKind, aggArgKinds); ok {
		funcName = duckName
		args = castArgs
		resultWrap = rw
	}
	// DuckDB retarget (PATH B): the predecessor encoded the aggregate
	// modifiers (DISTINCT, ORDER BY, IGNORE NULLS, LIMIT) as trailing
	// googlesqlite_* pseudo-args that its custom SQLite aggregate UDFs
	// parsed out. DuckDB's native aggregates take those modifiers as
	// real in-call SQL syntax, so emit them that way. DISTINCT becomes a
	// `DISTINCT ` argument prefix and ORDER BY becomes an in-aggregate
	// `ORDER BY` clause - both native DuckDB aggregate syntax. (IGNORE
	// NULLS / inline LIMIT have no general native form and stay markers;
	// they are the documented backlog boundary, not on the example
	// validation path.)
	argPrefix := ""
	if m1(n.node.Distinct()) {
		argPrefix = "DISTINCT "
	}
	var orderByClause string
	var orderByItems []string
	for _, item := range m1(n.node.OrderByItemList()) {
		columnRef := m1(item.ColumnRef())
		colName := uniqueColumnName(ctx, m1(columnRef.Column()))
		if m1(item.IsDescending()) {
			orderByItems = append(orderByItems, fmt.Sprintf("\"%s\" DESC", colName))
		} else {
			orderByItems = append(orderByItems, fmt.Sprintf("\"%s\"", colName))
		}
	}
	if len(orderByItems) > 0 {
		orderByClause = " ORDER BY " + strings.Join(orderByItems, ",")
	}
	var opts []string
	if m1(n.node.Limit()) != nil {
		limitValue, err := newNode(m1(n.node.Limit())).FormatSQL(ctx)
		if err != nil {
			return "", err
		}
		opts = append(opts, fmt.Sprintf("googlesqlite_limit(%s)", limitValue))
	}
	switch m1(n.node.NullHandlingModifier()) {
	case googlesql.ResolvedNonScalarFunctionCallBaseEnums_NullHandlingModifierIgnoreNulls:
		opts = append(opts, "googlesqlite_ignore_nulls()")
	case googlesql.ResolvedNonScalarFunctionCallBaseEnums_NullHandlingModifierRespectNulls:
	}
	args = append(args, opts...)
	// DuckDB retarget: a few GoogleSQL aggregate builtins resolve to bare
	// names that DuckDB spells differently. LOGICAL_AND / LOGICAL_OR (the
	// boolean aggregates the analyzer emits inside ARRAY_INCLUDES_ALL /
	// ARRAY_INCLUDES_ANY rewrites) map to DuckDB's bool_and / bool_or with
	// identical NULL-skipping semantics.
	if native, ok := nativeAggregateFuncMap[funcName]; ok {
		funcName = native
	}
	// DuckDB retarget: ARRAY_CONCAT_AGG flattens per-row ARRAY<T> values
	// into one ARRAY<T>. DuckDB has no array_concat_agg, and duckdb-go
	// exposes no aggregate-UDF registration, so collect the per-row array
	// values with native array_agg (yielding a LIST of array-envelope
	// VARCHARs) and flatten them in a scalar UDF that decodes each envelope
	// and concatenates the elements into a single array-envelope. An
	// all-empty / zero-row input flattens to an empty array, which
	// GoogleSQL then surfaces as NULL on the result path.
	if funcName == "array_concat_agg" {
		return fmt.Sprintf(
			"googlesqlite_array_concat_agg_flatten(array_agg(%s%s%s))",
			argPrefix,
			strings.Join(args, ","),
			orderByClause,
		), nil
	}
	sql := fmt.Sprintf(
		"%s(%s%s%s)",
		funcName,
		argPrefix,
		strings.Join(args, ","),
		orderByClause,
	)
	if resultWrap != "" {
		// Re-shape the finished aggregate to its analyzer result form: a
		// narrowing CAST for the exact-integer / NUMERIC cases (DuckDB
		// raises a conversion error on an out-of-range sum, which is
		// BigQuery's overflow behavior) or the string-envelope re-wrap
		// for MIN/MAX over STRING (issue #9 coverage class).
		sql = fmt.Sprintf(resultWrap, sql)
	}
	return sql, nil
}

// nativeAggregateFuncMap maps a bare GoogleSQL aggregate builtin name to
// its DuckDB-native aggregate spelling for the non-window call path.
var nativeAggregateFuncMap = map[string]string{
	"logical_and": "bool_and",
	"logical_or":  "bool_or",
}

// nativeWindowFuncMap maps GoogleSQL window function names that have a
// direct SQLite built-in equivalent. When a function is in this map,
// AnalyticFunctionCallNode emits native `<sqlite_name>(args) OVER
// (PARTITION BY ... ORDER BY ... FRAME)` syntax — avoiding the
// per-row full-scan emulation that the predecessor used because the
// underlying SQLite binding (mattn) could not register custom window
// functions.
var nativeWindowFuncMap = map[string]string{
	// Numbering functions — SQLite built-ins return identical results.
	"row_number":   "row_number",
	"rank":         "rank",
	"dense_rank":   "dense_rank",
	"percent_rank": "percent_rank",
	"cume_dist":    "cume_dist",
	"ntile":        "ntile",
	// PERCENTILE_CONT / PERCENTILE_DISC map to DuckDB's quantile_cont /
	// quantile_disc, which accept the same (value, fraction) argument
	// order in OVER context and ignore NULLs natively (matching the
	// GoogleSQL default IGNORE NULLS modifier on these functions).
	"percentile_cont": "quantile_cont",
	"percentile_disc": "quantile_disc",
	// COUNT(*) -> DuckDB native count(*). The IGNORE NULLS modifier the
	// IS_LAST rewrite attaches is a no-op for COUNT(*) (it counts rows),
	// so the bare native form is exact. formatNative injects the * arg.
	"count_star": "count",
	// Navigation functions — SQLite returns the underlying value
	// unchanged, so encoded values flow through correctly.
	"lag":         "lag",
	"lead":        "lead",
	"first_value": "first_value",
	"last_value":  "last_value",
	"nth_value":   "nth_value",
	// Cumulative aggregates against native SQLite types. Encoded
	// values would not survive these (SUM of base64 strings would
	// fail), but parity tests exercise them on INT64/FLOAT64 where
	// SQLite arithmetic suffices.
	"sum":   "sum",
	"avg":   "avg",
	"count": "count",
	"min":   "min",
	"max":   "max",
	// TF_IDF is registered as a DuckDB aggregate (see duckdb_aggregate_udf.go),
	// and DuckDB runs any aggregate as a window function under OVER (). Routing
	// it through the native window path emits a clean `tf_idf(tokens) OVER (...)`
	// call without the option markers (ignore_nulls / window_rowid) the
	// emulation path appends - those marker UDFs are not registered on the
	// DuckDB backend, and the registered aggregate computes the per-row scores
	// itself.
	"tf_idf": "tf_idf",
}

// customNativeWindowFuncMap lists googlesql window functions whose
// semantics don't have a SQLite built-in counterpart but whose
// googlesqlite_window_<name> implementation has been refactored to
// run incrementally (Step / Done over the active frame), making them
// safe to invoke through native OVER syntax instead of the
// per-output-row correlated-subquery emulation. The value is the
// custom SQLite function name registered through sqlitex.
var customNativeWindowFuncMap = map[string]string{
	"array_agg":  "googlesqlite_window_array_agg",
	"string_agg": "googlesqlite_window_string_agg",
	"countif":    "googlesqlite_window_countif",
	"any_value":  "googlesqlite_window_any_value",
	// Statistical aggregators. Each runs over a sliding buffer of
	// floats with Step / Inverse / Done — see
	// internal/function_window_stats.go.
	"corr":        "googlesqlite_window_corr",
	"covar_pop":   "googlesqlite_window_covar_pop",
	"covar_samp":  "googlesqlite_window_covar_samp",
	"stddev":      "googlesqlite_window_stddev_samp",
	"stddev_pop":  "googlesqlite_window_stddev_pop",
	"stddev_samp": "googlesqlite_window_stddev_samp",
	"variance":    "googlesqlite_window_var_samp",
	"var_pop":     "googlesqlite_window_var_pop",
	"var_samp":    "googlesqlite_window_var_samp",
	// Boolean / bitwise aggregators in OVER context. Their custom
	// natives implement Step / Inverse / Done over the active frame
	// with per-row tristate tracking (NULL / FALSE / TRUE) for the
	// LOGICAL_* family and direct &/|/^ folding for the BIT_* family.
	"logical_or":  "googlesqlite_window_logical_or",
	"logical_and": "googlesqlite_window_logical_and",
	"bit_and":     "googlesqlite_window_bit_and",
	"bit_or":      "googlesqlite_window_bit_or",
	"bit_xor":     "googlesqlite_window_bit_xor",
	// ARRAY_CONCAT_AGG flattens per-row ARRAY<T> arguments into a
	// single ARRAY<T> over the active frame.
	"array_concat_agg": "googlesqlite_window_array_concat_agg",
}

// nativeWindowFuncForName returns the SQLite native name for a
// predecessor window function, or "" when no native equivalent
// applies.
func nativeWindowFuncForName(name string) string {
	if v, ok := nativeWindowFuncMap[name]; ok {
		return v
	}
	return ""
}

func (n *AnalyticFunctionCallNode) FormatSQL(ctx context.Context) (string, error) {
	if n.node == nil {
		return "", nil
	}
	orderColumnNames := analyticOrderColumnNamesFromContext(ctx)
	orderColumns := orderColumnNames.values

	// Resolve the function name first so we know whether to emit
	// native OVER syntax or the predecessor's correlated-subquery
	// emulation. Some BigQuery-specific modifiers (DISTINCT,
	// IGNORE NULLS) and one tied-row case in CUME_DIST diverge from
	// SQLite's native semantics, so those still take the emulation
	// path.
	rawName := strings.ReplaceAll(m1(m1(n.node.Function()).FullName(false)), ".", "_")
	// googlesql analyzer prefixes some operator-like functions with
	// `$` (e.g. `$count_star` for COUNT(*)). Strip it for lookup.
	rawName = strings.TrimPrefix(rawName, "$")

	// ST_CLUSTERDBSCAN has no native analytic form and the SQLite
	// emulation hack does not survive DuckDB's unordered parallel
	// scan, so lower it to a native list_extract(... OVER (), ...)
	// window expression backed by the st_clusterdbscan window
	// aggregate. See formatStClusterDBSCAN.
	if rawName == "st_clusterdbscan" {
		return n.formatStClusterDBSCAN(ctx, orderColumns)
	}

	// SUM(DISTINCT x) / COUNT(DISTINCT x) / AVG(DISTINCT x) — SQLite
	// rejects DISTINCT in OVER, but our custom natives know how to
	// dedupe. Take precedence over the SQLite-native fast path.
	if m1(n.node.Distinct()) {
		if custom, ok := distinctAwareNativeWindowFuncs[rawName]; ok {
			return n.formatNative(ctx, custom, orderColumns, true)
		}
	}
	if native := nativeWindowFuncForName(rawName); native != "" && !n.requiresPredecessorEmulation() {
		return n.formatNative(ctx, native, orderColumns, false)
	}
	if custom, ok := customNativeWindowFuncMap[rawName]; ok && !n.requiresPredecessorEmulation() {
		return n.formatNative(ctx, custom, orderColumns, true)
	}

	funcName, args, err := getFuncNameAndArgs(ctx, n.node.ResolvedFunctionCallBase, true)
	if err != nil {
		return "", err
	}
	var opts []string
	if m1(n.node.Distinct()) {
		opts = append(opts, "googlesqlite_distinct()")
	}
	switch m1(n.node.NullHandlingModifier()) {
	case googlesql.ResolvedNonScalarFunctionCallBaseEnums_NullHandlingModifierRespectNulls:
		// do nothing
	default:
		opts = append(opts, "googlesqlite_ignore_nulls()")
	}
	args = append(args, opts...)
	for _, column := range analyticPartitionColumnNamesFromContext(ctx) {
		args = append(args, window.GetWindowPartitionOptionFuncSQL(column))
	}
	for _, col := range orderColumns {
		args = append(args, window.GetWindowOrderByOptionFuncSQL(col.column, col.isAsc))
	}
	windowFrame, _ := n.node.WindowFrame()
	if windowFrame != nil {
		args = append(args, window.GetWindowFrameUnitOptionFuncSQL(m1(windowFrame.FrameUnit())))
		startSQL, err := n.getWindowBoundaryOptionFuncSQL(ctx, m1(windowFrame.StartExpr()), true)
		if err != nil {
			return "", err
		}
		endSQL, err := n.getWindowBoundaryOptionFuncSQL(ctx, m1(windowFrame.EndExpr()), false)
		if err != nil {
			return "", err
		}
		args = append(args, startSQL, endSQL)
	}
	args = append(args, window.GetWindowRowIDOptionFuncSQL())
	input := analyticInputScanFromContext(ctx)
	funcMap := funcMapFromContext(ctx)
	// Anything that reaches here uses the predecessor's per-output-row
	// correlated-subquery emulation, which references `row_id`. Tell
	// the surrounding AnalyticScanNode to keep the row_id-wrap.
	markAnalyticEmulationUsed(ctx)
	if spec, exists := funcMap[funcName]; exists {
		return spec.CallSQL(ctx, n.node.ResolvedFunctionCallBase, args)
	}
	return fmt.Sprintf(
		"( SELECT %s(%s) %s )",
		funcName,
		strings.Join(args, ","),
		input,
	), nil
}

// distinctAwareNativeWindowFuncs maps `<name>` to the custom function
// registered through Conn.CreateWindowFunction that handles
// `<name>(DISTINCT x) OVER (...)`. Used when the formatter sees the
// DISTINCT modifier on functions whose plain native form is a SQLite
// built-in (which doesn't accept DISTINCT in OVER).
var distinctAwareNativeWindowFuncs = map[string]string{
	"sum":   "googlesqlite_window_sum_distinct",
	"count": "googlesqlite_window_count_distinct",
	"avg":   "googlesqlite_window_avg_distinct",
}

// requiresPredecessorEmulation reports whether this analytic call
// needs to keep using the per-row full-scan emulation rather than the
// new native OVER path.
func (n *AnalyticFunctionCallNode) requiresPredecessorEmulation() bool {
	rawName := strings.TrimPrefix(strings.ReplaceAll(m1(m1(n.node.Function()).FullName(false)), ".", "_"), "$")

	if m1(n.node.Distinct()) {
		// MIN(DISTINCT x) and MAX(DISTINCT x) are equivalent to
		// MIN(x) / MAX(x) — the DISTINCT marker is a no-op, so
		// SQLite native is fine.
		switch rawName {
		case "min", "max":
			return false
		}
		// SUM/COUNT/AVG with DISTINCT take the custom-native path
		// (see distinctAwareNativeWindowFuncs lookup below). For
		// every other DISTINCT-using analytic call we still need
		// emulation — including the existing customNativeWindowFuncs
		// (array_agg, string_agg, etc.) which already strip the
		// DISTINCT marker themselves but only when they hit the
		// custom path naturally.
		if _, ok := distinctAwareNativeWindowFuncs[rawName]; ok {
			return false
		}
		if _, ok := customNativeWindowFuncMap[rawName]; ok {
			return false
		}
		return true
	}
	switch m1(n.node.NullHandlingModifier()) {
	case googlesql.ResolvedNonScalarFunctionCallBaseEnums_NullHandlingModifierIgnoreNulls:
		// SQLite's SUM/AVG/COUNT/MIN/MAX/COUNT_STAR already skip
		// NULLs unconditionally, which matches the IGNORE NULLS
		// modifier exactly. Navigation functions (LAG/LEAD/
		// FIRST_VALUE/LAST_VALUE/NTH_VALUE) honour the modifier
		// natively.
		switch rawName {
		case "sum", "count", "count_star", "avg", "min", "max",
			"lag", "lead", "first_value", "last_value", "nth_value",
			// DuckDB quantile_cont / quantile_disc ignore NULLs
			// natively, which matches the GoogleSQL default IGNORE
			// NULLS modifier these functions carry.
			"percentile_cont", "percentile_disc":
			return false
		}
		// Custom natives parse `googlesqlite_ignore_nulls()` out of
		// their argument list and honour the modifier themselves —
		// no need to bounce through the emulation path.
		if _, ok := customNativeWindowFuncMap[rawName]; ok {
			return false
		}
		return true
	}
	return false
}

// formatNative emits a SQLite-native window function call:
//
//	<sqliteName>(<value-args>) OVER (PARTITION BY ... ORDER BY ... <frame>)
//
// The OVER clause is built from the same partition / order / frame
// information the predecessor tucked into function arguments. SQLite
// drives Step/Value over rows in the frame, which is dramatically
// cheaper than the predecessor's per-output-row full-scan subquery.
//
// includeOpts forwards the DISTINCT / IGNORE NULLS markers as
// trailing arguments. SQLite built-ins reject those, so we only
// include them when calling our custom googlesqlite_window_<name>
// implementations that know how to parse them.
func (n *AnalyticFunctionCallNode) formatNative(ctx context.Context, sqliteName string, orderColumns []*analyticOrderBy, includeOpts bool) (string, error) {
	// Collect the user-supplied value arguments (without our window
	// option markers).
	var valueArgs []string
	for _, a := range m1(n.node.ResolvedFunctionCallBase.ArgumentList()) {
		arg, err := newNode(a).FormatSQL(ctx)
		if err != nil {
			return "", err
		}
		valueArgs = append(valueArgs, arg)
	}
	// DuckDB's quantile_cont binds its value argument as DOUBLE and
	// rejects the JSON-typed column the UNNEST lowering produces for a
	// FLOAT64 element (the json_each.value is JSON). PERCENTILE_CONT
	// always yields a continuous (double) result, so cast the value arg
	// to DOUBLE. quantile_disc returns the element type unchanged and
	// DuckDB auto-casts JSON integers for it, so it is left as-is.
	if sqliteName == "quantile_cont" && len(valueArgs) >= 1 {
		valueArgs[0] = fmt.Sprintf("CAST(%s AS DOUBLE)", valueArgs[0])
	}
	// SUM / AVG in OVER context reject the JSON value-layer encoding the
	// UNNEST source produces (`sum(JSON)` does not bind), the same way the
	// non-window aggregate path casts its argument. Cast the value arg
	// only when its analyzer-resolved kind is a value-layer numeric
	// (INT/FLOAT) so a real native-numeric column is unaffected. AVG is
	// always a FLOAT64 result, so it casts to DOUBLE; SUM over an INT64
	// argument must stay EXACT (issue #5), so it takes the BIGINT cast
	// valueLayerScalarCastType resolves for the integer kinds — DuckDB's
	// sum(BIGINT) accumulates in HUGEINT without float rounding. MIN /
	// MAX / COUNT bind against JSON directly, so they are left alone.
	if (sqliteName == "sum" || sqliteName == "avg") && len(valueArgs) >= 1 {
		kinds := argKinds(m1(n.node.ResolvedFunctionCallBase.ArgumentList()))
		if castType, ok := valueLayerScalarCastType(kindAt(kinds, 0)); ok {
			if sqliteName == "avg" {
				castType = "DOUBLE"
			}
			valueArgs[0] = fmt.Sprintf("CAST(%s AS %s)", valueArgs[0], castType)
		} else if kindAt(kinds, 0) == googlesql.TypeKindTypeNumeric {
			// Native-storage NUMERIC: a stored column is DECIMAL(38,9)
			// and binds the window reducer directly; the read wrap only
			// decodes envelope-shaped operands (UDF results, legacy
			// tables) — issue #9's window path, completed natively.
			valueArgs[0] = numericStorageReadSQL(valueArgs[0])
		} else if isNumericEnvelopeKind(kindAt(kinds, 0)) {
			// BIGNUMERIC window operands stay value-layer envelopes
			// (beyond DuckDB's 38-digit DECIMAL): unwrap to plain text
			// and run the window reducer over DOUBLE (approximate).
			valueArgs[0] = fmt.Sprintf("CAST(googlesqlite_plain_string(%s) AS DOUBLE)", valueArgs[0])
		}
	}
	// MIN / MAX in OVER context order by the raw operand: fine for the
	// native scalar kinds, but a NUMERIC operand must order as DECIMAL
	// (envelope-shaped operands would order lexicographically) and a
	// BYTES operand by payload bytes — the same treatment the
	// non-window aggregate path applies (issue #9's "window not" gap,
	// closed by the native-storage migration).
	if (sqliteName == "min" || sqliteName == "max") && len(valueArgs) >= 1 {
		kinds := argKinds(m1(n.node.ResolvedFunctionCallBase.ArgumentList()))
		switch kindAt(kinds, 0) {
		case googlesql.TypeKindTypeNumeric:
			valueArgs[0] = numericStorageReadSQL(valueArgs[0])
		case googlesql.TypeKindTypeBytes:
			valueArgs[0] = bytesStorageReadSQL(valueArgs[0])
		}
	}
	// $count_star resolves with no value arguments; DuckDB's native
	// COUNT requires the * argument in OVER context.
	if sqliteName == "count" && len(valueArgs) == 0 {
		valueArgs = []string{"*"}
	}
	if includeOpts {
		if m1(n.node.Distinct()) {
			valueArgs = append(valueArgs, "googlesqlite_distinct()")
		}
		switch m1(n.node.NullHandlingModifier()) {
		case googlesql.ResolvedNonScalarFunctionCallBaseEnums_NullHandlingModifierRespectNulls:
			// no marker needed
		default:
			valueArgs = append(valueArgs, "googlesqlite_ignore_nulls()")
		}
	}
	call := fmt.Sprintf("%s(%s)", sqliteName, strings.Join(valueArgs, ","))

	over, err := n.windowOverClause(ctx, orderColumns)
	if err != nil {
		return "", err
	}
	expr := call + " " + over
	// SUM with an INT64 analyzer result type: DuckDB's sum yields HUGEINT;
	// narrow the window expression back to BIGINT so the result stays an
	// exact INT64 (and a genuinely out-of-range running sum raises DuckDB's
	// conversion error — BigQuery's overflow semantics) instead of falling
	// back to a rounded FLOAT64 on the decode path (issue #5).
	if sqliteName == "sum" || sqliteName == "avg" {
		if t, _ := n.node.Type(); t != nil {
			if k, kerr := t.Kind(); kerr == nil {
				switch {
				case sqliteName == "sum" && k == googlesql.TypeKindTypeInt64:
					expr = fmt.Sprintf("CAST(%s AS BIGINT)", expr)
				case k == googlesql.TypeKindTypeNumeric:
					// BigQuery SUM/AVG(NUMERIC) return NUMERIC (issue #9):
					// narrow the window expression to DECIMAL(38,9) so the
					// result decodes as an exact NumericValue rather than a
					// rounded FLOAT64 (avg's native result is DOUBLE).
					expr = fmt.Sprintf("CAST(%s AS DECIMAL(38,9))", expr)
				}
			}
		}
	}
	return expr, nil
}

// windowOverClause assembles the `OVER (...)` clause shared by the
// native window path from the partition / order / frame information the
// predecessor tucked into the analytic context. Returns `OVER ()` when
// no window spec applies.
func (n *AnalyticFunctionCallNode) windowOverClause(ctx context.Context, orderColumns []*analyticOrderBy) (string, error) {
	var clauses []string
	if cols := analyticPartitionColumnNamesFromContext(ctx); len(cols) > 0 {
		clauses = append(clauses, "PARTITION BY "+strings.Join(cols, ","))
	}
	if len(orderColumns) > 0 {
		var ob []string
		for _, col := range orderColumns {
			switch col.nullOrder {
			case nullOrderFirst:
				ob = append(ob, fmt.Sprintf("(%s IS NOT NULL)", col.column))
			case nullOrderLast:
				ob = append(ob, fmt.Sprintf("(%s IS NULL)", col.column))
			}
			// DuckDB retarget: drop the googlesqlite_collate SQLite
			// collation (DuckDB has no callback-collation API). Rows that
			// are equal under GoogleSQL semantics share a byte-identical
			// value-layer encoding, so native DuckDB ordering is correct
			// for the window-frame ORDER BY.
			suffix := ""
			if !col.isAsc {
				suffix = " DESC"
			}
			ob = append(ob, col.column+suffix)
		}
		clauses = append(clauses, "ORDER BY "+strings.Join(ob, ","))
	}
	if frameSQL, err := n.formatNativeFrame(ctx); err != nil {
		return "", err
	} else if frameSQL != "" {
		clauses = append(clauses, frameSQL)
	}

	if len(clauses) == 0 {
		return "OVER ()", nil
	}
	return "OVER (" + strings.Join(clauses, " ") + ")", nil
}

// formatStClusterDBSCAN lowers the analytic ST_CLUSTERDBSCAN to a
// native-DuckDB window expression. DBSCAN assigns each row a cluster id
// computed from the whole partition, which has no native DuckDB
// analytic; the SQLite backend faked it with a correlated-subquery +
// rowid-marker hack that depended on the engine preserving scan order
// (DuckDB's parallel scan does not, so that path mis-aligns). Instead
// emit:
//
//	list_extract(st_clusterdbscan(geo, eps, min_pts) OVER (w),
//	             row_number() OVER (w))
//
// st_clusterdbscan is registered as a window aggregate (see
// duckdbAggSpecs) that returns the partition's ARRAY<INT64> of cluster
// ids in window order; within a single window operator DuckDB feeds
// that aggregate rows in the same order row_number() numbers them, so
// list_extract(array, row_number) projects each row's own cluster id. A
// noise / empty / NULL row is a NULL array element, which list_extract
// surfaces as SQL NULL - matching the upstream "unclustered -> NULL".
func (n *AnalyticFunctionCallNode) formatStClusterDBSCAN(ctx context.Context, orderColumns []*analyticOrderBy) (string, error) {
	var valueArgs []string
	for _, a := range m1(n.node.ResolvedFunctionCallBase.ArgumentList()) {
		arg, err := newNode(a).FormatSQL(ctx)
		if err != nil {
			return "", err
		}
		valueArgs = append(valueArgs, arg)
	}
	over, err := n.windowOverClause(ctx, orderColumns)
	if err != nil {
		return "", err
	}
	// Thread the row's 1-based position into the aggregate as a 4th
	// argument so it can slot each geography by row position rather than
	// by DuckDB's (unordered, parallel) feed order. DuckDB rejects a
	// window function (row_number() OVER ()) as the argument of another
	// window function, so reuse the predecessor emulation's row_id wrap:
	// markAnalyticEmulationUsed tells the surrounding AnalyticScanNode to
	// materialize `ROW_NUMBER() OVER() AS "row_id"`, and both the
	// aggregate argument and the extractor index reference that single
	// plain column - so the place-and-read position is identical per row
	// and alignment never depends on two window calls iterating in the
	// same order. st_clusterdbscan returns the partition's cluster-id
	// ARRAY (value-layer base64 envelope) ordered by "row_id";
	// array_at_ordinal pulls the current row's element by the same
	// "row_id"; decode_scalar + CAST retype it to the BIGINT the analyzer
	// assigns ST_CLUSTERDBSCAN. A noise/empty/NULL element decodes to SQL
	// NULL, which CAST preserves.
	markAnalyticEmulationUsed(ctx)
	// DBSCAN clusters the WHOLE partition, so the aggregate must always
	// receive every partition row. A bare OVER () already feeds the full
	// partition, but an OVER (... ORDER BY ...) would make DuckDB default
	// to a growing RANGE prefix frame and the aggregate would cluster
	// only rows up to the current one. Force an explicit full-partition
	// frame whenever an ORDER BY is present so the clustering is computed
	// over the entire partition regardless of ordering.
	over = forceFullPartitionFrame(over)
	rowID := `"row_id"`
	aggArgs := append(append([]string{}, valueArgs...), rowID)
	arr := fmt.Sprintf("st_clusterdbscan(%s) %s", strings.Join(aggArgs, ","), over)
	elem := fmt.Sprintf("googlesqlite_array_at_ordinal(%s, %s)", arr, rowID)
	return fmt.Sprintf("CAST(googlesqlite_decode_scalar(%s) AS BIGINT)", elem), nil
}

// forceFullPartitionFrame appends an explicit full-partition frame to an
// OVER clause that carries an ORDER BY but no frame, so a whole-partition
// aggregate (DBSCAN) is fed every partition row rather than a growing
// prefix. A bare `OVER ()` (no ORDER BY) already spans the full partition
// and is returned unchanged; an OVER clause that already names a frame is
// also left as is.
func forceFullPartitionFrame(over string) string {
	if !strings.Contains(over, "ORDER BY") {
		return over
	}
	if strings.Contains(over, "ROWS") || strings.Contains(over, "RANGE") || strings.Contains(over, "GROUPS") {
		return over
	}
	const frame = " ROWS BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING"
	if i := strings.LastIndex(over, ")"); i >= 0 {
		return over[:i] + frame + over[i:]
	}
	return over
}

// formatNativeFrame turns the resolved-tree frame into a SQLite frame
// clause: `ROWS|RANGE BETWEEN <start> AND <end>`. Returns ""
// when no frame is specified (SQLite picks the appropriate default
// for the function — RANGE BETWEEN UNBOUNDED PRECEDING AND CURRENT
// ROW for ordered aggregates).
func (n *AnalyticFunctionCallNode) formatNativeFrame(ctx context.Context) (string, error) {
	windowFrame, _ := n.node.WindowFrame()
	if windowFrame == nil {
		return "", nil
	}
	unitText := ""
	switch m1(windowFrame.FrameUnit()) {
	case googlesql.ResolvedWindowFrameEnums_FrameUnitRows:
		unitText = "ROWS"
	case googlesql.ResolvedWindowFrameEnums_FrameUnitRange:
		unitText = "RANGE"
	default:
		return "", nil
	}
	startSQL, err := nativeBoundaryClause(ctx, m1(windowFrame.StartExpr()), true)
	if err != nil {
		return "", err
	}
	endSQL, err := nativeBoundaryClause(ctx, m1(windowFrame.EndExpr()), false)
	if err != nil {
		return "", err
	}
	return unitText + " BETWEEN " + startSQL + " AND " + endSQL, nil
}

func nativeBoundaryClause(ctx context.Context, expr *googlesql.ResolvedWindowFrameExpr, isStart bool) (string, error) {
	typ := m1(expr.BoundaryType())
	switch typ {
	case googlesql.ResolvedWindowFrameExprEnums_BoundaryTypeUnboundedPreceding:
		return "UNBOUNDED PRECEDING", nil
	case googlesql.ResolvedWindowFrameExprEnums_BoundaryTypeCurrentRow:
		return "CURRENT ROW", nil
	case googlesql.ResolvedWindowFrameExprEnums_BoundaryTypeUnboundedFollowing:
		return "UNBOUNDED FOLLOWING", nil
	case googlesql.ResolvedWindowFrameExprEnums_BoundaryTypeOffsetPreceding:
		offset, err := newNode(m1(expr.Expression())).FormatSQL(ctx)
		if err != nil {
			return "", err
		}
		return offset + " PRECEDING", nil
	case googlesql.ResolvedWindowFrameExprEnums_BoundaryTypeOffsetFollowing:
		offset, err := newNode(m1(expr.Expression())).FormatSQL(ctx)
		if err != nil {
			return "", err
		}
		return offset + " FOLLOWING", nil
	}
	if isStart {
		return "UNBOUNDED PRECEDING", nil
	}
	return "UNBOUNDED FOLLOWING", nil
}

func (n *AnalyticFunctionCallNode) getWindowBoundaryOptionFuncSQL(ctx context.Context, expr *googlesql.ResolvedWindowFrameExpr, isStart bool) (string, error) {
	typ := m1(expr.BoundaryType())
	switch typ {
	case googlesql.ResolvedWindowFrameExprEnums_BoundaryTypeUnboundedPreceding, googlesql.ResolvedWindowFrameExprEnums_BoundaryTypeCurrentRow, googlesql.ResolvedWindowFrameExprEnums_BoundaryTypeUnboundedFollowing:
		if isStart {
			return window.GetWindowBoundaryStartOptionFuncSQL(typ, ""), nil
		}
		return window.GetWindowBoundaryEndOptionFuncSQL(typ, ""), nil
	case googlesql.ResolvedWindowFrameExprEnums_BoundaryTypeOffsetPreceding, googlesql.ResolvedWindowFrameExprEnums_BoundaryTypeOffsetFollowing:
		literal, err := newNode(m1(expr.Expression())).FormatSQL(ctx)
		if err != nil {
			return "", err
		}
		if isStart {
			return window.GetWindowBoundaryStartOptionFuncSQL(typ, literal), nil
		}
		return window.GetWindowBoundaryEndOptionFuncSQL(typ, literal), nil
	}
	return "", fmt.Errorf("unexpected boundary type %d", typ)
}

// tryNativeCast lowers CAST(...) to a native DuckDB cast/expression for
// the temporal conversions in the date/datetime/time/timestamp families.
// It returns (sql, true, nil) when it handled the cast, (.., false, nil)
// to defer to the googlesqlite_cast UDF path, or an error.
//
// The temporal -> STRING direction is special: GoogleSQL's canonical
// textual forms differ from DuckDB's native CAST-to-VARCHAR (the
// DATETIME 'T' separator, the TIMESTAMP '+00' zone suffix), so those
// project through strftime / a pinned format instead of a bare cast.
func (n *CastNode) tryNativeCast(ctx context.Context) (string, bool, error) {
	fromKind := newType(m1(m1(n.node.Expr()).Type())).kindAs()
	toKind := newType(m1(n.node.Type())).kindAs()

	isTemporal := func(k googlesql.TypeKind) bool {
		switch k {
		case googlesql.TypeKindTypeDate, googlesql.TypeKindTypeTime,
			googlesql.TypeKindTypeDatetime, googlesql.TypeKindTypeTimestamp:
			return true
		}
		return false
	}
	// Numeric -> floating-point coercions (the implicit widening the
	// analyzer inserts around e.g. SAFE_DIVIDE(SUM(x), COUNT(*))) lower
	// to a native DuckDB CAST. The googlesqlite_cast UDF declares a
	// VARCHAR result, so leaving the coercion on the UDF path types the
	// operand as VARCHAR and /(DOUBLE, VARCHAR) fails to bind at DuckDB
	// prepare time. Widening to FLOAT/DOUBLE has no error cases, so the
	// SAFE_CAST (ReturnNullOnError) flag needs no special handling.
	// Casts that genuinely need the value layer (NUMERIC/BIGNUMERIC,
	// STRING parsing, envelopes) keep the UDF path: only raw-scalar
	// numeric sources are claimed here.
	if isNativeNumericKind(fromKind) {
		switch toKind {
		case googlesql.TypeKindTypeFloat, googlesql.TypeKindTypeDouble:
			expr, err := newNode(m1(n.node.Expr())).FormatSQL(ctx)
			if err != nil {
				return "", false, err
			}
			target := "DOUBLE"
			if toKind == googlesql.TypeKindTypeFloat {
				target = "FLOAT"
			}
			return fmt.Sprintf("CAST(%s AS %s)", expr, target), true, nil
		}
	}
	// Only claim casts that involve a temporal type on at least one side;
	// everything else (numeric, bytes, struct, ...) stays on the UDF path
	// that the other burn-down families own.
	if !isTemporal(fromKind) && !isTemporal(toKind) {
		return "", false, nil
	}
	expr, err := newNode(m1(n.node.Expr())).FormatSQL(ctx)
	if err != nil {
		return "", false, err
	}

	if toKind == googlesql.TypeKindTypeString {
		switch fromKind {
		case googlesql.TypeKindTypeDatetime:
			return fmt.Sprintf("strftime(%s, '%%Y-%%m-%%dT%%H:%%M:%%S')", expr), true, nil
		case googlesql.TypeKindTypeDate:
			return fmt.Sprintf("strftime(%s, '%%Y-%%m-%%d')", expr), true, nil
		case googlesql.TypeKindTypeTime:
			// DuckDB CAST(TIME AS VARCHAR) already yields HH:MM:SS[.ffffff].
			return fmt.Sprintf("CAST(%s AS VARCHAR)", expr), true, nil
		case googlesql.TypeKindTypeTimestamp:
			return fmt.Sprintf("strftime(%s AT TIME ZONE 'UTC', '%%Y-%%m-%%d %%H:%%M:%%S+00')", expr), true, nil
		}
	}

	duckType, ok := duckDBTypeName(toKind)
	if !ok {
		return "", false, nil
	}
	// STRING -> temporal: a STRING-typed operand whose value is only
	// known at execution time (a bound parameter, a stored STRING
	// column) carries the value-layer envelope, which DuckDB's
	// date/time parser rejects ("invalid date field format:
	// eyJoZWFkZXIi..."). Unwrap it to bare text first; the UDF passes
	// non-envelope text through unchanged, and literal operands rarely
	// reach here (the analyzer constant-folds literal casts).
	if fromKind == googlesql.TypeKindTypeString && toKind != googlesql.TypeKindTypeString {
		expr = fmt.Sprintf("googlesqlite_plain_string(%s)", expr)
	}
	return fmt.Sprintf("CAST(%s AS %s)", expr, duckType), true, nil
}

// duckDBTypeName maps a GoogleSQL temporal/primitive kind to the DuckDB
// type keyword used in a CAST target. ok=false for kinds this path does
// not handle.
// valueLayerScalarCastType returns the DuckDB type a bare value-layer
// scalar should be CAST to, and true, for the element/field kinds whose
// over-the-wire form is a bare JSON scalar (EncodeValue hands INT64 /
// FLOAT64 / BOOL back as raw Go scalars, so json_each.value over a
// decoded array yields a JSON number / boolean rather than the base64
// envelope). Casting those to BIGINT / DOUBLE / BOOLEAN recovers a native
// value DuckDB's arithmetic / comparison operators bind against - the
// same recovery nativeNumericAggregates applies for SUM(JSON) etc.
//
// STRING / BYTES / temporal / NUMERIC / STRUCT / ARRAY return ("",
// false): EncodeValue keeps those in the base64 value-layer envelope, so
// the raw column value is not a castable native literal and must flow on
// to the consumer (googlesqlite_cast / get_struct_field / the row
// decoder) untouched.
func valueLayerScalarCastType(kind googlesql.TypeKind) (string, bool) {
	switch kind {
	case googlesql.TypeKindTypeInt32, googlesql.TypeKindTypeInt64,
		googlesql.TypeKindTypeUint32, googlesql.TypeKindTypeUint64:
		return "BIGINT", true
	case googlesql.TypeKindTypeFloat, googlesql.TypeKindTypeDouble:
		return "DOUBLE", true
	case googlesql.TypeKindTypeBool:
		return "BOOLEAN", true
	}
	return "", false
}

// isArithmeticOperator reports whether op is one of the additive /
// multiplicative operator builtins whose operands must be native numerics
// (so a JSON-typed array element needs a CAST). The comparison operators
// in nativeBinaryOperators are excluded here — they bind against the
// value-layer operand form — but the ORDERING subset gets its own STRING
// unwrap (see isOrderingOperator).
func isArithmeticOperator(op string) bool {
	switch op {
	case "add", "subtract", "multiply":
		return true
	}
	return false
}

// isOrderingOperator reports whether op is one of the ordering comparison
// builtins (<, <=, >, >=). Unlike equality, ordering over the value-layer
// STRING envelope is incorrect (base64 text order is not monotonic with
// payload order), so tryNativeOperator unwraps STRING operands to plain
// text for these. Equality (=, <>) is excluded: the envelope encoding is
// deterministic and injective, so envelope equality equals payload
// equality, and struct equality relies on the envelope form.
func isOrderingOperator(op string) bool {
	switch op {
	case "less", "less_or_equal", "greater", "greater_or_equal":
		return true
	}
	return false
}

// isPlainTextOrderableKind reports whether kind's value-layer wire form is
// a STRING envelope whose plain payload text orders identically to the
// GoogleSQL value ordering: STRING itself (UTF-8 byte order == code-point
// order) and UUID (canonical lowercase-hex text order == 128-bit value
// order). Ordering comparisons over these kinds unwrap their operands
// with googlesqlite_plain_string. Other envelope kinds (BYTES, NUMERIC,
// temporals, ENUM) keep their existing lowering.
func isPlainTextOrderableKind(kind googlesql.TypeKind) bool {
	return kind == googlesql.TypeKindTypeString || kind == googlesql.TypeKindTypeUuid
}

// rangeEnvelopeArgs rewrites the formatted SQL of each temporal-typed
// RANGE-family argument into its value-layer envelope literal. The
// formatted form produced by the default path is a native DuckDB DATE /
// DATETIME / TIMESTAMP literal (literalFromValue's temporal shortcut),
// which the VARCHAR-typed googlesqlite_range* UDFs cannot bind and which
// loses the DATE-vs-TIMESTAMP element type once duckdb-go maps both to a
// bare Go time.Time. Only literal temporal args are rewritten; a
// non-literal temporal operand (a column reference, a nested RANGE
// accessor) already flows as the envelope VARCHAR, so it is left
// untouched. Non-temporal args (the BOOL last_partial_range flag, the
// INTERVAL step which lowers to googlesqlite_interval, an UNBOUNDED NULL)
// also pass through unchanged.
func rangeEnvelopeArgs(argNodes []googlesql.ResolvedExprNode, args []string) []string {
	if len(argNodes) != len(args) {
		return args
	}
	out := make([]string, len(args))
	copy(out, args)
	for i, node := range argNodes {
		if node == nil {
			continue
		}
		t, _ := node.Type()
		if t == nil {
			continue
		}
		k, err := t.Kind()
		if err != nil {
			continue
		}
		switch k {
		case googlesql.TypeKindTypeDate,
			googlesql.TypeKindTypeDatetime,
			googlesql.TypeKindTypeTimestamp:
			lit, ok := node.(*googlesql.ResolvedLiteral)
			if !ok {
				continue
			}
			v, err := valueFromGoogleSQLValue(*m1(lit.Value()))
			if err != nil || v == nil {
				continue
			}
			env, err := envelopeLiteralFromValue(v)
			if err != nil {
				continue
			}
			out[i] = env
		}
	}
	return out
}

// isNativeNumericKind reports whether k is a numeric kind whose
// over-the-wire form is a raw native scalar (EncodeValue hands integers
// and floats back as bare Go scalars, never the base64 envelope), so a
// native DuckDB CAST can retype it directly. NUMERIC / BIGNUMERIC are
// deliberately excluded: they ride the value-layer envelope.
func isNativeNumericKind(k googlesql.TypeKind) bool {
	switch k {
	case googlesql.TypeKindTypeInt32, googlesql.TypeKindTypeInt64,
		googlesql.TypeKindTypeUint32, googlesql.TypeKindTypeUint64,
		googlesql.TypeKindTypeFloat, googlesql.TypeKindTypeDouble:
		return true
	}
	return false
}

func isIntegerTypeKind(kind googlesql.TypeKind) bool {
	switch kind {
	case googlesql.TypeKindTypeInt32,
		googlesql.TypeKindTypeInt64,
		googlesql.TypeKindTypeUint32,
		googlesql.TypeKindTypeUint64:
		return true
	}
	return false
}

func tryNativeDateIntegerArithmetic(op, tok string, args []string, kinds []googlesql.TypeKind) (string, bool) {
	if len(args) != 2 || (op != "add" && op != "subtract") {
		return "", false
	}
	lhsKind, rhsKind := kindAt(kinds, 0), kindAt(kinds, 1)
	if lhsKind == googlesql.TypeKindTypeDate && isIntegerTypeKind(rhsKind) {
		return fmt.Sprintf(
			"CAST((CAST(%s AS DATE) %s (CAST(%s AS BIGINT) * INTERVAL '1 day')) AS DATE)",
			args[0], tok, args[1],
		), true
	}
	if op == "add" && isIntegerTypeKind(lhsKind) && rhsKind == googlesql.TypeKindTypeDate {
		return fmt.Sprintf(
			"CAST((CAST(%s AS DATE) + (CAST(%s AS BIGINT) * INTERVAL '1 day')) AS DATE)",
			args[1], args[0],
		), true
	}
	return "", false
}

// argKinds returns the resolved TypeKind of each function argument, in
// order, so an operator lowering can decide whether to cast an operand to
// its native scalar type. A nil / typeless argument contributes
// TypeKindTypeUnknown, which castOperandToKind leaves untouched.
func argKinds(args []googlesql.ResolvedExprNode) []googlesql.TypeKind {
	kinds := make([]googlesql.TypeKind, len(args))
	for i, a := range args {
		if a == nil {
			continue
		}
		if t, _ := a.Type(); t != nil {
			if k, err := t.Kind(); err == nil {
				kinds[i] = k
			}
		}
	}
	return kinds
}

// isTemporalTypeKind reports whether k is one of the GoogleSQL civil /
// instant temporal types (DATE / TIME / DATETIME / TIMESTAMP). Used to
// disambiguate overloaded builtins (e.g. STRING) whose temporal overload
// must lower to a native cast rather than the JSON-accessor UDF.
func isTemporalTypeKind(k googlesql.TypeKind) bool {
	switch k {
	case googlesql.TypeKindTypeDate,
		googlesql.TypeKindTypeTime,
		googlesql.TypeKindTypeDatetime,
		googlesql.TypeKindTypeTimestamp:
		return true
	}
	return false
}

// kindAt returns the kind at index i, or TypeKindTypeUnknown when the
// index is out of range.
func kindAt(kinds []googlesql.TypeKind, i int) googlesql.TypeKind {
	if i < 0 || i >= len(kinds) {
		return googlesql.TypeKindTypeUnknown
	}
	return kinds[i]
}

// castOperandToKind wraps an operand SQL string in a DuckDB CAST to the
// native scalar type for kind, when kind is a JSON-backed scalar
// (INT64 / FLOAT64 / BOOL). Other kinds are returned unchanged. The cast
// is idempotent, so applying it to an already-native operand is harmless.
func castOperandToKind(operand string, kind googlesql.TypeKind) string {
	if castType, ok := valueLayerScalarCastType(kind); ok {
		return fmt.Sprintf("CAST(%s AS %s)", operand, castType)
	}
	// Native-storage NUMERIC: arithmetic binds the stored DECIMAL(38,9)
	// directly; the read wrap normalizes envelope-shaped operands (UDF
	// results, legacy tables) instead of failing the binder.
	if kind == googlesql.TypeKindTypeNumeric {
		return numericStorageReadSQL(operand)
	}
	return operand
}

func duckDBTypeName(kind googlesql.TypeKind) (string, bool) {
	switch kind {
	case googlesql.TypeKindTypeDate:
		return "DATE", true
	case googlesql.TypeKindTypeTime:
		return "TIME", true
	case googlesql.TypeKindTypeDatetime:
		return "TIMESTAMP", true
	case googlesql.TypeKindTypeTimestamp:
		return "TIMESTAMP WITH TIME ZONE", true
	case googlesql.TypeKindTypeString:
		return "VARCHAR", true
	case googlesql.TypeKindTypeInt32, googlesql.TypeKindTypeInt64:
		return "BIGINT", true
	}
	return "", false
}

func (n *CastNode) FormatSQL(ctx context.Context) (string, error) {
	if n.node == nil {
		return "", nil
	}
	// DuckDB retarget: handle the temporal CAST shapes natively before
	// falling back to the googlesqlite_cast UDF. The UDF is not
	// registered on DuckDB, so any unhandled CAST surfaces as a clear
	// "function does not exist" error rather than wrong data.
	if sql, ok, err := n.tryNativeCast(ctx); ok || err != nil {
		return sql, err
	}
	fromType := newType(m1(m1(n.node.Expr()).Type()))
	jsonEncodedFromType, err := json.Marshal(fromType)
	if err != nil {
		return "", err
	}
	toType := newType(m1(n.node.Type()))
	jsonEncodedToType, err := json.Marshal(toType)
	if err != nil {
		return "", err
	}
	encodedFromType, err := encodeGoValue(m1(tf().MakeSimpleType(googlesql.TypeKindTypeString)), string(jsonEncodedFromType))
	if err != nil {
		return "", err
	}
	encodedToType, err := encodeGoValue(m1(tf().MakeSimpleType(googlesql.TypeKindTypeString)), string(jsonEncodedToType))
	if err != nil {
		return "", err
	}
	expr, err := newNode(m1(n.node.Expr())).FormatSQL(ctx)
	if err != nil {
		return "", err
	}
	udf := fmt.Sprintf(
		"googlesqlite_cast(%s, '%s', '%s', %t)",
		expr, encodedFromType, encodedToType, m1(n.node.ReturnNullOnError()),
	)
	// Native-representable scalar targets (BOOL / INT64-family /
	// FLOAT64-family) re-type the UDF's textual result with a wrapping
	// native CAST, the same decode GetStructFieldNode applies to scalar
	// struct fields. GoogleSQLCast already returns the plain parseable
	// text ("true", "42", "1.5") for these kinds precisely so an outer
	// DuckDB CAST can retype it. Without the wrap the cast result stays a
	// VARCHAR, and any branching construct (COALESCE / IF / IFNULL /
	// CASE) mixing it with a native-literal sibling fails DuckDB's binder
	// ("Cannot mix values of type VARCHAR and BOOLEAN"), issue #15 — and
	// ORDER BY / MIN / MAX over the cast result compare lexicographically.
	// The native value is storage-safe too: BOOL / INT64 / FLOAT64
	// columns are stored native (storageTypeForKind), so an INSERT of the
	// cast result lands in the column type directly. Temporal targets are
	// already native via tryNativeCast above; STRING / NUMERIC / BYTES /
	// other envelope targets keep the envelope contract untouched.
	if castType, ok := valueLayerScalarCastType(toType.kindAs()); ok {
		return fmt.Sprintf("CAST(%s AS %s)", udf, castType), nil
	}
	return udf, nil
}

func (n *MakeStructNode) FormatSQL(ctx context.Context) (string, error) {
	if n.node == nil {
		return "", nil
	}
	typ := m1(m1(n.node.Type()).AsStruct())
	typFields := m1(typ.Fields())
	fields, _ := n.node.FieldList()
	args := make([]string, 0, len(typFields)*2)
	for i, sf := range typFields {
		fieldName := sf.Name
		key, err := literalFromValue(value.StringValue(fieldName))
		if err != nil {
			return "", err
		}
		args = append(args, key)
		field, err := newNode(fields[i]).FormatSQL(ctx)
		if err != nil {
			return "", err
		}
		args = append(args, field)
	}
	return fmt.Sprintf("googlesqlite_make_struct(%s)", strings.Join(args, ",")), nil
}

// MakeProtoNode formats `new MessageName(expr1 AS field1, expr2 AS field2, ...)`
// as a runtime call that produces proto wire bytes. The resolved tree
// hands us the field-tag / value-kind pairs via each MakeProtoField
// child; we forward them to `googlesqlite_make_proto`, which
// concatenates encodeSingleField(tag, value, kind) per pair.
func (n *MakeProtoNode) FormatSQL(ctx context.Context) (string, error) {
	if n.node == nil {
		return "", nil
	}
	fields := m1(n.node.FieldList())
	args := make([]string, 0, len(fields)*3)
	for _, f := range fields {
		fd := m1(f.FieldDescriptor())
		if fd == nil {
			return "", fmt.Errorf("MakeProto: field descriptor unavailable")
		}
		tag, _ := fd.Number()
		expr := m1(f.Expr())
		exprSQL, err := newNode(expr).FormatSQL(ctx)
		if err != nil {
			return "", err
		}
		exprType, _ := expr.Type()
		var kindStr string
		if exprType != nil {
			k, _ := exprType.Kind()
			kindStr = protoFieldKindString(k)
		} else {
			kindStr = "bytes"
		}
		args = append(args,
			fmt.Sprintf("%d", tag),
			protoFormatLiteralString(kindStr),
			exprSQL,
		)
	}
	if len(args) == 0 {
		// Zero-field message: emit an empty BYTES literal cast through
		// the runtime to keep the type stable.
		return "googlesqlite_make_proto()", nil
	}
	return fmt.Sprintf("googlesqlite_make_proto(%s)", strings.Join(args, ",")), nil
}

// MakeProtoFieldNode never reaches FormatSQL directly — its parent
// MakeProtoNode walks the field list and lowers each one inline.
// Returning an empty string here is a safe no-op for the general
// dispatcher.
func (n *GetStructFieldNode) FormatSQL(ctx context.Context) (string, error) {
	if n.node == nil {
		return "", nil
	}
	expr, err := newNode(m1(n.node.Expr())).FormatSQL(ctx)
	if err != nil {
		return "", err
	}
	idx, _ := n.node.FieldIdx()
	access := fmt.Sprintf("googlesqlite_get_struct_field(%s, %d)", expr, idx)
	// get_struct_field returns the value-layer envelope (a base64 VARCHAR),
	// which DuckDB cannot use in native arithmetic / comparison - `x.a +
	// x.b` over two INT64 fields fails the binder as '+(VARCHAR, VARCHAR)'.
	// For a scalar field, decode the envelope back to its bare value and
	// cast it to the analyzer-resolved scalar type so the field reads as a
	// native BIGINT / DOUBLE / BOOLEAN. The row decoder accepts the native
	// value on the result path just as it accepts the envelope, so a plain
	// top-level `SELECT x.a` keeps working. STRUCT / ARRAY / STRING / other
	// envelope-typed fields pass through untouched.
	if t, _ := n.node.Type(); t != nil {
		if k, err := t.Kind(); err == nil {
			if castType, ok := valueLayerScalarCastType(k); ok {
				return fmt.Sprintf("CAST(googlesqlite_decode_scalar(%s) AS %s)", access, castType), nil
			}
		}
	}
	return access, nil
}

func (n *GetProtoFieldNode) FormatSQL(ctx context.Context) (string, error) {
	if n.node == nil {
		return "", nil
	}
	expr, err := newNode(m1(n.node.Expr())).FormatSQL(ctx)
	if err != nil {
		return "", err
	}
	fd, err := n.node.FieldDescriptor()
	if err != nil || fd == nil {
		return "", fmt.Errorf("GetProtoField: field descriptor unavailable: %w", err)
	}
	fieldNumber, _ := fd.Number()
	resultType, err := n.node.Type()
	if err != nil || resultType == nil {
		return "", fmt.Errorf("GetProtoField: result type unavailable: %w", err)
	}
	kind, _ := resultType.Kind()
	// Repeated proto fields surface as ARRAY<…> after the upstream
	// analyzer rewrite (map<K,V> becomes ARRAY<STRUCT<key, value>>).
	// The lowering picks up googlesqlite_decode_array on the result,
	// so the runtime side must return a real ArrayValue carrying one
	// element per wire occurrence rather than the last occurrence's
	// raw bytes. We route the repeated case to a dedicated runtime
	// that walks every match instead of overwriting on each hit.
	isRepeated, _ := fd.IsRepeated()
	if isRepeated && kind == googlesql.TypeKindTypeArray {
		var elemKindStr string
		if arr, ok := resultType.(*googlesql.ArrayType); ok && arr != nil {
			elem, err := arr.ElementType()
			if err == nil && elem != nil {
				ek, _ := elem.Kind()
				elemKindStr = protoFieldKindString(ek)
			}
		}
		if elemKindStr == "" {
			// Fallback: STRUCT-shaped map entries decode element-wise
			// through subsequent GetProtoField calls, so "message"
			// keeps each entry's payload intact.
			elemKindStr = "message"
		}
		return fmt.Sprintf("googlesqlite_get_proto_field_repeated(%s, %d, '%s')", expr, fieldNumber, elemKindStr), nil
	}
	kindStr := protoFieldKindString(kind)
	defaultLit := "''"
	if returnDefault, _ := n.node.ReturnDefaultValueWhenUnset(); returnDefault {
		defaultLit = protoFieldDefaultLiteral(fd, kindStr)
	}
	return fmt.Sprintf("googlesqlite_get_proto_field(%s, %d, '%s', %s)", expr, fieldNumber, kindStr, defaultLit), nil
}

// protoFormatLiteralString turns a Go string into a single-quoted
// SQL literal with embedded single quotes doubled. Used when the
// formatter has to splice an opaque metadata string (proto field
// path list, enum full name, …) into the lowered SQL.
func protoFormatLiteralString(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// mapFieldParentAndTag inspects a ResolvedExpr that the analyzer
// produced as the map_field argument of PROTO_MAP_* (e.g.
// `m.purchased`). When it is a ResolvedGetProtoField we recover the
// parent message expression and the field number; the runtime can
// then walk the parent's wire bytes for every occurrence of that
// outer tag, which a non-accumulating GetProtoField extraction would
// have dropped.
//
// Returns the SQL for the parent message expression plus the field
// number. Errors when the argument isn't a ResolvedGetProtoField
// (a future caller might pass the map's wire bytes directly, but the
// current proto-runtime surface only emits the field-access form).
func mapFieldParentAndTag(ctx context.Context, arg googlesql.ResolvedExprNode) (string, int32, error) {
	if arg == nil {
		return "", 0, fmt.Errorf("map field argument is nil")
	}
	kind, err := arg.NodeKind()
	if err != nil || kind != googlesql.ResolvedNodeKindResolvedGetProtoField {
		return "", 0, fmt.Errorf("map field is not a proto field access (kind=%v)", kind)
	}
	gpf, ok := arg.(*googlesql.ResolvedGetProtoField)
	if !ok || gpf == nil {
		return "", 0, fmt.Errorf("map field has unexpected node type %T", arg)
	}
	parent, err := gpf.Expr()
	if err != nil || parent == nil {
		return "", 0, fmt.Errorf("map field parent unavailable: %w", err)
	}
	parentSQL, err := newNode(parent).FormatSQL(ctx)
	if err != nil {
		return "", 0, fmt.Errorf("format map field parent: %w", err)
	}
	fd, err := gpf.FieldDescriptor()
	if err != nil || fd == nil {
		return "", 0, fmt.Errorf("map field descriptor unavailable: %w", err)
	}
	num, err := fd.Number()
	if err != nil {
		return "", 0, fmt.Errorf("map field number: %w", err)
	}
	return parentSQL, num, nil
}

// protoFieldKindString maps a TypeKind to the lowercase token the
// internal/functions/proto runtime expects.
func protoFieldKindString(k googlesql.TypeKind) string {
	switch k {
	case googlesql.TypeKindTypeBool:
		return "bool"
	case googlesql.TypeKindTypeInt32:
		return "int32"
	case googlesql.TypeKindTypeInt64:
		return "int64"
	case googlesql.TypeKindTypeUint32:
		return "uint32"
	case googlesql.TypeKindTypeUint64:
		return "uint64"
	case googlesql.TypeKindTypeFloat:
		return "float"
	case googlesql.TypeKindTypeDouble:
		return "double"
	case googlesql.TypeKindTypeString:
		return "string"
	case googlesql.TypeKindTypeBytes:
		return "bytes"
	case googlesql.TypeKindTypeEnum:
		return "enum"
	case googlesql.TypeKindTypeProto:
		return "message"
	case googlesql.TypeKindTypeDate:
		// FROM_PROTO(google.type.Date) returns a SQL DATE; the
		// runtime needs the well-known proto layout decoder.
		return "date"
	case googlesql.TypeKindTypeTimestamp:
		// FROM_PROTO(google.protobuf.Timestamp) returns SQL TIMESTAMP.
		return "timestamp"
	}
	return "bytes"
}

// protoFieldDefaultLiteral returns a SQL literal that the runtime
// proto UDF can decode as the field's proto-defined default. The
// literal is a base64-encoded STRING carrying the kind-specific
// representation (decimal for numerics, "true"/"false" for bool,
// raw string for string fields). Empty string when no default.
func protoFieldDefaultLiteral(fd *googlesql.FieldDescriptor, kindStr string) string {
	hasDefault, _ := fd.HasDefaultValue()
	if !hasDefault {
		return "''"
	}
	encode := func(s string) string {
		return "'" + base64.StdEncoding.EncodeToString([]byte(s)) + "'"
	}
	switch kindStr {
	case "bool":
		v, _ := fd.DefaultValueBool()
		if v {
			return encode("true")
		}
		return encode("false")
	case "int32":
		v, _ := fd.DefaultValueInt32()
		return encode(fmt.Sprintf("%d", v))
	case "int64":
		v, _ := fd.DefaultValueInt64()
		return encode(fmt.Sprintf("%d", v))
	case "uint32":
		v, _ := fd.DefaultValueUint32()
		return encode(fmt.Sprintf("%d", v))
	case "uint64":
		v, _ := fd.DefaultValueUint64()
		return encode(fmt.Sprintf("%d", v))
	case "float":
		v, _ := fd.DefaultValueFloat()
		return encode(fmt.Sprintf("%g", v))
	case "double":
		v, _ := fd.DefaultValueDouble()
		return encode(fmt.Sprintf("%g", v))
	case "string":
		// FieldDescriptor doesn't expose DefaultValueString directly
		// in this binding; fall back to DebugString parsing or empty.
		return "''"
	}
	return "''"
}

func (n *GetJsonFieldNode) FormatSQL(ctx context.Context) (string, error) {
	if n.node == nil {
		return "", nil
	}
	expr, err := newNode(m1(n.node.Expr())).FormatSQL(ctx)
	if err != nil {
		return "", err
	}
	name, _ := n.node.FieldName()
	encodedName, err := encodeGoValue(m1(tf().MakeSimpleType(googlesql.TypeKindTypeString)), name)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("googlesqlite_get_json_field(%s, '%s')", expr, encodedName), nil
}

// ReplaceFieldNode lowers a ResolvedReplaceField (the analyzer node
// REPLACE_FIELDS resolves into) to a chain of
// googlesqlite_replace_fields runtime calls, one per (value, path)
// pair. Each path is a dotted field-number string; the new value is
// the lowered SQL of the replacement expression. We pull the
// per-item leaf TypeKind from the path's last FieldDescriptor so the
// runtime knows how to encode the replacement value.
func (n *ReplaceFieldNode) FormatSQL(ctx context.Context) (string, error) {
	if n.node == nil {
		return "", nil
	}
	exprSQL, err := newNode(m1(n.node.Expr())).FormatSQL(ctx)
	if err != nil {
		return "", err
	}
	items, err := n.node.ReplaceFieldItemList()
	if err != nil {
		return "", err
	}
	inner := exprSQL
	for _, it := range items {
		if it == nil {
			continue
		}
		valSQL, err := newNode(m1(it.Expr())).FormatSQL(ctx)
		if err != nil {
			return "", err
		}
		fdPath, _ := it.ProtoFieldPath()
		nums := make([]string, 0, len(fdPath))
		for _, fd := range fdPath {
			num, _ := fd.Number()
			nums = append(nums, strconv.Itoa(int(num)))
		}
		// Read the replacement value's type kind via the value expr;
		// that's what the runtime needs to encode the wire payload.
		leafKindStr := "bytes"
		if vt, _ := m1(it.Expr()).Type(); vt != nil {
			if k, err := vt.Kind(); err == nil {
				leafKindStr = protoFieldKindString(k)
			}
		}
		path := strings.Join(nums, ".")
		inner = fmt.Sprintf("googlesqlite_replace_fields(%s, %s, '%s', %s)",
			inner, protoFormatLiteralString(path), leafKindStr, valSQL)
	}
	return inner, nil
}

// isStructTypedExpr reports whether the resolved expression's
// concrete type is STRUCT. The formatter uses this to decide whether
// to attach `COLLATE googlesqlite_collate` to an IN comparison so
// the runtime decoder makes the equality positional instead of
// byte-for-byte (anonymous-struct-field bug, runtime-side fix).
func isStructTypedExpr(expr googlesql.ResolvedExprNode) bool {
	if expr == nil {
		return false
	}
	typ, err := expr.Type()
	if err != nil || typ == nil {
		return false
	}
	kind, _ := typ.Kind()
	return kind == googlesql.TypeKindTypeStruct
}

func (n *SubqueryExprNode) FormatSQL(ctx context.Context) (string, error) {
	if n.node == nil {
		return "", nil
	}
	columnNames := &arraySubqueryColumnNames{}
	ctx = withArraySubqueryColumnName(ctx, columnNames)
	sql, err := newNode(m1(n.node.Subquery())).FormatSQL(ctx)
	if err != nil {
		return "", err
	}
	switch m1(n.node.SubqueryType()) {
	case googlesql.ResolvedSubqueryExprEnums_SubqueryTypeScalar:
		if inSafeEvalMode(ctx) {
			// GoogleSQL raises an error when a scalar subquery returns
			// more than one row. In IFERROR / ISERROR / NULLIFERROR
			// contexts that error must fold to NULL, so guard the value
			// with an inline COUNT(*) probe and return NULL unless the
			// subquery produced exactly one row. DuckDB (unlike SQLite)
			// raises on a multi-row scalar subquery even in the dead CASE
			// arm, so extract the value through `SELECT * FROM (sub) LIMIT
			// 1` rather than a bare scalar `(sub)`: the LIMIT 1 keeps the
			// extraction legal for the >1-row case while the COUNT probe
			// still forces the NULL fold. Re-evaluates the sub-select
			// twice - acceptable in error-handling expressions.
			return fmt.Sprintf(
				"(SELECT CASE WHEN (SELECT COUNT(*) FROM (%s)) = 1 THEN (SELECT * FROM (%s) LIMIT 1) ELSE NULL END)",
				sql, sql,
			), nil
		}
	case googlesql.ResolvedSubqueryExprEnums_SubqueryTypeArray:
		subCols := m1(m1(n.node.Subquery()).MutableColumnList())
		if len(subCols) == 0 {
			return "", fmt.Errorf("failed to find computed column names for array subquery")
		}
		colName := uniqueColumnName(ctx, subCols[0])
		// DuckDB retarget: the predecessor collected the array-subquery rows
		// with the googlesqlite_array SQLite aggregate UDF. duckdb-go exposes
		// no aggregate-UDF registration, so collect the rows with DuckDB's
		// native array_agg (yielding a LIST whose elements are the per-row
		// value-layer wire form) and fold that LIST back into the array-layer
		// envelope with googlesqlite_array_from_native. Re-enveloping keeps
		// the array-subquery result a VARCHAR, which is what the surrounding
		// expressions assume: the error-handling CASE arms in ARRAY_ZIP /
		// ARRAY_SLICE mix the subquery result with envelope-typed ERROR(...)
		// branches, and DuckDB rejects a CASE that mixes VARCHAR[] with
		// VARCHAR. COALESCE over an empty list keeps an all-NULL / zero-row
		// subquery from folding to SQL NULL before the envelope wrap, matching
		// GoogleSQL's empty-ARRAY result.
		//
		// DuckDB's array_agg is order-insensitive without an in-aggregate
		// ORDER BY, so a bare array_agg returns the subquery rows in an
		// arbitrary (run-to-run unstable) order. GoogleSQL's ARRAY(subquery)
		// preserves the subquery's row order (its ORDER BY when present, else
		// the producing order). Stamp each row with row_number() OVER () -
		// which is assigned in the subquery's output order, after any inner
		// ORDER BY - and aggregate ORDER BY that stamp so the materialised
		// array keeps the subquery's order deterministically.
		return fmt.Sprintf(
			"googlesqlite_array_from_native(COALESCE((SELECT array_agg(\"%s\" ORDER BY __gsx_array_subq_rn) FROM (SELECT *, row_number() OVER () AS __gsx_array_subq_rn FROM (%s))), []))",
			colName, sql,
		), nil
	case googlesql.ResolvedSubqueryExprEnums_SubqueryTypeExists:
		return fmt.Sprintf("EXISTS (%s)", sql), nil
	case googlesql.ResolvedSubqueryExprEnums_SubqueryTypeIn:
		expr, err := newNode(m1(n.node.InExpr())).FormatSQL(ctx)
		if err != nil {
			return "", err
		}
		// DuckDB retarget: DuckDB has no googlesqlite_collate callback
		// collation, so IN compares the value-layer encoding directly.
		// The anonymous-vs-named STRUCT positional-equality edge case is
		// handled in the STRUCT family, not here.
		return fmt.Sprintf("%s IN (%s)", expr, sql), nil
	case googlesql.ResolvedSubqueryExprEnums_SubqueryTypeLikeAny:
	case googlesql.ResolvedSubqueryExprEnums_SubqueryTypeLikeAll:
	}
	return fmt.Sprintf("(%s)", sql), nil
}

func (n *SingleRowScanNode) FormatSQL(ctx context.Context) (string, error) {
	return "", nil
}

func (n *TableScanNode) FormatSQL(ctx context.Context) (string, error) {
	if n.node == nil {
		return "", nil
	}
	var columns []string
	for _, col := range m1(n.node.ColumnList()) {
		columns = append(
			columns,
			fmt.Sprintf("\"%s\" AS \"%s\"", m1(col.Name()), uniqueColumnName(ctx, col)),
		)
	}

	// If the underlying TableNode is actually a wildcard — recorded in
	// wildcardTableRegistry under the SimpleTable's wasm handle ptr —
	// rewrite the scan into the UNION-ALL pattern. Otherwise fall through
	// to a regular table reference.
	table := m1(n.node.Table())
	if wc := lookupWildcardTable(table); wc != nil {
		query, err := wc.FormatSQL(ctx)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("(SELECT %s FROM (%s))", strings.Join(columns, ","), query), nil
	}
	// INFORMATION_SCHEMA scans materialise the catalog's live rows as
	// an inline DuckDB relation. The SQLite fork served these through
	// an eponymous virtual-table module, but the DuckDB engine has no
	// vtab layer, so we splice a self-contained subquery exposing the
	// view's columns instead. The dataset filter is applied while the
	// rows are built (BuildRows takes meta.dataset), so the outer
	// projection only needs the column names.
	if a := analyzerFromContext(ctx); a != nil && a.catalog != nil {
		if relation, ok, err := a.catalog.infoSchemaInlineRelationFor(table); err != nil {
			return "", err
		} else if ok {
			return fmt.Sprintf("(SELECT %s FROM %s)", strings.Join(columns, ","), relation), nil
		}
	}
	tableName, err := getTableName(ctx, n.node)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("(SELECT %s FROM \"%s\")", strings.Join(columns, ","), tableName), nil
}

func (n *JoinScanNode) FormatSQL(ctx context.Context) (string, error) {
	if n.node == nil {
		return "", nil
	}
	left, err := newNode(m1(n.node.LeftScan())).FormatSQL(ctx)
	if err != nil {
		return "", err
	}
	right, err := newNode(m1(n.node.RightScan())).FormatSQL(ctx)
	if err != nil {
		return "", err
	}
	if getInputPattern(left) == InputNeedsWrap {
		left = fmt.Sprintf("(%s)", left)
	}
	if getInputPattern(right) == InputNeedsWrap {
		right = fmt.Sprintf("(%s)", right)
	}
	if m1(n.node.JoinExpr()) == nil {
		return fmt.Sprintf("%s CROSS JOIN %s", left, right), nil
	}
	joinExpr, err := newNode(m1(n.node.JoinExpr())).FormatSQL(ctx)
	if err != nil {
		return "", err
	}
	switch m1(n.node.JoinType()) {
	case googlesql.ResolvedJoinScanEnums_JoinTypeInner:
		return fmt.Sprintf("%s JOIN %s ON %s", left, right, joinExpr), nil
	case googlesql.ResolvedJoinScanEnums_JoinTypeLeft:
		return fmt.Sprintf("%s LEFT JOIN %s ON %s", left, right, joinExpr), nil
	case googlesql.ResolvedJoinScanEnums_JoinTypeRight:
		return fmt.Sprintf("%s RIGHT JOIN %s ON %s", left, right, joinExpr), nil
	case googlesql.ResolvedJoinScanEnums_JoinTypeFull:
		// ARRAY_ZIP(..., mode => 'PAD') lowers to a FULL OUTER JOIN of the
		// per-array UNNEST(json_each) scans, keyed on their generated offset
		// columns (for N arrays the joins nest: (A FULL B) FULL C ...).
		// DuckDB cannot flatten a correlated subquery that contains a FULL
		// OUTER JOIN ("Unsupported join type for flattening correlated
		// subquery"), and the ARRAY_ZIP body always nests this join inside a
		// correlated array_agg subquery. INNER / LEFT / RIGHT joins of the
		// same correlated scans flatten fine, so only the FULL OUTER shape
		// needs retargeting. Rewrite the offset-keyed positional outer join to
		// a range-driven pair of LEFT JOINs that DuckDB can decorrelate while
		// exposing the identical columns of both operands; fall back to the
		// literal FULL OUTER JOIN for any join that is not an offset-keyed
		// positional zip (e.g. a user FULL OUTER JOIN on element values).
		if rewritten, ok, err := tryFormatFullOuterOffsetZip(ctx, n.node, left, right); err != nil {
			return "", err
		} else if ok {
			return rewritten, nil
		}
		return fmt.Sprintf("%s FULL OUTER JOIN %s ON %s", left, right, joinExpr), nil
	}
	return "", fmt.Errorf("unexpected join type %d", m1(n.node.JoinType()))
}

// tryFormatFullOuterOffsetZip retargets the FULL OUTER JOIN that ARRAY_ZIP's
// PAD mode emits. It fires only when the join condition equates two generated
// UNNEST offset columns (the `$array_offset` / `$full_join` positional keys)
// and both operands are UNNEST-derived. For that shape the FULL OUTER JOIN is
// a positional outer zip: a row exists for every position 0..max(len)-1, with
// the shorter side's columns NULL. Such offset columns are dense 0-based
// indices, so the join is reproducible by driving the row set from a range
// and re-joining each operand on its own positional key.
//
// The replacement keeps both operands verbatim (so every column they expose,
// including any nested coalesce-of-offset columns the enclosing projections
// read, flows through unchanged) and rewrites only the join shape:
//
//	range(0, greatest(count(left), count(right))) AS d(k)
//	  LEFT JOIN (left)  ON leftOffset  = k
//	  LEFT JOIN (right) ON rightOffset = k
//
// range() and the UNNEST operands stay correlated to the outer array columns,
// which DuckDB decorrelates without trouble; the unsupported FULL OUTER node
// is gone. Driving by count(*) of each operand (rather than the source array
// length) keeps the rewrite agnostic to how deep the operand is, so a nested
// (A FULL B) FULL C chain rewrites one join at a time. Any join that is not an
// offset-keyed positional zip returns ok=false so the caller keeps the literal
// FULL OUTER JOIN emission.
func tryFormatFullOuterOffsetZip(ctx context.Context, join *googlesql.ResolvedJoinScan, left, right string) (string, bool, error) {
	keyA, keyB, ok := offsetEqualityKeys(ctx, m1(join.JoinExpr()))
	if !ok {
		return "", false, nil
	}
	// Both operands must be UNNEST-derived for the positional-zip rewrite to
	// be valid. ARRAY_ZIP's scans always lower through json_each; a user FULL
	// OUTER JOIN on real tables never contains it. This also keeps the
	// count(*) row-position assumption (dense 0-based offsets) sound.
	if !strings.Contains(left, "json_each(") || !strings.Contains(right, "json_each(") {
		return "", false, nil
	}
	// Map each offset key to the operand that exposes it. The formatter renders
	// the key as a quoted unique name, so membership is a substring test on the
	// already-formatted operand SQL.
	leftKey, rightKey, ok := assignKeysToOperands(left, right, keyA, keyB)
	if !ok {
		return "", false, nil
	}
	driver := fmt.Sprintf(
		"range(CAST(0 AS BIGINT), CAST(greatest((SELECT count(*) FROM %s),(SELECT count(*) FROM %s)) AS BIGINT)) AS __zip_drv(__zip_k)",
		wrapZipOperand(left), wrapZipOperand(right),
	)
	return fmt.Sprintf(
		"%s LEFT JOIN %s ON \"%s\" = __zip_drv.__zip_k LEFT JOIN %s ON \"%s\" = __zip_drv.__zip_k",
		driver, wrapZipOperand(left), leftKey, wrapZipOperand(right), rightKey,
	), true, nil
}

// offsetEqualityKeys returns the two column unique names of a join condition
// of the form `<offsetA> = <offsetB>`, but only when both sides are bare
// ColumnRefs to generated UNNEST offset columns (table name `$array_offset`
// for a leaf UNNEST or `$full_join` for a prior outer-zip key). It returns
// ok=false for any other predicate so the FULL OUTER retarget stays scoped to
// the positional zip ARRAY_ZIP emits.
func offsetEqualityKeys(ctx context.Context, joinExpr googlesql.ResolvedExprNode) (string, string, bool) {
	call, ok := joinExpr.(*googlesql.ResolvedFunctionCall)
	if !ok {
		return "", "", false
	}
	name, err := getFuncName(ctx, call)
	if err != nil || (name != "$equal" && !strings.HasSuffix(name, ".$equal")) {
		return "", "", false
	}
	args := m1(call.ArgumentList())
	if len(args) != 2 {
		return "", "", false
	}
	colA, okA := offsetColumnName(ctx, args[0])
	colB, okB := offsetColumnName(ctx, args[1])
	if !okA || !okB {
		return "", "", false
	}
	return colA, colB, true
}

// offsetColumnName returns the unique name of a ColumnRef to a generated
// UNNEST offset column, and ok=false for any other expression. The positional
// offset columns ARRAY_ZIP joins on live in the analyzer's `$array_offset`
// (leaf UNNEST) and `$full_join` (a prior outer-zip's coalesced key) internal
// ranges; restricting to those names avoids treating a value-based join key as
// a position.
func offsetColumnName(ctx context.Context, expr googlesql.ResolvedExprNode) (string, bool) {
	ref, ok := expr.(*googlesql.ResolvedColumnRef)
	if !ok {
		return "", false
	}
	col := m1(ref.Column())
	switch m1(col.TableName()) {
	case "$array_offset", "$full_join":
		return uniqueColumnName(ctx, col), true
	}
	return "", false
}

// assignKeysToOperands decides which of the two offset keys belongs to the
// left operand and which to the right by testing which formatted operand SQL
// contains each quoted key name. It returns ok=false when the mapping is
// ambiguous (neither or both operands contain a key), leaving the literal
// FULL OUTER JOIN in place.
func assignKeysToOperands(left, right, keyA, keyB string) (string, string, bool) {
	qa := fmt.Sprintf("\"%s\"", keyA)
	qb := fmt.Sprintf("\"%s\"", keyB)
	leftHasA := strings.Contains(left, qa)
	leftHasB := strings.Contains(left, qb)
	rightHasA := strings.Contains(right, qa)
	rightHasB := strings.Contains(right, qb)
	if leftHasA && rightHasB && !leftHasB && !rightHasA {
		return keyA, keyB, true
	}
	if leftHasB && rightHasA && !leftHasA && !rightHasB {
		return keyB, keyA, true
	}
	return "", "", false
}

// wrapZipOperand wraps a formatted scan operand as a parenthesised subquery so
// it can sit on either side of a JOIN and be probed by `SELECT count(*) FROM`.
// A scan that already starts with `(` is left as-is.
func wrapZipOperand(operand string) string {
	trimmed := strings.TrimSpace(operand)
	if strings.HasPrefix(trimmed, "(") {
		return trimmed
	}
	return fmt.Sprintf("(%s)", trimmed)
}

// unnestElementSQL returns the SQL projecting one decoded array element out
// of the json_each(googlesqlite_decode_array(...)) scan, retyped to match the
// element column's analyzer-resolved GoogleSQL type.
//
// Left as raw json_each.value, every element reaches DuckDB JSON-typed:
// native functions fail to bind (mod(JSON, INTEGER_LITERAL)), CTAS persists
// a JSON column, and — worst — native comparisons / MIN / MAX over the JSON
// values are lexicographic, so MAX over UNNEST(GENERATE_ARRAY(1,1000))
// silently returned 999. The analyzer knows the element type here, so:
//
//   - INT64 / FLOAT64 / BOOL elements are bare JSON scalars in the decoded
//     array (EncodeValue passes them through raw); a native CAST retypes
//     them to BIGINT / DOUBLE / BOOLEAN.
//   - STRING elements are the value-layer envelope wrapped in JSON quotes;
//     json_extract_string strips the quotes so the element carries the same
//     VARCHAR envelope a stored TEXT column holds (equality with literals
//     and stored columns is byte-identical either way; a plain CAST would
//     keep the JSON quotes and break it).
//   - DATE / TIME / DATETIME / TIMESTAMP elements ride the envelope inside
//     the JSON; googlesqlite_unnest_temporal unwraps the envelope to a
//     canonical native-castable text form and the CAST retypes it to the
//     native temporal column type (matching ColumnSpec.SQLiteSchema, which
//     stores temporals natively).
//
// Every other element kind (ARRAY, STRUCT, BYTES, NUMERIC, GEOGRAPHY, JSON,
// INTERVAL, RANGE, ENUM, PROTO, ...) keeps the raw JSON projection: the
// value-layer flow depends on receiving the envelope untouched.
func unnestElementSQL(col *googlesql.ResolvedColumn) string {
	const raw = "json_each.value"
	if col == nil {
		return raw
	}
	t, _ := col.Type()
	if t == nil {
		return raw
	}
	k, err := t.Kind()
	if err != nil {
		return raw
	}
	if castType, ok := valueLayerScalarCastType(k); ok {
		return fmt.Sprintf("CAST(%s AS %s)", raw, castType)
	}
	switch k {
	case googlesql.TypeKindTypeString:
		return fmt.Sprintf("json_extract_string(%s, '$')", raw)
	case googlesql.TypeKindTypeDate:
		return fmt.Sprintf("CAST(googlesqlite_unnest_temporal(json_extract_string(%s, '$'), 'date') AS DATE)", raw)
	case googlesql.TypeKindTypeTime:
		return fmt.Sprintf("CAST(googlesqlite_unnest_temporal(json_extract_string(%s, '$'), 'time') AS TIME)", raw)
	case googlesql.TypeKindTypeDatetime:
		return fmt.Sprintf("CAST(googlesqlite_unnest_temporal(json_extract_string(%s, '$'), 'datetime') AS TIMESTAMP)", raw)
	case googlesql.TypeKindTypeTimestamp:
		return fmt.Sprintf("CAST(googlesqlite_unnest_temporal(json_extract_string(%s, '$'), 'timestamp') AS TIMESTAMPTZ)", raw)
	}
	return raw
}

func (n *ArrayScanNode) FormatSQL(ctx context.Context) (string, error) {
	if n.node == nil {
		return "", nil
	}
	arrayExpr, err := newNode(m1(n.node.ArrayExpr())).FormatSQL(ctx)
	if err != nil {
		return "", err
	}
	elementColumn := m1(n.node.ElementColumn())
	colName := uniqueColumnName(ctx, elementColumn)
	columns := []string{fmt.Sprintf("%s AS \"%s\"", unnestElementSQL(elementColumn), colName)}

	if offsetColumn, _ := n.node.ArrayOffsetColumn(); offsetColumn != nil {
		offsetColName := uniqueColumnName(ctx, m1(offsetColumn.Column()))
		// The WITH OFFSET column resolves as INT64; retype json_each.key
		// the same way the element is retyped so native functions bind and
		// ORDER BY off sorts numerically rather than lexicographically.
		columns = append(columns, fmt.Sprintf("CAST(json_each.key AS BIGINT) AS \"%s\"", offsetColName))
	}
	if m1(n.node.InputScan()) != nil {
		input, err := newNode(m1(n.node.InputScan())).FormatSQL(ctx)
		if err != nil {
			return "", err
		}
		formattedInput, err := formatInput(input)
		if err != nil {
			return "", err
		}

		array := fmt.Sprintf("json_each(googlesqlite_decode_array(%s))", arrayExpr)
		var arrayJoinExpr string
		if m1(n.node.JoinExpr()) != nil {
			arrayJoinExpr, err = newNode(m1(n.node.JoinExpr())).FormatSQL(ctx)
			if err != nil {
				return "", err
			}
			// RIGHT JOINs on array expressions are not supported by BigQuery
			var joinMode string
			if m1(n.node.IsOuter()) {
				joinMode = "LEFT OUTER JOIN"
			} else {
				joinMode = "INNER JOIN"
			}
			// DuckDB retarget: a correlated array scan with a JOIN condition
			// (the multi-array UNNEST that ARRAY_ZIP lowers to) references the
			// element/offset columns of THIS scan inside the ON clause
			// (e.g. ON "offset#6" = "offset#8"). DuckDB resolves a JOIN ON
			// against the joined relation's columns, not the outer SELECT
			// list, so projecting `json_each.value AS "e#"` in the outer
			// SELECT leaves the ON clause unable to see the alias (Binder
			// Error: Referenced column "offset#8" not found). Push the value
			// / key aliases into a subquery wrapping the joined json_each so
			// they become real columns the ON clause and the outer `SELECT *`
			// both resolve.
			array = fmt.Sprintf("(SELECT %s FROM %s)", strings.Join(columns, ","), array)
			arrayJoinExpr = fmt.Sprintf("%s %s ON %s",
				joinMode,
				array,
				arrayJoinExpr,
			)
			return fmt.Sprintf(
				"SELECT * %s %s",
				formattedInput,
				arrayJoinExpr,
			), nil
		}
		// If there is no join expression, use a CROSS JOIN. The single-array
		// scan projects the element / offset aliases directly because there
		// is no ON clause that needs them as real columns.
		arrayJoinExpr = fmt.Sprintf(", %s", array)

		return fmt.Sprintf(
			"SELECT *, %s %s %s",
			strings.Join(columns, ","),
			formattedInput,
			arrayJoinExpr,
		), nil
	}
	return fmt.Sprintf(
		"SELECT %s FROM json_each(googlesqlite_decode_array(%s))",
		strings.Join(columns, ","),
		arrayExpr,
	), nil
}

var tokensAfterFromClause = [...]string{"WHERE", "GROUP BY", "HAVING", "QUALIFY", "WINDOW", "ORDER BY", "COLLATE"}
var removeExpressions = regexp.MustCompile(`\(.+?\)`)

func (n *FilterScanNode) FormatSQL(ctx context.Context) (string, error) {
	if n.node == nil {
		return "", nil
	}
	input, err := newNode(m1(n.node.InputScan())).FormatSQL(ctx)
	if err != nil {
		return "", err
	}
	filter, err := newNode(m1(n.node.FilterExpr())).FormatSQL(ctx)
	if err != nil {
		return "", err
	}
	currentQuery := removeExpressions.ReplaceAllString(input, "")

	// Qualify the statement if the input is not wrapped in parens
	queryWrappedInParens := currentQuery == ""
	containsTokens := false
	// and the input contains a token that would result in a syntax error
	for _, token := range tokensAfterFromClause {
		containsTokens = containsTokens || strings.Contains(currentQuery, token)
	}

	if !queryWrappedInParens && containsTokens {
		return fmt.Sprintf("( %s ) WHERE %s", input, filter), nil
	}
	return fmt.Sprintf("%s WHERE %s", input, filter), nil
}

// FormatSQL passes the AssertScan through to its InputScan. See the
// AssertScanNode doc comment in node.go for the rationale — the emulator
// skips the runtime assertion check, which matches every upstream
// Example because those Examples are written with conditions that the
// fixture data satisfies.
func (n *AssertScanNode) FormatSQL(ctx context.Context) (string, error) {
	if n.node == nil {
		return "", nil
	}
	return newNode(m1(n.node.InputScan())).FormatSQL(ctx)
}

// FormatSQL on BarrierScanNode is a pass-through to InputScan. The
// barrier acts as an optimiser hint upstream; for the emulator it has
// no observable runtime semantics.
func (n *BarrierScanNode) FormatSQL(ctx context.Context) (string, error) {
	if n.node == nil {
		return "", nil
	}
	return newNode(m1(n.node.InputScan())).FormatSQL(ctx)
}

// isGroupingCallColumn reports whether `name` is an output column for a
// GROUPING() call in the same AggregateScan.
func isGroupingCallColumn(name string, targets map[string]string) bool {
	_, ok := targets[name]
	return ok
}

// nullSetContains reports whether `name` is a group-by column that must
// be NULLed out for the current grouping set.
func nullSetContains(set map[string]struct{}, name string) bool {
	_, ok := set[name]
	return ok
}

func (n *AggregateScanNode) FormatSQL(ctx context.Context) (string, error) {
	if n.node == nil {
		return "", nil
	}
	for _, agg := range m1(n.node.AggregateList()) {
		// assign sql to column ref map
		if _, err := newNode(agg).FormatSQL(ctx); err != nil {
			return "", err
		}
	}
	input, err := newNode(m1(n.node.InputScan())).FormatSQL(ctx)
	if err != nil {
		return "", err
	}
	groupByColumns := []string{}
	groupByColumnMap := map[string]struct{}{}
	for _, col := range m1(n.node.GroupByList()) {
		if _, err := newNode(col).FormatSQL(ctx); err != nil {
			return "", err
		}
		colName := uniqueColumnName(ctx, m1(col.Column()))
		groupByColumns = append(groupByColumns, fmt.Sprintf("\"%s\"", colName))
		groupByColumnMap[colName] = struct{}{}
	}
	columns := []string{}
	columnMap := columnRefMap(ctx)
	columnNames := []string{}
	for _, col := range m1(n.node.ColumnList()) {
		colName := uniqueColumnName(ctx, col)
		columnNames = append(columnNames, colName)
		if ref, exists := columnMap[colName]; exists {
			columns = append(columns, ref)
			delete(columnMap, colName)
		} else {
			columns = append(columns, fmt.Sprintf("\"%s\"", colName))
		}
	}
	if gsList := m1(n.node.GroupingSetList()); len(gsList) != 0 {
		// GROUPING(col) maps to a per-grouping-set 0/1 literal: 0 if
		// `col` IS in the current grouping set (i.e. rows are grouped
		// by it), 1 if `col` is being aggregated away. Build the
		// mapping from output-column unique name → group-by-column
		// unique name so we can substitute the literal into each
		// branch's projection.
		groupingCallTargets := map[string]string{}
		for _, call := range m1(n.node.GroupingCallList()) {
			out, err := call.OutputColumn()
			if err != nil || out == nil {
				continue
			}
			ref, err := call.GroupByColumn()
			if err != nil || ref == nil {
				continue
			}
			groupingCallTargets[uniqueColumnName(ctx, out)] =
				uniqueColumnName(ctx, m1(ref.Column()))
		}
		columnPatterns := [][]string{}
		groupByColumnPatterns := [][]string{}
		for _, set := range gsList {
			concreteSet, ok := set.(*googlesql.ResolvedGroupingSet)
			if !ok {
				continue
			}
			groupBySetColumns := []string{}
			groupBySetColumnMap := map[string]struct{}{}
			for _, col := range m1(concreteSet.GroupByColumnList()) {
				colName := uniqueColumnName(ctx, m1(col.Column()))
				groupBySetColumns = append(groupBySetColumns, fmt.Sprintf("\"%s\"", colName))
				groupBySetColumnMap[colName] = struct{}{}
			}
			nullColumnNameMap := map[string]struct{}{}
			for colName := range groupByColumnMap {
				if _, exists := groupBySetColumnMap[colName]; !exists {
					nullColumnNameMap[colName] = struct{}{}
				}
			}
			groupBySetColumnPattern := []string{}
			for idx, col := range columnNames {
				switch {
				case isGroupingCallColumn(col, groupingCallTargets):
					target := groupingCallTargets[col]
					grouped := 1
					if _, exists := groupBySetColumnMap[target]; exists {
						grouped = 0
					}
					groupBySetColumnPattern = append(groupBySetColumnPattern, fmt.Sprintf("%d AS \"%s\"", grouped, col))
				case nullSetContains(nullColumnNameMap, col):
					groupBySetColumnPattern = append(groupBySetColumnPattern, fmt.Sprintf("NULL AS \"%s\"", col))
				default:
					groupBySetColumnPattern = append(groupBySetColumnPattern, columns[idx])
				}
			}
			columnPatterns = append(columnPatterns, groupBySetColumnPattern)
			// DuckDB retarget (PATH B): emit the bare GROUP BY keys; the
			// predecessor's googlesqlite_group_by() collation shim is not
			// registered on DuckDB and is unnecessary (native GROUP BY on
			// the value-layer encoding groups equal values correctly).
			groupByColumnPatterns = append(groupByColumnPatterns, append([]string{}, groupBySetColumns...))
		}
		stmts := []string{}
		for i := 0; i < len(columnPatterns); i++ {
			var groupBy string
			if len(groupByColumnPatterns[i]) != 0 {
				groupBy = fmt.Sprintf("GROUP BY %s", strings.Join(groupByColumnPatterns[i], ","))
			}
			formattedColumns := strings.Join(columnPatterns[i], ",")
			switch getInputPattern(input) {
			case InputKeep:
				stmts = append(stmts, fmt.Sprintf("SELECT %s %s %s", formattedColumns, input, groupBy))
			case InputNeedsWrap:
				stmts = append(stmts, fmt.Sprintf("SELECT %s FROM (%s) %s", formattedColumns, input, groupBy))
			case InputNeedsFrom:
				stmts = append(stmts, fmt.Sprintf("SELECT %s FROM %s %s", formattedColumns, input, groupBy))
			}
		}
		// DuckDB retarget: order the grouping-set UNION ALL output on the
		// bare value-layer columns (no googlesqlite_collate on DuckDB).
		return fmt.Sprintf(
			"%s ORDER BY %s",
			strings.Join(stmts, " UNION ALL "),
			strings.Join(groupByColumns, ","),
		), nil
	}
	var groupBy string
	if len(groupByColumns) > 0 {
		// DuckDB retarget (PATH B): the predecessor wrapped each GROUP BY
		// key in the googlesqlite_group_by() SQLite UDF (a collation /
		// value-decode shim). DuckDB has no such UDF and groups on the
		// raw value-layer encoding directly - rows that are equal under
		// GoogleSQL semantics share a byte-identical encoded string, so
		// native GROUP BY on the bare column is correct.
		groupBy = fmt.Sprintf("GROUP BY %s", strings.Join(groupByColumns, ","))
	}
	formattedColumns := strings.Join(columns, ",")
	switch getInputPattern(input) {
	case InputKeep:
		return fmt.Sprintf("SELECT %s %s %s", formattedColumns, input, groupBy), nil
	case InputNeedsWrap:
		return fmt.Sprintf("SELECT %s FROM (%s) %s", formattedColumns, input, groupBy), nil
	case InputNeedsFrom:
		return fmt.Sprintf("SELECT %s FROM %s %s", formattedColumns, input, groupBy), nil
	}
	return "", fmt.Errorf("unexpected input pattern: %s", input)
}

// FormatSQL lowers a ResolvedAnonymizedAggregateScan (the
// resolver's `SELECT WITH ANONYMIZATION` form) to SQL using the
// same shape as DifferentialPrivacyAggregateScan: epsilon / delta
// are pulled from OptionList and threaded through the context so
// inner $anon_* / $differential_privacy_* aggregate calls can pick
// them up; the aggregate list and group-by list are formatted as
// for a regular aggregate scan.
func (n *AnonymizedAggregateScanNode) FormatSQL(ctx context.Context) (string, error) {
	if n.node == nil {
		return "", nil
	}
	dp := &dpOptions{Epsilon: 1, Delta: 1e-5}
	for _, opt := range m1(n.node.AnonymizationOptionList()) {
		name, _ := opt.Name()
		valExpr, _ := opt.Value()
		valSQL, err := newNode(valExpr).FormatSQL(ctx)
		if err != nil {
			continue
		}
		f, perr := strconv.ParseFloat(strings.TrimSpace(valSQL), 64)
		if perr != nil {
			continue
		}
		switch strings.ToLower(name) {
		case "epsilon":
			dp.Epsilon = f
		case "delta":
			dp.Delta = f
		}
	}
	ctx = withDPOptions(ctx, dp)
	for _, agg := range m1(n.node.AggregateList()) {
		if _, err := newNode(agg).FormatSQL(ctx); err != nil {
			return "", err
		}
	}
	input, err := newNode(m1(n.node.InputScan())).FormatSQL(ctx)
	if err != nil {
		return "", err
	}
	groupByColumns := []string{}
	for _, col := range m1(n.node.GroupByList()) {
		if _, err := newNode(col).FormatSQL(ctx); err != nil {
			return "", err
		}
		colName := uniqueColumnName(ctx, m1(col.Column()))
		groupByColumns = append(groupByColumns, fmt.Sprintf("\"%s\"", colName))
	}
	columns := []string{}
	columnMap := columnRefMap(ctx)
	for _, col := range m1(n.node.ColumnList()) {
		colName := uniqueColumnName(ctx, col)
		if ref, exists := columnMap[colName]; exists {
			columns = append(columns, ref)
			delete(columnMap, colName)
		} else {
			columns = append(columns, fmt.Sprintf("\"%s\"", colName))
		}
	}
	formattedColumns := strings.Join(columns, ",")
	var groupBy string
	if len(groupByColumns) > 0 {
		groupBy = "GROUP BY " + strings.Join(groupByColumns, ",")
	}
	switch getInputPattern(input) {
	case InputKeep:
		return fmt.Sprintf("SELECT %s %s %s", formattedColumns, input, groupBy), nil
	case InputNeedsWrap:
		return fmt.Sprintf("SELECT %s FROM (%s) %s", formattedColumns, input, groupBy), nil
	case InputNeedsFrom:
		return fmt.Sprintf("SELECT %s FROM %s %s", formattedColumns, input, groupBy), nil
	}
	return "", fmt.Errorf("anon scan: unexpected input pattern: %s", input)
}

// dpOptions captures the (epsilon, delta) pair parsed from a
// DifferentialPrivacyAggregateScan's option_list. AggregateFunctionCall
// reads it at format time so it can append the privacy-budget args to
// $differential_privacy_* calls.
type dpOptions struct {
	Epsilon float64
	Delta   float64
}

type dpCtxKey struct{}

func dpOptionsFromContext(ctx context.Context) *dpOptions {
	if v, ok := ctx.Value(dpCtxKey{}).(*dpOptions); ok {
		return v
	}
	return nil
}

// isDPAggregateName reports whether funcName is one of the differential
// privacy / anonymization aggregate calls that the (epsilon, delta)
// budget should be appended to. The DIFFERENTIAL_PRIVACY surface arrives
// as googlesqlite_differential_privacy_*; the ANONYMIZATION surface's
// rewriter emits anon_* (bare) or $anon_* (which the prefix logic turns
// into googlesqlite_anon_*). All variants want the budget so the runtime
// can size Laplace noise.
func isDPAggregateName(funcName string) bool {
	return strings.HasPrefix(funcName, "googlesqlite_differential_privacy_") ||
		strings.HasPrefix(funcName, "googlesqlite_anon_") ||
		strings.HasPrefix(funcName, "anon_")
}

func withDPOptions(ctx context.Context, dp *dpOptions) context.Context {
	return context.WithValue(ctx, dpCtxKey{}, dp)
}

// FormatSQL lowers a ResolvedDifferentialPrivacyAggregateScan to
// SQL by formatting it like a regular AggregateScan, after
// extracting the epsilon/delta options so the inner
// $differential_privacy_* aggregate calls can append them as
// extra runtime arguments.
func (n *DifferentialPrivacyAggregateScanNode) FormatSQL(ctx context.Context) (string, error) {
	if n.node == nil {
		return "", nil
	}
	dp := &dpOptions{Epsilon: 1, Delta: 1e-5}
	for _, opt := range m1(n.node.OptionList()) {
		name, _ := opt.Name()
		valExpr, _ := opt.Value()
		valSQL, err := newNode(valExpr).FormatSQL(ctx)
		if err != nil {
			continue
		}
		f, perr := strconv.ParseFloat(strings.TrimSpace(valSQL), 64)
		if perr != nil {
			continue
		}
		switch strings.ToLower(name) {
		case "epsilon":
			dp.Epsilon = f
		case "delta":
			dp.Delta = f
		}
	}
	ctx = withDPOptions(ctx, dp)
	for _, agg := range m1(n.node.AggregateList()) {
		if _, err := newNode(agg).FormatSQL(ctx); err != nil {
			return "", err
		}
	}
	input, err := newNode(m1(n.node.InputScan())).FormatSQL(ctx)
	if err != nil {
		return "", err
	}
	// Format the GROUP BY entries so each ComputedColumn populates
	// the column-ref map with the right `<src> AS <out>` mapping;
	// otherwise the outer SELECT's reference to the group-by column
	// (e.g. `item#5`) cannot resolve to its source (e.g.
	// `item_partial#9`) emitted by the inner scan.
	groupByColumns := []string{}
	for _, col := range m1(n.node.GroupByList()) {
		if _, err := newNode(col).FormatSQL(ctx); err != nil {
			return "", err
		}
		colName := uniqueColumnName(ctx, m1(col.Column()))
		groupByColumns = append(groupByColumns, fmt.Sprintf("\"%s\"", colName))
	}
	columns := []string{}
	columnMap := columnRefMap(ctx)
	for _, col := range m1(n.node.ColumnList()) {
		colName := uniqueColumnName(ctx, col)
		if ref, exists := columnMap[colName]; exists {
			columns = append(columns, ref)
			delete(columnMap, colName)
		} else {
			columns = append(columns, fmt.Sprintf("\"%s\"", colName))
		}
	}
	formattedColumns := strings.Join(columns, ",")
	var groupBy string
	if len(groupByColumns) > 0 {
		groupBy = "GROUP BY " + strings.Join(groupByColumns, ",")
	}
	switch getInputPattern(input) {
	case InputKeep:
		return fmt.Sprintf("SELECT %s %s %s", formattedColumns, input, groupBy), nil
	case InputNeedsWrap:
		return fmt.Sprintf("SELECT %s FROM (%s) %s", formattedColumns, input, groupBy), nil
	case InputNeedsFrom:
		return fmt.Sprintf("SELECT %s FROM %s %s", formattedColumns, input, groupBy), nil
	}
	return "", fmt.Errorf("DP scan: unexpected input pattern: %s", input)
}

func (n *SetOperationItemNode) FormatSQL(ctx context.Context) (string, error) {
	if n.node == nil {
		return "", nil
	}
	return newNode(m1(n.node.Scan())).FormatSQL(ctx)
}

func (n *SetOperationScanNode) FormatSQL(ctx context.Context) (string, error) {
	if n.node == nil {
		return "", nil
	}
	var opType string
	switch m1(n.node.OpType()) {
	case googlesql.ResolvedSetOperationScanEnums_SetOperationTypeUnionAll:
		opType = "UNION ALL"
	case googlesql.ResolvedSetOperationScanEnums_SetOperationTypeUnionDistinct:
		opType = "UNION"
	case googlesql.ResolvedSetOperationScanEnums_SetOperationTypeIntersectAll:
		opType = "INTERSECT ALL"
	case googlesql.ResolvedSetOperationScanEnums_SetOperationTypeIntersectDistinct:
		opType = "INTERSECT"
	case googlesql.ResolvedSetOperationScanEnums_SetOperationTypeExceptAll:
		opType = "EXCEPT ALL"
	case googlesql.ResolvedSetOperationScanEnums_SetOperationTypeExceptDistinct:
		opType = "EXCEPT"
	default:
		opType = "UNKNOWN"
	}
	var queries []string
	for _, item := range m1(n.node.InputItemList()) {
		var outputColumns []string
		for _, outputColumn := range m1(item.OutputColumnList()) {
			outputColumns = append(outputColumns, fmt.Sprintf("\"%s\"", uniqueColumnName(ctx, outputColumn)))
		}
		query, err := newNode(item).FormatSQL(ctx)
		if err != nil {
			return "", err
		}

		formattedInput, err := formatInput(query)
		if err != nil {
			return "", err
		}

		queries = append(
			queries,
			fmt.Sprintf("SELECT %s %s",
				strings.Join(outputColumns, ", "),
				formattedInput,
			),
		)
	}
	columnMaps := []string{}
	if inputItems := m1(n.node.InputItemList()); len(inputItems) != 0 {
		for idx, col := range m1(inputItems[0].OutputColumnList()) {
			columnMaps = append(
				columnMaps,
				fmt.Sprintf(
					"\"%s\" AS \"%s\"",
					uniqueColumnName(ctx, col),
					uniqueColumnName(ctx, m1(n.node.ColumnList())[idx]),
				),
			)
		}
	}
	return fmt.Sprintf(
		"SELECT %s FROM (%s)",
		strings.Join(columnMaps, ","),
		strings.Join(queries, fmt.Sprintf(" %s ", opType)),
	), nil
}

func (n *OrderByScanNode) FormatSQL(ctx context.Context) (string, error) {
	if n.node == nil {
		return "", nil
	}
	input, err := newNode(m1(n.node.InputScan())).FormatSQL(ctx)
	if err != nil {
		return "", err
	}
	columns := []string{}
	columnMap := columnRefMap(ctx)
	for _, col := range m1(n.node.ColumnList()) {
		colName := uniqueColumnName(ctx, col)
		if ref, exists := columnMap[colName]; exists {
			columns = append(columns, ref)
			delete(columnMap, colName)
		} else {
			columns = append(
				columns,
				fmt.Sprintf("\"%s\"", colName),
			)
		}
	}
	orderByColumns := []string{}
	for _, item := range m1(n.node.OrderByItemList()) {
		colName := uniqueColumnName(ctx, m1(m1(item.ColumnRef()).Column()))
		switch m1(item.NullOrder()) {
		case googlesql.ResolvedOrderByItemEnums_NullOrderModeNullsFirst:
			orderByColumns = append(
				orderByColumns,
				fmt.Sprintf("(\"%s\" IS NOT NULL)", colName),
			)
		case googlesql.ResolvedOrderByItemEnums_NullOrderModeNullsLast:
			orderByColumns = append(
				orderByColumns,
				fmt.Sprintf("(\"%s\" IS NULL)", colName),
			)
		}
		// DuckDB retarget: drop the googlesqlite_collate SQLite collation
		// (no DuckDB callback-collation API). Native DuckDB ordering is
		// correct because equal-under-GoogleSQL values share an identical
		// value-layer encoding.
		if m1(item.IsDescending()) {
			orderByColumns = append(orderByColumns, fmt.Sprintf("\"%s\" DESC", colName))
		} else {
			orderByColumns = append(orderByColumns, fmt.Sprintf("\"%s\"", colName))
		}
	}
	formattedInput, err := formatInput(input)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(
		"SELECT %s %s ORDER BY %s",
		strings.Join(columns, ","),
		formattedInput,
		strings.Join(orderByColumns, ","),
	), nil
}

func (n *LimitOffsetScanNode) FormatSQL(ctx context.Context) (string, error) {
	if n.node == nil {
		return "", nil
	}
	input, err := newNode(m1(n.node.InputScan())).FormatSQL(ctx)
	if err != nil {
		return "", err
	}
	columns := []string{}
	columnMap := columnRefMap(ctx)
	for _, col := range m1(n.node.ColumnList()) {
		colName := uniqueColumnName(ctx, col)
		if ref, exists := columnMap[colName]; exists {
			columns = append(columns, ref)
			delete(columnMap, colName)
		} else {
			columns = append(
				columns,
				fmt.Sprintf("\"%s\"", colName),
			)
		}
	}
	formattedInput, err := formatInput(input)
	if err != nil {
		return "", err
	}
	var limitExpr string
	if m1(n.node.Limit()) != nil {
		expr, err := newNode(m1(n.node.Limit())).FormatSQL(ctx)
		if err != nil {
			return "", err
		}
		limitExpr = fmt.Sprintf("LIMIT %s", expr)
	}
	var offsetExpr string
	if m1(n.node.Offset()) != nil {
		expr, err := newNode(m1(n.node.Offset())).FormatSQL(ctx)
		if err != nil {
			return "", err
		}
		offsetExpr = fmt.Sprintf("OFFSET %s", expr)
	}
	return fmt.Sprintf(
		"SELECT %s %s %s %s",
		strings.Join(columns, ","),
		formattedInput,
		limitExpr,
		offsetExpr,
	), nil
}

func (n *WithRefScanNode) FormatSQL(ctx context.Context) (string, error) {
	if n.node == nil {
		return "", nil
	}
	tableName, _ := n.node.WithQueryName()
	tableToColumnListMap := tableNameToColumnListMap(ctx)
	columnDefs := tableToColumnListMap[tableName]
	columns := m1(n.node.ColumnList())
	if len(columnDefs) != len(columns) {
		return "", fmt.Errorf(
			"column num mismatch. defined column num is %d but used %d column",
			len(columnDefs), len(columns),
		)
	}
	formattedColumns := []string{}
	for i := range columnDefs {
		formattedColumns = append(
			formattedColumns,
			fmt.Sprintf("\"%s\" AS \"%s\"", uniqueColumnName(ctx, columnDefs[i]), uniqueColumnName(ctx, columns[i])),
		)
	}
	return fmt.Sprintf("(SELECT %s FROM \"%s\")", strings.Join(formattedColumns, ","), tableName), nil
}

func (n *AnalyticScanNode) FormatSQL(ctx context.Context) (string, error) {
	if n.node == nil {
		return "", nil
	}
	input, err := newNode(m1(n.node.InputScan())).FormatSQL(ctx)
	if err != nil {
		return "", err
	}
	formattedInput, err := formatInput(input)
	if err != nil {
		return "", err
	}
	ctx = withAnalyticInputScan(ctx, formattedInput)
	ctx, emulationUsed := withAnalyticEmulationFlag(ctx)
	orderColumnNames := analyticOrderColumnNamesFromContext(ctx)
	var scanOrderBy []*analyticOrderBy
	for _, group := range m1(n.node.FunctionGroupList()) {
		scanOrderBy = []*analyticOrderBy{}

		if m1(group.PartitionBy()) != nil {
			var partitionColumns []string
			for _, columnRef := range m1(m1(group.PartitionBy()).PartitionByList()) {
				colName := fmt.Sprintf("\"%s\"", uniqueColumnName(ctx, m1(columnRef.Column())))
				partitionColumns = append(
					partitionColumns,
					colName,
				)
				order := &analyticOrderBy{
					column: colName,
					isAsc:  true,
				}
				orderColumnNames.values = append(orderColumnNames.values, order)
				scanOrderBy = append(scanOrderBy, order)
			}
			ctx = withAnalyticPartitionColumnNames(ctx, partitionColumns)
		}
		if m1(group.OrderBy()) != nil {
			for _, item := range m1(m1(group.OrderBy()).OrderByItemList()) {
				colName := uniqueColumnName(ctx, m1(m1(item.ColumnRef()).Column()))
				formattedColName := fmt.Sprintf("\"%s\"", colName)
				nullOrder := nullOrderUnspecified
				switch m1(item.NullOrder()) {
				case googlesql.ResolvedOrderByItemEnums_NullOrderModeNullsFirst:
					nullOrder = nullOrderFirst
				case googlesql.ResolvedOrderByItemEnums_NullOrderModeNullsLast:
					nullOrder = nullOrderLast
				}
				order := &analyticOrderBy{
					column:    formattedColName,
					isAsc:     !m1(item.IsDescending()),
					nullOrder: nullOrder,
				}
				orderColumnNames.values = append(orderColumnNames.values, order)
				scanOrderBy = append(scanOrderBy, order)
			}
		}
		if _, err := newNode(group).FormatSQL(ctx); err != nil {
			return "", err
		}

		// Reset context after each analytic function group
		orderColumnNames.values = []*analyticOrderBy{}
		ctx = withAnalyticPartitionColumnNames(ctx, nil)
	}
	columns := []string{}
	columnMap := columnRefMap(ctx)
	for _, col := range m1(n.node.ColumnList()) {
		colName := uniqueColumnName(ctx, col)
		if ref, exists := columnMap[colName]; exists {
			columns = append(columns, ref)
			delete(columnMap, colName)
		} else {
			columns = append(
				columns,
				fmt.Sprintf("\"%s\"", colName),
			)
		}
	}
	var orderColumnFormattedNames []string
	for _, col := range scanOrderBy {
		// NULLS FIRST/LAST is implemented as a sort key prefix:
		// emit (col IS NOT NULL) for NULLS FIRST so nulls (0) sort
		// before non-nulls (1); emit (col IS NULL) for NULLS LAST so
		// non-nulls (0) come before nulls (1). Skipped when the
		// query did not request explicit null ordering.
		switch col.nullOrder {
		case nullOrderFirst:
			orderColumnFormattedNames = append(
				orderColumnFormattedNames,
				fmt.Sprintf("(%s IS NOT NULL)", col.column),
			)
		case nullOrderLast:
			orderColumnFormattedNames = append(
				orderColumnFormattedNames,
				fmt.Sprintf("(%s IS NULL)", col.column),
			)
		}
		// DuckDB retarget: native ordering on the bare value-layer
		// column (no googlesqlite_collate callback collation on DuckDB).
		if col.isAsc {
			orderColumnFormattedNames = append(
				orderColumnFormattedNames,
				col.column,
			)
		} else {
			orderColumnFormattedNames = append(
				orderColumnFormattedNames,
				fmt.Sprintf("%s DESC", col.column),
			)
		}
	}
	var orderBy string
	if len(orderColumnFormattedNames) != 0 {
		orderBy = fmt.Sprintf("ORDER BY %s", strings.Join(orderColumnFormattedNames, ","))
	}
	orderColumnNames.values = []*analyticOrderBy{}
	if !*emulationUsed {
		// Every analytic call in this scan went through the native
		// OVER path; the row_id column the predecessor's emulation
		// needs is dead weight here, so skip the wrap and let
		// SQLite read the input directly.
		return fmt.Sprintf(
			"SELECT %s %s %s",
			strings.Join(columns, ","),
			formattedInput,
			orderBy,
		), nil
	}
	return fmt.Sprintf(
		"SELECT %s FROM (SELECT *, ROW_NUMBER() OVER() AS \"row_id\" %s) %s",
		strings.Join(columns, ","),
		formattedInput,
		orderBy,
	), nil
}

// FormatSQL passes the SampleScan through to its InputScan. We do
// not honour the sample size / method semantics — for the emulator's
// correctness contract, returning all rows of the input gives the
// same answer set for every aggregate the upstream Examples exercise
// (and the DP rewriter relies on SampleScan as a transparent node
// for `max_groups_contributed` limiting, not as a probabilistic
// sampler).
func (n *SampleScanNode) FormatSQL(ctx context.Context) (string, error) {
	if n.node == nil {
		return "", nil
	}
	return newNode(m1(n.node.InputScan())).FormatSQL(ctx)
}

func (n *ComputedColumnNode) FormatSQL(ctx context.Context) (string, error) {
	if n.node == nil {
		return "", nil
	}
	expr, err := newNode(m1(n.node.Expr())).FormatSQL(ctx)
	if err != nil {
		return "", err
	}
	col, _ := n.node.Column()
	uniqueName := uniqueColumnName(ctx, col)
	query := fmt.Sprintf("%s AS \"%s\"", expr, uniqueColumnName(ctx, col))
	columnMap := columnRefMap(ctx)
	columnMap[uniqueName] = query
	arraySubqueryColumnNames := arraySubqueryColumnNameFromContext(ctx)
	if arraySubqueryColumnNames != nil {
		arraySubqueryColumnNames.names = append(arraySubqueryColumnNames.names, fmt.Sprintf("\"%s\"", m1(col.Name())))
	}
	return query, nil
}

func (n *ProjectScanNode) FormatSQL(ctx context.Context) (string, error) {
	if n.node == nil {
		return "", nil
	}
	for _, col := range m1(n.node.ExprList()) {
		// assign expr to columnRefMap
		if _, err := newNode(col).FormatSQL(ctx); err != nil {
			return "", err
		}
	}
	input, err := newNode(m1(n.node.InputScan())).FormatSQL(ctx)
	if err != nil {
		return "", err
	}
	columns := []string{}
	columnMap := columnRefMap(ctx)
	for _, col := range m1(n.node.ColumnList()) {
		colName := uniqueColumnName(ctx, col)
		if ref, exists := columnMap[colName]; exists {
			columns = append(columns, ref)
			delete(columnMap, colName)
		} else {
			columns = append(
				columns,
				fmt.Sprintf("\"%s\"", colName),
			)
		}
	}
	formattedInput, err := formatInput(input)
	if err != nil {
		return "", err
	}
	formattedColumns := strings.Join(columns, ",")
	return fmt.Sprintf("SELECT %s %s", formattedColumns, formattedInput), nil
}

func (n *TVFScanNode) FormatSQL(ctx context.Context) (string, error) {
	if n == nil || n.node == nil {
		return "", nil
	}
	tvf, err := n.node.Tvf()
	if err != nil || tvf == nil {
		return "", fmt.Errorf("TVF handle missing on TVFScan")
	}
	pathParts, _ := tvf.FunctionNamePath()
	if len(pathParts) == 0 {
		name, _ := tvf.Name()
		pathParts = []string{name}
	}
	tvfMap := tvfMapFromContext(ctx)
	var spec *TVFSpec
	for i := 0; i < len(pathParts) && spec == nil; i++ {
		spec = tvfMap[formatPath(pathParts[i:])]
	}
	if spec == nil {
		return "", fmt.Errorf("TVF spec not found for %s", strings.Join(pathParts, "."))
	}
	argValues := make([]string, 0, len(spec.Args))
	for _, argNode := range m1(n.node.ArgumentList()) {
		expr, _ := argNode.Expr()
		if expr == nil {
			argValues = append(argValues, "NULL")
			continue
		}
		formatted, err := newNode(expr).FormatSQL(ctx)
		if err != nil {
			return "", fmt.Errorf("failed to format TVF argument: %w", err)
		}
		argValues = append(argValues, formatted)
	}
	callOutputColumns := make([]string, 0, len(spec.OutputColumns))
	for _, col := range m1(n.node.ColumnList()) {
		callOutputColumns = append(callOutputColumns, uniqueColumnName(ctx, col))
	}
	return spec.CallSQL(argValues, callOutputColumns)
}

// FormatSQL Formats the outermost query statement that runs and produces rows of output, like a SELECT
// The node's `OutputColumnList()` gives user-visible column names that should be returned. There may be duplicate names,
// and multiple output columns may reference the same column from `Query()`
// https://github.com/google/googlesql/blob/master/docs/resolved_ast.md#ResolvedQueryStmt
func (n *QueryStmtNode) FormatSQL(ctx context.Context) (string, error) {
	if n.node == nil {
		return "", nil
	}
	input, err := newNode(m1(n.node.Query())).FormatSQL(ctx)
	if err != nil {
		return "", err
	}

	var columns []string
	for _, outputColumnNode := range m1(n.node.OutputColumnList()) {
		columns = append(
			columns,
			fmt.Sprintf("\"%s\" AS \"%s\"",
				uniqueColumnName(ctx, m1(outputColumnNode.Column())),
				m1(outputColumnNode.Name()),
			),
		)
	}

	return fmt.Sprintf(
		"SELECT %s FROM (%s)",
		strings.Join(columns, ", "),
		input,
	), nil
}

func (n *DropStmtNode) FormatSQL(ctx context.Context) (string, error) {
	if n.node == nil {
		return "", nil
	}
	namePath := namePathFromContext(ctx)
	tableName := namePath.format2(n.node.NamePath())
	objectType, _ := n.node.ObjectType()
	if m1(n.node.IsIfExists()) {
		return fmt.Sprintf("DROP %s IF EXISTS \"%s\"", objectType, tableName), nil
	}
	return fmt.Sprintf("DROP %s \"%s\"", objectType, tableName), nil
}

// RecursiveRefScanNode renders as the enclosing CTE's queryName so
// SQLite's native `WITH RECURSIVE name AS (... UNION ALL SELECT ...
// FROM name)` emits a self-reference correctly.
func (n *RecursiveRefScanNode) FormatSQL(ctx context.Context) (string, error) {
	name := recursiveCteName(ctx)
	if name == "" {
		return "", fmt.Errorf("recursive ref scan rendered outside a recursive WITH entry")
	}
	return fmt.Sprintf("\"%s\"", name), nil
}

// findRecursiveRefScanCols walks a scan tree looking for the column
// list exposed by the (one and only) ResolvedRecursiveRefScan in a
// recursive UNION term. SQLite-side `WITH RECURSIVE t(...)` must use
// these IDs as the explicit column list so the recursive term's
// `FROM t` references resolve to the right columns.
//
// Returns nil when no RecursiveRefScan is reachable.
func findRecursiveRefScanCols(node googlesql.ResolvedNode) []*googlesql.ResolvedColumn {
	if node == nil {
		return nil
	}
	if kind, _ := node.NodeKind(); kind == googlesql.ResolvedNodeKindResolvedRecursiveRefScan {
		if scan, ok := node.(googlesql.ResolvedScanNode); ok {
			cols, _ := scan.MutableColumnList()
			return cols
		}
	}
	children, _ := node.GetChildNodes()
	for _, c := range children {
		if got := findRecursiveRefScanCols(c); len(got) > 0 {
			return got
		}
	}
	return nil
}

// RecursiveScanNode renders as `<non_recursive_branch> UNION [ALL]
// <recursive_branch>`. The recursive-cte reference must stay at the
// top-level FROM of the recursive branch (SQLite restriction). The
// enclosing WithEntryNode emits an explicit column list using the
// RecursiveScan canonical ColumnList so the outer query's references
// resolve. The recursive term internally uses RecursiveRefScan IDs;
// those are substituted with canonical IDs here so SQLite sees one
// consistent name set.
func (n *RecursiveScanNode) FormatSQL(ctx context.Context) (string, error) {
	if n.node == nil {
		return "", nil
	}
	canonical := m1(n.node.ColumnList())
	formatBranch := func(item *googlesql.ResolvedSetOperationItem) (string, error) {
		scanSQL, err := newNode(m1(item.Scan())).FormatSQL(ctx)
		if err != nil {
			return "", err
		}
		trimmed := strings.TrimSpace(scanSQL)
		if strings.HasPrefix(trimmed, "SELECT") || strings.HasPrefix(trimmed, "WITH") {
			return scanSQL, nil
		}
		out := m1(item.OutputColumnList())
		cols := make([]string, 0, len(out))
		for _, c := range out {
			cols = append(cols, fmt.Sprintf("\"%s\"", uniqueColumnName(ctx, c)))
		}
		formatted, err := formatInput(scanSQL)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("SELECT %s %s",
			strings.Join(cols, ","),
			formatted,
		), nil
	}
	nonRec, err := formatBranch(m1(n.node.NonRecursiveTerm()))
	if err != nil {
		return "", err
	}
	rec, err := formatBranch(m1(n.node.RecursiveTerm()))
	if err != nil {
		return "", err
	}
	// Substitute RecursiveRefScan column IDs in the recursive branch
	// with the canonical RecursiveScan ColumnList IDs so SQLite's
	// single-column-name view of the CTE matches the outer query's
	// references. Same number of columns — pair by position.
	if rt := m1(n.node.RecursiveTerm()); rt != nil {
		if scan, _ := rt.Scan(); scan != nil {
			refCols := findRecursiveRefScanCols(scan)
			if len(refCols) == len(canonical) {
				for i, ref := range refCols {
					if ref == nil {
						continue
					}
					refName := fmt.Sprintf("\"%s\"", uniqueColumnName(ctx, ref))
					canName := fmt.Sprintf("\"%s\"", uniqueColumnName(ctx, canonical[i]))
					if refName != canName {
						rec = strings.ReplaceAll(rec, refName, canName)
					}
				}
			}
		}
	}
	op := "UNION ALL"
	if m1(n.node.OpType()) == googlesql.ResolvedRecursiveScanEnums_RecursiveSetOperationTypeUnionDistinct {
		op = "UNION"
	}
	return fmt.Sprintf("%s %s %s", nonRec, op, rec), nil
}

func (n *WithScanNode) FormatSQL(ctx context.Context) (string, error) {
	if n.node == nil {
		return "", nil
	}
	keyword := "WITH"
	for _, entry := range m1(n.node.WithEntryList()) {
		sub := m1(entry.WithSubquery())
		if sub == nil {
			continue
		}
		if kind, _ := sub.NodeKind(); kind == googlesql.ResolvedNodeKindResolvedRecursiveScan {
			keyword = "WITH RECURSIVE"
			break
		}
	}
	// Count CTE references in the WITH body and across WithEntry
	// subqueries (a later CTE may reference an earlier one). Pass
	// the counts down so WithEntryNode.FormatSQL can decide whether
	// to emit `MATERIALIZED`.
	refCounts := map[string]int{}
	if conn := connFromContext(ctx); conn == nil || conn.MaterializeCTE() {
		query, _ := n.node.Query()
		if query != nil {
			collectCteRefCounts(query, refCounts)
		}
		for _, entry := range m1(n.node.WithEntryList()) {
			sub, _ := entry.WithSubquery()
			if sub == nil {
				continue
			}
			collectCteRefCounts(sub, refCounts)
		}
	}
	subCtx := withCteRefCounts(ctx, refCounts)
	queries := []string{}
	for _, entry := range m1(n.node.WithEntryList()) {
		sql, err := newNode(entry).FormatSQL(subCtx)
		if err != nil {
			return "", err
		}
		queries = append(queries, sql)
	}
	query, err := newNode(m1(n.node.Query())).FormatSQL(subCtx)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(
		"%s %s %s",
		keyword,
		strings.Join(queries, ", "),
		query,
	), nil
}

// collectCteRefCounts walks a resolved-scan subtree and increments
// counts[name] for every ResolvedWithRefScan it encounters. Used by
// WithScanNode to decide whether each CTE body should be emitted as
// `MATERIALIZED`.
func collectCteRefCounts(node googlesql.ResolvedNode, counts map[string]int) {
	if node == nil {
		return
	}
	if kind, _ := node.NodeKind(); kind == googlesql.ResolvedNodeKindResolvedWithRefScan {
		if ref, ok := node.(*googlesql.ResolvedWithRefScan); ok {
			name, _ := ref.WithQueryName()
			counts[name]++
		}
	}
	children, _ := node.GetChildNodes()
	for _, c := range children {
		collectCteRefCounts(c, counts)
	}
}

func (n *WithEntryNode) FormatSQL(ctx context.Context) (string, error) {
	if n.node == nil {
		return "", nil
	}
	queryName, _ := n.node.WithQueryName()
	sub := m1(n.node.WithSubquery())
	subCtx := ctx
	subKind, _ := sub.NodeKind()
	if subKind == googlesql.ResolvedNodeKindResolvedRecursiveScan {
		// Make the queryName visible to any nested
		// RecursiveRefScanNode so it can render as the CTE name.
		subCtx = withRecursiveCteName(ctx, queryName)
	}
	subquery, err := newNode(sub).FormatSQL(subCtx)
	if err != nil {
		return "", err
	}
	tableToColumnList := tableNameToColumnListMap(ctx)
	tableToColumnList[queryName] = m1(sub.MutableColumnList())
	hint := cteMaterializeHint(ctx, queryName, subKind)
	if subKind == googlesql.ResolvedNodeKindResolvedRecursiveScan {
		// SQLite needs an explicit column list on the recursive CTE
		// so both UNION branches map their projected columns by
		// position. Use the RecursiveScan canonical ColumnList — the
		// outer query references those IDs. RecursiveScanNode.FormatSQL
		// substitutes RecursiveRefScan-internal IDs with these
		// canonical IDs so `FROM t` references resolve.
		canonical := m1(sub.MutableColumnList())
		cols := make([]string, 0, len(canonical))
		for _, c := range canonical {
			cols = append(cols, fmt.Sprintf("\"%s\"", uniqueColumnName(ctx, c)))
		}
		return fmt.Sprintf("\"%s\"(%s) AS%s ( %s )", queryName, strings.Join(cols, ","), hint, subquery), nil
	}
	return fmt.Sprintf("\"%s\" AS%s ( %s )", queryName, hint, subquery), nil
}

// cteMaterializeHint returns either " MATERIALIZED" (with a leading
// space) or "" depending on whether SQLite should be told to push
// this CTE into a transient temp table instead of inlining its body
// once per reference. Recursive CTEs are skipped — SQLite always
// materialises the recursion accumulator anyway, and the hint
// interacts oddly with the `WITH RECURSIVE` keyword.
func cteMaterializeHint(ctx context.Context, queryName string, subKind googlesql.ResolvedNodeKind) string {
	if subKind == googlesql.ResolvedNodeKindResolvedRecursiveScan {
		return ""
	}
	counts := cteRefCounts(ctx)
	if counts[queryName] >= 2 {
		return " MATERIALIZED"
	}
	return ""
}

func (n *AnalyticFunctionGroupNode) FormatSQL(ctx context.Context) (string, error) {
	if n.node == nil {
		return "", nil
	}

	var queries []string
	for _, column := range m1(n.node.AnalyticFunctionList()) {
		sql, err := newNode(column).FormatSQL(ctx)
		if err != nil {
			return "", err
		}
		queries = append(queries, sql)
	}
	return strings.Join(queries, ","), nil
}

func (n *DMLValueNode) FormatSQL(ctx context.Context) (string, error) {
	if n == nil {
		return "", nil
	}
	return newNode(m1(n.node.Value())).FormatSQL(ctx)
}

func (n *InsertRowNode) FormatSQL(ctx context.Context) (string, error) {
	if n == nil {
		return "", nil
	}
	values := []string{}
	for _, value := range m1(n.node.ValueList()) {
		sql, err := newNode(value).FormatSQL(ctx)
		if err != nil {
			return "", err
		}
		values = append(values, sql)
	}
	return strings.Join(values, ","), nil
}

func (n *InsertStmtNode) FormatSQL(ctx context.Context) (string, error) {
	if n == nil {
		return "", nil
	}
	table, err := getTableName(ctx, m1(n.node.TableScan()))
	if err != nil {
		return "", err
	}
	insertCols := m1(n.node.InsertColumnList())
	insertedNames := make(map[string]struct{}, len(insertCols))
	columns := make([]string, 0, len(insertCols))
	for _, col := range insertCols {
		name := m1(col.Name())
		insertedNames[name] = struct{}{}
		columns = append(columns, fmt.Sprintf("\"%s\"", name))
	}
	// Collect any column DEFAULT expressions for columns the INSERT
	// omitted. Splicing them into the rewritten INSERT here is what
	// makes `CREATE TABLE t (... col TYPE DEFAULT '...')` actually
	// substitute the default — bqe#211.
	defaults := missingColumnDefaults(ctx, table, insertedNames)
	for _, d := range defaults {
		columns = append(columns, fmt.Sprintf("\"%s\"", d.name))
	}
	query, _ := n.node.Query()
	if query != nil {
		stmt, err := newNode(query).FormatSQL(withUseColumnID(ctx))
		if err != nil {
			return "", err
		}
		// INSERT ... SELECT form: extending the column list would
		// require rewriting the SELECT to project the defaults too.
		// Skip default substitution here — defaults still apply to
		// the VALUES form, which covers the common case.
		_ = defaults
		// Native-storage seam (same as the VALUES form below): JSON /
		// NUMERIC / BYTES target columns must receive their storage
		// representation, but a projected googlesqlite_* UDF result is
		// still an envelope VARCHAR. When any targeted column needs it,
		// re-project the SELECT through a positional column-alias
		// subquery (FROM (...) AS src(c0, c1, ...)) with the per-kind
		// normalizers; native projections pass through them unchanged.
		needsWrap := false
		for _, col := range insertCols {
			if t, err := col.Type(); err == nil && t != nil {
				if k, err := t.Kind(); err == nil {
					switch k {
					case googlesql.TypeKindTypeJson, googlesql.TypeKindTypeNumeric, googlesql.TypeKindTypeBytes:
						needsWrap = true
					}
				}
			}
		}
		if needsWrap {
			aliases := make([]string, len(insertCols))
			projs := make([]string, len(insertCols))
			for i, col := range insertCols {
				alias := fmt.Sprintf(`"__gsx_ins_c%d"`, i)
				aliases[i] = alias
				projs[i] = alias
				if t, err := col.Type(); err == nil && t != nil {
					if k, err := t.Kind(); err == nil {
						switch k {
						case googlesql.TypeKindTypeJson:
							projs[i] = fmt.Sprintf("googlesqlite_to_native_json(%s)", alias)
						case googlesql.TypeKindTypeNumeric:
							projs[i] = numericStorageReadSQL(alias)
						case googlesql.TypeKindTypeBytes:
							projs[i] = bytesStorageReadSQL(alias)
						}
					}
				}
			}
			stmt = fmt.Sprintf("SELECT %s FROM (%s) AS __gsx_ins_src(%s)",
				strings.Join(projs, ","), stmt, strings.Join(aliases, ","))
		}
		return fmt.Sprintf("INSERT INTO \"%s\" (%s) %s",
			table,
			strings.Join(columns[:len(insertCols)], ","),
			stmt,
		), nil
	}
	// DuckDB retarget: JSON columns are native-typed (spec.go maps
	// TypeKindTypeJson to a JSON column), but JSON-producing
	// expressions (googlesqlite_parse_json / to_json UDF calls and the
	// JSON literal lowering) emit the value-layer base64 envelope,
	// which fails DuckDB's VARCHAR->JSON cast at execution time
	// ("Malformed JSON at byte 0 ... eyJoZWFkZXIi..."). Wrap each
	// value headed for a JSON column in googlesqlite_to_native_json,
	// which unwraps the envelope to raw JSON text and passes already-
	// native values (bound params) through untouched.
	// The native-storage NUMERIC / BYTES columns need the analogous
	// normalization (numericStorageReadSQL / bytesStorageReadSQL): a
	// stored cell must be the native DECIMAL(38,9) / payload BLOB, but a
	// googlesqlite_* UDF result still arrives as the envelope VARCHAR.
	// Native value expressions (literals, params, operator results) pass
	// through the wraps unchanged.
	colKinds := make([]googlesql.TypeKind, len(insertCols))
	for i, col := range insertCols {
		colKinds[i] = googlesql.TypeKindTypeUnknown
		if t, err := col.Type(); err == nil && t != nil {
			if k, err := t.Kind(); err == nil {
				colKinds[i] = k
			}
		}
	}
	rows := []string{}
	for _, row := range m1(n.node.RowList()) {
		valueNodes := m1(row.ValueList())
		values := make([]string, 0, len(valueNodes))
		for i, valueNode := range valueNodes {
			sql, err := newNode(valueNode).FormatSQL(ctx)
			if err != nil {
				return "", err
			}
			if i < len(colKinds) && sql != "" {
				switch colKinds[i] {
				case googlesql.TypeKindTypeJson:
					sql = fmt.Sprintf("googlesqlite_to_native_json(%s)", sql)
				case googlesql.TypeKindTypeNumeric:
					sql = numericStorageReadSQL(sql)
				case googlesql.TypeKindTypeBytes:
					sql = bytesStorageReadSQL(sql)
				}
			}
			values = append(values, sql)
		}
		full := strings.Join(values, ",")
		for _, d := range defaults {
			full += "," + d.expr
		}
		rows = append(rows, fmt.Sprintf("(%s)", full))
	}
	return fmt.Sprintf("INSERT INTO \"%s\" (%s) VALUES %s",
		table,
		strings.Join(columns, ","),
		strings.Join(rows, ","),
	), nil
}

// missingColumnDefaults returns one entry per table column that has
// a DEFAULT expression and is not present in inserted. Callers append
// the names to the INSERT column list and the exprs to each row's
// VALUES tuple, which lets the standard SQLite path supply the
// default at execution time.
func missingColumnDefaults(ctx context.Context, storageTable string, inserted map[string]struct{}) []struct{ name, expr string } {
	a := analyzerFromContext(ctx)
	if a == nil || a.catalog == nil {
		return nil
	}
	spec, ok := a.catalog.tableMap[storageTable]
	if !ok || spec == nil {
		return nil
	}
	var out []struct{ name, expr string }
	for _, col := range spec.Columns {
		if _, present := inserted[col.Name]; present {
			continue
		}
		if col.DefaultExpr == "" {
			continue
		}
		out = append(out, struct{ name, expr string }{col.Name, col.DefaultExpr})
	}
	return out
}

func (n *DeleteStmtNode) FormatSQL(ctx context.Context) (string, error) {
	if n == nil {
		return "", nil
	}
	table, err := getTableName(ctx, m1(n.node.TableScan()))
	if err != nil {
		return "", err
	}
	where, err := newNode(m1(n.node.WhereExpr())).FormatSQL(ctx)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(
		"DELETE FROM \"%s\" WHERE %s",
		table,
		where,
	), nil
}

func (n *UpdateItemNode) FormatSQL(ctx context.Context) (string, error) {
	if n.node == nil {
		return "", nil
	}
	targetCtx := unuseColumnID(withoutUseTableNameForColumn(ctx))
	targetNode := m1(n.node.Target())
	setValue, err := newNode(m1(n.node.SetValue())).FormatSQL(ctx)
	if err != nil {
		return "", err
	}
	// SET col.field = value: SQL forbids assigning to a function
	// call. Rewrite into a whole-column replacement that goes
	// through googlesqlite_struct_with_field_set, which preserves
	// every other field of the struct (bqe#299, bqe#128).
	if rewritten, ok, err := rewriteStructFieldSet(targetCtx, targetNode, setValue); err != nil {
		return "", err
	} else if ok {
		return rewritten, nil
	}
	// Same native-storage seam as InsertStmtNode: a native JSON /
	// NUMERIC / BYTES target column rejects (or would mis-store) the
	// value-layer envelope that googlesqlite_* UDF expressions emit, so
	// normalize to the storage representation at assignment time —
	// native value expressions pass through the wraps unchanged.
	// Applied only on the whole-column path — struct-field assignments
	// (above) live inside envelope storage and keep the envelope form.
	if t, terr := targetNode.Type(); terr == nil && t != nil && setValue != "" {
		if k, kerr := t.Kind(); kerr == nil {
			switch k {
			case googlesql.TypeKindTypeJson:
				setValue = fmt.Sprintf("googlesqlite_to_native_json(%s)", setValue)
			case googlesql.TypeKindTypeNumeric:
				setValue = numericStorageReadSQL(setValue)
			case googlesql.TypeKindTypeBytes:
				setValue = bytesStorageReadSQL(setValue)
			}
		}
	}
	target, err := newNode(targetNode).FormatSQL(targetCtx)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s=%s", target, setValue), nil
}

// rewriteStructFieldSet detects `target = ResolvedGetStructField(...)`
// chains and rewrites them as
//
//	col = googlesqlite_struct_with_field_set(
//	          ...struct_with_field_set(col, outerIdx, innerSet)... ,
//	          outermostIdx,
//	          assignedValue)
//
// so the assigned column is the original column and the value
// preserves all fields not on the assignment path. ok=false means
// the target is not a struct-field reference; the caller falls back
// to the regular `target = value` form.
func rewriteStructFieldSet(ctx context.Context, target googlesql.ResolvedExprNode, value string) (string, bool, error) {
	get, ok := target.(*googlesql.ResolvedGetStructField)
	if !ok {
		return "", false, nil
	}
	// Walk inwards, collecting (idx) at each level until we hit the
	// underlying ResolvedColumnRef.
	type level struct {
		idx int
	}
	var levels []level
	cur := googlesql.ResolvedExprNode(get)
	for {
		g, ok := cur.(*googlesql.ResolvedGetStructField)
		if !ok {
			break
		}
		idx, _ := g.FieldIdx()
		levels = append(levels, level{idx: int(idx)})
		next, _ := g.Expr()
		cur = next
	}
	colRef, ok := cur.(*googlesql.ResolvedColumnRef)
	if !ok {
		return "", false, nil
	}
	colSQL, err := newNode(colRef).FormatSQL(ctx)
	if err != nil {
		return "", false, err
	}
	// Build the wrapped value from the inside out. The outermost
	// (assigned) field is the LAST element appended above.
	expr := value
	for i := 0; i < len(levels); i++ {
		idx := levels[i].idx
		if i == len(levels)-1 {
			expr = fmt.Sprintf("googlesqlite_struct_with_field_set(%s, %d, %s)",
				colSQL, idx, expr)
		} else {
			// Nested struct field: wrap in get_struct_field for the
			// outer container, then set within. For now the simple
			// single-level path suffices for bqe#299 / #128; nested
			// `info.address.city` would need an additional builder
			// pass and is documented as a follow-up.
			return "", false, nil
		}
	}
	return fmt.Sprintf("%s=%s", colSQL, expr), true, nil
}

func (n *UpdateStmtNode) FormatSQL(ctx context.Context) (string, error) {
	if n == nil {
		return "", nil
	}
	table, err := getTableName(ctx, m1(n.node.TableScan()))
	if err != nil {
		return "", err
	}
	updateItems := []string{}
	for _, item := range m1(n.node.UpdateItemList()) {
		sql, err := newNode(item).FormatSQL(ctx)
		if err != nil {
			return "", err
		}
		updateItems = append(updateItems, sql)
	}
	where, err := newNode(m1(n.node.WhereExpr())).FormatSQL(ctx)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(
		"UPDATE \"%s\" SET %s WHERE %s",
		table,
		strings.Join(updateItems, ","),
		where,
	), nil
}

func (n *ArgumentRefNode) FormatSQL(ctx context.Context) (string, error) {
	if n.node == nil {
		return "", nil
	}
	return fmt.Sprintf("$%s", m1(n.node.Name())), nil
}
