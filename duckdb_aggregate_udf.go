//go:build cgo

package googlesqlite

// This file is a CONTAINED cgo bridge that registers GoogleSQL aggregate /
// window UDFs against DuckDB through the C aggregate-function API
// (duckdb_create_aggregate_function / duckdb_register_aggregate_function and
// the duckdb_aggregate_function_set_functions setters). duckdb-go (v2) only
// exposes scalar / table / window-over-frame UDF registration, not aggregate
// registration, so the aggregate families (the BigQuery KLL_QUANTILES sketch
// aggregates, the geography ST_*_AGG aggregates, and TF_IDF) had no lowering
// target on the DuckDB backend. Rather than fork duckdb-go, this file calls
// the C API directly: libduckdb is already linked (duckdb_use_lib), and the C
// aggregate surface is fully present in the staged duckdb.h.
//
// Reuse strategy: every aggregate body already exists as a pure-Go
// helper.Aggregator constructor (kll.BindInit, geography.BindStCentroidAgg,
// ...). A helper.Aggregator has a Step(...any) that decodes the value-layer
// wire form and a Done() that re-encodes it. We do not reimplement any
// aggregate math here; we only adapt DuckDB's parallel state model onto the
// Step/Done shape.
//
// State model: DuckDB drives aggregates with a fixed-size per-group state
// buffer plus init / update / combine / finalize / destroy callbacks, and may
// partition a single GROUP across several states for parallel partial
// aggregation. The helper.Aggregator interface has no partial-merge hook, so
// each DuckDB state simply COLLECTS the raw per-row argument tuples (as
// []driver.Value). combine concatenates a source state's collected rows into
// the target. finalize then builds ONE fresh helper.Aggregator, replays every
// collected row through Step in arrival order, and calls Done once. This is
// correct regardless of how DuckDB splits the group, and keeps the existing
// bodies untouched. Collected rows are the value-layer wire form DuckDB
// produces (int64 / float64 for numeric inputs, the base64 VARCHAR envelope
// for BYTES / GEOGRAPHY / ARRAY inputs), which is exactly what Step expects.

