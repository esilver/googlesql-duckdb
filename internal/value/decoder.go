package value

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/goccy/go-json"
)

// jsonUnmarshalPreserveNumbers decodes JSON into v with json.Number so
// integer literals don't collapse to float64 when landing in an
// interface{} slot. The post-pass rewrites json.Number nodes to int64
// or float64 based on the source shape (presence of '.' or 'e').
func jsonUnmarshalPreserveNumbers(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(v); err != nil {
		return err
	}
	return nil
}

// coerceJSONNumber converts a json.Number into int64 (no decimal /
// exponent) or float64 (otherwise). Non-json.Number values pass
// through untouched.
func coerceJSONNumber(v any) any {
	n, ok := v.(json.Number)
	if !ok {
		return v
	}
	s := string(n)
	if !strings.ContainsAny(s, ".eE") {
		if i, err := strconv.ParseInt(s, 10, 64); err == nil {
			return i
		}
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return f
	}
	return v
}

// ConvertArgs decodes a sequence of SQLite-side Go values (the form
// the ncruces driver hands us in Step / scalar callbacks) into a slice
// of googlesqlite Values. It is a thin loop around DecodeValue.
func ConvertArgs(args ...any) ([]Value, error) {
	values := make([]Value, 0, len(args))
	for _, arg := range args {
		v, err := DecodeValue(arg)
		if err != nil {
			return nil, err
		}
		values = append(values, v)
	}
	return values, nil
}

