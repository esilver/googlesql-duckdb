package googlesqlite

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"

	duckdb "github.com/goccy/googlesqlite/internal/duckdbgo"

	internal "github.com/goccy/googlesqlite/internal"
)

// registerDuckDBUDFs installs the minimal set of googlesqlite_* scalar
// UDFs that the Milestone-0 boot/critical path needs onto a DuckDB
// database handle. Today that is only googlesqlite_in_array, which the
// formatter emits when lowering `IN UNNEST(@param)` (used by every
// metadata FindProject/FindDataset/... query, so it is on the boot and
// per-request path). The full ~348-function surface (function_register_*)
// stays on the SQLite-coupled sqlitex adapter and is the Milestone-1
// retarget; this file only proves the registration + value round-trip
// mechanism works against duckdb-go.
//
// Registration happens once per inner *sql.DB. DuckDB registers scalar
// functions at catalog scope, so a single registration is visible to all
// connections of the same database (the inner pool is pinned to one
// connection regardless).
func registerDuckDBUDFs(db *sql.DB) error {
	// The formatter's getFuncNameAndArgs lowers a builtin to its
	// googlesqlite_<name> shell only when the name is present in
	// normalFuncMap. Fill the maps so the string-family builtins stop
	// falling through to a bare DuckDB-native call on the base64 envelope.
	internal.EnsureFuncMaps()

	conn, err := db.Conn(context.Background())
	if err != nil {
		return fmt.Errorf("googlesqlite: duckdb udf conn: %w", err)
	}
	defer conn.Close()
	if err := duckdb.RegisterScalarUDF(conn, "googlesqlite_in_array", &inArrayUDF{}); err != nil {
		return fmt.Errorf("googlesqlite: register googlesqlite_in_array: %w", err)
	}
	if err := duckdb.RegisterScalarUDF(conn, "googlesqlite_decode_array", &decodeArrayUDF{}); err != nil {
		return fmt.Errorf("googlesqlite: register googlesqlite_decode_array: %w", err)
	}
	if err := duckdb.RegisterScalarUDF(conn, "googlesqlite_decode_scalar", &decodeScalarUDF{}); err != nil {
		return fmt.Errorf("googlesqlite: register googlesqlite_decode_scalar: %w", err)
	}
	if err := duckdb.RegisterScalarUDF(conn, "googlesqlite_unnest_temporal", &unnestTemporalUDF{}); err != nil {
		return fmt.Errorf("googlesqlite: register googlesqlite_unnest_temporal: %w", err)
	}
	if err := duckdb.RegisterScalarUDF(conn, "googlesqlite_cast", &castUDF{}); err != nil {
		return fmt.Errorf("googlesqlite: register googlesqlite_cast: %w", err)
	}
	// Math builtins whose arguments are value-layer ARRAY envelopes
	// (RANGE_BUCKET, EUCLIDEAN_DISTANCE, COSINE_DISTANCE) cannot lower to
	// a native DuckDB list expression, so they route through these UDFs
	// that decode the envelope and reuse the shared pure-Go bodies.
	if err := duckdb.RegisterScalarUDF(conn, "googlesqlite_range_bucket", &rangeBucketUDF{}); err != nil {
		return fmt.Errorf("googlesqlite: register googlesqlite_range_bucket: %w", err)
	}
	if err := duckdb.RegisterScalarUDF(conn, "googlesqlite_euclidean_distance", &euclideanDistanceUDF{}); err != nil {
		return fmt.Errorf("googlesqlite: register googlesqlite_euclidean_distance: %w", err)
	}
	if err := duckdb.RegisterScalarUDF(conn, "googlesqlite_cosine_distance", &cosineDistanceUDF{}); err != nil {
		return fmt.Errorf("googlesqlite: register googlesqlite_cosine_distance: %w", err)
	}
	for name, fn := range envelopeArrayUDFs {
		if err := duckdb.RegisterScalarUDF(conn, name, newValueLayerUDF(fn, duckdb.TYPE_VARCHAR)); err != nil {
			return fmt.Errorf("googlesqlite: register %s: %w", name, err)
		}
	}
	for name, u := range nativeScalarArrayUDFs {
		if err := duckdb.RegisterScalarUDF(conn, name, newValueLayerUDF(u.fn, u.resultType)); err != nil {
			return fmt.Errorf("googlesqlite: register %s: %w", name, err)
		}
	}
	if err := registerNetUDFs(conn); err != nil {
		return err
	}
	if err := registerStructUDFs(conn); err != nil {
		return err
	}
	if err := registerProtoUDFs(conn); err != nil {
		return err
	}
	if err := registerExtraScalarUDFs(conn); err != nil {
		return err
	}
	if err := registerRangeUDFs(conn); err != nil {
		return err
	}
	if err := registerNormalFuncUDFs(conn); err != nil {
		return err
	}
	if err := registerChangeHistoryTableUDFs(conn); err != nil {
		return err
	}
	return nil
}