/*
#cgo CPPFLAGS: -I${SRCDIR}
#include <stdlib.h>
#include "duckdb_agg_header.h"

// Forward declarations of the Go callbacks (exported below). cgo generates
// these from the //export directives; the trampolines reference them.
extern idx_t  googlesqliteAggStateSize(duckdb_function_info info);
extern void   googlesqliteAggInit(duckdb_function_info info, duckdb_aggregate_state state);
extern void   googlesqliteAggUpdate(duckdb_function_info info, duckdb_data_chunk input, duckdb_aggregate_state *states);
extern void   googlesqliteAggCombine(duckdb_function_info info, duckdb_aggregate_state *source, duckdb_aggregate_state *target, idx_t count);
extern void   googlesqliteAggFinalize(duckdb_function_info info, duckdb_aggregate_state *source, duckdb_vector result, idx_t count, idx_t offset);
extern void   googlesqliteAggDestroy(duckdb_aggregate_state *states, idx_t count);

// Thin C trampolines with the exact duckdb_* callback signatures. They simply
// forward to the Go callbacks. Keeping them in C avoids depending on Go
// function values matching the C typedefs by coincidence.
static idx_t agg_state_size_cb(duckdb_function_info info) {
    return googlesqliteAggStateSize(info);
}
static void agg_init_cb(duckdb_function_info info, duckdb_aggregate_state state) {
    googlesqliteAggInit(info, state);
}
static void agg_update_cb(duckdb_function_info info, duckdb_data_chunk input, duckdb_aggregate_state *states) {
    googlesqliteAggUpdate(info, input, states);
}
static void agg_combine_cb(duckdb_function_info info, duckdb_aggregate_state *source, duckdb_aggregate_state *target, idx_t count) {
    googlesqliteAggCombine(info, source, target, count);
}
static void agg_finalize_cb(duckdb_function_info info, duckdb_aggregate_state *source, duckdb_vector result, idx_t count, idx_t offset) {
    googlesqliteAggFinalize(info, source, result, count, offset);
}
static void agg_destroy_cb(duckdb_aggregate_state *states, idx_t count) {
    googlesqliteAggDestroy(states, count);
}

// build_one_overload constructs a single aggregate-function overload with
// param_count ANY-typed parameters and a VARCHAR result. Parameters are
// declared ANY so DuckDB binds the actual call-site type without inserting a
// cast: the aggregated value reaches the update callback in its native form
// (an int64 / double for numeric inputs, the JSON value json_each produces for
// an UNNEST element, or the base64 VARCHAR envelope for a BYTES / GEOGRAPHY /
// ARRAY input), and the Go value decoder re-types it. The result is VARCHAR
// because every aggregate here produces a richer value-layer type that
// EncodeValue renders as the base64 VARCHAR envelope.
static duckdb_aggregate_function build_one_overload(const char *name, idx_t param_count, duckdb_type result_type_id, void *extra_info) {
    duckdb_aggregate_function fn = duckdb_create_aggregate_function();
    duckdb_aggregate_function_set_name(fn, name);

    duckdb_logical_type any_type = duckdb_create_logical_type(DUCKDB_TYPE_ANY);
    // Most aggregates here produce a value-layer envelope and declare VARCHAR;
    // the differential-privacy aggregates produce a native DOUBLE / BIGINT
    // because their result is consumed by ordinary SQL (CAST AS INT64, ABS,
    // arithmetic) and by the result-set materializer, neither of which decode
    // the base64 envelope.
    duckdb_logical_type result_type = duckdb_create_logical_type(result_type_id);
    for (idx_t i = 0; i < param_count; i++) {
        duckdb_aggregate_function_add_parameter(fn, any_type);
    }
    duckdb_aggregate_function_set_return_type(fn, result_type);
    duckdb_destroy_logical_type(&any_type);
    duckdb_destroy_logical_type(&result_type);

    duckdb_aggregate_function_set_functions(
        fn, agg_state_size_cb, agg_init_cb, agg_update_cb, agg_combine_cb, agg_finalize_cb);
    duckdb_aggregate_function_set_destructor(fn, agg_destroy_cb);
    // Route NULL inputs into the body: the GoogleSQL aggregates implement their
    // own NULL semantics (ST_UNION_AGG skips NULL, ST_CENTROID_AGG skips NULL,
    // TF_IDF treats a NULL doc as empty), so DuckDB must not drop NULL rows.
    duckdb_aggregate_function_set_special_handling(fn);
    // extra_info identifies the Go constructor.
    duckdb_aggregate_function_set_extra_info(fn, extra_info, NULL);
    return fn;
}

// register_googlesqlite_aggregate registers a name with a SET of arity
// overloads. The GoogleSQL analyzer injects default trailing arguments for some
// aggregates (e.g. KLL_QUANTILES.INIT_INT64(x) resolves to init_int64(x, 1000,
// 1)), so the exact arity the formatter emits is not always the user-visible
// one. Registering overloads for min_params..max_params ANY parameters lets the
// binder match whatever arity arrives; the Go body only reads the arguments it
// needs and ignores the rest. Returns DuckDBError / DuckDBSuccess.
static duckdb_state register_googlesqlite_aggregate(
        duckdb_connection con, const char *name, idx_t min_params, idx_t max_params, duckdb_type result_type_id, void *extra_info) {
    duckdb_aggregate_function_set set = duckdb_create_aggregate_function_set(name);
    for (idx_t p = min_params; p <= max_params; p++) {
        duckdb_aggregate_function fn = build_one_overload(name, p, result_type_id, extra_info);
        duckdb_state add_state = duckdb_add_aggregate_function_to_set(set, fn);
        duckdb_destroy_aggregate_function(&fn);
        if (add_state == DuckDBError) {
            duckdb_destroy_aggregate_function_set(&set);
            return DuckDBError;
        }
    }
    duckdb_state state = duckdb_register_aggregate_function_set(con, set);
    duckdb_destroy_aggregate_function_set(&set);
    return state;
}

// Helpers the Go update / finalize callbacks use to touch DuckDB vectors
// through the C ABI without Go needing to model duckdb_string_t / validity
// bitsets directly.

// agg_chunk_size / agg_chunk_vector expose the input chunk shape.
static idx_t agg_chunk_size(duckdb_data_chunk c) { return duckdb_data_chunk_get_size(c); }
static idx_t agg_chunk_cols(duckdb_data_chunk c) { return duckdb_data_chunk_get_column_count(c); }
static duckdb_vector agg_chunk_vector(duckdb_data_chunk c, idx_t i) { return duckdb_data_chunk_get_vector(c, i); }

// agg_vec_type_id returns the DUCKDB_TYPE of a vector so the Go reader can
// dispatch the flat-array read.
static duckdb_type agg_vec_type_id(duckdb_vector v) {
    duckdb_logical_type t = duckdb_vector_get_column_type(v);
    duckdb_type id = duckdb_get_type_id(t);
    duckdb_destroy_logical_type(&t);
    return id;
}

// agg_vec_is_json reports whether a vector's logical type carries the JSON
// alias. DuckDB's JSON type is backed by VARCHAR storage, so duckdb_get_type_id
// returns DUCKDB_TYPE_VARCHAR for it; the alias is the only way to tell a JSON
// column (whose scalar values arrive JSON-encoded, e.g. a base64 envelope wrapped
// in JSON-string quotes) apart from a plain VARCHAR envelope column. The
// aggregate value argument is JSON-typed whenever it flows from an UNNEST
// (json_each.value), which is exactly the geography / KLL INIT input shape.
static bool agg_vec_is_json(duckdb_vector v) {
    duckdb_logical_type t = duckdb_vector_get_column_type(v);
    char *alias = duckdb_logical_type_get_alias(t);
    bool is_json = false;
    if (alias != NULL) {
        is_json = (alias[0] == 'J' && alias[1] == 'S' && alias[2] == 'O' && alias[3] == 'N' && alias[4] == '\0');
        duckdb_free(alias);
    }
    duckdb_destroy_logical_type(&t);
    return is_json;
}

// agg_row_valid reports whether row r of vector v is non-NULL.
static bool agg_row_valid(duckdb_vector v, idx_t r) {
    uint64_t *validity = duckdb_vector_get_validity(v);
    if (validity == NULL) {
        return true; // no mask => all valid
    }
    return duckdb_validity_row_is_valid(validity, r);
}

// Flat-typed value readers. Each indexes the vector's data array by row.
static int64_t  agg_read_i64(duckdb_vector v, idx_t r)  { return ((int64_t  *)duckdb_vector_get_data(v))[r]; }
static int32_t  agg_read_i32(duckdb_vector v, idx_t r)  { return ((int32_t  *)duckdb_vector_get_data(v))[r]; }
static int16_t  agg_read_i16(duckdb_vector v, idx_t r)  { return ((int16_t  *)duckdb_vector_get_data(v))[r]; }
static int8_t   agg_read_i8(duckdb_vector v, idx_t r)   { return ((int8_t   *)duckdb_vector_get_data(v))[r]; }
static double   agg_read_f64(duckdb_vector v, idx_t r)  { return ((double   *)duckdb_vector_get_data(v))[r]; }
static float    agg_read_f32(duckdb_vector v, idx_t r)  { return ((float    *)duckdb_vector_get_data(v))[r]; }
static bool     agg_read_bool(duckdb_vector v, idx_t r) { return ((bool     *)duckdb_vector_get_data(v))[r]; }

// agg_read_decimal_double reads a DECIMAL element at row r and converts it to a
// double. A DECIMAL vector stores its value in its internal integer type
// (int16 / int32 / int64 / hugeint, selected by width); we reconstruct the
// duckdb_decimal with the column's width / scale and let duckdb_decimal_to_double
// apply the scale. GoogleSQL FLOAT literals like the 0.5 rank argument to
// KLL_QUANTILES.MERGE_POINT bind as DECIMAL, so the aggregate's constant numeric
// arguments arrive here.
static double agg_read_decimal_double(duckdb_vector v, idx_t r) {
    duckdb_logical_type t = duckdb_vector_get_column_type(v);
    duckdb_decimal dec;
    dec.width = duckdb_decimal_width(t);
    dec.scale = duckdb_decimal_scale(t);
    duckdb_type internal = duckdb_decimal_internal_type(t);
    duckdb_destroy_logical_type(&t);

    void *data = duckdb_vector_get_data(v);
    switch (internal) {
        case DUCKDB_TYPE_SMALLINT: {
            int16_t raw = ((int16_t *)data)[r];
            dec.value.lower = (uint64_t)(int64_t)raw;
            dec.value.upper = raw < 0 ? -1 : 0;
            break;
        }
        case DUCKDB_TYPE_INTEGER: {
            int32_t raw = ((int32_t *)data)[r];
            dec.value.lower = (uint64_t)(int64_t)raw;
            dec.value.upper = raw < 0 ? -1 : 0;
            break;
        }
        case DUCKDB_TYPE_BIGINT: {
            int64_t raw = ((int64_t *)data)[r];
            dec.value.lower = (uint64_t)raw;
            dec.value.upper = raw < 0 ? -1 : 0;
            break;
        }
        case DUCKDB_TYPE_HUGEINT: {
            duckdb_hugeint raw = ((duckdb_hugeint *)data)[r];
            dec.value = raw;
            break;
        }
        default: {
            dec.value.lower = 0;
            dec.value.upper = 0;
        }
    }
    return duckdb_decimal_to_double(dec);
}

// agg_read_string_len returns the byte length of the VARCHAR / BLOB at row r.
static uint32_t agg_read_string_len(duckdb_vector v, idx_t r) {
    duckdb_string_t *data = (duckdb_string_t *)duckdb_vector_get_data(v);
    return duckdb_string_t_length(data[r]);
}
// agg_read_string_ptr returns a pointer to the VARCHAR / BLOB bytes at row r.
// The returned pointer is valid only while the input chunk is alive; the Go
// caller copies the bytes immediately into a Go string.
static const char *agg_read_string_ptr(duckdb_vector v, idx_t r) {
    duckdb_string_t *data = (duckdb_string_t *)duckdb_vector_get_data(v);
    return duckdb_string_t_data(&data[r]);
}

// agg_write_string assigns one VARCHAR result element. duckdb_string_t storage
// (inlined vs heap) is handled internally by DuckDB.
static void agg_write_string(duckdb_vector v, idx_t r, const char *s, idx_t len) {
    duckdb_vector_assign_string_element_len(v, r, s, len);
}
// agg_write_double / agg_write_int64 assign one native numeric result element.
// Used by the differential-privacy aggregates whose result vector is declared
// DOUBLE / BIGINT (so the surrounding SQL's CAST / arithmetic operate on a real
// number rather than the base64 value-layer envelope).
static void agg_write_double(duckdb_vector v, idx_t r, double d) {
    ((double *)duckdb_vector_get_data(v))[r] = d;
}
static void agg_write_int64(duckdb_vector v, idx_t r, int64_t i) {
    ((int64_t *)duckdb_vector_get_data(v))[r] = i;
}
// agg_write_null marks one result element NULL.
static void agg_write_null(duckdb_vector v, idx_t r) {
    duckdb_vector_ensure_validity_writable(v);
    uint64_t *validity = duckdb_vector_get_validity(v);
    duckdb_validity_set_row_invalid(validity, r);
}

// agg_extra_info / agg_set_error reach the function metadata from inside the
// callbacks.
static void *agg_extra_info(duckdb_function_info info) {
    return duckdb_aggregate_function_get_extra_info(info);
}
static void agg_set_error(duckdb_function_info info, const char *msg) {
    duckdb_aggregate_function_set_error(info, msg);
}

// State accessor: the aggregate state is an opaque pointer-sized slot. We store
// a single uintptr (a cgo.Handle) in it. These read / write that slot.
static uintptr_t agg_state_load(duckdb_aggregate_state state) {
    return *(uintptr_t *)state;
}
static void agg_state_store(duckdb_aggregate_state state, uintptr_t h) {
    *(uintptr_t *)state = h;
}
*/
import "C"

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"reflect"
	"runtime/cgo"
	"strconv"
	"sync"
	"unsafe"

	duckdb "github.com/goccy/googlesqlite/internal/duckdbgo"

	"github.com/goccy/googlesqlite/internal/functions/aggregate"
	geofn "github.com/goccy/googlesqlite/internal/functions/geography"
	"github.com/goccy/googlesqlite/internal/functions/helper"
	"github.com/goccy/googlesqlite/internal/functions/hll"
	"github.com/goccy/googlesqlite/internal/functions/kll"
	"github.com/goccy/googlesqlite/internal/value"
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
	minParams C.idx_t
	maxParams C.idx_t
	newAgg    func() *helper.Aggregator
	// resultType is the DuckDB result type to declare. Zero value means
	// VARCHAR (the value-layer envelope every non-DP aggregate returns).
	// The differential-privacy aggregates declare DOUBLE / BIGINT so their
	// numeric result flows through native SQL and the result materializer.
	resultType C.duckdb_type
	// singleState routes every input row to states[0] in the update callback
	// instead of scattering row r to states[r]. It is set for aggregates that
	// only ever run through DuckDB's window path under OVER () (tf_idf): there
	// the C scatter-update callback is reused with a states array shorter than
	// the input-chunk size (a single frame accumulator for the whole partition),
	// so reading states[r>0] would read past the array. For a plain GROUP BY
	// aggregate DuckDB fills the states array with one entry per input row
	// (all pointing at the group's state for a single group), so the default
	// per-row scatter is correct and required for multi-group queries.
	singleState bool
}