// DecodeValue is the inverse of EncodeValue: given the over-the-wire
// form (Go primitive or base64-encoded ValueLayout JSON) produce the
// matching googlesqlite Value.
func DecodeValue(v any) (Value, error) {
	if isNullValue(v) {
		return nil, nil
	}
	// Pass an already-decoded Value straight through. The generate-array
	// bridges pre-wrap a native time.Time bound as a DateValue /
	// TimestampValue (ConvertArgs cannot decode a bare time.Time), so the
	// decoder must treat an incoming Value as a no-op rather than reject it.
	if vv, ok := v.(Value); ok {
		return vv, nil
	}
	v = coerceJSONNumber(v)
	switch vv := v.(type) {
	case int64:
		return IntValue(vv), nil
	// DuckDB returns narrower integer widths natively (e.g. INT -> int32,
	// SELECT 1 -> int32, SMALLINT -> int16), whereas SQLite always handed
	// back int64. Widen them so scalar columns and integer literals decode.
	case int32:
		return IntValue(int64(vv)), nil
	case int16:
		return IntValue(int64(vv)), nil
	case int8:
		return IntValue(int64(vv)), nil
	case int:
		return IntValue(int64(vv)), nil
	case uint64:
		return IntValue(int64(vv)), nil
	case uint32:
		return IntValue(int64(vv)), nil
	case uint16:
		return IntValue(int64(vv)), nil
	case uint8:
		return IntValue(int64(vv)), nil
	case float64:
		return FloatValue(vv), nil
	case float32:
		return FloatValue(float64(vv)), nil
	case bool:
		return BoolValue(vv), nil
	case []byte:
		// DuckDB hands a BLOB column / BLOB-returning UDF result back as a
		// Go []byte. The raw-BYTES geography serializers (ST_ASBINARY -> WKB
		// declared TYPE_BLOB) flow their result into a value-layer UDF
		// wrapper (TO_HEX / BYTE_LENGTH); without this case the wrapper's
		// ConvertArgs rejects the []byte. Wrap it as the BYTES value the
		// body expects.
		//
		// Stored BYTES columns are the exception (issue #9): the column is
		// a native BLOB, but the INSERT path persists the value-layer
		// base64 envelope as the blob's ASCII content, so a stored cell
		// arrives here as []byte("eyJoZWFkZXIi..."). Without unwrapping,
		// engine-side consumers (googlesqlite_cast for CAST(b AS STRING),
		// TO_HEX, BYTE_LENGTH, ...) operate on the envelope text instead
		// of the payload bytes. Sniff the canonical envelope exactly the
		// way the string path below does; genuine binary blobs (WKB, proto
		// wire bytes) fail the base64/JSON/header probe and pass through
		// untouched.
		if dv, ok, err := decodeEnvelopeString(string(vv)); ok {
			return dv, err
		}
		return BytesValue(vv), nil
	case *big.Int:
		// DuckDB returns HUGEINT (128-bit) results as *big.Int, which the
		// SQLite backend never produced. COUNT / COUNTIF / SUM(INTEGER)
		// promote to HUGEINT in DuckDB even when the true result fits in
		// 64 bits, so map a representable value to INT64 and fall back to
		// FLOAT64 only when it genuinely overflows int64.
		if vv == nil {
			return nil, nil
		}
		if vv.IsInt64() {
			return IntValue(vv.Int64()), nil
		}
		f, _ := new(big.Float).SetInt(vv).Float64()
		return FloatValue(f), nil
	case big.Int:
		bi := vv
		if bi.IsInt64() {
			return IntValue(bi.Int64()), nil
		}
		f, _ := new(big.Float).SetInt(&bi).Float64()
		return FloatValue(f), nil
	case []any:
		// DuckDB-native LIST: duckdb-go scans a LIST / ARRAY column to a Go
		// []any. Each element is either a value-layer envelope (a richer
		// element type re-encoded as VARCHAR by a UDF) or a native Go
		// primitive (INT64 / FLOAT64 / BOOL element), so recurse through
		// DecodeValue per element to materialise the ArrayValue. This lets a
		// native aggregate (array_agg / list / the ARRAY(SELECT ...)
		// subquery lowering) round-trip back through ToArray on the result
		// path without the base64 envelope.
		elems := make([]Value, 0, len(vv))
		for _, e := range vv {
			ev, err := DecodeValue(e)
			if err != nil {
				return nil, err
			}
			elems = append(elems, ev)
		}
		return &ArrayValue{Values: elems}, nil
	case map[string]any:
		// DuckDB-native STRUCT: duckdb-go scans a STRUCT column to a Go
		// map[string]any keyed by field name. The struct value layer carries
		// field names on the column type rather than the value, so we keep
		// the keys for round-tripping but the row decoder reads fields
		// positionally; preserve a stable key order so the positional read
		// matches the declared field order is the caller's concern. Recurse
		// per field value (envelope or native primitive).
		keys := make([]string, 0, len(vv))
		for k := range vv {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		values := make([]Value, 0, len(keys))
		m := make(map[string]Value, len(keys))
		for _, k := range keys {
			fv, err := DecodeValue(vv[k])
			if err != nil {
				return nil, err
			}
			values = append(values, fv)
			m[k] = fv
		}
		return &StructValue{Keys: keys, Values: values, M: m}, nil
	}
	s, ok := v.(string)
	if !ok {
		return nil, fmt.Errorf("unexpected value type: %T", v)
	}
	// Try the canonical base64-of-JSON envelope first. If either step
	// fails the input is most likely a raw SQL string literal (e.g.
	// `'int32'`, `'date'`, `'bytes'`) that the formatter inlined
	// without the envelope. Fall back to a plain StringValue so the
	// receiver can still call .ToString() on it.
	if dv, ok, err := decodeEnvelopeString(s); ok {
		return dv, err
	}
	return StringValue(s), nil
}

// decodeEnvelopeString probes s for the canonical value-layer carrier
// (base64 of a {"header","body"} ValueLayout JSON). ok reports whether s
// is such an envelope; when it is, the decoded Value (or the layout's
// decode error) is returned. Callers fall back to their raw reading
// (StringValue / BytesValue) when ok is false.
func decodeEnvelopeString(s string) (Value, bool, error) {
	decoded, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, false, nil
	}
	var layout ValueLayout
	if err := json.Unmarshal(decoded, &layout); err != nil {
		return nil, false, nil
	}
	if layout.Header == "" {
		return nil, false, nil
	}
	v, err := decodeFromValueLayout(&layout)
	return v, true, err
}

