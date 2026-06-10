package internal

// Analysis cache (issue 7): the GoogleSQL analyze step — parser-AST
// pre-pass walks, AnalyzeStatementFromParserAST, and the resolved-AST
// formatting walk — runs on the go-googlesql wasm module and costs
// two orders of magnitude more than engine execution. Workloads
// repeat identical query text constantly (the bigquery-emulator runs
// its metadata lookups through this driver on every REST request), so
// Analyzer.Analyze caches its per-statement output keyed by the
// post-rewrite query text, the parameter shape, the analyzer name
// path, and a catalog generation counter.
//
// What is cached is deliberately the *downstream* artifact, not the
// resolved AST: the formatted DuckDB/SQLite SQL, the output column
// specs, and the resolved parameter list. The ResolvedParameter and
// type-node wrappers are wasm handles; holding them in the cache
// keeps their backing wasm objects alive (go-googlesql wrappers pin
// their parents via the keepAlive convention), so they stay valid for
// the cache entry's lifetime.
//
// Invalidation: every semantic catalog mutation (table / function /
// TVF add, replace, delete, catalog reset, proto / enum registration,
// property-graph add) bumps the catalog's analysis generation and
// clears the cache. Entries also record the generation they were
// built under and are only stored if the generation is unchanged, so
// an analysis that races a DDL is never cached.
//
// Conn-local state that can influence formatting disables caching for
// that call: system variables (consulted by the formatter through
// context) skip the cache entirely; script variables are already
// applied as text rewrites before the key is computed; the
// materialize-CTE flag is part of the key.

import (
	"database/sql/driver"
	"fmt"
	"strings"
	"sync/atomic"

	googlesql "github.com/goccy/go-googlesql"

	"github.com/goccy/googlesqlite/internal/value"
)

// analysisCacheMaxEntries bounds the cache. When full, the whole map
// is dropped — workloads with more distinct query texts than this
// simply churn, and dropping wholesale releases the pinned wasm
// handles without LRU bookkeeping.
const analysisCacheMaxEntries = 512

// analysisCacheEntry is the plain-data projection of one successfully
// analyzed single-statement script.
type analysisCacheEntry struct {
	gen            uint64
	isDML          bool
	formattedQuery string
	params         []*googlesql.ResolvedParameter
	outputColumns  []*ColumnSpec                    // query statements
	colTypes       []googlesql.Googlesql_TypeNode   // INSERT column types (DML)
	mode           googlesql.ParameterMode          // arg-slicing reproduction
	positionalCount int                             // ParameterModeParameterPositional
	namedParams    []string                         // ParameterModeParameterNamed coverage check
}

// sliceArgs reproduces Analyzer.sliceArgsPerStatement for the cached
// single statement, including its "not enough query arguments"
// validation, without any parser-AST walks.
func (e *analysisCacheEntry) sliceArgs(args []driver.NamedValue) ([]driver.NamedValue, error) {
	switch e.mode {
	case googlesql.ParameterModeParameterPositional:
		consumed := min(e.positionalCount, len(args))
		return args[:consumed], nil
	case googlesql.ParameterModeParameterNamed:
		providedNamed := map[string]struct{}{}
		for _, v := range args {
			if v.Name != "" {
				providedNamed[strings.ToLower(v.Name)] = struct{}{}
			}
		}
		allByName := true
		for _, name := range e.namedParams {
			if _, ok := providedNamed[name]; !ok {
				allByName = false
				break
			}
		}
		if len(args) > 0 && !allByName && len(args) < len(e.namedParams) {
			return nil, fmt.Errorf("not enough query arguments")
		}
		return args, nil
	default:
		return args, nil
	}
}

