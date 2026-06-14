package internal

import (
	"context"
	"sync"
	"time"

	googlesql "github.com/goccy/go-googlesql"
)

type (
	analyzerKey                     struct{}
	namePathKey                     struct{}
	columnRefMapKey                 struct{}
	funcMapKey                      struct{}
	tvfMapKey                       struct{}
	systemVarsKey                   struct{}
	connKey                         struct{}
	cteRefCountsKey                 struct{}
	cteSetOperationOrdinalKey       struct{}
	analyticOrderColumnNamesKey     struct{}
	analyticPartitionColumnNamesKey struct{}
	analyticInputScanKey            struct{}
	analyticEmulationFlagKey        struct{}
	arraySubqueryColumnNameKey      struct{}
	arrayAggTopNRewriteKey          struct{}
	currentTimeKey                  struct{}
	tableNameToColumnListMapKey     struct{}
	useColumnIDKey                  struct{}
	useTableNameForColumnKey        struct{}
	paramCollectorKey               struct{}
	safeEvalModeKey                 struct{}
	sqlCollectorKey                 struct{}
)

// withSafeEvalMode marks a sub-context as the argument to IFERROR /
// ISERROR / NULLIFERROR. When set, getFuncNameAndArgs routes normal
// function calls through their `googlesqlite_safe_<name>` variants
// (NULL-on-error) instead of the raising form, and the formatter
// short-circuits inner ERROR(msg) calls to a SQL NULL literal so
// DuckDB never has to abort the statement on the would-be error.
func withSafeEvalMode(ctx context.Context) context.Context {
	return context.WithValue(ctx, safeEvalModeKey{}, true)
}

func inSafeEvalMode(ctx context.Context) bool {
	v := ctx.Value(safeEvalModeKey{})
	if v == nil {
		return false
	}
	b, ok := v.(bool)
	return ok && b
}

func analyzerFromContext(ctx context.Context) *Analyzer {
	value := ctx.Value(analyzerKey{})
	if value == nil {
		return nil
	}
	return value.(*Analyzer)
}

func withAnalyzer(ctx context.Context, analyzer *Analyzer) context.Context {
	return context.WithValue(ctx, analyzerKey{}, analyzer)
}

func namePathFromContext(ctx context.Context) *NamePath {
	value := ctx.Value(namePathKey{})
	if value == nil {
		return nil
	}
	return value.(*NamePath)
}

func withNamePath(ctx context.Context, namePath *NamePath) context.Context {
	return context.WithValue(ctx, namePathKey{}, namePath)
}

func withColumnRefMap(ctx context.Context, m map[string]string) context.Context {
	return context.WithValue(ctx, columnRefMapKey{}, m)
}

func columnRefMap(ctx context.Context) map[string]string {
	value := ctx.Value(columnRefMapKey{})
	if value == nil {
		return nil
	}
	return value.(map[string]string)
}

func withFuncMap(ctx context.Context, m map[string]*FunctionSpec) context.Context {
	return context.WithValue(ctx, funcMapKey{}, m)
}

func funcMapFromContext(ctx context.Context) map[string]*FunctionSpec {
	value := ctx.Value(funcMapKey{})
	if value == nil {
		return nil
	}
	return value.(map[string]*FunctionSpec)
}

func withTVFMap(ctx context.Context, m map[string]*TVFSpec) context.Context {
	return context.WithValue(ctx, tvfMapKey{}, m)
}

func tvfMapFromContext(ctx context.Context) map[string]*TVFSpec {
	value := ctx.Value(tvfMapKey{})
	if value == nil {
		return nil
	}
	return value.(map[string]*TVFSpec)
}

// withSystemVars carries the session-scope @@system_variable map
// through the formatter so SystemVariableNode.FormatSQL can inline
// the current value as a literal.
func withSystemVars(ctx context.Context, m map[string]string) context.Context {
	return context.WithValue(ctx, systemVarsKey{}, m)
}

func systemVarsFromContext(ctx context.Context) map[string]string {
	value := ctx.Value(systemVarsKey{})
	if value == nil {
		return nil
	}
	return value.(map[string]string)
}

func withConn(ctx context.Context, conn *Conn) context.Context {
	return context.WithValue(ctx, connKey{}, conn)
}

func connFromContext(ctx context.Context) *Conn {
	value := ctx.Value(connKey{})
	if value == nil {
		return nil
	}
	return value.(*Conn)
}

// withCteRefCounts carries a CTE-name → reference-count map through
// the formatter, populated by WithScanNode just before it formats
// the WITH entries. WithEntryNode reads it to decide whether to
// emit the DuckDB `MATERIALIZED` hint for entries that are
// referenced more than once.
func withCteRefCounts(ctx context.Context, m map[string]int) context.Context {
	return context.WithValue(ctx, cteRefCountsKey{}, m)
}

func cteRefCounts(ctx context.Context) map[string]int {
	value := ctx.Value(cteRefCountsKey{})
	if value == nil {
		return nil
	}
	return value.(map[string]int)
}

