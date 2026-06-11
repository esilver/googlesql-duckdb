package internal

import (
	"fmt"
	"math"
	"time"

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

// GoogleSQLDecodeScalar is the DuckDB-facing bridge for the
// googlesqlite_decode_scalar scalar UDF. It decodes a value-layer scalar
// envelope (the base64-of-JSON VARCHAR that googlesqlite_get_struct_field
// returns for a scalar field) back into a value.Value and re-encodes it
// to its bare over-the-wire form via EncodeValue: INT64 / FLOAT64 / BOOL
// come back as raw Go scalars, which the UDF's VARCHAR result type renders
// as the plain textual value ("3", "5.5", "true"). The formatter wraps the
// call in a DuckDB CAST to the field's resolved scalar type, so
// `x.a + x.b` binds as native arithmetic instead of failing on
// '+(VARCHAR, VARCHAR)'. A NULL envelope decodes to a NULL result.
func GoogleSQLDecodeScalar(envelope any) (any, error) {
	decoded, err := DecodeValue(envelope)
	if err != nil {
		return nil, err
	}
	if decoded == nil {
		return nil, nil
	}
	bare, err := EncodeValue(decoded)
	if err != nil {
		return nil, err
	}
	if bare == nil {
		return nil, nil
	}
	// The UDF declares a VARCHAR result, and duckdb-go does not coerce a
	// bare Go int64 / float64 / bool return into the declared string type
	// (it raises "cannot cast int64 to string"). Render the bare scalar as
	// its plain textual value so the formatter's wrapping DuckDB CAST can
	// retype it: json.Marshal yields "3" / "5.5" / "true", all of which
	// CAST cleanly to BIGINT / DOUBLE / BOOLEAN.
	if s, ok := bare.(string); ok {
		return s, nil
	}
	b, err := json.Marshal(bare)
	if err != nil {
		return nil, err
	}
	return string(b), nil
}

// GoogleSQLUnnestTemporal is the DuckDB-facing bridge for the
// googlesqlite_unnest_temporal scalar UDF, which the formatter emits when
// projecting a DATE / TIME / DATETIME / TIMESTAMP element out of the
// json_each(googlesqlite_decode_array(...)) UNNEST lowering. The element
// arrives in one of two wire forms: the value-layer base64 envelope (a
// literal array, GENERATE_DATE_ARRAY / GENERATE_TIMESTAMP_ARRAY, any
// value-layer producer) or a plain temporal text rendering (a DuckDB-native
// LIST handed to googlesqlite_decode_array — e.g. array_agg over a natively
// stored temporal column — json-marshals its time.Time elements to RFC3339).
// kind is the analyzer-resolved element kind ('date', 'time', 'datetime',
// 'timestamp'); the result is the canonical text form that DuckDB CASTs
// cleanly to the matching native temporal type, mirroring the literal
// rendering in literalFromValue.
func GoogleSQLUnnestTemporal(elem, kind any) (any, error) {
	if elem == nil {
		return nil, nil
	}
	kindName, ok := kind.(string)
	if !ok {
		return nil, fmt.Errorf("googlesqlite_unnest_temporal: kind must be a string, got %T", kind)
	}
	decoded, err := DecodeValue(elem)
	if err != nil {
		return nil, err
	}
	if decoded == nil {
		return nil, nil
	}
	var t time.Time
	switch vv := decoded.(type) {
	case value.DateValue:
		t = time.Time(vv)
	case value.TimeValue:
		t = time.Time(vv)
	case value.DatetimeValue:
		t = time.Time(vv)
	case value.TimestampValue:
		t = time.Time(vv)
	case value.StringValue:
		parsed, err := parseTemporalElementText(string(vv))
		if err != nil {
			return nil, fmt.Errorf("googlesqlite_unnest_temporal: %w", err)
		}
		t = parsed
	default:
		return nil, fmt.Errorf("googlesqlite_unnest_temporal: unexpected element value %T", decoded)
	}
	switch kindName {
	case "date":
		return t.Format("2006-01-02"), nil
	case "time":
		return t.Format("15:04:05.999999"), nil
	case "datetime":
		return t.Format("2006-01-02 15:04:05.999999"), nil
	case "timestamp":
		// GoogleSQL TIMESTAMP is an absolute instant -> DuckDB TIMESTAMPTZ.
		// Normalize to UTC and pin the +00 offset so the instant
		// round-trips regardless of DuckDB's session TimeZone.
		return t.UTC().Format("2006-01-02 15:04:05.999999+00"), nil
	}
	return nil, fmt.Errorf("googlesqlite_unnest_temporal: unknown kind %q", kindName)
}

// GoogleSQLTimestampEnvelope wraps a DuckDB-native TIMESTAMPTZ carrier in the
// value-layer TIMESTAMP envelope before handing it to polymorphic UDFs. DuckDB's
// ANY UDF carrier erases TIMESTAMP-vs-DATETIME to a bare time.Time; the
// formatter inserts this bridge only at call sites where the resolved argument
// kind is TIMESTAMP.
func GoogleSQLTimestampEnvelope(v any) (any, error) {
	if v == nil {
		return nil, nil
	}
	if t, ok := v.(time.Time); ok {
		return value.EncodeValue(value.TimestampValue(t))
	}
	decoded, err := DecodeValue(v)
	if err != nil {
		return nil, err
	}
	if decoded == nil {
		return nil, nil
	}
	t, err := decoded.ToTime()
	if err != nil {
		return nil, err
	}
	return value.EncodeValue(value.TimestampValue(t))
}

// parseTemporalElementText parses the plain-text temporal forms a
// DuckDB-native LIST element can arrive in: RFC3339 (json.Marshal of a Go
// time.Time), the DuckDB render of a TIMESTAMP/DATETIME, a bare DATE, or a
// bare TIME.
func parseTemporalElementText(s string) (time.Time, error) {
	for _, layout := range []string{
		time.RFC3339Nano,
		"2006-01-02 15:04:05.999999999Z07:00",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02",
		"15:04:05.999999999",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("cannot parse temporal element %q", s)
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
	encoded, err := EncodeValue(out)
	if err != nil {
		return nil, err
	}
	if encoded == nil {
		return nil, nil
	}
	// The UDF declares a VARCHAR result, and duckdb-go does not coerce a bare
	// Go int64 / float64 / bool return into the declared string type (it raises
	// "cannot cast float64 to string"). A CAST to a scalar numeric / boolean
	// target therefore has to come back as its plain textual value so the
	// formatter's wrapping DuckDB CAST can retype it - mirroring
	// GoogleSQLDecodeScalar. Without this, the ANON_AVG partial rewrite, which
	// emits CAST(googlesqlite_cast(x, INT64, DOUBLE, false) AS DOUBLE), fails
	// before the outer CAST can apply.
	switch ev := encoded.(type) {
	case string:
		return ev, nil
	case float64:
		// json.Marshal rejects NaN / ±Inf, but GoogleSQL CAST('NaN' AS
		// FLOAT64) is legal and a FLOAT64 cast result rides this textual
		// path (the formatter wraps the call in CAST(... AS DOUBLE)).
		// Emit the spellings DuckDB's VARCHAR->DOUBLE cast parses.
		switch {
		case math.IsNaN(ev):
			return "nan", nil
		case math.IsInf(ev, 1):
			return "inf", nil
		case math.IsInf(ev, -1):
			return "-inf", nil
		}
	}
	b, err := json.Marshal(encoded)
	if err != nil {
		return nil, err
	}
	return string(b), nil
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