// castUDF adapts internal.GoogleSQLCast to DuckDB's scalar-UDF interface. The
// formatter (CastNode) emits googlesqlite_cast(expr, fromTypeJSON, toTypeJSON,
// isSafe) for every CAST / SAFE_CAST. The expression argument is polymorphic:
// value-layer primitives (BOOL/INT/FLOAT) arrive as their native Go types and
// richer types as a base64 VARCHAR envelope, so the whole signature is
// declared variadic over TYPE_ANY. The result is the value-layer wire form,
// declared VARCHAR (the encoded envelope DuckDB stores and the value decoder
// reads back on scan).
type castUDF struct{}

func (*castUDF) Config() duckdb.ScalarFuncConfig {
	any_, _ := duckdb.NewTypeInfo(duckdb.TYPE_ANY)
	varchar, _ := duckdb.NewTypeInfo(duckdb.TYPE_VARCHAR)
	return duckdb.ScalarFuncConfig{
		// No fixed leading params; every argument matches the variadic ANY
		// type so DuckDB accepts the native-typed expression plus the three
		// encoded metadata args without per-call coercion.
		VariadicTypeInfo: any_,
		ResultTypeInfo:   varchar,
		// CAST of a NULL input is NULL, and SAFE_CAST folds errors to NULL,
		// so route NULLs through the body rather than DuckDB's NULL-in
		// short-circuit.
		SpecialNullHandling: true,
	}
}

func (*castUDF) Executor() duckdb.ScalarFuncExecutor {
	return duckdb.ScalarFuncExecutor{
		RowExecutor: func(values []driver.Value) (any, error) {
			if len(values) != 4 {
				return nil, fmt.Errorf("googlesqlite_cast: want 4 args, got %d", len(values))
			}
			return internal.GoogleSQLCast(values[0], values[1], values[2], values[3])
		},
	}
}

// decodeArrayUDF adapts internal.GoogleSQLDecodeArray to DuckDB's scalar-UDF
// interface. The single argument is the engine's value-layer wire form for an
// ARRAY (a base64-encoded JSON envelope VARCHAR); the result is a plain JSON
// array string that DuckDB's json_each table function iterates. The formatter
// emits this as the inner expression of every UNNEST(<array>) lowering.
type decodeArrayUDF struct{}

func (*decodeArrayUDF) Config() duckdb.ScalarFuncConfig {
	varchar, _ := duckdb.NewTypeInfo(duckdb.TYPE_VARCHAR)
	// The argument is polymorphic: the value-layer ARRAY envelope arrives as
	// a VARCHAR, while a DuckDB-native aggregate (array_agg / list / the
	// ARRAY(SELECT ...) subquery lowering) hands back a native LIST. Declare
	// the single input as TYPE_ANY so the binder accepts both without an
	// explicit cast; the bridge branches on the Go type it receives.
	anyType, _ := duckdb.NewTypeInfo(duckdb.TYPE_ANY)
	return duckdb.ScalarFuncConfig{
		InputTypeInfos: []duckdb.TypeInfo{anyType},
		ResultTypeInfo: varchar,
		// A NULL array argument must decode to "[]" (an empty unnest), so
		// route NULLs through the body rather than short-circuiting to NULL.
		SpecialNullHandling: true,
	}
}

func (*decodeArrayUDF) Executor() duckdb.ScalarFuncExecutor {
	return duckdb.ScalarFuncExecutor{
		RowExecutor: func(values []driver.Value) (any, error) {
			if len(values) != 1 {
				return nil, fmt.Errorf("googlesqlite_decode_array: want 1 arg, got %d", len(values))
			}
			return internal.GoogleSQLDecodeArray(values[0])
		},
	}
}