// buildAction rebuilds a StmtAction from the cached artifact and the
// caller's (fresh) arguments. It mirrors newQueryStmtAction /
// newDMLStmtAction minus every wasm analysis round-trip.
func (e *analysisCacheEntry) buildAction(a *Analyzer, query string, args []driver.NamedValue) (StmtAction, error) {
	stmtArgs, err := e.sliceArgs(args)
	if err != nil {
		return nil, err
	}
	if e.isDML {
		stmtArgs = reshapeInsertArgsFromTypes(stmtArgs, e.colTypes)
		queryArgs, err := getArgsFromParams(stmtArgs, e.params)
		if err != nil {
			return nil, err
		}
		return &DMLStmtAction{
			query:          query,
			params:         e.params,
			args:           queryArgs,
			colTypes:       e.colTypes,
			formattedQuery: e.formattedQuery,
		}, nil
	}
	queryArgs, err := getArgsFromParams(stmtArgs, e.params)
	if err != nil {
		return nil, err
	}
	return &QueryStmtAction{
		query:          query,
		params:         e.params,
		args:           queryArgs,
		formattedQuery: e.formattedQuery,
		outputColumns:  e.outputColumns,
		isExplainMode:  a.isExplainMode,
	}, nil
}

// maybeStoreAnalysis projects a freshly built StmtAction into a cache
// entry. Only plain query and DML statements are cacheable — every
// DDL / script / export action carries side effects or catalog writes
// that must re-run per call.
func (a *Analyzer) maybeStoreAnalysis(key string, gen uint64, stmt googlesql.ASTStatementNode, mode googlesql.ParameterMode, action StmtAction) {
	entry := &analysisCacheEntry{gen: gen, mode: mode}
	switch act := action.(type) {
	case *QueryStmtAction:
		entry.formattedQuery = act.formattedQuery
		entry.params = act.params
		entry.outputColumns = act.outputColumns
	case *DMLStmtAction:
		entry.isDML = true
		entry.formattedQuery = act.formattedQuery
		entry.params = act.params
		entry.colTypes = act.colTypes
	default:
		return
	}
	switch mode {
	case googlesql.ParameterModeParameterPositional:
		entry.positionalCount = countPositionalParams(stmt)
	case googlesql.ParameterModeParameterNamed:
		entry.namedParams = collectNamedParamNames(stmt)
	}
	a.catalog.storeAnalysis(key, entry)
}

// reshapeInsertArgsFromTypes is reshapeInsertArgs driven by the cached
// per-column type handles instead of the resolved INSERT node.
func reshapeInsertArgsFromTypes(args []driver.NamedValue, colTypes []googlesql.Googlesql_TypeNode) []driver.NamedValue {
	if len(args) == 0 || len(colTypes) == 0 {
		return args
	}
	out := make([]driver.NamedValue, len(args))
	copy(out, args)
	n := min(len(colTypes), len(out))
	for i := 0; i < n; i++ {
		if colTypes[i] == nil {
			continue
		}
		out[i].Value = reshapeArgToType(out[i].Value, colTypes[i])
	}
	return out
}

// analysisGeneration returns the catalog's current analysis
// generation. Entries built under an older generation are dead.
func (c *Catalog) analysisGeneration() uint64 {
	return c.analysisGen.Load()
}

// bumpAnalysisGeneration invalidates every cached analysis. Called
// from each semantic catalog mutation point.
func (c *Catalog) bumpAnalysisGeneration() {
	c.analysisGen.Add(1)
	c.analysisCacheMu.Lock()
	c.analysisCache = nil
	c.analysisCacheMu.Unlock()
}

// analysisCacheHits / analysisCacheMisses are process-wide counters
// for observability and tests (AnalysisCacheCounters on the driver
// package surface).
var analysisCacheHits, analysisCacheMisses atomic.Uint64

// AnalysisCacheCounters reports the process-wide analysis-cache hit /
// miss counts.
func AnalysisCacheCounters() (hits, misses uint64) {
	return analysisCacheHits.Load(), analysisCacheMisses.Load()
}

// lookupAnalysis returns the entry for key if it exists and was built
// under generation gen.
func (c *Catalog) lookupAnalysis(key string, gen uint64) *analysisCacheEntry {
	c.analysisCacheMu.Lock()
	defer c.analysisCacheMu.Unlock()
	e := c.analysisCache[key]
	if e == nil || e.gen != gen {
		analysisCacheMisses.Add(1)
		return nil
	}
	analysisCacheHits.Add(1)
	return e
}