// aggConstructor is the Go object referenced by an aggregate's DuckDB
// extra-info handle. The update / finalize callbacks recover it to build fresh
// helper.Aggregators and to learn the aggregate's state-scatter mode.
type aggConstructor struct {
	newAgg      func() *helper.Aggregator
	singleState bool
	// resultType mirrors aggSpec.resultType so the finalize callback can
	// decide whether to write a native numeric or the VARCHAR envelope.
	resultType C.duckdb_type
}

// aggState is the Go side of one DuckDB aggregate state. It accumulates the raw
// per-row argument tuples a single state observed; finalize replays them.
type aggState struct {
	ctor *aggConstructor
	rows [][]any
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
		// `extract` leaf to it). HLL_COUNT.MERGE_PARTIAL stays on the SQLite path
		// (its spec is status=partial, documentation-only on this target).
		{name: "init", minParams: 1, maxParams: 2, newAgg: hll.BindHllCountInit()},
		{name: "merge", minParams: 1, maxParams: 1, newAgg: hll.BindHllCountMerge()},

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
		{name: "anon_sum", minParams: 1, maxParams: 5, newAgg: aggregate.BindDifferentialPrivacySum(), resultType: C.DUCKDB_TYPE_DOUBLE},
		{name: "anon_count", minParams: 1, maxParams: 5, newAgg: aggregate.BindDifferentialPrivacyCount(), resultType: C.DUCKDB_TYPE_BIGINT},
		{name: "anon_count_star", minParams: 1, maxParams: 5, newAgg: aggregate.BindDifferentialPrivacyCountStar(), resultType: C.DUCKDB_TYPE_BIGINT},
		{name: "anon_avg", minParams: 1, maxParams: 5, newAgg: aggregate.BindDifferentialPrivacyAvg(), resultType: C.DUCKDB_TYPE_DOUBLE},
		{name: "anon_var_pop", minParams: 1, maxParams: 5, newAgg: aggregate.BindDifferentialPrivacyVarPop(), resultType: C.DUCKDB_TYPE_DOUBLE},
		{name: "anon_stddev_pop", minParams: 1, maxParams: 5, newAgg: aggregate.BindDifferentialPrivacyStddevPop(), resultType: C.DUCKDB_TYPE_DOUBLE},
		{name: "anon_percentile_cont", minParams: 1, maxParams: 6, newAgg: aggregate.BindDifferentialPrivacyPercentileCont(), resultType: C.DUCKDB_TYPE_DOUBLE},
		// QUANTILES returns an ARRAY of cut points; it stays on the VARCHAR
		// value-layer envelope. Its examples are nondeterministic (verify the
		// query runs), so the envelope result is not asserted by value.
		{name: "anon_quantiles", minParams: 1, maxParams: 6, newAgg: aggregate.BindDifferentialPrivacyQuantiles()},

		{name: "googlesqlite_differential_privacy_sum", minParams: 1, maxParams: 5, newAgg: aggregate.BindDifferentialPrivacySum(), resultType: C.DUCKDB_TYPE_DOUBLE},
		{name: "googlesqlite_differential_privacy_count", minParams: 1, maxParams: 5, newAgg: aggregate.BindDifferentialPrivacyCount(), resultType: C.DUCKDB_TYPE_BIGINT},
		{name: "googlesqlite_differential_privacy_count_star", minParams: 1, maxParams: 5, newAgg: aggregate.BindDifferentialPrivacyCountStar(), resultType: C.DUCKDB_TYPE_BIGINT},
		{name: "googlesqlite_differential_privacy_avg", minParams: 1, maxParams: 5, newAgg: aggregate.BindDifferentialPrivacyAvg(), resultType: C.DUCKDB_TYPE_DOUBLE},
		{name: "googlesqlite_differential_privacy_var_pop", minParams: 1, maxParams: 5, newAgg: aggregate.BindDifferentialPrivacyVarPop(), resultType: C.DUCKDB_TYPE_DOUBLE},
		{name: "googlesqlite_differential_privacy_stddev_pop", minParams: 1, maxParams: 5, newAgg: aggregate.BindDifferentialPrivacyStddevPop(), resultType: C.DUCKDB_TYPE_DOUBLE},
		{name: "googlesqlite_differential_privacy_percentile_cont", minParams: 1, maxParams: 6, newAgg: aggregate.BindDifferentialPrivacyPercentileCont(), resultType: C.DUCKDB_TYPE_DOUBLE},
		{name: "googlesqlite_differential_privacy_quantiles", minParams: 1, maxParams: 6, newAgg: aggregate.BindDifferentialPrivacyQuantiles()},
	}
}

