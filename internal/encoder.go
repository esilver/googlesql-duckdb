package internal

import (
	"database/sql"
	"database/sql/driver"
	"encoding/base64"
	"fmt"
	"math/big"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	googlesql "github.com/goccy/go-googlesql"
	"github.com/goccy/go-json"

	jsonfn "github.com/goccy/googlesqlite/internal/functions/json"
	"github.com/goccy/googlesqlite/internal/value"
)

func encodeNamedValues(v []driver.NamedValue, params []*googlesql.ResolvedParameter) ([]sql.NamedArg, error) {
	if len(v) != len(params) {
		return nil, fmt.Errorf(
			"failed to match named values num (%d) and params num (%d)",
			len(v), len(params),
		)
	}
	ret := make([]sql.NamedArg, 0, len(v))
	for idx, vv := range v {
		converted, err := encodeNamedValue(vv, params[idx])
		if err != nil {
			return nil, fmt.Errorf("failed to convert value from %+v: %w", vv, err)
		}
		ret = append(ret, converted)
	}
	return ret, nil
}

func EncodeGoValues(v []any, params []*googlesql.ResolvedParameter) ([]any, error) {
	if len(v) != len(params) {
		return nil, fmt.Errorf(
			"failed to match args values num (%d) and params num (%d)",
			len(v), len(params),
		)
	}
	ret := make([]any, 0, len(v))
	for idx, vv := range v {
		value, err := encodeValueAgainstParamType(vv, m1(params[idx].Type()))
		if err != nil {
			return nil, err
		}
		ret = append(ret, value)
	}
	return ret, nil
}

func encodeGoValue(t googlesql.Googlesql_TypeNode, v any) (any, error) {
	value, err := ValueFromGoValue(v)
	if err != nil {
		return nil, err
	}
	casted, err := CastValue(t, value)
	if err != nil {
		return nil, err
	}
	return EncodeValue(casted)
}

// EncodeValue is a thin shim that forwards to the canonical
// implementation in internal/value. Kept un-prefixed so the existing
// per-spec callers in internal/ still compile.
func EncodeValue(v value.Value) (any, error) {
	return value.EncodeValue(v)
}

func literalFromValue(v value.Value) (string, error) {
	if v == nil {
		return "null", nil
	}
	switch vv := v.(type) {
	case value.IntValue:
		i64, err := v.ToInt64()
		if err != nil {
			return "", err
		}
		return fmt.Sprint(i64), nil
	case value.FloatValue:
		f64, err := v.ToFloat64()
		if err != nil {
			return "", err
		}
		value := strconv.FormatFloat(f64, 'g', -1, 64)
		if !strings.Contains(value, ".") && !strings.Contains(value, "e") {
			// append x.0 suffix to keep float value context
			value = fmt.Sprintf("%s.0", value)
		}
		return value, nil
	case value.BoolValue:
		b, err := v.ToBool()
		if err != nil {
			return "", err
		}
		return fmt.Sprint(b), nil
	case *value.SafeValue:
		return literalFromValue(vv.Value)
	case value.DateValue:
		// DuckDB retarget: emit a native typed literal so DuckDB binds
		// the operand as DATE. GoogleSQL DATE is a civil calendar date,
		// matching DuckDB DATE exactly.
		return fmt.Sprintf("DATE '%s'", time.Time(vv).Format("2006-01-02")), nil
	case value.TimeValue:
		// GoogleSQL TIME is a civil time-of-day -> DuckDB TIME. Carry
		// microsecond precision (GoogleSQL's resolution).
		return fmt.Sprintf("TIME '%s'", time.Time(vv).Format("15:04:05.999999")), nil
	case value.DatetimeValue:
		// GoogleSQL DATETIME is a civil date+time with no zone -> DuckDB
		// TIMESTAMP (zoneless). The base64 envelope would otherwise reach
		// DuckDB as VARCHAR and miss every DATETIME_* overload.
		return fmt.Sprintf("TIMESTAMP '%s'", time.Time(vv).Format("2006-01-02 15:04:05.999999")), nil
	case value.TimestampValue:
		// GoogleSQL TIMESTAMP is an absolute instant -> DuckDB
		// TIMESTAMPTZ. Normalize to UTC and pin the +00 offset so the
		// instant round-trips regardless of DuckDB's session TimeZone.
		return fmt.Sprintf("TIMESTAMPTZ '%s+00'", time.Time(vv).UTC().Format("2006-01-02 15:04:05.999999")), nil
	case *value.NumericValue:
		// Native-storage migration: NUMERIC literals are typed native
		// DECIMAL(38,9) literals, matching the stored column type, so
		// operators / reducers / INSERTs bind the value directly.
		// BIGNUMERIC stays on the value-layer envelope below (it
		// exceeds DuckDB's 38-digit DECIMAL).
		if !vv.IsBigNumeric {
			s, err := vv.ToString()
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("CAST('%s' AS DECIMAL(38,9))", s), nil
		}
	case value.BytesValue:
		// Native-storage migration: BYTES literals are DuckDB blob
		// literals carrying the payload bytes ('\xNN...'::BLOB),
		// matching the payload-byte BLOB cells of stored columns.
		var sb strings.Builder
		for _, b := range []byte(vv) {
			fmt.Fprintf(&sb, "\\x%02X", b)
		}
		return fmt.Sprintf("'%s'::BLOB", sb.String()), nil
	}
	layout, err := value.ValueLayoutFromValue(v)
	if err != nil {
		return "", err
	}
	b, err := json.Marshal(layout)
	if err != nil {
		return "", fmt.Errorf("failed to encode value: %w", err)
	}
	// DuckDB retarget: the value-layer envelope (base64 of the JSON
	// layout) must be spliced into SQL as a single-quoted string
	// literal. The predecessor used Go's %q, which double-quotes; DuckDB
	// reads a double-quoted token as an identifier and raises a Binder
	// Error ('Referenced column "<base64>" not found'). Emit a SQL
	// string literal with embedded single quotes doubled instead.
	encoded := base64.StdEncoding.EncodeToString(b)
	return "'" + strings.ReplaceAll(encoded, "'", "''") + "'", nil
}