func withCteSetOperationOrdinals(ctx context.Context, m map[string]bool) context.Context {
	return context.WithValue(ctx, cteSetOperationOrdinalKey{}, m)
}

func cteSetOperationOrdinals(ctx context.Context) map[string]bool {
	value := ctx.Value(cteSetOperationOrdinalKey{})
	if value == nil {
		return nil
	}
	return value.(map[string]bool)
}

// nullOrderMode represents the OVER (ORDER BY ... NULLS FIRST/LAST)
// clause as captured from the resolved AST. The default zero value
// "unspecified" leaves null placement to the DuckDB default for the
// emitted ORDER BY expression.
type nullOrderMode int

const (
	nullOrderUnspecified nullOrderMode = iota
	nullOrderFirst
	nullOrderLast
)

type analyticOrderBy struct {
	column    string
	isAsc     bool
	nullOrder nullOrderMode
	// sortExpr is the SQL expression to use when this entry appears as a
	// native ORDER BY sort key (window OVER clause or the analytic scan's
	// trailing output ORDER BY). For value-layer STRING / UUID columns the
	// base64 envelope text does NOT order like the payload, so sortExpr
	// carries the googlesqlite_plain_string unwrap; for every other kind
	// it equals column. The emulation argument path keeps using column
	// (it needs the envelope form for value-layer decoding).
	sortExpr string
	// fromPartition marks entries that were derived from the window's
	// PARTITION BY keys rather than its ORDER BY items. The native OVER
	// clause must not repeat partition keys inside ORDER BY: they are
	// constant per partition (semantically a no-op) and a RANGE frame
	// with an offset requires exactly one ORDER BY key, which a leading
	// partition key would break.
	fromPartition bool
}

// sortSQL returns the expression to order by for this entry, preferring
// the native sortExpr when set.
func (a *analyticOrderBy) sortSQL() string {
	if a.sortExpr != "" {
		return a.sortExpr
	}
	return a.column
}

type analyticOrderColumnNames struct {
	values []*analyticOrderBy
}

func withAnalyticOrderColumnNames(ctx context.Context, v *analyticOrderColumnNames) context.Context {
	return context.WithValue(ctx, analyticOrderColumnNamesKey{}, v)
}

func analyticOrderColumnNamesFromContext(ctx context.Context) *analyticOrderColumnNames {
	value := ctx.Value(analyticOrderColumnNamesKey{})
	if value == nil {
		return nil
	}
	return value.(*analyticOrderColumnNames)
}

func withAnalyticPartitionColumnNames(ctx context.Context, names []string) context.Context {
	return context.WithValue(ctx, analyticPartitionColumnNamesKey{}, names)
}

func analyticPartitionColumnNamesFromContext(ctx context.Context) []string {
	value := ctx.Value(analyticPartitionColumnNamesKey{})
	if value == nil {
		return nil
	}
	return value.([]string)
}

func withAnalyticInputScan(ctx context.Context, input string) context.Context {
	return context.WithValue(ctx, analyticInputScanKey{}, input)
}

func analyticInputScanFromContext(ctx context.Context) string {
	value := ctx.Value(analyticInputScanKey{})
	if value == nil {
		return ""
	}
	return value.(string)
}

// withAnalyticEmulationFlag attaches a fresh "emulation used" flag to
// ctx. AnalyticScanNode installs the flag, then reads it after
// formatting all of its function groups; if no AnalyticFunctionCallNode
// flipped it, the row_id wrap is unnecessary and can be skipped.
func withAnalyticEmulationFlag(ctx context.Context) (context.Context, *bool) {
	used := new(bool)
	return context.WithValue(ctx, analyticEmulationFlagKey{}, used), used
}

// markAnalyticEmulationUsed flips the closest enclosing emulation flag
// to true. Safe to call when no flag is installed (no-op).
func markAnalyticEmulationUsed(ctx context.Context) {
	if v := ctx.Value(analyticEmulationFlagKey{}); v != nil {
		*v.(*bool) = true
	}
}

type arraySubqueryColumnNames struct {
	names []string
}

func withArraySubqueryColumnName(ctx context.Context, v *arraySubqueryColumnNames) context.Context {
	return context.WithValue(ctx, arraySubqueryColumnNameKey{}, v)
}

func arraySubqueryColumnNameFromContext(ctx context.Context) *arraySubqueryColumnNames {
	value := ctx.Value(arraySubqueryColumnNameKey{})
	if value == nil {
		return nil
	}
	return value.(*arraySubqueryColumnNames)
}

func withArrayAggTopNRewrite(ctx context.Context, plans map[string]arrayAggTopNRewritePlan) context.Context {
	if len(plans) == 0 {
		return ctx
	}
	merged := map[string]arrayAggTopNRewritePlan{}
	if existing := arrayAggTopNRewriteFromContext(ctx); existing != nil {
		for k, v := range existing {
			merged[k] = v
		}
	}
	for k, v := range plans {
		merged[k] = v
	}
	return context.WithValue(ctx, arrayAggTopNRewriteKey{}, merged)
}

