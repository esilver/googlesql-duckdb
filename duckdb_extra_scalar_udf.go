package googlesqlite

import (
	"database/sql"
	"database/sql/driver"
	"fmt"

	duckdb "github.com/goccy/googlesqlite/internal/duckdbgo"

	internal "github.com/goccy/googlesqlite/internal"
)

// extraScalarUDFs lists googlesqlite_<name> scalar UDFs the formatter emits
// from the analyzer's higher-order array rewrites (ARRAY_SLICE,
// ARRAY_TRANSFORM, ARRAY_FILTER) that are not in the string family and have
// a fixed native result type. Each has a matching body in
// internal.normalFuncs reachable through internal.CallNormalFunc.
//
//   - between: ARRAY_SLICE lowers its inclusive [start, end] offset window
//     to a WHERE googlesqlite_between(idx, start, end) over the UNNEST'd
//     element index. The body compares the index against the bounds and
//     returns a BOOL, so the UDF declares a BOOLEAN result.
var extraScalarUDFs = []struct {
	name       string
	resultType duckdb.Type
}{
	{"between", duckdb.TYPE_BOOLEAN},
}

// registerExtraScalarUDFs installs the non-string-family scalar UDFs the
// array rewrites depend on. Each takes a variadic ANY argument list (the
// operands arrive as native primitives from json_each.key / json_each.value
// or as value-layer envelopes) and delegates to internal.CallNormalFunc,
// which decodes, runs the body, and encodes the result to the declared
// native type.
func registerExtraScalarUDFs(conn *sql.Conn) error {
	for _, u := range extraScalarUDFs {
		resultInfo, err := duckdb.NewTypeInfo(u.resultType)
		if err != nil {
			return fmt.Errorf("googlesqlite: result type for %s: %w", u.name, err)
		}
		udf := &extraScalarUDF{name: u.name, resultInfo: resultInfo}
		if err := duckdb.RegisterScalarUDF(conn, "googlesqlite_"+u.name, udf); err != nil {
			return fmt.Errorf("googlesqlite: register googlesqlite_%s: %w", u.name, err)
		}
	}
	return nil
}

type extraScalarUDF struct {
	name       string
	resultInfo duckdb.TypeInfo
}

func (u *extraScalarUDF) Config() duckdb.ScalarFuncConfig {
	anyInfo, _ := duckdb.NewTypeInfo(duckdb.TYPE_ANY)
	return duckdb.ScalarFuncConfig{
		VariadicTypeInfo:    anyInfo,
		ResultTypeInfo:      u.resultInfo,
		SpecialNullHandling: true,
	}
}

func (u *extraScalarUDF) Executor() duckdb.ScalarFuncExecutor {
	name := u.name
	return duckdb.ScalarFuncExecutor{
		RowExecutor: func(values []driver.Value) (any, error) {
			args := make([]any, len(values))
			for i, v := range values {
				args[i] = v
			}
			return internal.CallNormalFunc(name, false, args...)
		},
	}
}