func literalFromGoogleSQLValue(v googlesql.Value) (string, error) {
	value, err := valueFromGoogleSQLValue(v)
	if err != nil {
		return "", err
	}
	return literalFromValue(value)
}

// envelopeLiteralFromValue renders v as the value-layer base64 envelope
// SQL literal unconditionally, bypassing literalFromValue's native-typed
// shortcuts for temporal kinds. The RANGE family needs its DATE /
// DATETIME / TIMESTAMP bounds to arrive at the googlesqlite_range UDF in
// the envelope form (which carries the element-type header the
// constructor reads), not as a native DuckDB DATE / TIMESTAMP literal
// that the VARCHAR-typed UDF cannot bind and that erases the DATE vs
// midnight-TIMESTAMP distinction once duckdb-go maps both to a bare Go
// time.Time.
func envelopeLiteralFromValue(v value.Value) (string, error) {
	if v == nil {
		return "null", nil
	}
	layout, err := value.ValueLayoutFromValue(v)
	if err != nil {
		return "", err
	}
	b, err := json.Marshal(layout)
	if err != nil {
		return "", fmt.Errorf("failed to encode value: %w", err)
	}
	encoded := base64.StdEncoding.EncodeToString(b)
	return "'" + strings.ReplaceAll(encoded, "'", "''") + "'", nil
}

func valueFromGoogleSQLValue(v googlesql.Value) (value.Value, error) {
	if m1(v.IsNull()) {
		return nil, nil
	}
	switch m1(v.TypeKind()) {
	case googlesql.TypeKindTypeInt32, googlesql.TypeKindTypeInt64, googlesql.TypeKindTypeUint32, googlesql.TypeKindTypeUint64:
		return intValueFromLiteral(m1(v.GetSQLLiteral()))
	case googlesql.TypeKindTypeBool:
		return boolValueFromLiteral(m1(v.GetSQLLiteral()))
	case googlesql.TypeKindTypeFloat, googlesql.TypeKindTypeDouble:
		return floatValueFromLiteral(m1(v.GetSQLLiteral()))
	case googlesql.TypeKindTypeString:
		return value.StringValue(m1(v.StringValue())), nil
	case googlesql.TypeKindTypeEnum:
		return stringValueFromLiteral(m1(v.GetSQLLiteral()))
	case googlesql.TypeKindTypeBytes:
		return bytesValueFromLiteral(m1(v.GetSQLLiteral())), nil
	case googlesql.TypeKindTypeDate:
		return dateValueFromLiteral(m1(v.ToInt64())), nil
	case googlesql.TypeKindTypeDatetime:
		return datetimeValueFromLiteral(m1(v.ToPacked64DatetimeMicros())), nil
	case googlesql.TypeKindTypeTime:
		return timeValueFromLiteral(m1(v.ToPacked64TimeMicros())), nil
	case googlesql.TypeKindTypeTimestamp:
		microsec, _ := v.ToUnixMicros()
		microSecondsInSecond := int64(time.Second) / int64(time.Microsecond)
		sec := microsec / microSecondsInSecond
		remainder := microsec - (sec * microSecondsInSecond)
		return timestampValueFromLiteral(time.Unix(sec, remainder*int64(time.Microsecond)))
	case googlesql.TypeKindTypeNumeric, googlesql.TypeKindTypeBignumeric:
		return numericValueFromLiteral(m1(v.GetSQLLiteral()))
	case googlesql.TypeKindTypeInterval:
		return intervalValueFromLiteral(m1(v.GetSQLLiteral()))
	case googlesql.TypeKindTypeJson:
		return jsonValueFromLiteral(m1(v.JsonString()))
	case googlesql.TypeKindTypeArray:
		return arrayValueFromLiteral(v)
	case googlesql.TypeKindTypeStruct:
		return structValueFromLiteral(v)
	case googlesql.TypeKindTypeRange:
		return rangeValueFromLiteral(v)
	case googlesql.TypeKindTypeUuid:
		return uuidValueFromLiteral(v)
	}
	return nil, fmt.Errorf("unsupported literal type: %v", m1(v.TypeKind()))
}