// decodeScalarUDF adapts internal.GoogleSQLDecodeScalar to DuckDB's
// scalar-UDF interface. The formatter wraps a scalar struct-field access
// (googlesqlite_get_struct_field, which returns the value-layer envelope)
// in googlesqlite_decode_scalar so the bare textual value falls out of the
// envelope, then casts it to the field's resolved scalar type. The argument
// is usually the envelope VARCHAR, but the safe-eval wrap (IFERROR /
// NULLIFERROR try arms) can hand it a value that is already native — e.g. a
// scalar subquery over an UNNEST element, which the typed UNNEST projection
// now emits as BIGINT / DOUBLE / BOOLEAN — so the input is declared TYPE_ANY
// and the bridge's DecodeValue round-trips native scalars to their textual
// form unchanged. The result is the bare value rendered through the VARCHAR
// result type.
type decodeScalarUDF struct{}

func (*decodeScalarUDF) Config() duckdb.ScalarFuncConfig {
	varchar, _ := duckdb.NewTypeInfo(duckdb.TYPE_VARCHAR)
	anyType, _ := duckdb.NewTypeInfo(duckdb.TYPE_ANY)
	return duckdb.ScalarFuncConfig{
		InputTypeInfos: []duckdb.TypeInfo{anyType},
		ResultTypeInfo: varchar,
		// A NULL field value (struct field access on a NULL struct) must stay
		// NULL rather than decode, so route NULLs through the body.
		SpecialNullHandling: true,
	}
}

func (*decodeScalarUDF) Executor() duckdb.ScalarFuncExecutor {
	return duckdb.ScalarFuncExecutor{
		RowExecutor: func(values []driver.Value) (any, error) {
			if len(values) != 1 {
				return nil, fmt.Errorf("googlesqlite_decode_scalar: want 1 arg, got %d", len(values))
			}
			return internal.GoogleSQLDecodeScalar(values[0])
		},
	}
}

// unnestTemporalUDF adapts internal.GoogleSQLUnnestTemporal to DuckDB's
// scalar-UDF interface. The formatter emits
// CAST(googlesqlite_unnest_temporal(json_extract_string(json_each.value,
// '$'), '<kind>') AS <native temporal type>) when projecting a temporal
// element out of the UNNEST lowering. Both arguments are VARCHAR (the
// element's envelope-or-text wire form, and the resolved kind name); the
// result is the canonical native-castable text rendering.
type unnestTemporalUDF struct{}

func (*unnestTemporalUDF) Config() duckdb.ScalarFuncConfig {
	varchar, _ := duckdb.NewTypeInfo(duckdb.TYPE_VARCHAR)
	return duckdb.ScalarFuncConfig{
		InputTypeInfos: []duckdb.TypeInfo{varchar, varchar},
		ResultTypeInfo: varchar,
		// A NULL array element must stay NULL; the body folds it before
		// touching the kind argument.
		SpecialNullHandling: true,
	}
}

func (*unnestTemporalUDF) Executor() duckdb.ScalarFuncExecutor {
	return duckdb.ScalarFuncExecutor{
		RowExecutor: func(values []driver.Value) (any, error) {
			if len(values) != 2 {
				return nil, fmt.Errorf("googlesqlite_unnest_temporal: want 2 args, got %d", len(values))
			}
			return internal.GoogleSQLUnnestTemporal(values[0], values[1])
		},
	}
}

// inArrayUDF adapts the engine's pure-Go internal.GoogleSQLInArray body
// to DuckDB's scalar-UDF interface. The array argument arrives as the
// engine's value-layer wire form (a VARCHAR holding base64-encoded JSON).
// The element argument is polymorphic: a VARCHAR envelope from the
// value-layer encode path, or a native BIGINT / DOUBLE / BOOLEAN when the
// probe is a typed UNNEST element (the ARRAY_INCLUDES_* rewrites lower to
// `e IN UNNEST(...)` over the decoded element), so it is declared TYPE_ANY
// and DecodeValue normalises both forms. We decode each argument back into
// a value.Value, delegate to the shared membership body, and hand DuckDB a
// native Go bool.
type inArrayUDF struct{}

