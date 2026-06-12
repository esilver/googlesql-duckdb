//go:build !cgo

package googlesqlite

// This file is the pure-Go (CGO_ENABLED=0) counterpart of duckdb_aggregate_udf.go
// (which is tagged //go:build cgo): it registers the same GoogleSQL aggregate /
// window UDF surface against the wasm2go pure-Go DuckDB engine through
// github.com/esilver/duckdb-go-pure's RegisterAggregateBandConn instead of the C aggregate
// API. The two files are mutually exclusive by build tag and intentionally share
// helper names (aggSpec, duckdbAggSpecs, normalizeJSONScalar, toFloat64Result,
// toInt64Result, ...) so they stay textually comparable.
//
// Engine-side differences from the cgo bridge:
//   - No C trampolines: the engine injects the aggregate callbacks as Go
//     closures into the wasm2go indirect-function table and hands DuckDB the
//     table index as the "C function pointer".
//   - No cgo.Handle: the engine's per-group state blob holds an int64 key into
//     a Go-side handle table instead of a pinned cgo.Handle uintptr.
//   - No vector access here: the engine decodes input chunks into plain Go
//     values ([]any per row) before calling Update, and encodes the Finalize
//     return into the result vector, so this file never touches DuckDB memory.
//     JSON-typed cells (the UNNEST/json_each shape the geography / KLL INIT
//     inputs ride) arrive wrapped as convergeduckdb.JSONValue, replacing the
//     cgo bridge's agg_vec_is_json alias probe.
//
// Reuse strategy (identical to the cgo file): every aggregate body already
// exists as a pure-Go helper.Aggregator constructor (kll.BindInit,
// geofn.BindStCentroidAgg, ...). A helper.Aggregator has a Step(...any) that
// decodes the value-layer wire form and a Done() that re-encodes it. We do not
// reimplement any aggregate math here; we only adapt DuckDB's parallel state
// model onto the Step/Done shape.
//
// State model (identical to the cgo file): DuckDB drives aggregates with a
// per-group state plus init / update / combine / finalize callbacks, and may
// partition a single GROUP across several states for parallel partial
// aggregation. The helper.Aggregator interface has no partial-merge hook, so
// each state simply COLLECTS the raw per-row argument tuples. Combine
// concatenates a source state's collected rows into the target. Finalize then
// builds ONE fresh helper.Aggregator, replays every collected row through Step
// in arrival order, and calls Done once. This is correct regardless of how
// DuckDB splits the group, and keeps the existing bodies untouched.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"

	convergeduckdb "github.com/esilver/duckdb-go-pure"

	"github.com/goccy/googlesqlite/internal/functions/aggregate"
	approx "github.com/goccy/googlesqlite/internal/functions/approx_aggregate"
	geofn "github.com/goccy/googlesqlite/internal/functions/geography"
	"github.com/goccy/googlesqlite/internal/functions/helper"
	"github.com/goccy/googlesqlite/internal/functions/hll"
	"github.com/goccy/googlesqlite/internal/functions/kll"
	"github.com/goccy/googlesqlite/internal/functions/window"
	"github.com/goccy/googlesqlite/internal/value"
)

// stepDoneAggregator is the replay surface every registered body exposes:
// helper.Aggregator (the plain aggregate shape) and the window-native
// Step/Inverse/Done bodies (e.g. the percentile accumulators) both
// satisfy it. The replay model only ever needs Step + Done — Inverse is
// a sliding-frame optimization the collect/replay pipeline doesn't use.
type stepDoneAggregator interface {
	Step(stepArgs ...any) error
	Done() (any, error)
}

// DuckDB type-id constants for the aggregate result columns, mirroring the
// duckdb_type C enum values the cgo file passes (DUCKDB_TYPE_BIGINT = 5,
// DUCKDB_TYPE_DOUBLE = 11, DUCKDB_TYPE_VARCHAR = 17).
const (
	aggTypeBigint  int32 = 5
	aggTypeDouble  int32 = 11
	aggTypeVarchar int32 = 17
)

func jsonUnmarshalString(s string, out *string) error { return json.Unmarshal([]byte(s), out) }

func parseInt64(s string) (int64, error) { return strconv.ParseInt(s, 10, 64) }

func parseFloat64(s string) (float64, error) { return strconv.ParseFloat(s, 64) }

