package googlesqlite

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"

	duckdb "github.com/duckdb/duckdb-go/v2"

	"github.com/goccy/googlesqlite/internal/functions/kll"
	"github.com/goccy/googlesqlite/internal/value"
)

// kllExtractFn is a KLL scalar body that takes already-decoded value.Values and
// returns a value.Value, matching kll.BindExtract* / kll.BindExtractPoint*.
type kllExtractFn func(args ...value.Value) (value.Value, error)

// kllExtractSpec pairs a KLL EXTRACT body with the DuckDB result type to
// declare. EXTRACT_* returns an ARRAY (EncodeValue renders it as the base64
// VARCHAR envelope), while EXTRACT_POINT_* returns a scalar quantile that
// EncodeValue hands back as a bare INT64 / FLOAT64, so those declare the
// matching native result type.
type kllExtractSpec struct {
	fn         kllExtractFn
	resultType duckdb.Type
}

// kllExtractScalars maps the bare lowered name the formatter emits for the KLL
// EXTRACT family to its body and result type. These are scalar (not aggregate)
// functions: EXTRACT(_POINT)_* consume a finalized sketch (the value-layer
// BYTES envelope produced by the INIT / MERGE_PARTIAL aggregate) plus a count
// or rank argument. They are absent from the DuckDB normal-UDF surface
// (duckDBNormalUDFNames), and the formatter emits the bare leaf name
// (extract_int64, not the googlesqlite_-prefixed flattened form), so they need
// a standalone registration under exactly that name.
var kllExtractScalars = map[string]kllExtractSpec{
	"extract_int64":         {kll.BindExtractInt64, duckdb.TYPE_VARCHAR},
	"extract_float64":       {kll.BindExtractFloat64, duckdb.TYPE_VARCHAR},
	"extract_point_int64":   {kll.BindExtractPointInt64, duckdb.TYPE_BIGINT},
	"extract_point_float64": {kll.BindExtractPointFloat64, duckdb.TYPE_DOUBLE},
}

// registerKLLExtractScalars installs the KLL EXTRACT scalar UDFs on a DuckDB
// database handle. Mirrors registerDuckDBAggregates: one connection, catalog-
// scoped registration visible to every connection of the same database.
func registerKLLExtractScalars(db *sql.DB) error {
	conn, err := db.Conn(context.Background())
	if err != nil {
		return fmt.Errorf("googlesqlite: kll extract conn: %w", err)
	}
	defer conn.Close()
	for name, spec := range kllExtractScalars {
		udf := &kllExtractUDF{fn: spec.fn, resultType: spec.resultType}
		if err := duckdb.RegisterScalarUDF(conn, name, udf); err != nil {
			return fmt.Errorf("googlesqlite: register %s: %w", name, err)
		}
	}
	return nil
}

// kllExtractUDF adapts a KLL EXTRACT body to DuckDB's scalar-UDF interface. The
// arguments are variadic TYPE_ANY so the single registration accepts the
// sketch envelope (a VARCHAR) plus the count / rank argument in whichever
// native type the binder picks (INT64 for the count, DECIMAL / DOUBLE for the
// rank); the value decoder normalises each. The result is the value-layer
// envelope VARCHAR: EXTRACT returns an ARRAY and EXTRACT_POINT a scalar, and
// the spec wraps EXTRACT in TO_JSON_STRING / compares EXTRACT_POINT through the
// value-layer column decode, both of which read the envelope back.
type kllExtractUDF struct {
	fn         kllExtractFn
	resultType duckdb.Type
}

func (u *kllExtractUDF) Config() duckdb.ScalarFuncConfig {
	anyInfo, _ := duckdb.NewTypeInfo(duckdb.TYPE_ANY)
	result, _ := duckdb.NewTypeInfo(u.resultType)
	return duckdb.ScalarFuncConfig{
		VariadicTypeInfo: anyInfo,
		ResultTypeInfo:   result,
		// EXTRACT over a NULL / empty sketch yields NULL, so let the body
		// decide rather than short-circuiting on a NULL argument.
		SpecialNullHandling: true,
	}
}

func (u *kllExtractUDF) Executor() duckdb.ScalarFuncExecutor {
	fn := u.fn
	return duckdb.ScalarFuncExecutor{
		RowExecutor: func(values []driver.Value) (any, error) {
			args := make([]any, len(values))
			for i, v := range values {
				args[i] = normalizeExtractArg(v)
			}
			decoded, err := value.ConvertArgs(args...)
			if err != nil {
				return nil, err
			}
			ret, err := fn(decoded...)
			if err != nil {
				return nil, err
			}
			return value.EncodeValue(ret)
		},
	}
}

// normalizeExtractArg coerces a DuckDB-native DECIMAL rank argument (e.g. the
// 0.5 in EXTRACT_POINT_*(sketch, 0.5)) to float64 so the value decoder reads it
// as a FloatValue. Other argument types pass through unchanged.
func normalizeExtractArg(v driver.Value) any {
	if d, ok := v.(duckdb.Decimal); ok {
		return d.Float64()
	}
	return v
}