// storeAnalysis records entry under key unless the generation moved
// since the entry was built (a concurrent DDL invalidated it).
func (c *Catalog) storeAnalysis(key string, entry *analysisCacheEntry) {
	if c.analysisGen.Load() != entry.gen {
		return
	}
	c.analysisCacheMu.Lock()
	defer c.analysisCacheMu.Unlock()
	if c.analysisCache == nil {
		c.analysisCache = make(map[string]*analysisCacheEntry)
	} else if len(c.analysisCache) >= analysisCacheMaxEntries {
		c.analysisCache = make(map[string]*analysisCacheEntry)
	}
	c.analysisCache[key] = entry
}

// analysisCacheKey builds the lookup key: name path, post-rewrite
// query text, materialize-CTE flag, and the shape (name + type tag)
// of every argument. Two calls whose arguments would declare
// different GoogleSQL parameter types must not collide; the tags
// mirror googleSQLTypeForValue's deduction using pure-Go decoding.
func analysisCacheKey(namePath *NamePath, query string, args []driver.NamedValue, materializeCTE bool) string {
	var b strings.Builder
	b.Grow(len(query) + 64)
	for _, p := range namePath.path {
		b.WriteString(p)
		b.WriteByte(0x01)
	}
	fmt.Fprintf(&b, "%d", namePath.maxNum)
	b.WriteByte(0x00)
	if materializeCTE {
		b.WriteByte('M')
	}
	b.WriteByte(0x00)
	b.WriteString(query)
	for _, v := range args {
		b.WriteByte(0x00)
		b.WriteString(strings.ToLower(v.Name))
		b.WriteByte(0x02)
		b.WriteString(paramShapeTag(v.Value))
	}
	return b.String()
}

// paramShapeTag is the pure-Go shadow of googleSQLTypeForValue: it
// classifies a driver value by the GoogleSQL type the analyzer would
// be told about, without touching the wasm module. Unknown shapes get
// a distinct tag so they key separately.
func paramShapeTag(v any) string {
	if v == nil {
		return "nil"
	}
	switch x := v.(type) {
	case bool:
		return "b"
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return "i"
	case float32, float64:
		return "f"
	case string:
		if tag, ok := encodedShapeTag(x); ok {
			return tag
		}
		return "s"
	case []byte:
		return "y"
	case []bool:
		return "ab"
	case []int, []int8, []int16, []int32, []int64, []uint, []uint16, []uint32, []uint64:
		return "ai"
	case []float32, []float64:
		return "af"
	case []string:
		return "as"
	default:
		_ = x
		return fmt.Sprintf("?%T", v)
	}
}

// encodedShapeTag mirrors googleSQLTypeForEncodedString over the
// googlesqlite-encoded string layout (pure-Go DecodeValue).
func encodedShapeTag(s string) (string, bool) {
	if len(s) == 0 {
		return "", false
	}
	decoded, err := DecodeValue(s)
	if err != nil || decoded == nil {
		return "", false
	}
	tag, ok := decodedValueTag(decoded)
	if !ok {
		return "", false
	}
	return "e:" + tag, true
}

func decodedValueTag(v value.Value) (string, bool) {
	switch dv := v.(type) {
	case value.IntValue:
		return "i", true
	case value.FloatValue:
		return "f", true
	case value.BoolValue:
		return "b", true
	case value.StringValue:
		return "s", true
	case value.BytesValue:
		return "y", true
	case value.DateValue:
		return "d", true
	case value.TimestampValue:
		return "t", true
	case value.DatetimeValue:
		return "dt", true
	case *value.ArrayValue:
		elem := "s"
		for _, e := range dv.Values {
			if e == nil {
				continue
			}
			t, ok := decodedValueTag(e)
			if !ok || strings.HasPrefix(t, "a") {
				// googleSQLTypeForEncodedString only deduces arrays of
				// scalars; match its ok=false for anything nested.
				return "", false
			}
			elem = t
			break
		}
		return "a" + elem, true
	}
	return "", false
}