// aggSpec describes one aggregate to register: the SQL name DuckDB resolves, an
// inclusive range of input-parameter arities to register as overloads, and the
// constructor that mints a fresh helper.Aggregator per finalize. The arity
// range absorbs the analyzer's default-argument injection (e.g.
// KLL_QUANTILES.INIT_INT64(x) resolves to a 3-arg call); the body only reads
// the leading arguments it needs.
type aggSpec struct {
	name      string
	minParams int
	maxParams int
	newAgg    func() *helper.Aggregator
	// newWindowAgg, when set, supplies the replay body instead of newAgg.
	// Used for the window-native Step/Done accumulators (percentile
	// family) that don't come in the helper.Aggregator shape.
	newWindowAgg func() stepDoneAggregator
	// resultType is the DuckDB result type to declare. Zero value means
	// VARCHAR (the value-layer envelope every non-DP aggregate returns).
	// The differential-privacy aggregates declare DOUBLE / BIGINT so their
	// numeric result flows through native SQL and the result materializer.
	resultType int32
	// singleState routes every input row into one accumulator for the whole
	// frame instead of scattering row r to states[r]. It is set for aggregates
	// that only ever run through DuckDB's window path under OVER () (tf_idf,
	// st_clusterdbscan): there the scatter-update callback is reused with a
	// states array shorter than the input-chunk size (a single frame
	// accumulator for the whole partition), so reading states[r>0] would read
	// past the array. For a plain GROUP BY aggregate DuckDB fills the states
	// array with one entry per input row (all pointing at the group's state
	// for a single group), so the default per-row scatter is correct and
	// required for multi-group queries.
	singleState bool
}