// liveAggHandles pins the cgo.Handle for every registered aggregate for the
// process lifetime. The aggregate registration is per-connection, but DuckDB
// shares the registered catalog entry across the connection's database, and
// the extra-info pointer (a *cgo.Handle) must stay valid and unmoved for as
// long as any query can call the aggregate. We never unregister, so we keep
// the handles alive here rather than wiring a DuckDB delete callback (the C
// registration passes NULL for the destructor). Holding *cgo.Handle and
// passing unsafe.Pointer(&h) to C is the runtime/cgo idiom for storing a Go
// object behind a C void*.
var (
	liveAggMu      sync.Mutex
	liveAggHandles []*cgo.Handle
)

// registerDuckDBAggregates installs the aggregate / window UDF surface onto a
// DuckDB database handle. Mirrors registerDuckDBGeography: it grabs one
// connection (catalog-scoped registration is visible to every connection of
// the same database) and registers each aggregate through the C API.
func registerDuckDBAggregates(db *sql.DB) error {
	conn, err := db.Conn(context.Background())
	if err != nil {
		return fmt.Errorf("googlesqlite: duckdb aggregate conn: %w", err)
	}
	defer conn.Close()

	rawConn, err := rawDuckDBConn(conn)
	if err != nil {
		return err
	}

	for _, spec := range duckdbAggSpecs() {
		// A zero resultType means "use the default VARCHAR envelope".
		// DUCKDB_TYPE_INVALID is the C enum's zero value, so resolve it here.
		resultType := spec.resultType
		if resultType == C.duckdb_type(0) {
			resultType = C.DUCKDB_TYPE_VARCHAR
		}
		ctor := &aggConstructor{newAgg: spec.newAgg, singleState: spec.singleState, resultType: resultType}
		h := cgo.NewHandle(ctor)
		hp := &h
		liveAggMu.Lock()
		liveAggHandles = append(liveAggHandles, hp)
		liveAggMu.Unlock()

		cName := C.CString(spec.name)
		state := C.register_googlesqlite_aggregate(
			rawConn, cName, spec.minParams, spec.maxParams, resultType, unsafe.Pointer(hp))
		C.free(unsafe.Pointer(cName))
		if state == C.DuckDBError {
			return fmt.Errorf("googlesqlite: register aggregate %q failed", spec.name)
		}
	}
	return nil
}