func decodeFromValueLayout(layout *ValueLayout) (Value, error) {
	switch layout.Header {
	case StringValueType:
		return StringValue(layout.Body), nil
	case IntValueType:
		i, err := strconv.ParseInt(layout.Body, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("failed to decode int body %q: %w", layout.Body, err)
		}
		return IntValue(i), nil
	case FloatValueType:
		f, err := strconv.ParseFloat(layout.Body, 64)
		if err != nil {
			return nil, fmt.Errorf("failed to decode float body %q: %w", layout.Body, err)
		}
		return FloatValue(f), nil
	case BoolValueType:
		return BoolValue(layout.Body == "true"), nil
	case BytesValueType:
		decoded, err := base64.StdEncoding.DecodeString(layout.Body)
		if err != nil {
			return nil, err
		}
		return BytesValue(decoded), nil
	case NumericValueType:
		r := new(big.Rat)
		r.SetString(layout.Body)
		return &NumericValue{Rat: r}, nil
	case BigNumericValueType:
		r := new(big.Rat)
		r.SetString(layout.Body)
		return &NumericValue{Rat: r, IsBigNumeric: true}, nil
	case DateValueType:
		t, err := parseDate(layout.Body)
		if err != nil {
			return nil, err
		}
		return DateValue(t), nil
	case DatetimeValueType:
		t, err := parseDatetime(layout.Body)
		if err != nil {
			return nil, err
		}
		return DatetimeValue(t), nil
	case TimeValueType:
		t, err := parseTime(layout.Body)
		if err != nil {
			return nil, err
		}
		return TimeValue(t), nil
	case TimestampValueType:
		microsec, err := strconv.ParseInt(layout.Body, 10, 64)
		microSecondsInSecond := int64(time.Second) / int64(time.Microsecond)
		sec := microsec / microSecondsInSecond
		remainder := microsec - (sec * microSecondsInSecond)
		if err != nil {
			return nil, fmt.Errorf("failed to parse unixmicro for timestamp value %s: %w", layout.Body, err)
		}
		return TimestampValue(time.Unix(sec, remainder*int64(time.Microsecond))), nil
	case IntervalValueType:
		return parseInterval(layout.Body)
	case JsonValueType:
		return JsonValue(layout.Body), nil
	case ArrayValueType:
		var arr []any
		if err := jsonUnmarshalPreserveNumbers([]byte(layout.Body), &arr); err != nil {
			return nil, fmt.Errorf("failed to decode array body: %w", err)
		}
		ret := &ArrayValue{
			Values: make([]Value, 0, len(arr)),
		}
		for _, elem := range arr {
			val, err := DecodeValue(elem)
			if err != nil {
				return nil, err
			}
			ret.Values = append(ret.Values, val)
		}
		return ret, nil
	case StructValueType:
		var structLayout StructValueLayout
		if err := jsonUnmarshalPreserveNumbers([]byte(layout.Body), &structLayout); err != nil {
			return nil, err
		}
		m := map[string]Value{}
		values := make([]Value, 0, len(structLayout.Values))
		for i, data := range structLayout.Values {
			val, err := DecodeValue(data)
			if err != nil {
				return nil, err
			}
			m[structLayout.Keys[i]] = val
			values = append(values, val)
		}
		ret := &StructValue{}
		ret.Keys = structLayout.Keys
		ret.Values = values
		ret.M = m
		return ret, nil
	case GeographyValueType:
		body := layout.Body
		inverted := false
		if strings.HasPrefix(body, "INVERTED ") {
			inverted = true
			body = body[len("INVERTED "):]
		}
		ret, err := GeographyFromWKT(body)
		if err != nil {
			return nil, fmt.Errorf("decodeFromValueLayout failed: %w", err)
		}
		if inverted {
			ret.MarkInverted()
		}
		return ret, nil
	case RangeValueType:
		var rl struct {
			Elem  string `json:"elem"`
			Start any    `json:"start"`
			End   any    `json:"end"`
		}
		if err := jsonUnmarshalPreserveNumbers([]byte(layout.Body), &rl); err != nil {
			return nil, fmt.Errorf("failed to decode range body: %w", err)
		}
		ret := &RangeValue{ElemHeader: ValueType(rl.Elem)}
		if rl.Start != nil {
			s, err := DecodeValue(rl.Start)
			if err != nil {
				return nil, err
			}
			ret.Start = s
		}
		if rl.End != nil {
			e, err := DecodeValue(rl.End)
			if err != nil {
				return nil, err
			}
			ret.End = e
		}
		return ret, nil
	}
	return nil, fmt.Errorf("unexpected value header: %s", layout.Header)
}