// uuidValueFromLiteral converts a UUID-typed googlesql.Value into a
// StringValue holding the canonical 8-4-4-4-12 hex form. The runtime
// stores UUIDs as STRING-shaped scalars; the analyzer-side type is
// UUID via the V14_UUID_TYPE language feature.
func uuidValueFromLiteral(v googlesql.Value) (value.Value, error) {
	uv, err := v.UuidValue()
	if err != nil {
		return nil, fmt.Errorf("uuid value: %w", err)
	}
	if uv == nil {
		return nil, nil
	}
	s, err := uv.AppendToString()
	if err != nil {
		return nil, fmt.Errorf("uuid string: %w", err)
	}
	return value.StringValue(s), nil
}

// rangeValueFromLiteral converts a RANGE-typed googlesql.Value into the
// runtime's *value.RangeValue. RANGE is a half-open interval over a
// comparable element type (DATE / DATETIME / TIMESTAMP); either bound
// can be unbounded, surfaced through googlesql.Value as a null-valued
// Start/End sub-Value. We recurse into ValueFromGoogleSQLValue for the
// bound values so they inherit the same conversion rules as standalone
// literals of the element type.
func rangeValueFromLiteral(v googlesql.Value) (value.Value, error) {
	startV, err := v.Start()
	if err != nil {
		return nil, fmt.Errorf("range Start: %w", err)
	}
	endV, err := v.End()
	if err != nil {
		return nil, fmt.Errorf("range End: %w", err)
	}
	rv := &value.RangeValue{}
	if startV != nil && !m1(startV.IsNull()) {
		start, err := valueFromGoogleSQLValue(*startV)
		if err != nil {
			return nil, fmt.Errorf("range start element: %w", err)
		}
		rv.Start = start
	}
	if endV != nil && !m1(endV.IsNull()) {
		end, err := valueFromGoogleSQLValue(*endV)
		if err != nil {
			return nil, fmt.Errorf("range end element: %w", err)
		}
		rv.End = end
	}
	return rv, nil
}

func intValueFromLiteral(lit string) (value.IntValue, error) {
	v, err := strconv.ParseInt(lit, 10, 64)
	if err != nil {
		return 0, err
	}
	return value.IntValue(v), nil
}

func boolValueFromLiteral(lit string) (value.BoolValue, error) {
	v, err := strconv.ParseBool(lit)
	if err != nil {
		return false, err
	}
	return value.BoolValue(v), nil
}

func floatValueFromLiteral(lit string) (value.FloatValue, error) {
	v, err := strconv.ParseFloat(lit, 64)
	if err != nil {
		return 0, err
	}
	return value.FloatValue(v), nil
}

func stringValueFromLiteral(lit string) (value.StringValue, error) {
	v, err := strconv.Unquote(lit)
	if err != nil {
		return "", fmt.Errorf("failed to unquote from string literal: %w", err)
	}
	return value.StringValue(v), nil
}

func bytesValueFromLiteral(lit string) value.BytesValue {
	// use a workaround because ToBytes doesn't work with certain values.
	unquoted, err := strconv.Unquote(lit[1:])
	if err != nil {
		return value.BytesValue(lit)
	}
	return value.BytesValue(unquoted)
}

func dateValueFromLiteral(days int64) value.DateValue {
	// time.Unix returns a local-zone time.Time; a DATE is a civil value,
	// so it must stay at UTC midnight. Formatting a local-zone value in
	// any zone west of UTC rendered the previous day (DATE "2024-03-22"
	// stored and displayed as "2024-03-21" in America/New_York).
	t := time.Unix(int64(time.Duration(days)*24*(time.Hour/time.Second)), 0).UTC()
	return value.DateValue(t)
}

