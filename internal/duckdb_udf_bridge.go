package internal

import (
	"github.com/goccy/go-json"

	arrfn "github.com/goccy/googlesqlite/internal/functions/array"
	mathfn "github.com/goccy/googlesqlite/internal/functions/math"
	"github.com/goccy/googlesqlite/internal/value"
)

// GoogleSQLDecodeArray is the DuckDB-facing bridge for the
// googlesqlite_decode_array scalar UDF, which the formatter emits when
// lowering UNNEST(<array>) -> json_each(googlesqlite_decode_array(<array>)).
//
// The argument arrives as the engine's value-layer wire form for an ARRAY
// (a VARCHAR holding base64-encoded JSON under the {header:"array",body}
// envelope). We decode it back into a value.Value, re-encode every element
// to its over-the-wire primitive form (INT64/FLOAT64/BOOL pass through as
// raw JSON scalars; richer element types become base64 strings), and
// json.Marshal the resulting slice. DuckDB's json_each table function then
// iterates that JSON array, exposing json_each.value (the element) and
// json_each.key (its 0-based offset) - identical to the SQLite path the
// body was written for in function_register.go.
func GoogleSQLDecodeArray(arr any) (string, error) {
	// DuckDB-native LIST input: native aggregates (array_agg, list, the
	// ARRAY(SELECT ...) subquery lowering) hand back a Go []any whose
	// elements are already the per-element over-the-wire form (an envelope
	// VARCHAR for richer element types, a native int64/float64/bool for
	// primitives). json_each only needs each element re-marshalled as a
	// JSON scalar, so forward the slice straight to json.Marshal without
	// the value-layer envelope decode the VARCHAR path requires.
	if list, ok := arr.([]any); ok {
		b, err := json.Marshal(list)
		if err != nil {
			return "", err
		}
		return string(b), nil
	}
	decoded, err := DecodeValue(arr)
	if err != nil {
		return "", err
	}
	if decoded == nil {
		return "[]", nil
	}
	array, err := decoded.ToArray()
	if err != nil {
		return "", err
	}
	encodedValues := make([]any, 0, len(array.Values))
	for _, v := range array.Values {
		ev, err := EncodeValue(v)
		if err != nil {
			return "", err
		}
		encodedValues = append(encodedValues, ev)
	}
	b, err := json.Marshal(encodedValues)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// GoogleSQLCast is the DuckDB-facing bridge for the googlesqlite_cast scalar
// UDF, which the formatter (CastNode) emits for every GoogleSQL CAST /
// SAFE_CAST. The four arguments are the value-layer-encoded expression, the
// JSON-encoded source Type (a value-layer STRING), the JSON-encoded target
// Type (a value-layer STRING), and a native bool selecting SAFE semantics
// (NULL-on-error). We decode each into a value.Value, run the shared pure-Go
// bindCast body, and re-encode the result to the over-the-wire form DuckDB
// stores. A NULL result encodes back to a NULL DuckDB value.
func GoogleSQLCast(expr, fromType, toType, isSafe any) (any, error) {
	args := make([]value.Value, 4)
	for i, raw := range []any{expr, fromType, toType, isSafe} {
		v, err := DecodeValue(raw)
		if err != nil {
			return nil, err
		}
		args[i] = v
	}
	out, err := bindCast(args...)
	if err != nil {
		return nil, err
	}
	return EncodeValue(out)
}

// GoogleSQLInArray is the DuckDB-facing bridge for the googlesqlite_in_array
// scalar UDF (the lowering of `IN UNNEST(...)`). It decodes the two
// value-layer-encoded arguments DuckDB passes (element, array) back into
// value.Value, delegates to the shared pure-Go membership body, and
// returns a native Go bool DuckDB can store directly.
//
// It lives in the internal package so it can reach the unexported value
// decoder and the functions/array body; the duckdb-go ScalarFunc shell in
// the root package calls it.
func GoogleSQLInArray(elem, array any) (bool, error) {
	elemV, err := value.DecodeValue(elem)
	if err != nil {
		return false, err
	}
	arrayV, err := value.DecodeValue(array)
	if err != nil {
		return false, err
	}
	out, err := arrfn.BindInArray(elemV, arrayV)
	if err != nil {
		return false, err
	}
	if out == nil {
		return false, nil
	}
	return out.ToBool()
}

// GoogleSQLRangeBucket is the DuckDB-facing bridge for
// googlesqlite_range_bucket. The point arrives as a value-layer scalar
// and the boundaries as the value-layer ARRAY envelope; we decode both
// and delegate to the shared RANGE_BUCKET body, returning the bucket
// index as a native int64 (or nil for a NULL point, matching the body).
func GoogleSQLRangeBucket(point, array any) (any, error) {
	pointV, err := value.DecodeValue(point)
	if err != nil {
		return nil, err
	}
	arrayV, err := value.DecodeValue(array)
	if err != nil {
		return nil, err
	}
	out, err := arrfn.BindRangeBucket(pointV, arrayV)
	if err != nil {
		return nil, err
	}
	if out == nil {
		return nil, nil
	}
	i, err := out.ToInt64()
	if err != nil {
		return nil, err
	}
	return i, nil
}

// GoogleSQLEuclideanDistance and GoogleSQLCosineDistance are the
// DuckDB-facing bridges for the vector-distance UDFs. Both ARRAY<FLOAT64>
// arguments arrive as value-layer envelopes; we decode each, delegate to
// the shared body, and return a native float64 (nil propagates NULL).
func GoogleSQLEuclideanDistance(a, b any) (any, error) {
	return vectorDistanceBridge(mathfn.EUCLIDEAN_DISTANCE, a, b)
}

func GoogleSQLCosineDistance(a, b any) (any, error) {
	return vectorDistanceBridge(mathfn.COSINE_DISTANCE, a, b)
}

func vectorDistanceBridge(body func(...value.Value) (value.Value, error), a, b any) (any, error) {
	av, err := value.DecodeValue(a)
	if err != nil {
		return nil, err
	}
	bv, err := value.DecodeValue(b)
	if err != nil {
		return nil, err
	}
	out, err := body(av, bv)
	if err != nil {
		return nil, err
	}
	if out == nil {
		return nil, nil
	}
	f, err := out.ToFloat64()
	if err != nil {
		return nil, err
	}
	return f, nil
}
