package internal

import (
	"math/big"

	duckdb "github.com/duckdb/duckdb-go/v2"

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
	return value.DecodeValue(v)
}