// duckdbAggSpecs lists every aggregate this bridge registers. The names are the
// bare lowered forms the formatter emits when aggregateFuncMap / windowFuncMap
// are empty on the DuckDB path (it flattens the resolved Function's leaf name;
// KLL_QUANTILES.INIT_INT64 -> init_int64, ST_CENTROID_AGG -> st_centroid_agg,
// TF_IDF -> tf_idf). TF_IDF registers as an ordinary aggregate: DuckDB lets any
// aggregate run as a window function under OVER (), which is exactly how the
// tf_idf spec invokes it.
func duckdbAggSpecs() []aggSpec {
	return []aggSpec{
		// BigQuery KLL_QUANTILES sketch aggregates. INIT resolves with two
		// injected precision args (init_int64(x, 1000, 1)); the MERGE family
		// takes the sketch plus an optional num_quantiles / rank, which can
		// also pick up an injected default. Register a 1..3 arity band so the
		// binder matches whatever the analyzer emits; BindInit / the merge
		// bodies read only args[0] (and args[1] for the array / point merges).
		{name: "init_int64", minParams: 1, maxParams: 3, newAgg: kll.BindInit()},
		{name: "init_float64", minParams: 1, maxParams: 3, newAgg: kll.BindInit()},
		{name: "merge_int64", minParams: 1, maxParams: 3, newAgg: kll.BindMergeInt64()},
		{name: "merge_float64", minParams: 1, maxParams: 3, newAgg: kll.BindMergeFloat64()},
		{name: "merge_partial", minParams: 1, maxParams: 3, newAgg: kll.BindMergePartial()},
		{name: "merge_point_int64", minParams: 1, maxParams: 3, newAgg: kll.BindMergePointInt64()},
		{name: "merge_point_float64", minParams: 1, maxParams: 3, newAgg: kll.BindMergePointFloat64()},

		// BigQuery HLL_COUNT sketch aggregates (HyperLogLog++). HLL_COUNT.INIT
		// resolves to the bare leaf `init` and takes the value plus an optional
		// precision (the analyzer may inject a default trailing arg), so a 1..2
		// arity band covers both call shapes; the body reads args[0] and the
		// optional args[1] precision. HLL_COUNT.MERGE resolves to `merge` and
		// folds the per-group sketch BYTES into one cardinality. The bodies are
		// the same pure-Go aggregators the SQLite path uses (hll.BindHllCount*).
		// HLL_COUNT.EXTRACT is a scalar: it rides the googlesqlite_hll_count_extract
		// UDF registered by registerNormalFuncUDFs (the formatter remaps the bare
		// `extract` leaf to it).
		{name: "init", minParams: 1, maxParams: 2, newAgg: hll.BindHllCountInit()},
		{name: "merge", minParams: 1, maxParams: 1, newAgg: hll.BindHllCountMerge()},
		{name: "hll_count_merge_partial", minParams: 1, maxParams: 1, newAgg: hll.BindHllCountMergePartial()},

		// BigQuery approximate aggregates. Keep these on the existing Go
		// bodies instead of DuckDB's native approximations so result shape
		// and the small conformance examples match the SQLite baseline.
		{name: "approx_quantiles", minParams: 2, maxParams: 5, newAgg: approx.BindApproxQuantiles()},
		{name: "approx_top_count", minParams: 2, maxParams: 5, newAgg: approx.BindApproxTopCount()},
		{name: "approx_top_sum", minParams: 3, maxParams: 6, newAgg: approx.BindApproxTopSum()},

		// ARRAY_AGG(DISTINCT ...) cannot use DuckDB's native DISTINCT aggregate
		// when the formatter also supplies the hidden first-seen ordinal for
		// BigQuery output order: DuckDB requires every ORDER BY expression to
		// appear in the DISTINCT argument list. Route that shape through the
		// existing GoogleSQL aggregate body under a non-conflicting name.
		{name: "googlesqlite_array_agg", minParams: 1, maxParams: 8, newAgg: aggregate.BindArrayAgg()},

		// ANY_VALUE(value HAVING { MAX | MIN } key): the formatter lowers
		// the HAVING modifier to having_any_value(value, key, 'MAX'|'MIN'),
		// always emitting exactly three arguments. The aggregate buffers
		// every (value, key) pair and at finalize returns a value tied to
		// the extremum of key.
		{name: "having_any_value", minParams: 3, maxParams: 3, newAgg: aggregate.BindHavingAnyValue()},

		// Property-graph MEASURE re-aggregation: AGG(measure) over a graph
		// element. The graph lowering (internal/graph_scan_node.go) emits
		// googlesqlite_agg(value_expr, locking_key, 'KIND'), always three
		// arguments. The aggregator dedups contributions by locking key and
		// finalizes with the measure's inner aggregate kind (SUM / AVG / MIN /
		// MAX / COUNT / COUNT_DISTINCT / ANY_VALUE), returning the value-layer
		// envelope like the other non-DP aggregates.
		{name: "googlesqlite_agg", minParams: 1, maxParams: 3, newAgg: aggregate.BindAGG()},

		// Geography aggregates: single GEOGRAPHY argument (the value-layer
		// envelope).
		{name: "st_centroid_agg", minParams: 1, maxParams: 1, newAgg: geofn.BindStCentroidAgg()},
		{name: "st_union_agg", minParams: 1, maxParams: 1, newAgg: geofn.BindStUnionAgg()},
		{name: "st_extent", minParams: 1, maxParams: 1, newAgg: geofn.BindStExtentAgg()},

		// ST_CLUSTERDBSCAN(geo, eps, min_pts) OVER (w) has no native
		// DuckDB analytic form (DBSCAN assigns a per-row cluster id
		// computed from the whole partition). The formatter lowers it to
		// array_at_ordinal(st_clusterdbscan(geo, eps, min_pts,
		// row_number() OVER (w)) OVER (w), row_number() OVER (w)): this
		// aggregate runs as the window function and returns the
		// partition's ARRAY<INT64> of cluster ids ordered by the 4th
		// (row_number) argument, and array_at_ordinal picks the current
		// row's id by the same row_number. Like tf_idf it only ever runs
		// under OVER (), so singleState routes every frame row into the
		// one accumulator. Arity is exactly 4 (geo, eps, min_pts,
		// row_number); no option markers are appended on this path.
		{name: "st_clusterdbscan", minParams: 4, maxParams: 4, newAgg: geofn.BindStClusterDBSCANAgg(), singleState: true},

		// Text analysis: a registered aggregate auto-works as a window
		// function under OVER (). The formatter is taught to emit tf_idf
		// through the native window path (nativeWindowFuncMap), so it arrives
		// as a clean tf_idf(tokens) OVER () call with no option markers.
		{name: "tf_idf", minParams: 1, maxParams: 1, newAgg: newTfIdfAggregator, singleState: true},

		// PERCENTILE_CONT / PERCENTILE_DISC with RESPECT NULLS. DuckDB's
		// quantile_cont / quantile_disc always ignore NULLs, so the
		// formatter routes the RESPECT NULLS modifier to these names and
		// the pure-Go window percentile bodies do the null-respecting
		// sort. Arity band: (value, fraction) plus up to two option
		// markers. PERCENTILE_* only ever runs under OVER () spanning the
		// whole partition (GoogleSQL forbids ORDER BY / frames on it), so
		// singleState routes every partition row into one accumulator.
		{
			name: "googlesqlite_window_percentile_cont", minParams: 2, maxParams: 4,
			newWindowAgg: func() stepDoneAggregator {
				return window.NewPercentileContWindowNative()().(stepDoneAggregator)
			},
			singleState: true,
		},
		{
			name: "googlesqlite_window_percentile_disc", minParams: 2, maxParams: 4,
			newWindowAgg: func() stepDoneAggregator {
				return window.NewPercentileDiscWindowNative()().(stepDoneAggregator)
			},
			singleState: true,
		},

		// Differential-privacy / anonymization OUTER aggregates. The
		// analyzer's RewriteAnonymization stage lowers SELECT WITH
		// ANONYMIZATION / WITH DIFFERENTIAL_PRIVACY into a per-privacy-unit
		// partial aggregate feeding one of these. The WITH ANONYMIZATION
		// surface emits the bare catalog names (anon_sum, anon_avg, ...) -
		// note ANON_COUNT lowers to anon_sum over per-user counts. The WITH
		// DIFFERENTIAL_PRIVACY surface emits $differential_privacy_* names
		// that the formatter prefixes to googlesqlite_differential_privacy_*
		// (and COUNT lowers to _sum likewise). The arity bands span the
		// minimal (partial only) through the fullest (array, positional,
		// lo, hi, epsilon, delta = 6) call shapes; each body reads the
		// arguments it needs via parseDPArgs and ignores the rest.
		{name: "anon_sum", minParams: 1, maxParams: 5, newAgg: aggregate.BindDifferentialPrivacySum(), resultType: aggTypeDouble},
		{name: "anon_count", minParams: 1, maxParams: 5, newAgg: aggregate.BindDifferentialPrivacyCount(), resultType: aggTypeBigint},
		{name: "anon_count_star", minParams: 1, maxParams: 5, newAgg: aggregate.BindDifferentialPrivacyCountStar(), resultType: aggTypeBigint},
		{name: "anon_avg", minParams: 1, maxParams: 5, newAgg: aggregate.BindDifferentialPrivacyAvg(), resultType: aggTypeDouble},
		{name: "anon_var_pop", minParams: 1, maxParams: 5, newAgg: aggregate.BindDifferentialPrivacyVarPop(), resultType: aggTypeDouble},
		{name: "anon_stddev_pop", minParams: 1, maxParams: 5, newAgg: aggregate.BindDifferentialPrivacyStddevPop(), resultType: aggTypeDouble},
		{name: "anon_percentile_cont", minParams: 1, maxParams: 6, newAgg: aggregate.BindDifferentialPrivacyPercentileCont(), resultType: aggTypeDouble},
		// QUANTILES returns an ARRAY of cut points; it stays on the VARCHAR
		// value-layer envelope. Its examples are nondeterministic (verify the
		// query runs), so the envelope result is not asserted by value.
		{name: "anon_quantiles", minParams: 1, maxParams: 6, newAgg: aggregate.BindDifferentialPrivacyQuantiles()},

		{name: "googlesqlite_differential_privacy_sum", minParams: 1, maxParams: 5, newAgg: aggregate.BindDifferentialPrivacySum(), resultType: aggTypeDouble},
		{name: "googlesqlite_differential_privacy_count", minParams: 1, maxParams: 5, newAgg: aggregate.BindDifferentialPrivacyCount(), resultType: aggTypeBigint},
		{name: "googlesqlite_differential_privacy_count_star", minParams: 1, maxParams: 5, newAgg: aggregate.BindDifferentialPrivacyCountStar(), resultType: aggTypeBigint},
		{name: "googlesqlite_differential_privacy_avg", minParams: 1, maxParams: 5, newAgg: aggregate.BindDifferentialPrivacyAvg(), resultType: aggTypeDouble},
		{name: "googlesqlite_differential_privacy_var_pop", minParams: 1, maxParams: 5, newAgg: aggregate.BindDifferentialPrivacyVarPop(), resultType: aggTypeDouble},
		{name: "googlesqlite_differential_privacy_stddev_pop", minParams: 1, maxParams: 5, newAgg: aggregate.BindDifferentialPrivacyStddevPop(), resultType: aggTypeDouble},
		{name: "googlesqlite_differential_privacy_percentile_cont", minParams: 1, maxParams: 6, newAgg: aggregate.BindDifferentialPrivacyPercentileCont(), resultType: aggTypeDouble},
		{name: "googlesqlite_differential_privacy_quantiles", minParams: 1, maxParams: 6, newAgg: aggregate.BindDifferentialPrivacyQuantiles()},
	}
}

