package duckdbgo

import (
	convergeduckdb "github.com/esilver/duckdb-go-pure"
)

// Decimal is the compat mirror of duckdb-go's duckdb.Decimal. It is a TYPE
// ALIAS for the engine's Decimal — not a distinct type — because the engine
// driver delivers Decimal values directly on BOTH data paths (result-row scan
// in result.go and UDF argument decode in udf_vec.go), and the googlesqlite
// emulator type-switches on duckdb.Decimal (its row decoder calls String(),
// the geography/KLL argument normalizers call Float64()). With an alias the
// engine-made values match those switches with no conversion layer.
//
// Value is the unscaled integer; the represented number is Value / 10^Scale.
// Width is the total precision (digit count). String() renders the exact
// decimal text; Float64() collapses to float64 — both defined on the engine
// type (converge/duckdb/decimal.go).
type Decimal = convergeduckdb.Decimal

// JSONValue is likewise an ALIAS for the engine's JSON-cell carrier. Since
// the v0.3.3/v0.3.4 engine releases, JSON-alias RESULT columns scan as
// convergeduckdb.JSONValue (raw JSON text, type-marked) instead of a plain
// Go string; consumers above the duckdbgo façade (internal.DecodeValue and
// the value-layer foreign-decoder hook) type-switch through this alias.
type JSONValue = convergeduckdb.JSONValue