const (
	secShift     = 0
	minShift     = 6
	hourShift    = 12
	dayShift     = 17
	monthShift   = 22
	yearShift    = 26
	microSecMask = 0xFFFFF
	secMask      = 0b111111
	minMask      = 0b111111 << minShift
	hourMask     = 0b11111 << hourShift
	dayMask      = 0b11111 << dayShift
	monthMask    = 0b1111 << monthShift
	yearMask     = 0x3FFF << yearShift
)

func datetimeValueFromLiteral(bit int64) value.DatetimeValue {
	b := bit >> 20
	year := (b & yearMask) >> yearShift
	month := (b & monthMask) >> monthShift
	day := (b & dayMask) >> dayShift
	hour := (b & hourMask) >> hourShift
	min := (b & minMask) >> minShift
	sec := (b & secMask) >> secShift
	microSec := (bit & microSecMask) >> 0
	t := time.Date(
		int(year),
		time.Month(month),
		int(day),
		int(hour),
		int(min),
		int(sec),
		int(microSec)*1000, time.UTC,
	)
	return value.DatetimeValue(t)
}

func timeValueFromLiteral(bit int64) value.TimeValue {
	b := bit >> 20
	hour := (b & hourMask) >> hourShift
	min := (b & minMask) >> minShift
	sec := (b & secMask) >> secShift
	microSec := (bit & microSecMask) >> 0
	t := time.Date(0, 0, 0, int(hour), int(min), int(sec), int(microSec)*1000, time.UTC)
	return value.TimeValue(t)
}

func timestampValueFromLiteral(t time.Time) (value.TimestampValue, error) {
	return value.TimestampValue(t), nil
}

var (
	numericLiteralPattern = regexp.MustCompile(`NUMERIC "(.+)"`)
)

func numericValueFromLiteral(lit string) (*value.NumericValue, error) {
	matches := numericLiteralPattern.FindAllStringSubmatch(lit, -1)
	if len(matches) == 0 {
		return nil, fmt.Errorf("unexpected numeric literal: %s", lit)
	}
	if len(matches[0]) != 2 {
		return nil, fmt.Errorf("unexpected numeric literal: %s", lit)
	}
	numericLit := matches[0][1]
	r := new(big.Rat)
	r.SetString(numericLit)
	if strings.Contains(lit, "BIGNUMERIC") {
		return &value.NumericValue{Rat: r, IsBigNumeric: true}, nil
	}
	return &value.NumericValue{Rat: r}, nil
}

func jsonValueFromLiteral(lit string) (value.JsonValue, error) {
	return value.JsonValue(lit), nil
}

var (
	intervalLiteralPattern = regexp.MustCompile(`INTERVAL "(.+)"`)
)

func intervalValueFromLiteral(lit string) (*value.IntervalValue, error) {
	matches := intervalLiteralPattern.FindAllStringSubmatch(lit, -1)
	if len(matches) == 0 {
		return nil, fmt.Errorf("unexpected interval literal: %s", lit)
	}
	if len(matches[0]) != 2 {
		return nil, fmt.Errorf("unexpected interval literal: %s", lit)
	}
	intervalLit := matches[0][1]
	return value.ParseInterval(intervalLit)
}

func arrayValueFromLiteral(v googlesql.Value) (*value.ArrayValue, error) {
	ret := &value.ArrayValue{}
	n, _ := v.NumElements()
	for i := range n {
		elem, _ := v.Element(i)
		value, err := valueFromGoogleSQLValue(*elem)
		if err != nil {
			return nil, fmt.Errorf("failed to convert from googlesql value: %w", err)
		}
		ret.Values = append(ret.Values, value)
	}
	return ret, nil
}

func structValueFromLiteral(v googlesql.Value) (*value.StructValue, error) {
	ret := &value.StructValue{
		M: map[string]value.Value{},
	}
	var fieldNames []string
	if t, err := v.Type(); err == nil && t != nil {
		if st, err := t.AsStruct(); err == nil && st != nil {
			if fields, err := st.Fields(); err == nil {
				for _, f := range fields {
					fieldNames = append(fieldNames, f.Name)
				}
			}
		}
	}
	n, _ := v.NumFields()
	for i := range n {
		field, _ := v.Field(i)
		var name string
		if int(i) < len(fieldNames) && fieldNames[int(i)] != "" {
			name = fieldNames[int(i)]
		} else {
			name = fmt.Sprintf("_field_%d", i)
		}
		val, err := valueFromGoogleSQLValue(*field)
		if err != nil {
			return nil, err
		}
		ret.Keys = append(ret.Keys, name)
		ret.Values = append(ret.Values, val)
		ret.M[name] = val
	}
	return ret, nil
}

