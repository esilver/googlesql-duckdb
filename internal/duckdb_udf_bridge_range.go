package internal

import (
	intervalfn "github.com/goccy/googlesqlite/internal/functions/interval"
	rangefn "github.com/goccy/googlesqlite/internal/functions/rangefn"
	"github.com/goccy/googlesqlite/internal/value"
)

// rangeScalarBridge decodes the value-layer-encoded UDF arguments DuckDB
// passes (the base64 JSON envelope the formatter splices in as VARCHAR
// literals, plus native bool / int / float for primitive literals),
// delegates to the shared pure-Go RANGE function body, and re-encodes the
// result into the same wire form the rows-layer decoder expects. This is
// the DuckDB UDF registration contract
// (ConvertArgs -> BindFunc -> EncodeValue), so the RANGE family resolves
// on the DuckDB engine.
//
// EncodeValue returns the natural Go shape for the result: a base64
// VARCHAR for RANGE / ARRAY / DATE / DATETIME / TIMESTAMP results, and a
// native Go bool for the predicate functions. Each registration declares
// the matching DuckDB result type so the returned Go value type lines up.
func rangeScalarBridge(bind func(...value.Value) (value.Value, error), args []any) (any, error) {
	values, err := value.ConvertArgs(args...)
	if err != nil {
		return nil, err
	}
	ret, err := bind(values...)
	if err != nil {
		return nil, err
	}
	return value.EncodeValue(ret)
}

// GoogleSQLRange is the DuckDB bridge for the RANGE(start, end)
// constructor.
func GoogleSQLRange(args []any) (any, error) {
	return rangeScalarBridge(rangefn.BindRange, args)
}

// GoogleSQLRangeStart is the DuckDB bridge for RANGE_START(range).
func GoogleSQLRangeStart(args []any) (any, error) {
	return rangeScalarBridge(rangefn.BindRangeStart, args)
}

// GoogleSQLRangeEnd is the DuckDB bridge for RANGE_END(range).
func GoogleSQLRangeEnd(args []any) (any, error) {
	return rangeScalarBridge(rangefn.BindRangeEnd, args)
}

// GoogleSQLRangeIsStartUnbounded is the DuckDB bridge for
// RANGE_IS_START_UNBOUNDED(range).
func GoogleSQLRangeIsStartUnbounded(args []any) (any, error) {
	return rangeScalarBridge(rangefn.BindRangeIsStartUnbounded, args)
}

// GoogleSQLRangeIsEndUnbounded is the DuckDB bridge for
// RANGE_IS_END_UNBOUNDED(range).
func GoogleSQLRangeIsEndUnbounded(args []any) (any, error) {
	return rangeScalarBridge(rangefn.BindRangeIsEndUnbounded, args)
}

// GoogleSQLRangeContains is the DuckDB bridge for
// RANGE_CONTAINS(outer, inner) and RANGE_CONTAINS(range, point).
func GoogleSQLRangeContains(args []any) (any, error) {
	return rangeScalarBridge(rangefn.BindRangeContains, args)
}

// GoogleSQLRangeOverlaps is the DuckDB bridge for
// RANGE_OVERLAPS(range, range).
func GoogleSQLRangeOverlaps(args []any) (any, error) {
	return rangeScalarBridge(rangefn.BindRangeOverlaps, args)
}

// GoogleSQLRangeIntersect is the DuckDB bridge for
// RANGE_INTERSECT(range, range).
func GoogleSQLRangeIntersect(args []any) (any, error) {
	return rangeScalarBridge(rangefn.BindRangeIntersect, args)
}

// GoogleSQLGenerateRangeArray is the DuckDB bridge for
// GENERATE_RANGE_ARRAY(range, step [, last_partial_range]).
func GoogleSQLGenerateRangeArray(args []any) (any, error) {
	return rangeScalarBridge(rangefn.BindGenerateRangeArray, args)
}

// GoogleSQLInterval is the DuckDB bridge for the INTERVAL(n, datepart)
// constructor. The GoogleSQL analyzer lowers an `INTERVAL n DATEPART`
// literal into this two-argument constructor call rather than a folded
// literal, so GENERATE_RANGE_ARRAY's step argument resolves through it.
// The constructor body lives in internal/functions/interval; this is the
// same value.Value -> wire-form contract the other bridges use.
func GoogleSQLInterval(args []any) (any, error) {
	return rangeScalarBridge(intervalfn.BindInterval, args)
}