// aggState is the Go side of one engine aggregate state. It accumulates the raw
// per-row argument tuples a single state observed; Finalize replays them.
type aggState struct {
	rows [][]any
}

// replayAggregate adapts one aggSpec onto the engine's AggregateImpl shape: a
// collect / concat / replay implementation mirroring the cgo bridge's
// googlesqliteAggUpdate / googlesqliteAggCombine / aggState.finalize +
// writeResult pipeline.
type replayAggregate struct {
	newAgg     func() stepDoneAggregator
	resultType int32
}

var _ convergeduckdb.AggregateImpl = (*replayAggregate)(nil)

// NewState mints an empty row collector (the cgo googlesqliteAggInit).
func (a *replayAggregate) NewState() any {
	return &aggState{rows: nil}
}

// Update collects one row's argument tuple, normalizing each value into the
// value-layer wire form helper.Aggregator.Step expects (the cgo
// googlesqliteAggUpdate + readVectorValue): JSON-typed cells are unwrapped via
// normalizeJSONScalar exactly where the cgo bridge applied it to JSON-alias
// vectors, and BLOB bytes become a string (the cgo reader returned VARCHAR /
// BLOB uniformly as a Go string). args is freshly allocated per row, so the
// normalized slice is retained directly.
func (a *replayAggregate) Update(state any, args []any) {
	st := state.(*aggState)
	for i, v := range args {
		switch t := v.(type) {
		case convergeduckdb.JSONValue:
			args[i] = normalizeJSONScalar(string(t))
		case []byte:
			args[i] = string(t)
		}
	}
	st.rows = append(st.rows, args)
}