// hasDuplicateFieldNames reports whether two or more declared
// target-struct fields share the same non-empty name.
func hasDuplicateFieldNames(fields []*googlesql.StructField) bool {
	seen := make(map[string]struct{}, len(fields))
	for _, f := range fields {
		if f == nil || f.Name == "" {
			continue
		}
		if _, ok := seen[f.Name]; ok {
			return true
		}
		seen[f.Name] = struct{}{}
	}
	return false
}

// hasDuplicateKeys reports whether two or more source-struct keys
// share the same non-empty name.
func hasDuplicateKeys(keys []string) bool {
	seen := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		if k == "" {
			continue
		}
		if _, ok := seen[k]; ok {
			return true
		}
		seen[k] = struct{}{}
	}
	return false
}

// structHasMatchingNames reports whether at least one of the
// target's named fields exists in the source struct's map. False
// means every target field is absent (typically the source has
// auto-generated names like `_field_0`), in which case CastValue
// must fall back to positional zip.
func structHasMatchingNames(s *value.StructValue, fields []*googlesql.StructField) bool {
	if s == nil || len(s.M) == 0 {
		return false
	}
	for _, f := range fields {
		if _, ok := s.M[f.Name]; ok {
			return true
		}
	}
	return false
}

func CastValue(t googlesql.Googlesql_TypeNode, v value.Value) (value.Value, error) {
	if v == nil {
		return nil, nil
	}
	// `t` is nil when the row column's static type was not reported
	// (sub-query unwrap that keeps only the projected expression's
	// runtime value). Without a target type CastValue has nothing to
	// coerce against, so pass the decoded value straight through.
	if t == nil {
		return v, nil
	}
	// Googlesql_TypeNode carries Kind directly, no upcast needed.
	return castValueWithKind(m1(t.Kind()), t, v)
}