// rawDuckDBConn extracts the underlying duckdb_connection C pointer from a
// database/sql connection. duckdb-go's *Conn holds the connection in an
// unexported field of type mapping.Connection (a struct with an exported Ptr
// unsafe.Pointer that IS the duckdb_connection), and exposes no public
// accessor, so we read the field through reflect + unsafe. This is the only way
// to register against the C aggregate API without forking duckdb-go.
func rawDuckDBConn(conn *sql.Conn) (C.duckdb_connection, error) {
	var ptr unsafe.Pointer
	err := conn.Raw(func(driverConn any) error {
		dc, ok := driverConn.(*duckdb.Conn)
		if !ok {
			return fmt.Errorf("googlesqlite: unexpected driver conn type %T", driverConn)
		}
		p, err := connPtrFromDuckDBConn(dc)
		if err != nil {
			return err
		}
		ptr = p
		return nil
	})
	if err != nil {
		return nil, err
	}
	return C.duckdb_connection(ptr), nil
}

// connPtrFromDuckDBConn reads the unexported `conn mapping.Connection` field of
// a duckdb-go *Conn and returns its exported Ptr. mapping.Connection is
// `struct { Ptr unsafe.Pointer }`, so the field offset is zero and Ptr is the
// raw duckdb_connection.
func connPtrFromDuckDBConn(dc *duckdb.Conn) (unsafe.Pointer, error) {
	rv := reflect.ValueOf(dc).Elem()
	field := rv.FieldByName("conn")
	if !field.IsValid() {
		return nil, fmt.Errorf("googlesqlite: duckdb.Conn has no conn field")
	}
	// The field is unexported; bypass the export check with unsafe so we can
	// read the embedded mapping.Connection struct value.
	connVal := reflect.NewAt(field.Type(), unsafe.Pointer(field.UnsafeAddr())).Elem()
	ptrField := connVal.FieldByName("Ptr")
	if !ptrField.IsValid() {
		return nil, fmt.Errorf("googlesqlite: mapping.Connection has no Ptr field")
	}
	return unsafe.Pointer(ptrField.Pointer()), nil
}