func (*inArrayUDF) Config() duckdb.ScalarFuncConfig {
	varchar, _ := duckdb.NewTypeInfo(duckdb.TYPE_VARCHAR)
	boolean, _ := duckdb.NewTypeInfo(duckdb.TYPE_BOOLEAN)
	anyType, _ := duckdb.NewTypeInfo(duckdb.TYPE_ANY)
	return duckdb.ScalarFuncConfig{
		InputTypeInfos: []duckdb.TypeInfo{anyType, varchar},
		ResultTypeInfo: boolean,
		// The element argument can legitimately be NULL while the array is
		// not; defer NULL semantics to the membership body rather than
		// short-circuiting to NULL on any NULL input.
		SpecialNullHandling: true,
	}
}

func (*inArrayUDF) Executor() duckdb.ScalarFuncExecutor {
	return duckdb.ScalarFuncExecutor{
		RowExecutor: func(values []driver.Value) (any, error) {
			if len(values) != 2 {
				return nil, fmt.Errorf("googlesqlite_in_array: want 2 args, got %d", len(values))
			}
			return internal.GoogleSQLInArray(values[0], values[1])
		},
	}
}

// registerDuckDBGeography loads the spatial extension and registers the
// geography scalar UDF surface. Split from registerDuckDBUDFs so the
// connection-open path can call it after the base UDFs are in place.
func registerDuckDBGeography(db *sql.DB) error {
	if err := loadSpatialExtension(db); err != nil {
		return err
	}
	return registerGeographyUDFs(db)
}

// rangeBucketUDF adapts internal.GoogleSQLRangeBucket. Both arguments
// (the scalar point and the value-layer ARRAY envelope) arrive as
// VARCHAR; the result is the BIGINT bucket index.
type rangeBucketUDF struct{}

func (*rangeBucketUDF) Config() duckdb.ScalarFuncConfig {
	// The point arg is emitted as a bare numeric literal (not a value-layer
	// envelope), so accept it as DOUBLE - DuckDB widens INTEGER/DECIMAL
	// literals to it, and the bridge compares the decoded FloatValue
	// against the boundary elements via float coercion. The boundaries
	// arrive as the VARCHAR envelope.
	varchar, _ := duckdb.NewTypeInfo(duckdb.TYPE_VARCHAR)
	double, _ := duckdb.NewTypeInfo(duckdb.TYPE_DOUBLE)
	bigint, _ := duckdb.NewTypeInfo(duckdb.TYPE_BIGINT)
	return duckdb.ScalarFuncConfig{
		InputTypeInfos:      []duckdb.TypeInfo{double, varchar},
		ResultTypeInfo:      bigint,
		SpecialNullHandling: true,
	}
}

func (*rangeBucketUDF) Executor() duckdb.ScalarFuncExecutor {
	return duckdb.ScalarFuncExecutor{
		RowExecutor: func(values []driver.Value) (any, error) {
			if len(values) != 2 {
				return nil, fmt.Errorf("googlesqlite_range_bucket: want 2 args, got %d", len(values))
			}
			return internal.GoogleSQLRangeBucket(values[0], values[1])
		},
	}
}

// euclideanDistanceUDF and cosineDistanceUDF adapt the vector-distance
// bridges. Both ARRAY<FLOAT64> arguments arrive as VARCHAR envelopes; the
// result is a DOUBLE distance.
type euclideanDistanceUDF struct{}

func (*euclideanDistanceUDF) Config() duckdb.ScalarFuncConfig {
	return vectorDistanceUDFConfig()
}

func (*euclideanDistanceUDF) Executor() duckdb.ScalarFuncExecutor {
	return duckdb.ScalarFuncExecutor{
		RowExecutor: func(values []driver.Value) (any, error) {
			if len(values) != 2 {
				return nil, fmt.Errorf("googlesqlite_euclidean_distance: want 2 args, got %d", len(values))
			}
			return internal.GoogleSQLEuclideanDistance(values[0], values[1])
		},
	}
}

type cosineDistanceUDF struct{}

func (*cosineDistanceUDF) Config() duckdb.ScalarFuncConfig {
	return vectorDistanceUDFConfig()
}

func (*cosineDistanceUDF) Executor() duckdb.ScalarFuncExecutor {
	return duckdb.ScalarFuncExecutor{
		RowExecutor: func(values []driver.Value) (any, error) {
			if len(values) != 2 {
				return nil, fmt.Errorf("googlesqlite_cosine_distance: want 2 args, got %d", len(values))
			}
			return internal.GoogleSQLCosineDistance(values[0], values[1])
		},
	}
}