// castValueWithKind is CastValue with the type's Kind already resolved.
// Type.Kind() is a wasm round-trip into the GoogleSQL analyzer module, so
// per-exec hot paths (prepared-statement arg encoding) resolve the kind
// once at prepare time and call this variant; CastValue keeps the
// resolve-on-every-call behavior for the remaining callers.
func castValueWithKind(kind googlesql.TypeKind, t googlesql.Googlesql_TypeNode, v value.Value) (value.Value, error) {
	if v == nil {
		return nil, nil
	}
	if t == nil {
		return v, nil
	}
	switch kind {
	case googlesql.TypeKindTypeInt32, googlesql.TypeKindTypeInt64, googlesql.TypeKindTypeUint32, googlesql.TypeKindTypeUint64:
		i64, err := v.ToInt64()
		if err != nil {
			return nil, err
		}
		return value.IntValue(i64), nil
	case googlesql.TypeKindTypeBool:
		b, err := v.ToBool()
		if err != nil {
			return nil, err
		}
		return value.BoolValue(b), nil
	case googlesql.TypeKindTypeFloat, googlesql.TypeKindTypeDouble:
		f64, err := v.ToFloat64()
		if err != nil {
			return nil, err
		}
		return value.FloatValue(f64), nil
	case googlesql.TypeKindTypeString, googlesql.TypeKindTypeEnum:
		// BYTES -> STRING follows BigQuery CAST semantics: the payload
		// must be valid UTF-8 and decodes to that text (issue #9). The
		// generic ToString() on BytesValue is the base64 rendering (the
		// BigQuery JSON-API wire form used when a BYTES value itself is
		// displayed), which is NOT what CAST returns.
		if kind == googlesql.TypeKindTypeString {
			if bv, ok := v.(value.BytesValue); ok {
				if !utf8.Valid([]byte(bv)) {
					return nil, fmt.Errorf("CAST(BYTES AS STRING): invalid UTF-8 payload")
				}
				return value.StringValue(string(bv)), nil
			}
		}
		s, err := v.ToString()
		if err != nil {
			return nil, err
		}
		return value.StringValue(s), nil
	case googlesql.TypeKindTypeBytes:
		b, err := v.ToBytes()
		if err != nil {
			return nil, err
		}
		return value.BytesValue(b), nil
	case googlesql.TypeKindTypeDate:
		t, err := v.ToTime()
		if err != nil {
			return nil, err
		}
		return value.DateValue(t), nil
	case googlesql.TypeKindTypeDatetime:
		t, err := v.ToTime()
		if err != nil {
			return nil, err
		}
		return value.DatetimeValue(t), nil
	case googlesql.TypeKindTypeTime:
		t, err := v.ToTime()
		if err != nil {
			return nil, err
		}
		return value.TimeValue(t), nil
	case googlesql.TypeKindTypeTimestamp:
		t, err := v.ToTime()
		if err != nil {
			return nil, err
		}
		return value.TimestampValue(t), nil
	case googlesql.TypeKindTypeInterval:
		s, err := v.ToString()
		if err != nil {
			return nil, err
		}
		return value.ParseInterval(s)
	case googlesql.TypeKindTypeArray:
		array, err := v.ToArray()
		if err != nil {
			return nil, err
		}
		// ArrayType.ElementType() isn't exposed on the wasm bridge
		// yet; fall back to leaving the inner element untyped, which
		// preserves the value shape for the simple cast-through cases
		// tests exercise today.
		ret := &value.ArrayValue{}
		ret.Values = append(ret.Values, array.Values...)
		return ret, nil
	case googlesql.TypeKindTypeStruct:
		if array, ok := v.(*value.ArrayValue); ok {
			ret := &value.StructValue{M: map[string]value.Value{}}
			for _, value := range array.Values {
				st, err := value.ToStruct()
				if err != nil {
					return nil, err
				}
				ret.Keys = append(ret.Keys, st.Keys...)
				ret.Values = append(ret.Values, st.Values...)
				for i, k := range st.Keys {
					ret.M[k] = st.Values[i]
				}
			}
			return ret, nil
		}
		s, err := v.ToStruct()
		if err != nil {
			return nil, err
		}
		// Reshape the source struct to match the target struct type's
		// declared fields: preserve declaration order, fill absent
		// fields with nil, and recursively cast field values by type.
		// Required for Go-callers that build structs from sparse maps
		// (e.g. map[string]interface{}{"fieldB": ...} targeting a
		// STRUCT<fieldA, fieldB>). Without this the downstream SQL
		// STRUCT_FIELD(struct, index) call panics with
		// "index out of range" because only the supplied subset of
		// fields made it through.
		//
		// Anonymous-field structs (auto-generated names like
		// `_field_0`) cast to a named struct must zip positionally
		// — looking the target's named fields up in the source's
		// auto-generated map would miss every one (root cause of
		// anonymous-struct-field bug, runtime side).
		if st, ok := t.(*googlesql.StructType); ok && st != nil {
			fields, err := st.Fields()
			if err == nil && len(fields) > 0 {
				// Three reasons to zip positionally:
				//   - source has no field-name overlap with target (the
				//     usePositional path that already lived here);
				//   - the target type declares duplicate field names —
				//     looking up by name through the source's deduped
				//     `M` map collapses every duplicate-named field to
				//     the same value;
				//   - the source itself has duplicate keys for the same
				//     reason.
				dupTargetNames := hasDuplicateFieldNames(fields)
				dupSourceKeys := hasDuplicateKeys(s.Keys)
				usePositional := !structHasMatchingNames(s, fields) || dupTargetNames || dupSourceKeys
				reshaped := &value.StructValue{M: map[string]value.Value{}}
				for i, f := range fields {
					key := f.Name
					var existing value.Value
					switch {
					case usePositional:
						if i < len(s.Values) {
							existing = s.Values[i]
						}
					case key == "":
						// Mixed name + anonymous targets (e.g. result of
						// REGEXP_EXTRACT_GROUPS with `(?<key>...):(...)`):
						// the declared struct keeps anonymous fields as
						// empty strings, but the source struct names them
						// positionally (`$col2`, ...). Fall back to the
						// positional value for unnamed targets.
						if i < len(s.Values) {
							existing = s.Values[i]
						}
					default:
						if v, found := s.M[key]; found {
							existing = v
						}
					}
					var fieldValue value.Value
					if existing != nil {
						if f.Type_ != nil {
							casted, err := CastValue(f.Type_, existing)
							if err != nil {
								return nil, err
							}
							fieldValue = casted
						} else {
							fieldValue = existing
						}
					}
					reshaped.Keys = append(reshaped.Keys, key)
					reshaped.Values = append(reshaped.Values, fieldValue)
					reshaped.M[key] = fieldValue
				}
				return reshaped, nil
			}
		}
		return s, nil
	case googlesql.TypeKindTypeNumeric:
		r, err := v.ToRat()
		if err != nil {
			return nil, err
		}
		return &value.NumericValue{Rat: r}, nil
	case googlesql.TypeKindTypeBignumeric:
		r, err := v.ToRat()
		if err != nil {
			return nil, err
		}
		return &value.NumericValue{Rat: r, IsBigNumeric: true}, nil
	case googlesql.TypeKindTypeJson:
		// Use the shared JSON-literal renderer so temporal values
		// (including ones nested in arrays/structs) become quoted JSON
		// strings; bare ToJSON leaves them as invalid tokens like
		// 2024-02-29. Non-temporal values keep their ToJSON behavior.
		j, err := jsonfn.ToJSONLiteral(v)
		if err != nil {
			return nil, err
		}
		return value.JsonValue(j), nil
	case googlesql.TypeKindTypeGeography:
		return v, nil
	case googlesql.TypeKindTypeRange:
		return v, nil
	case googlesql.TypeKindTypeProto:
		// CAST(STRING AS Proto) feeds the literal as a proto-text
		// format payload: parse it against the registered Go-side
		// MessageType and emit the binary wire bytes. Any other source
		// (BytesValue already containing wire bytes, an existing
		// wire-bytes carrier) passes through unchanged.
		if sv, ok := v.(value.StringValue); ok {
			name := protoNameFromGoogleSQLType(t)
			if name != "" {
				if bytes, err := parseProtoTextLiteral(name, string(sv)); err == nil {
					return value.BytesValue(bytes), nil
				} else {
					return nil, fmt.Errorf("CAST to PROTO<%s>: %w", name, err)
				}
			}
		}
		return v, nil
	}
	return nil, fmt.Errorf("unsupported cast %v value", m1(t.Kind()))
}