// ---- C-exported aggregate callbacks ----

//export googlesqliteAggStateSize
func googlesqliteAggStateSize(_ C.duckdb_function_info) C.idx_t {
	// One pointer-sized slot holds the cgo.Handle of the *aggState.
	return C.idx_t(unsafe.Sizeof(uintptr(0)))
}

//export googlesqliteAggInit
func googlesqliteAggInit(info C.duckdb_function_info, state C.duckdb_aggregate_state) {
	ctor := constructorFromInfo(info)
	st := &aggState{ctor: ctor}
	h := cgo.NewHandle(st)
	C.agg_state_store(state, C.uintptr_t(h))
}

//export googlesqliteAggUpdate
func googlesqliteAggUpdate(info C.duckdb_function_info, input C.duckdb_data_chunk, states *C.duckdb_aggregate_state) {
	rowCount := int(C.agg_chunk_size(input))
	colCount := int(C.agg_chunk_cols(input))
	if rowCount == 0 {
		return
	}

	// Read every column's vector, type, and JSON-ness once, then walk rows.
	vecs := make([]C.duckdb_vector, colCount)
	types := make([]C.duckdb_type, colCount)
	isJSON := make([]bool, colCount)
	for c := 0; c < colCount; c++ {
		vecs[c] = C.agg_chunk_vector(input, C.idx_t(c))
		types[c] = C.agg_vec_type_id(vecs[c])
		isJSON[c] = bool(C.agg_vec_is_json(vecs[c]))
	}

	// In single-state mode (window aggregates under OVER ()), the states array
	// holds a single accumulator for the whole frame even though the input
	// chunk carries every frame row, so every row collects into states[0]. In
	// the default scatter mode, DuckDB sizes the states array to the input
	// chunk and row r belongs to states[r]; for a single GROUP every entry is
	// the same pointer, while multi-group queries get distinct per-row states.
	single := constructorFromInfo(info).singleState
	stateSlice := unsafe.Slice(states, rowCount)

	var singleSt *aggState
	if single {
		singleSt = stateFromHandle(stateSlice[0])
	}

	for r := 0; r < rowCount; r++ {
		var st *aggState
		if single {
			st = singleSt
		} else {
			st = stateFromHandle(stateSlice[r])
		}
		args := make([]any, colCount)
		for c := 0; c < colCount; c++ {
			v := readVectorValue(vecs[c], types[c], C.idx_t(r))
			if isJSON[c] {
				v = normalizeJSONScalar(v)
			}
			args[c] = v
		}
		st.rows = append(st.rows, args)
	}
}

