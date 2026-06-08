package internal

import (
	"fmt"
	"time"

	arrfn "github.com/goccy/googlesqlite/internal/functions/array"
	datefn "github.com/goccy/googlesqlite/internal/functions/date"
	jsonfn "github.com/goccy/googlesqlite/internal/functions/json"
	tsfn "github.com/goccy/googlesqlite/internal/functions/timestamp"
	"github.com/goccy/googlesqlite/internal/value"
)

// runValueLayerFunc is the DuckDB-facing bridge shared by the
// googlesqlite_<name> scalar UDFs that wrap a pure-Go BindFunction. Each
// raw argument arrives as the engine's value-layer wire form (a VARCHAR
// holding the base64 envelope, or a bare Go primitive for INT/FLOAT/BOOL
// columns). We decode every argument back into a value.Value, delegate to
// the shared body, then re-encode the result with the forced-envelope
// encoder so the single declared VARCHAR result type round-trips through
// the row decoder for any element type. A nil result becomes SQL NULL.
//
// It lives in the internal package so it can reach the unexported value
// decoder and the per-family function bodies; the duckdb-go ScalarFunc
// shells in the root package call it by name.
func runValueLayerFunc(bind func(...value.Value) (value.Value, error), args []any) (any, error) {
	vals, err := value.ConvertArgs(args...)
	if err != nil {
		return nil, err
	}
	out, err := bind(vals...)
	if err != nil {
		return nil, err
	}
	s, isNull, err := value.EncodeValueString(out)
	if err != nil {
		return nil, err
	}
	if isNull {
		return nil, nil
	}
	return s, nil
}

// CallValueLayerFunc is the DuckDB-facing bridge for a googlesqlite_<name>
// scalar UDF whose GoogleSQL result type is statically unknown (the STRUCT
// constructors and accessors: MAKE_STRUCT, GET_STRUCT_FIELD,
// STRUCT_WITH_FIELD_SET, REPLACE_FIELDS). It mirrors CallNormalFunc's
// decode -> body -> encode round-trip but forces the value-layer envelope
// on the way out via EncodeValueString, because the UDF declares a single
// VARCHAR result type and the field types vary per call. EncodeValue would
// instead hand a bare int64 / float64 / bool back for a primitive-typed
// field, which DuckDB cannot cast into the declared VARCHAR result (the
// observed "cannot cast int64 to string" on GET_STRUCT_FIELD over an
// INT64 field). The row decoder reads the envelope back and re-types the
// value against the resolved column type.
func CallValueLayerFunc(name string, args ...any) (any, error) {
	fn, ok := bindFuncForName(name)
	if !ok {
		return nil, fmt.Errorf("googlesqlite: no registered body for function %q", name)
	}
	vals, err := value.ConvertArgs(args...)
	if err != nil {
		return nil, err
	}
	out, err := fn(vals...)
	if err != nil {
		return nil, err
	}
	s, isNull, err := value.EncodeValueString(out)
	if err != nil {
		return nil, err
	}
	if isNull {
		return nil, nil
	}
	return s, nil
}

// runNativeScalarFunc is the bridge variant for value-layer functions
// whose GoogleSQL result type is a fixed scalar (INT64 / STRING / BOOL).
// It decodes the arguments, runs the body, then projects the result to
// the native Go type via conv so DuckDB stores a real BIGINT / VARCHAR /
// BOOLEAN. The envelope form (runValueLayerFunc) cannot be used for these
// because the analyzer reuses them inside SQL comparisons / aggregations
// that require a coercible native type. A nil result becomes SQL NULL.
func runNativeScalarFunc(bind func(...value.Value) (value.Value, error), args []any, conv func(value.Value) (any, error)) (any, error) {
	vals, err := value.ConvertArgs(args...)
	if err != nil {
		return nil, err
	}
	out, err := bind(vals...)
	if err != nil {
		return nil, err
	}
	if out == nil {
		return nil, nil
	}
	return conv(out)
}

// The following exported wrappers pin each array-family bridge to its
// pure-Go body. They exist so the root-package UDF shells (which cannot
// import the internal/functions/* element bodies without an import cycle
// through value) stay one-liners.

