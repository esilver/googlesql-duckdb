package internal

import (
	"math/big"

	"cloud.google.com/go/bigquery"
	duckdb "github.com/goccy/googlesqlite/internal/duckdbgo"

	"github.com/goccy/googlesqlite/internal/value"
)

// DecodeValue is a thin shim that forwards to the canonical
// implementation in internal/value. The decode logic lives in the
// leaf value package so per-category function sub-packages and the
// window infrastructure can use it without going through internal.
//
// The one DuckDB-specific case handled here is duckdb.Decimal: a DECIMAL
// column (and DECIMAL-typed literals like 1.7, plus the results of
// TRUNC/ROUND/FLOOR/CEIL/ABS/PI over them) comes back from the duckdb-go
// driver as a duckdb.Decimal, which the dialect-agnostic value decoder
// does not know. We convert it to a NumericValue carrying the exact
// rational from the decimal's string form so both float-typed
// (ToFloat64) and NUMERIC-typed (ToString) result columns decode
// losslessly. The duckdb import stays in this internal-layer shim,
// keeping the leaf value package free of the DuckDB dependency.
func DecodeValue(v any) (value.Value, error) {
	if dec, ok := v.(duckdb.Decimal); ok {
		r := new(big.Rat)
		if _, ok := r.SetString(dec.String()); ok {
			return &value.NumericValue{Rat: r}, nil
		}
	}
	if iv, ok := v.(duckdb.Interval); ok {
		return intervalValueFromDuckDB(iv), nil
	}
	// duckdb-go-pure >= v0.3.3 scans JSON-alias RESULT cells as
	// duckdb.JSONValue (the raw JSON text, type-marked) instead of the
	// parsed native value the v0.3.2-era driver delivered. Parse first
	// (duckdb-go semantics), then decode the native form — an envelope
	// string inside JSON quotes must arrive unquoted for the base64
	// path, and JSON null becomes Go nil.
	if jv, ok := v.(duckdb.JSONValue); ok {
		return value.DecodeValue(duckdb.DecodeJSONNative(string(jv)))
	}
	return value.DecodeValue(v)
}

// init installs the duckdb.Decimal case as a foreign-decoder hook on the
// leaf value package, so the SAME conversion runs everywhere
// value.DecodeValue is reached without going through this shim: scalar /
// aggregate UDF arguments (value.ConvertArgs — a native DECIMAL(38,9)
// NUMERIC column delivers duckdb.Decimal carriers since the
// native-storage migration), LIST elements ([]any recursion, e.g.
// array_agg over a stored NUMERIC column) and STRUCT field values. The
// explicit case in DecodeValue above stays as the fast path for the
// result-row scan.
func init() {
	value.RegisterForeignDecoder(func(v any) (value.Value, bool) {
		dec, ok := v.(duckdb.Decimal)
		if !ok {
			return nil, false
		}
		r := new(big.Rat)
		if _, ok := r.SetString(dec.String()); !ok {
			return nil, false
		}
		return &value.NumericValue{Rat: r}, true
	})
	value.RegisterForeignDecoder(func(v any) (value.Value, bool) {
		iv, ok := v.(duckdb.Interval)
		if !ok {
			return nil, false
		}
		return intervalValueFromDuckDB(iv), true
	})
	// Same hook for duckdb.JSONValue, so JSON-alias cells reached through
	// the recursion paths (LIST elements, STRUCT fields, UDF arguments
	// that skip the duckdbgo jsonAwareExecutor normalization) parse to
	// their native form instead of failing the string type-assert. A
	// JSON null yields (nil, true): DecodeValue then reports the cell
	// as SQL NULL, matching the v0.3.2-era driver's nil delivery.
	value.RegisterForeignDecoder(func(v any) (value.Value, bool) {
		jv, ok := v.(duckdb.JSONValue)
		if !ok {
			return nil, false
		}
		dv, err := value.DecodeValue(duckdb.DecodeJSONNative(string(jv)))
		if err != nil {
			return nil, false
		}
		return dv, true
	})
}

func intervalValueFromDuckDB(iv duckdb.Interval) value.Value {
	const (
		microsPerSecond = int64(1000000)
		microsPerMinute = 60 * microsPerSecond
		microsPerHour   = 60 * microsPerMinute
	)
	months := iv.Months
	years := months / 12
	months = months % 12
	micros := iv.Micros
	hours := micros / microsPerHour
	micros %= microsPerHour
	minutes := micros / microsPerMinute
	micros %= microsPerMinute
	seconds := micros / microsPerSecond
	micros %= microsPerSecond
	return &value.IntervalValue{IntervalValue: &bigquery.IntervalValue{
		Years:          years,
		Months:         months,
		Days:           iv.Days,
		Hours:          int32(hours),
		Minutes:        int32(minutes),
		Seconds:        int32(seconds),
		SubSecondNanos: int32(micros * 1000),
	}}
}