func vectorDistanceUDFConfig() duckdb.ScalarFuncConfig {
	varchar, _ := duckdb.NewTypeInfo(duckdb.TYPE_VARCHAR)
	double, _ := duckdb.NewTypeInfo(duckdb.TYPE_DOUBLE)
	return duckdb.ScalarFuncConfig{
		InputTypeInfos:      []duckdb.TypeInfo{varchar, varchar},
		ResultTypeInfo:      double,
		SpecialNullHandling: true,
	}
}

// envelopeArrayUDFs maps the registered DuckDB scalar UDF name to the
// internal value-layer bridge body for functions whose result is itself a
// value-layer envelope (an ARRAY, or an element of statically-unknown
// type). The formatter (tryArrayValueLayerFunc) and the analyzer's
// higher-order array rewrites emit these names so calls land on the
// pure-Go bodies operating over the base64 envelope wire form instead of
// DuckDB's native array_* functions, which expect native LIST values.
var envelopeArrayUDFs = map[string]func(...any) (any, error){
	"googlesqlite_array_concat":             internal.GoogleSQLArrayConcat,
	"googlesqlite_array_concat_agg_flatten": internal.GoogleSQLArrayConcatAggFlatten,
	"googlesqlite_array_from_native":        internal.GoogleSQLArrayFromNative,
	"googlesqlite_array_reverse":            internal.GoogleSQLArrayReverse,
	"googlesqlite_generate_array":           internal.GoogleSQLGenerateArray,
	"googlesqlite_generate_date_array":      internal.GoogleSQLGenerateDateArray,
	"googlesqlite_generate_timestamp_array": internal.GoogleSQLGenerateTimestampArray,
	"googlesqlite_array_at_offset":          internal.GoogleSQLArrayAtOffset,
	"googlesqlite_safe_array_at_offset":     internal.GoogleSQLSafeArrayAtOffset,
	"googlesqlite_array_at_ordinal":         internal.GoogleSQLArrayAtOrdinal,
	"googlesqlite_safe_array_at_ordinal":    internal.GoogleSQLSafeArrayAtOrdinal,
}

// nativeScalarArrayUDFs maps the registered DuckDB scalar UDF name to a
// bridge body plus the concrete DuckDB result type to declare. These
// functions have a fixed GoogleSQL scalar result type and are reused by
// the analyzer inside SQL comparisons / aggregations / json_each
// arguments, so they must return a coercible native value rather than an
// envelope VARCHAR.
//
// googlesqlite_decode_array is intentionally absent here: it is registered
// standalone above (decodeArrayUDF) so the array and aggregate paths share
// a single registration.
var nativeScalarArrayUDFs = map[string]struct {
	fn         func(...any) (any, error)
	resultType duckdb.Type
}{
	"googlesqlite_array_length":      {internal.GoogleSQLArrayLength, duckdb.TYPE_BIGINT},
	"googlesqlite_array_to_string":   {internal.GoogleSQLArrayToString, duckdb.TYPE_VARCHAR},
	"googlesqlite_array_is_distinct": {internal.GoogleSQLArrayIsDistinct, duckdb.TYPE_BOOLEAN},
	// to_json_string returns plain JSON text (not an envelope), so it
	// belongs with the native-typed bodies.
	"to_json_string": {internal.GoogleSQLToJsonString, duckdb.TYPE_VARCHAR},
}

// valueLayerUDF adapts a variadic value-layer bridge body to DuckDB's
// scalar-UDF interface. Every argument is declared TYPE_ANY variadic so a
// single registration serves the fixed- and variable-arity array
// functions alike and preserves the native Go type of each argument
// (INT64 / FLOAT64 / BOOL columns arrive as Go primitives; ARRAY / STRING
// arguments arrive as the base64 envelope VARCHAR). The declared result
// type is per-function: an envelope VARCHAR for array/element results, or
// a concrete native type for fixed-scalar results.
type valueLayerUDF struct {
	fn         func(...any) (any, error)
	resultType duckdb.Type
}

func newValueLayerUDF(fn func(...any) (any, error), resultType duckdb.Type) *valueLayerUDF {
	return &valueLayerUDF{fn: fn, resultType: resultType}
}