func arrayAggTopNRewriteFromContext(ctx context.Context) map[string]arrayAggTopNRewritePlan {
	value := ctx.Value(arrayAggTopNRewriteKey{})
	if value == nil {
		return nil
	}
	return value.(map[string]arrayAggTopNRewritePlan)
}

func withUseColumnID(ctx context.Context) context.Context {
	return context.WithValue(ctx, useColumnIDKey{}, true)
}

func useColumnID(ctx context.Context) bool {
	value := ctx.Value(useColumnIDKey{})
	if value == nil {
		return false
	}
	return value.(bool)
}

func unuseColumnID(ctx context.Context) context.Context {
	return context.WithValue(ctx, useColumnIDKey{}, false)
}

func withoutUseTableNameForColumn(ctx context.Context) context.Context {
	return context.WithValue(ctx, useTableNameForColumnKey{}, false)
}

func useTableNameForColumn(ctx context.Context) bool {
	value := ctx.Value(useTableNameForColumnKey{})
	if value == nil {
		return false
	}
	return value.(bool)
}

func withTableNameToColumnListMap(ctx context.Context, v map[string][]*googlesql.ResolvedColumn) context.Context {
	return context.WithValue(ctx, tableNameToColumnListMapKey{}, v)
}

// recursiveCteNameKey carries the name of the CTE currently being
// resolved as a recursive WITH entry, so a ResolvedRecursiveRefScan
// nested inside its body can render as that name.
type recursiveCteNameKey struct{}

func withRecursiveCteName(ctx context.Context, name string) context.Context {
	return context.WithValue(ctx, recursiveCteNameKey{}, name)
}

func recursiveCteName(ctx context.Context) string {
	value := ctx.Value(recursiveCteNameKey{})
	if value == nil {
		return ""
	}
	return value.(string)
}

func tableNameToColumnListMap(ctx context.Context) map[string][]*googlesql.ResolvedColumn {
	value := ctx.Value(tableNameToColumnListMapKey{})
	if value == nil {
		return nil
	}
	return value.(map[string][]*googlesql.ResolvedColumn)
}

// paramCollector is a side-channel collector populated as the
// formatter walks the resolved AST. The wasm bridge does not expose a
// generic descendants walker on ResolvedNode in v0.1.0, so this is the
// only path that surfaces every ResolvedParameter encountered during
// SQL formatting.
type paramCollector struct {
	params []*googlesql.ResolvedParameter
}

func withParamCollector(ctx context.Context, c *paramCollector) context.Context {
	return context.WithValue(ctx, paramCollectorKey{}, c)
}

func paramCollectorFromContext(ctx context.Context) *paramCollector {
	v := ctx.Value(paramCollectorKey{})
	if v == nil {
		return nil
	}
	return v.(*paramCollector)
}

// SQLCollector accumulates the DuckDB queries that the formatter emits
// while analyzing GoogleSQL statements. It is the introspection hook
// behind the CLI's debug mode: the caller installs one on the context
// via NewSQLCollectorContext, runs a statement through the driver, and
// reads back the translated DuckDB text with Queries.
//
// It lives in the internal package so cmd/googlesqlite (and the wasm
// Playground) can surface the translated SQL without the public
// googlesqlite API having to grow a debug surface.
type SQLCollector struct {
	mu      sync.Mutex
	queries []string
}

// Add records one translated DuckDB query. Called by the formatter
// chokepoint (collectFormatParams) once per analyzed statement.
func (c *SQLCollector) Add(query string) {
	c.mu.Lock()
	c.queries = append(c.queries, query)
	c.mu.Unlock()
}

// Queries returns a copy of every translated DuckDB query collected so
// far, in analysis order.
func (c *SQLCollector) Queries() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.queries))
	copy(out, c.queries)
	return out
}

// NewSQLCollectorContext derives a context carrying a fresh
// SQLCollector. Pass the returned context to database/sql's
// QueryContext / ExecContext and the analyzer will populate the
// collector with the DuckDB text it generates.
func NewSQLCollectorContext(ctx context.Context) (context.Context, *SQLCollector) {
	c := &SQLCollector{}
	return context.WithValue(ctx, sqlCollectorKey{}, c), c
}

func sqlCollectorFromContext(ctx context.Context) *SQLCollector {
	v := ctx.Value(sqlCollectorKey{})
	if v == nil {
		return nil
	}
	return v.(*SQLCollector)
}

func WithCurrentTime(ctx context.Context, now time.Time) context.Context {
	return context.WithValue(ctx, currentTimeKey{}, &now)
}

func CurrentTime(ctx context.Context) *time.Time {
	value := ctx.Value(currentTimeKey{})
	if value == nil {
		return nil
	}
	return value.(*time.Time)
}