//export googlesqliteAggCombine
func googlesqliteAggCombine(_ C.duckdb_function_info, source *C.duckdb_aggregate_state, target *C.duckdb_aggregate_state, count C.idx_t) {
	n := int(count)
	srcSlice := unsafe.Slice(source, n)
	tgtSlice := unsafe.Slice(target, n)
	for i := 0; i < n; i++ {
		src := stateFromHandle(srcSlice[i])
		tgt := stateFromHandle(tgtSlice[i])
		// Concatenate the source's collected rows into the target. Replay at
		// finalize then sees the full group regardless of partitioning.
		tgt.rows = append(tgt.rows, src.rows...)
	}
}

//export googlesqliteAggFinalize
func googlesqliteAggFinalize(info C.duckdb_function_info, source *C.duckdb_aggregate_state, result C.duckdb_vector, count C.idx_t, offset C.idx_t) {
	n := int(count)
	srcSlice := unsafe.Slice(source, n)
	for i := 0; i < n; i++ {
		st := stateFromHandle(srcSlice[i])
		out, err := st.finalize()
		if err != nil {
			C.agg_set_error(info, C.CString(err.Error()))
			return
		}
		rowIdx := C.idx_t(i) + offset
		writeResult(result, rowIdx, out, st.ctor.resultType)
	}
}

//export googlesqliteAggDestroy
func googlesqliteAggDestroy(states *C.duckdb_aggregate_state, count C.idx_t) {
	n := int(count)
	slice := unsafe.Slice(states, n)
	for i := 0; i < n; i++ {
		raw := C.agg_state_load(slice[i])
		if raw == 0 {
			continue
		}
		h := cgo.Handle(raw)
		h.Delete()
		C.agg_state_store(slice[i], 0)
	}
}

