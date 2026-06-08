package googlesqlite

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"

	duckdb "github.com/duckdb/duckdb-go/v2"

	internal "github.com/goccy/googlesqlite/internal"
)

// registerDuckDBUDFs installs the minimal set of googlesqlite_* scalar
// UDFs that the Milestone-0 boot/critical path needs onto a DuckDB
// database handle. Today that is only googlesqlite_in_array, which the
// formatter emits when lowering `IN UNNEST(@param)` (used by every
// metadata FindProject/FindDataset/... query, so it is on the boot and
// per-request path). The full ~348-function surface (function_register_*)
// stays on the SQLite-coupled sqlitex adapter and is the Milestone-1
// retarget; this file only proves the registration + value round-trip
// mechanism works against duckdb-go.
//
// Registration happens once per inner *sql.DB. DuckDB registers scalar
// functions at catalog scope, so a single registration is visible to all
// connections of the same database (the inner pool is pinned to one
// connection regardless).
func registerDuckDBUDFs(db *sql.DB) error {
	conn, err := db.Conn(context.Background())
	if err != nil {
		return fmt.Errorf("googlesqlite: duckdb udf conn: %w", err)
	}
	defer conn.Close()
	if err := duckdb.RegisterScalarUDF(conn, "googlesqlite_in_array", &inArrayUDF{}); err != nil {
		return fmt.Errorf("googlesqlite: register googlesqlite_in_array: %w", err)
	}
	return nil
}

// inArrayUDF adapts the engine's pure-Go internal.GoogleSQLInArray body
// to DuckDB's scalar-UDF interface. Both arguments arrive as the engine's
// value-layer wire form (a VARCHAR holding base64-encoded JSON for the
// non-primitive ARRAY argument, and the element in the same encoding),
// because the formatter routes both the column value and the bound param
// through the identical encode path. We decode each argument back into a
// value.Value, delegate to the shared membership body, and hand DuckDB a
// native Go bool.
type inArrayUDF struct{}

func (*inArrayUDF) Config() duckdb.ScalarFuncConfig {
	varchar, _ := duckdb.NewTypeInfo(duckdb.TYPE_VARCHAR)
	boolean, _ := duckdb.NewTypeInfo(duckdb.TYPE_BOOLEAN)
	return duckdb.ScalarFuncConfig{
		InputTypeInfos: []duckdb.TypeInfo{varchar, varchar},
		ResultTypeInfo: boolean,
		// The element argument can legitimately be NULL while the array is
		// not; defer NULL semantics to the membership body rather than
		// short-circuiting to NULL on any NULL input.
		SpecialNullHandling: true,
	}
}

func (*inArrayUDF) Executor() duckdb.ScalarFuncExecutor {
	return duckdb.ScalarFuncExecutor{
		RowExecutor: func(values []driver.Value) (any, error) {
			if len(values) != 2 {
				return nil, fmt.Errorf("googlesqlite_in_array: want 2 args, got %d", len(values))
			}
			return internal.GoogleSQLInArray(values[0], values[1])
		},
	}
}