// Combine concatenates the source's collected rows into the target (the cgo
// googlesqliteAggCombine). Replay at Finalize then sees the full group
// regardless of partitioning.
func (a *replayAggregate) Combine(dst, src any) {
	tgt := dst.(*aggState)
	tgt.rows = append(tgt.rows, src.(*aggState).rows...)
}

// Finalize builds one fresh helper.Aggregator, replays every collected row in
// arrival order, and returns the encoded result of Done (the cgo
// aggState.finalize + writeResult). An empty group (no rows) still constructs
// the aggregator and calls Done, matching GoogleSQL's "aggregate over zero
// rows" semantics (e.g. NULL). A Step / Done error aborts the query with the
// message, like duckdb_aggregate_function_set_error.
func (a *replayAggregate) Finalize(state any) (any, error) {
	st := state.(*aggState)
	agg := a.newAgg()
	for _, row := range st.rows {
		if err := agg.Step(row...); err != nil {
			return nil, err
		}
	}
	out, err := agg.Done()
	if err != nil {
		return nil, err
	}
	return encodeAggResult(out, a.resultType)
}

// encodeAggResult is the pure-function port of the cgo writeResult: it maps a
// helper.Aggregator.Done value (nil for NULL, a bare int64 / float64 / bool, or
// the base64 envelope string for richer types) onto the declared result column.
//
// resultType selects the encoding. DOUBLE / BIGINT result columns (the
// differential-privacy aggregates) take the native numeric value directly so
// the surrounding SQL's CAST / arithmetic and the result-set materializer see a
// real number. The default VARCHAR column re-encodes a bare primitive back into
// the value-layer envelope so it round-trips through DecodeValue on scan.
func encodeAggResult(out any, resultType int32) (any, error) {
	if out == nil {
		return nil, nil
	}
	switch resultType {
	case aggTypeDouble:
		if f, ok := toFloat64Result(out); ok {
			return f, nil
		}
		return nil, nil
	case aggTypeBigint:
		if i, ok := toInt64Result(out); ok {
			return i, nil
		}
		return nil, nil
	}
	s, ok := out.(string)
	if !ok {
		// Done handed back a bare Go primitive (an int64 / float64 / bool from
		// EncodeValue). Re-encode it as the value-layer envelope string so the
		// VARCHAR result column round-trips through DecodeValue on scan.
		v, err := value.ValueFromGoValue(out)
		if err == nil && v != nil {
			if enc, _, encErr := value.EncodeValueString(v); encErr == nil {
				s = enc
				ok = true
			}
		}
		if !ok {
			s = fmt.Sprint(out)
		}
	}
	return s, nil
}