// ValueFromGoValue is a thin shim that forwards to the canonical
// implementation in internal/value.
func ValueFromGoValue(v any) (value.Value, error) {
	return value.ValueFromGoValue(v)
}

// EncodeGoValueForDriver converts complex Go values (maps, nested slices,
// user structs) into the googlesqlite value-layout base64 string so the
// underlying sqlite3 driver accepts them as a plain string. Primitive
// values (int64, float64, bool, string, []byte, time.Time) and nil pass
// through untouched because the sqlite3 driver already understands them.
// Returning the original value unchanged for unsupported shapes would
// let sqlite3 report the "unsupported type" error itself, which is
// better than us guessing.
func encodeGoValueForDriver(v any) (any, error) {
	if v == nil {
		return nil, nil
	}
	switch v.(type) {
	case int64, float64, bool, string, []byte, time.Time:
		return v, nil
	}
	rv := reflect.ValueOf(v)
	k := rv.Kind()
	// Primitive kinds pass through; only encode maps / non-byte slices /
	// user structs which sqlite3 would otherwise reject.
	switch k {
	case reflect.Invalid, reflect.Bool,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64, reflect.String:
		return v, nil
	case reflect.Slice, reflect.Array:
		if rv.Type().Elem().Kind() == reflect.Uint8 {
			return v, nil
		}
	}
	val, err := ValueFromGoValue(v)
	if err != nil {
		return nil, err
	}
	encoded, err := EncodeValue(val)
	if err != nil {
		return nil, err
	}
	return encoded, nil
}

func encodeNamedValue(v driver.NamedValue, param *googlesql.ResolvedParameter) (sql.NamedArg, error) {
	value, err := encodeValueAgainstParamType(v.Value, m1(param.Type()))
	if err != nil {
		return sql.NamedArg{}, err
	}
	return sql.NamedArg{
		Name:  strings.ToLower(v.Name),
		Value: value,
	}, nil
}

// encodeValueAgainstParamType produces the over-the-wire form for v
// under the analyzer-declared param type. The value arriving here may
// already be a base64-encoded layout (because Conn.CheckNamedValue
// encodes by Go type before we know the declared param type), so first
// try to decode and re-encode against the declared type. If decoding
// fails, treat v as a raw Go value and run the standard cast path.
//
// DuckDB retarget: the bcf0852 storage seam made DATE / TIME / DATETIME
// / TIMESTAMP / JSON columns native-typed, and the expression lowering
// emits native temporal literals (DATE '...', TIMESTAMPTZ '...') for
// inline values. A bound parameter must arrive in the same native form
// — the legacy base64 envelope fails DuckDB's VARCHAR->DATE/JSON cast
// at bind time ("invalid date field format: eyJoZWFkZXIi...").
// encodeParamValue mirrors literalFromValue's native shortcuts for
// exactly those kinds (plus NUMERIC / BYTES since the native-storage
// migration); everything else (STRING, BIGNUMERIC, ARRAY, STRUCT, ...)
// keeps the envelope the TEXT-backed columns and googlesqlite_* UDFs
// store and decode.
func encodeValueAgainstParamType(v any, t googlesql.Googlesql_TypeNode) (any, error) {
	if v == nil {
		return nil, nil
	}
	if s, ok := v.(string); ok {
		if decoded, err := DecodeValue(s); err == nil && decoded != nil {
			cast, err := CastValue(t, decoded)
			if err == nil && cast != nil {
				return encodeParamValue(cast)
			}
		}
	}
	val, err := ValueFromGoValue(v)
	if err != nil {
		return nil, err
	}
	casted, err := CastValue(t, val)
	if err != nil {
		return nil, err
	}
	return encodeParamValue(casted)
}