// finalize builds one fresh helper.Aggregator, replays every collected row in
// arrival order, and returns the encoded result of Done. An empty group (no
// rows) still constructs the aggregator and calls Done, matching GoogleSQL's
// "aggregate over zero rows" semantics (e.g. NULL).
func (st *aggState) finalize() (any, error) {
	agg := st.ctor.newAgg()
	for _, row := range st.rows {
		if err := agg.Step(row...); err != nil {
			return nil, err
		}
	}
	return agg.Done()
}

// constructorFromInfo recovers the *aggConstructor pinned in the aggregate's
// extra-info pointer. The pointer is the *cgo.Handle stored at registration.
func constructorFromInfo(info C.duckdb_function_info) *aggConstructor {
	hp := (*cgo.Handle)(C.agg_extra_info(info))
	return hp.Value().(*aggConstructor)
}

// stateFromHandle recovers the *aggState a state slot points at.
func stateFromHandle(state C.duckdb_aggregate_state) *aggState {
	raw := C.agg_state_load(state)
	return cgo.Handle(uintptr(raw)).Value().(*aggState)
}

// readVectorValue reads one row of a DuckDB vector and returns the value in the
// value-layer wire form helper.Aggregator.Step expects: a Go int64 / float64 /
// bool for numeric/boolean inputs, or a Go string for VARCHAR / BLOB inputs
// (the base64 envelope for the BYTES / GEOGRAPHY / ARRAY families). A NULL row
// returns nil.
func readVectorValue(vec C.duckdb_vector, typeID C.duckdb_type, row C.idx_t) any {
	if !bool(C.agg_row_valid(vec, row)) {
		return nil
	}
	switch typeID {
	case C.DUCKDB_TYPE_BIGINT:
		return int64(C.agg_read_i64(vec, row))
	case C.DUCKDB_TYPE_INTEGER:
		return int64(C.agg_read_i32(vec, row))
	case C.DUCKDB_TYPE_SMALLINT:
		return int64(C.agg_read_i16(vec, row))
	case C.DUCKDB_TYPE_TINYINT:
		return int64(C.agg_read_i8(vec, row))
	case C.DUCKDB_TYPE_DOUBLE:
		return float64(C.agg_read_f64(vec, row))
	case C.DUCKDB_TYPE_FLOAT:
		return float64(C.agg_read_f32(vec, row))
	case C.DUCKDB_TYPE_DECIMAL:
		return float64(C.agg_read_decimal_double(vec, row))
	case C.DUCKDB_TYPE_BOOLEAN:
		return bool(C.agg_read_bool(vec, row))
	case C.DUCKDB_TYPE_VARCHAR, C.DUCKDB_TYPE_BLOB:
		n := C.agg_read_string_len(vec, row)
		ptr := C.agg_read_string_ptr(vec, row)
		return C.GoStringN(ptr, C.int(n))
	default:
		// Any other type the binder allowed: surface as a string so the value
		// decoder can attempt to interpret it. The aggregates in this file only
		// ever bind the numeric / VARCHAR / BLOB inputs above.
		n := C.agg_read_string_len(vec, row)
		ptr := C.agg_read_string_ptr(vec, row)
		return C.GoStringN(ptr, C.int(n))
	}
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

// writeResult writes one aggregate result into the result vector at rowIdx.
// helper.Aggregator.Done returns the value-layer wire form: nil for a NULL
// result, a bare int64 / float64 / bool for numeric/boolean results, or the
// base64 envelope string for the richer types.
//
// resultType selects the write path. DOUBLE / BIGINT result columns (the
// differential-privacy aggregates) take the native numeric value directly so
// the surrounding SQL's CAST / arithmetic and the result-set materializer see a
// real number. The default VARCHAR column re-encodes a bare primitive back into
// the value-layer envelope so it round-trips through DecodeValue on scan.
func writeResult(result C.duckdb_vector, rowIdx C.idx_t, out any, resultType C.duckdb_type) {
	if out == nil {
		C.agg_write_null(result, rowIdx)
		return
	}
	switch resultType {
	case C.DUCKDB_TYPE_DOUBLE:
		if f, ok := toFloat64Result(out); ok {
			C.agg_write_double(result, rowIdx, C.double(f))
			return
		}
		C.agg_write_null(result, rowIdx)
		return
	case C.DUCKDB_TYPE_BIGINT:
		if i, ok := toInt64Result(out); ok {
			C.agg_write_int64(result, rowIdx, C.int64_t(i))
			return
		}
		C.agg_write_null(result, rowIdx)
		return
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
	cs := C.CString(s)
	C.agg_write_string(result, rowIdx, cs, C.idx_t(len(s)))
	C.free(unsafe.Pointer(cs))
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