// registerDuckDBAggregates installs the aggregate / window UDF surface onto a
// pure-Go DuckDB database handle. Mirrors the cgo file: it grabs one connection
// (catalog-scoped registration is visible to every connection of the same
// database) and registers each aggregate as an arity band of ANY-parameter
// overloads through the engine's RegisterAggregateBandConn.
func registerDuckDBAggregates(db *sql.DB) error {
	conn, err := db.Conn(context.Background())
	if err != nil {
		return fmt.Errorf("googlesqlite: duckdb aggregate conn: %w", err)
	}
	defer conn.Close()

	for _, spec := range duckdbAggSpecs() {
		// A zero resultType means "use the default VARCHAR envelope".
		resultType := spec.resultType
		if resultType == 0 {
			resultType = aggTypeVarchar
		}
		newBody := spec.newWindowAgg
		if newBody == nil {
			newAgg := spec.newAgg
			newBody = func() stepDoneAggregator { return newAgg() }
		}
		impl := &replayAggregate{newAgg: newBody, resultType: resultType}
		opts := convergeduckdb.AggregateOptions{
			MinArgs:      spec.minParams,
			MaxArgs:      spec.maxParams,
			ResultTypeID: resultType,
			SingleState:  spec.singleState,
		}
		if err := convergeduckdb.RegisterAggregateBandConn(conn, spec.name, opts, impl); err != nil {
			return fmt.Errorf("googlesqlite: register aggregate %q failed: %w", spec.name, err)
		}
	}
	return nil
}

// normalizeJSONScalar unwraps a JSON-typed scalar (as DuckDB's json_each.value
// hands it to an UNNEST-fed aggregate) into the bare Go value the value-layer
// decoder expects. A JSON string carries the base64 envelope quoted and escaped
// ("eyJ...=="), so unquoting it yields the envelope DecodeValue knows; a JSON
// number / bool maps to the matching Go primitive. Anything that does not parse
// as a JSON scalar string / number / bool passes through unchanged so a value
// that was not actually JSON (the rare direct VARCHAR) is left intact.
func normalizeJSONScalar(v any) any {
	s, ok := v.(string)
	if !ok {
		return v
	}
	// A JSON string element: "..." (the common geography / sketch envelope
	// case). Unmarshal to recover the unescaped inner string.
	if len(s) >= 2 && s[0] == '"' {
		var inner string
		if err := jsonUnmarshalString(s, &inner); err == nil {
			return inner
		}
		return v
	}
	// A bare JSON scalar (number / bool / null). DecodeValue already coerces
	// json.Number and understands bool / nil, so hand it the parsed value.
	switch s {
	case "true":
		return true
	case "false":
		return false
	case "null":
		return nil
	}
	// Numeric JSON scalar (KLL INIT over an INT64 / FLOAT64 array element).
	if i, err := parseInt64(s); err == nil {
		return i
	}
	if f, err := parseFloat64(s); err == nil {
		return f
	}
	return v
}

// toFloat64Result coerces a Done() result (a bare float64 / int64, or a value-
// layer envelope string the body produced for a FLOAT/INT value) into a
// float64 for a DOUBLE result column.
func toFloat64Result(out any) (float64, bool) {
	switch v := out.(type) {
	case float64:
		return v, true
	case int64:
		return float64(v), true
	case string:
		if dv, err := value.DecodeValue(v); err == nil && dv != nil {
			if f, err := dv.ToFloat64(); err == nil {
				return f, true
			}
		}
	}
	return 0, false
}

// toInt64Result coerces a Done() result into an int64 for a BIGINT result
// column.
func toInt64Result(out any) (int64, bool) {
	switch v := out.(type) {
	case int64:
		return v, true
	case float64:
		return int64(v), true
	case string:
		if dv, err := value.DecodeValue(v); err == nil && dv != nil {
			if i, err := dv.ToInt64(); err == nil {
				return i, true
			}
		}
	}
	return 0, false
}