// encodeParamValue renders a declared-type-cast parameter value in the
// form the DuckDB storage layer expects: temporal kinds as the plain
// text DuckDB casts into its native DATE / TIME / TIMESTAMP /
// TIMESTAMPTZ columns (the same calendar/instant readings
// literalFromValue pins for inline literals), JSON as the raw JSON
// text a native JSON column accepts, NUMERIC as plain decimal text
// (cast into the native DECIMAL(38,9)), BYTES as the raw payload
// []byte (a native BLOB), and every other kind through EncodeValue
// (bare Go scalars for INT64 / FLOAT64 / BOOL, the base64 value-layer
// envelope for STRING / BIGNUMERIC / ARRAY / STRUCT / ...).
func encodeParamValue(v value.Value) (any, error) {
	switch vv := v.(type) {
	case *value.SafeValue:
		return encodeParamValue(vv.Value)
	case value.DateValue:
		return time.Time(vv).Format("2006-01-02"), nil
	case value.TimeValue:
		return time.Time(vv).Format("15:04:05.999999"), nil
	case value.DatetimeValue:
		return time.Time(vv).Format("2006-01-02 15:04:05.999999"), nil
	case value.TimestampValue:
		// Normalize to UTC and pin the +00 offset so the instant
		// round-trips regardless of DuckDB's session TimeZone.
		return time.Time(vv).UTC().Format("2006-01-02 15:04:05.999999+00"), nil
	case value.JsonValue:
		return string(vv), nil
	case *value.NumericValue:
		// Native-storage migration: a NUMERIC parameter binds as plain
		// decimal text, which DuckDB casts into the native
		// DECIMAL(38,9) storage / operand type (the same value reading
		// literalFromValue pins for inline literals). BIGNUMERIC keeps
		// the envelope (TEXT storage).
		if !vv.IsBigNumeric {
			return vv.ToString()
		}
	case value.BytesValue:
		// Native-storage migration: a BYTES parameter binds as the raw
		// payload []byte, which the driver delivers as a native BLOB —
		// matching the payload-byte cells of stored BYTES columns.
		return []byte(vv), nil
	}
	return EncodeValue(v)
}

// encodeValueAgainstParamTypeWithKind is encodeValueAgainstParamType with
// the param type's Kind pre-resolved (see castValueWithKind). It also
// short-circuits the scalar identity cases — an int64/float64/bool Go
// value bound to a matching scalar param kind round-trips unchanged
// through ValueFromGoValue → CastValue → EncodeValue, so the conversion
// is skipped entirely. hasKind is false when the param's Type() was
// unavailable at prepare time; the un-typed path then matches the
// original behavior (CastValue passthrough on nil type).
func encodeValueAgainstParamTypeWithKind(v any, t googlesql.Googlesql_TypeNode, kind googlesql.TypeKind, hasKind bool) (any, error) {
	if v == nil {
		return nil, nil
	}
	if !hasKind || t == nil {
		return encodeValueAgainstParamType(v, nil)
	}
	switch kind {
	case googlesql.TypeKindTypeInt32, googlesql.TypeKindTypeInt64, googlesql.TypeKindTypeUint32, googlesql.TypeKindTypeUint64:
		if iv, ok := v.(int64); ok {
			return iv, nil
		}
	case googlesql.TypeKindTypeFloat, googlesql.TypeKindTypeDouble:
		if fv, ok := v.(float64); ok {
			return fv, nil
		}
	case googlesql.TypeKindTypeBool:
		if bv, ok := v.(bool); ok {
			return bv, nil
		}
	}
	if s, ok := v.(string); ok {
		if decoded, err := DecodeValue(s); err == nil && decoded != nil {
			cast, err := castValueWithKind(kind, t, decoded)
			if err == nil && cast != nil {
				return encodeParamValue(cast)
			}
		}
	}
	val, err := ValueFromGoValue(v)
	if err != nil {
		return nil, err
	}
	casted, err := castValueWithKind(kind, t, val)
	if err != nil {
		return nil, err
	}
	return encodeParamValue(casted)
}