func GoogleSQLArrayConcat(args ...any) (any, error) {
	return runValueLayerFunc(arrfn.BindArrayConcat, args)
}

// GoogleSQLArrayFromNative folds a DuckDB-native LIST (the result of the
// native array_agg the ARRAY(SELECT ...) subquery lowering emits) back into
// the array-layer envelope. duckdb-go hands the LIST to the UDF as a Go
// []any whose elements are the per-row value-layer wire form (an envelope
// VARCHAR for richer element types, a native int64/float64/bool for
// primitives). Decoding each element and re-encoding the ArrayValue as one
// envelope keeps the array-subquery result a VARCHAR so it composes with the
// envelope-typed branches of the error-handling CASE expressions DuckDB
// builds for ARRAY_ZIP / ARRAY_SLICE (which otherwise reject a CASE mixing
// VARCHAR[] with VARCHAR). A nil / non-list argument (zero-row array_agg
// folds to SQL NULL) surfaces as SQL NULL, which GoogleSQL renders as the
// empty / NULL ARRAY on the result path.
func GoogleSQLArrayFromNative(args ...any) (any, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("googlesqlite_array_from_native: want 1 arg, got %d", len(args))
	}
	list, ok := args[0].([]any)
	if !ok {
		if args[0] == nil {
			return nil, nil
		}
		return nil, fmt.Errorf("googlesqlite_array_from_native: want LIST arg, got %T", args[0])
	}
	elems := make([]value.Value, 0, len(list))
	for _, e := range list {
		v, err := value.DecodeValue(e)
		if err != nil {
			return nil, err
		}
		elems = append(elems, v)
	}
	out := &value.ArrayValue{Values: elems}
	s, isNull, err := value.EncodeValueString(out)
	if err != nil {
		return nil, err
	}
	if isNull {
		return nil, nil
	}
	return s, nil
}

// GoogleSQLArrayConcatAggFlatten is the scalar half of the ARRAY_CONCAT_AGG
// lowering. The formatter collects each row's ARRAY<T> value with native
// array_agg, producing a DuckDB LIST (a Go []any) whose elements are the
// per-row array-envelope VARCHARs (or native-typed elements for primitive
// inner arrays). This bridge decodes each element to its ArrayValue and
// concatenates them into one ArrayValue, re-encoded as a single
// array-envelope. NULL per-row arrays are skipped (GoogleSQL ARRAY_CONCAT_AGG
// ignores NULL inputs); an empty / all-NULL result encodes to an empty array
// so the result path can surface the GoogleSQL NULL.
func GoogleSQLArrayConcatAggFlatten(args ...any) (any, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("googlesqlite_array_concat_agg_flatten: want 1 arg, got %d", len(args))
	}
	list, ok := args[0].([]any)
	if !ok {
		// array_agg over zero rows yields SQL NULL rather than an empty
		// list; mirror GoogleSQL's empty-input NULL result.
		if args[0] == nil {
			return nil, nil
		}
		return nil, fmt.Errorf("googlesqlite_array_concat_agg_flatten: want LIST arg, got %T", args[0])
	}
	var concatenated []value.Value
	for _, elem := range list {
		v, err := value.DecodeValue(elem)
		if err != nil {
			return nil, err
		}
		if v == nil {
			continue
		}
		arr, err := v.ToArray()
		if err != nil {
			return nil, err
		}
		concatenated = append(concatenated, arr.Values...)
	}
	out := &value.ArrayValue{Values: concatenated}
	s, isNull, err := value.EncodeValueString(out)
	if err != nil {
		return nil, err
	}
	if isNull {
		return nil, nil
	}
	return s, nil
}

// GoogleSQLArrayLength returns a native int64 rather than an envelope.
// The analyzer reuses array_length inside higher-order rewrites in a
// numeric comparison (e.g. array_length(arr) = 0 in ARRAY_FIRST), so the
// result has to coerce to a SQL integer; an envelope VARCHAR would fail
// the implicit cast.
func GoogleSQLArrayLength(args ...any) (any, error) {
	return runNativeScalarFunc(arrfn.BindArrayLength, args, func(v value.Value) (any, error) {
		return v.ToInt64()
	})
}