func (u *valueLayerUDF) Config() duckdb.ScalarFuncConfig {
	result, _ := duckdb.NewTypeInfo(u.resultType)
	anyType, _ := duckdb.NewTypeInfo(duckdb.TYPE_ANY)
	return duckdb.ScalarFuncConfig{
		ResultTypeInfo:   result,
		VariadicTypeInfo: anyType,
		// The bodies define their own NULL semantics (e.g. ARRAY_CONCAT of
		// a NULL array, GENERATE_ARRAY with a NULL bound), so do not let
		// DuckDB short-circuit to NULL on any NULL argument.
		SpecialNullHandling: true,
	}
}

func (u *valueLayerUDF) Executor() duckdb.ScalarFuncExecutor {
	return duckdb.ScalarFuncExecutor{
		RowExecutor: func(values []driver.Value) (any, error) {
			args := make([]any, len(values))
			for i, v := range values {
				args[i] = v
			}
			return u.fn(args...)
		},
	}
}

// netResultType selects the DuckDB result type for each NET.* scalar
// UDF. The formatter renders these calls with the analyzer-declared
// return type, so the encoded body output must match: INT64-returning
// functions hand back a Go int64, the membership predicate a Go bool,
// and the rest the value-layer base64-JSON envelope string.
func netResultType(name string) duckdb.Type {
	switch name {
	case "ipv4_to_int64", "parse_ip":
		return duckdb.TYPE_BIGINT
	case "ip_in_net":
		return duckdb.TYPE_BOOLEAN
	default:
		return duckdb.TYPE_VARCHAR
	}
}

// registerNetUDFs installs one scalar UDF per NET.* function the
// formatter lowers to a bare storage name (make_net, ip_to_string,
// ...). Each UDF accepts a variadic ANY argument list so a single
// shell covers the mix of value-layer VARCHAR arguments (strings /
// bytes round-tripped from nested NET calls or literals) and native
// integer-literal arguments, deferring arity and type checks to the
// shared pure-Go body.
func registerNetUDFs(conn *sql.Conn) error {
	for _, name := range internal.NetUDFNames() {
		udf, err := newNetUDF(name)
		if err != nil {
			return fmt.Errorf("googlesqlite: build net udf %q: %w", name, err)
		}
		if err := duckdb.RegisterScalarUDF(conn, name, udf); err != nil {
			return fmt.Errorf("googlesqlite: register net udf %q: %w", name, err)
		}
	}
	return nil
}

// netUDF adapts a single NET.* pure-Go body to DuckDB's scalar-UDF
// interface. name is the lowered storage name the formatter emits and
// the key the internal bridge dispatches on.
type netUDF struct {
	name       string
	anyInfo    duckdb.TypeInfo
	resultInfo duckdb.TypeInfo
}

func newNetUDF(name string) (*netUDF, error) {
	anyInfo, err := duckdb.NewTypeInfo(duckdb.TYPE_ANY)
	if err != nil {
		return nil, err
	}
	resultInfo, err := duckdb.NewTypeInfo(netResultType(name))
	if err != nil {
		return nil, err
	}
	return &netUDF{name: name, anyInfo: anyInfo, resultInfo: resultInfo}, nil
}

func (u *netUDF) Config() duckdb.ScalarFuncConfig {
	return duckdb.ScalarFuncConfig{
		// A variadic ANY arg list lets the same shell bind both the
		// 1-arg and 2-arg NET functions and accept either a VARCHAR
		// envelope or a native integer per argument.
		VariadicTypeInfo: u.anyInfo,
		ResultTypeInfo:   u.resultInfo,
		// NET.PARSE_IP(NULL) must fold to NULL inside the body, and the
		// helper-wrapped bodies already implement per-function NULL
		// semantics, so disable DuckDB's blanket null-in/null-out.
		SpecialNullHandling: true,
	}
}

func (u *netUDF) Executor() duckdb.ScalarFuncExecutor {
	name := u.name
	return duckdb.ScalarFuncExecutor{
		RowExecutor: func(values []driver.Value) (any, error) {
			args := make([]any, len(values))
			for i, v := range values {
				args[i] = v
			}
			return internal.GoogleSQLNetCall(name, args...)
		},
	}
}

