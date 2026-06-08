package internal

import (
	"fmt"

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
	return runValueLayerFunc(datefn.BindGenerateDateArray, args)
}

func GoogleSQLGenerateTimestampArray(args ...any) (any, error) {
	return runValueLayerFunc(tsfn.BindGenerateTimestampArray, args)
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

// GoogleSQLDecodeArray is the bridge for googlesqlite_decode_array. The
// analyzer emits json_each(googlesqlite_decode_array(<array>)) when it
// lowers higher-order array operators (ARRAY_INCLUDES / ARRAY_SUM /
// UNNEST(...) over a literal, etc.). DuckDB has a native json_each table
// function, so the bridge only has to turn the value-layer envelope into
// the JSON array text json_each parses. ArrayValue.ToString already emits
// canonical JSON (numbers bare, strings quoted, nested values recursed),
// which is exactly what json_each consumes. A NULL or non-array input
// yields SQL NULL so the surrounding json_each produces zero rows.
func GoogleSQLDecodeArray(args ...any) (any, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("googlesqlite_decode_array: want 1 arg, got %d", len(args))
	}
	v, err := value.DecodeValue(args[0])
	if err != nil {
		return nil, err
	}
	if v == nil {
		return nil, nil
	}
	arr, err := v.ToArray()
	if err != nil {
		return nil, err
	}
	return arr.ToString()
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