// GoogleSQLArrayToString returns the plain joined string rather than an
// envelope so it composes inside SQL string contexts.
func GoogleSQLArrayToString(args ...any) (any, error) {
	return runNativeScalarFunc(arrfn.BindArrayToString, args, func(v value.Value) (any, error) {
		return v.ToString()
	})
}

func GoogleSQLArrayReverse(args ...any) (any, error) {
	return runValueLayerFunc(arrfn.BindArrayReverse, args)
}

// GoogleSQLArrayIsDistinct returns a native bool.
func GoogleSQLArrayIsDistinct(args ...any) (any, error) {
	return runNativeScalarFunc(arrfn.ARRAY_IS_DISTINCT, args, func(v value.Value) (any, error) {
		return v.ToBool()
	})
}

func GoogleSQLGenerateArray(args ...any) (any, error) {
	return runValueLayerFunc(arrfn.BindGenerateArray, args)
}

func GoogleSQLGenerateDateArray(args ...any) (any, error) {
	// literalFromValue emits a DATE bound as a native DuckDB DATE literal, so
	// the UDF receives a Go time.Time for the start/end arguments rather than
	// the value-layer envelope ConvertArgs expects. Re-wrap each time.Time as
	// a DateValue (the GENERATE_DATE_ARRAY element type) so the shared body's
	// ToTime works; the step / interval arguments pass through untouched.
	return runValueLayerFunc(datefn.BindGenerateDateArray, retypeTimeArgs(args, func(t time.Time) value.Value {
		return value.DateValue(t)
	}))
}

func GoogleSQLGenerateTimestampArray(args ...any) (any, error) {
	// GENERATE_TIMESTAMP_ARRAY's bounds arrive as a native DuckDB TIMESTAMPTZ
	// literal -> Go time.Time; re-wrap as a TimestampValue (the element type)
	// before the shared body decodes them.
	return runValueLayerFunc(tsfn.BindGenerateTimestampArray, retypeTimeArgs(args, func(t time.Time) value.Value {
		return value.TimestampValue(t)
	}))
}

// retypeTimeArgs replaces every Go time.Time argument with the value.Value
// the wrap constructor produces, leaving all other argument kinds (envelope
// VARCHARs, native int steps, interval keyword strings) unchanged. The
// generate-array bridges use it because their temporal bounds reach the UDF
// as native time.Time (the formatter lowers DATE / TIMESTAMP literals to
// native typed literals), which value.ConvertArgs cannot decode on its own.
func retypeTimeArgs(args []any, wrap func(time.Time) value.Value) []any {
	out := make([]any, len(args))
	for i, a := range args {
		if t, ok := a.(time.Time); ok {
			out[i] = wrap(t)
			continue
		}
		out[i] = a
	}
	return out
}

func GoogleSQLArrayAtOffset(args ...any) (any, error) {
	return runValueLayerFunc(arrfn.BindArrayAtOffset, args)
}

func GoogleSQLSafeArrayAtOffset(args ...any) (any, error) {
	return runValueLayerFunc(arrfn.BindSafeArrayAtOffset, args)
}

func GoogleSQLArrayAtOrdinal(args ...any) (any, error) {
	return runValueLayerFunc(arrfn.BindArrayAtOrdinal, args)
}

func GoogleSQLSafeArrayAtOrdinal(args ...any) (any, error) {
	return runValueLayerFunc(arrfn.BindSafeArrayAtOrdinal, args)
}

// GoogleSQLToJsonString is the bridge for to_json_string. DuckDB has no
// to_json_string scalar, so route it to the shared pure-Go body, which
// renders the value-layer argument as a JSON string. The result is a
// plain JSON text (not an envelope), matching the STRING return type the
// analyzer assigns the call.
func GoogleSQLToJsonString(args ...any) (any, error) {
	vals, err := value.ConvertArgs(args...)
	if err != nil {
		return nil, err
	}
	out, err := jsonfn.BindToJsonString(vals...)
	if err != nil {
		return nil, err
	}
	if out == nil {
		return nil, nil
	}
	return out.ToString()
}