// rangeUDF adapts one of the engine's pure-Go RANGE function bodies to
// DuckDB's scalar-UDF interface. RANGE has no DuckDB-native type, so the
// formatter lowers every RANGE function (RANGE, RANGE_START, RANGE_END,
// RANGE_CONTAINS, RANGE_OVERLAPS, RANGE_INTERSECT, GENERATE_RANGE_ARRAY)
// to a bare scalar call whose arguments arrive in the value-layer wire
// form. Each argument is a VARCHAR base64 envelope for the RANGE / DATE /
// INTERVAL operands, or a native bool / int / float for primitive
// literals (e.g. GENERATE_RANGE_ARRAY's optional last_partial_range
// BOOL). The bridge decodes them, runs the shared body, and re-encodes
// the result.
type rangeUDF struct {
	bridge     func([]any) (any, error)
	fixedArgs  int
	variadic   bool
	resultBool bool
	// anyArgs declares the fixed inputs as TYPE_ANY instead of VARCHAR.
	// The INTERVAL(n, datepart) constructor takes a native integer first
	// argument that must not be coerced to VARCHAR before the bridge's
	// decoder sees it.
	anyArgs bool
}

func (u *rangeUDF) Config() duckdb.ScalarFuncConfig {
	varchar, _ := duckdb.NewTypeInfo(duckdb.TYPE_VARCHAR)
	fixedType := varchar
	if u.anyArgs {
		fixedType, _ = duckdb.NewTypeInfo(duckdb.TYPE_ANY)
	}
	inputs := make([]duckdb.TypeInfo, u.fixedArgs)
	for i := range inputs {
		inputs[i] = fixedType
	}
	cfg := duckdb.ScalarFuncConfig{
		InputTypeInfos: inputs,
		// RANGE bounds and the predicate inputs legitimately carry NULL
		// (UNBOUNDED bounds, NULL ranges); defer NULL semantics to the
		// pure-Go body rather than short-circuiting to NULL on any NULL
		// input.
		SpecialNullHandling: true,
	}
	if u.resultBool {
		cfg.ResultTypeInfo, _ = duckdb.NewTypeInfo(duckdb.TYPE_BOOLEAN)
	} else {
		cfg.ResultTypeInfo = varchar
	}
	if u.variadic {
		// Trailing arguments (GENERATE_RANGE_ARRAY's INTERVAL step and
		// optional BOOL) arrive with heterogeneous native types, so the
		// variadic tail must accept TYPE_ANY and let the bridge's decoder
		// normalise each value.
		cfg.VariadicTypeInfo, _ = duckdb.NewTypeInfo(duckdb.TYPE_ANY)
	}
	return cfg
}

func (u *rangeUDF) Executor() duckdb.ScalarFuncExecutor {
	return duckdb.ScalarFuncExecutor{
		RowExecutor: func(values []driver.Value) (any, error) {
			args := make([]any, len(values))
			for i, v := range values {
				args[i] = v
			}
			return u.bridge(args)
		},
	}
}

// registerRangeUDFs installs the RANGE-family scalar UDFs onto a DuckDB
// connection. The names match exactly what the formatter emits for the
// resolved GoogleSQL RANGE functions (the bare lowered names, since the
// legacy sqlitex normalFuncMap is not populated on the DuckDB path).
func registerRangeUDFs(conn *sql.Conn) error {
	udfs := []struct {
		name string
		udf  *rangeUDF
	}{
		{"googlesqlite_range", &rangeUDF{bridge: internal.GoogleSQLRange, fixedArgs: 2}},
		{"googlesqlite_range_start", &rangeUDF{bridge: internal.GoogleSQLRangeStart, fixedArgs: 1}},
		{"googlesqlite_range_end", &rangeUDF{bridge: internal.GoogleSQLRangeEnd, fixedArgs: 1}},
		{"googlesqlite_range_contains", &rangeUDF{bridge: internal.GoogleSQLRangeContains, fixedArgs: 2, resultBool: true}},
		{"googlesqlite_range_overlaps", &rangeUDF{bridge: internal.GoogleSQLRangeOverlaps, fixedArgs: 2, resultBool: true}},
		{"googlesqlite_range_intersect", &rangeUDF{bridge: internal.GoogleSQLRangeIntersect, fixedArgs: 2}},
		{"googlesqlite_generate_range_array", &rangeUDF{bridge: internal.GoogleSQLGenerateRangeArray, fixedArgs: 1, variadic: true}},
		{"googlesqlite_interval", &rangeUDF{bridge: internal.GoogleSQLInterval, fixedArgs: 2, anyArgs: true}},
	}
	for _, u := range udfs {
		if err := duckdb.RegisterScalarUDF(conn, u.name, u.udf); err != nil {
			return fmt.Errorf("googlesqlite: register %s: %w", u.name, err)
		}
	}
	return nil
}

