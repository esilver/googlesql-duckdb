package internal

import (
	arrfn "github.com/goccy/googlesqlite/internal/functions/array"
	"github.com/goccy/googlesqlite/internal/value"
)

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