// resultTypeInfo maps a DuckDBUDFResultKind to the duckdb-go TypeInfo the
// UDF declares as its result type. The kind mirrors the Go type EncodeValue
// produces for the builtin's return value.
func resultTypeInfo(kind internal.DuckDBUDFResultKind) (duckdb.TypeInfo, error) {
	switch kind {
	case internal.DuckDBResultBigint:
		return duckdb.NewTypeInfo(duckdb.TYPE_BIGINT)
	case internal.DuckDBResultDouble:
		return duckdb.NewTypeInfo(duckdb.TYPE_DOUBLE)
	case internal.DuckDBResultBoolean:
		return duckdb.NewTypeInfo(duckdb.TYPE_BOOLEAN)
	default:
		return duckdb.NewTypeInfo(duckdb.TYPE_VARCHAR)
	}
}

// registerNormalFuncUDFs installs the googlesqlite_<name> and
// googlesqlite_safe_<name> scalar UDFs for every ported builtin onto the
// DuckDB connection. Each UDF takes a variadic list of ANY-typed arguments
// (so the engine passes each operand through in its native Go form -
// envelope VARCHARs as strings, int/float operands as their numeric types)
// and delegates to internal.CallNormalFunc for the decode -> body -> encode
// round-trip.
func registerNormalFuncUDFs(conn *sql.Conn) error {
	for _, spec := range internal.DuckDBNormalUDFSpecs() {
		resultInfo, err := resultTypeInfo(spec.ResultKind)
		if err != nil {
			return fmt.Errorf("googlesqlite: result type for %s: %w", spec.Name, err)
		}
		raising := &normalFuncUDF{name: spec.Name, safe: false, resultInfo: resultInfo, volatile: spec.Volatile}
		if err := duckdb.RegisterScalarUDF(conn, "googlesqlite_"+spec.Name, raising); err != nil {
			return fmt.Errorf("googlesqlite: register googlesqlite_%s: %w", spec.Name, err)
		}
		safe := &normalFuncUDF{name: spec.Name, safe: true, resultInfo: resultInfo, volatile: spec.Volatile}
		if err := duckdb.RegisterScalarUDF(conn, "googlesqlite_safe_"+spec.Name, safe); err != nil {
			return fmt.Errorf("googlesqlite: register googlesqlite_safe_%s: %w", spec.Name, err)
		}
	}
	return nil
}

// normalFuncUDF adapts one googlesqlite_<name> builtin to DuckDB's scalar-UDF
// interface. The variadic ANY input lets a single registration serve every
// arity the builtin accepts (CONCAT, FORMAT, LPAD/RPAD with or without a pad
// string, SUBSTR with or without length, ...). SpecialNullHandling keeps NULL
// arguments flowing into the body so GoogleSQL's per-function NULL semantics
// (e.g. CONCAT returns NULL on any NULL arg, but LENGTH(NULL) is NULL while
// ARRAY_TO_STRING tolerates it) are decided by the pure-Go body, not by
// DuckDB's blanket NULL-in-NULL-out default.
type normalFuncUDF struct {
	name       string
	safe       bool
	resultInfo duckdb.TypeInfo
	volatile   bool
}

func (u *normalFuncUDF) Config() duckdb.ScalarFuncConfig {
	anyInfo, _ := duckdb.NewTypeInfo(duckdb.TYPE_ANY)
	return duckdb.ScalarFuncConfig{
		VariadicTypeInfo:    anyInfo,
		ResultTypeInfo:      u.resultInfo,
		SpecialNullHandling: true,
		Volatile:            u.volatile,
	}
}

func (u *normalFuncUDF) Executor() duckdb.ScalarFuncExecutor {
	name := u.name
	safe := u.safe
	return duckdb.ScalarFuncExecutor{
		RowExecutor: func(values []driver.Value) (any, error) {
			args := make([]any, len(values))
			for i, v := range values {
				args[i] = v
			}
			return internal.CallNormalFunc(name, safe, args...)
		},
	}
}
