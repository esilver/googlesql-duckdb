package googlesqlite

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"

	duckdb "github.com/goccy/googlesqlite/internal/duckdbgo"

	internal "github.com/goccy/googlesqlite/internal"
)

// registerDuckDBJSONUDFs installs the BigQuery JSON-function surface
// (TO_JSON_STRING, JSON_QUERY, JSON_VALUE, JSON_EXTRACT*, JSON_SET,
// JSON_REMOVE, JSON_STRIP_NULLS, the typed JSON accessors, ...) as
// googlesqlite_<name> scalar UDFs on a DuckDB handle. Each UDF decodes
// its value-layer-encoded arguments, delegates to the shared pure-Go
// body, and returns the canonical base64-envelope VARCHAR that the row
// decoder re-types against the statement's output column.
//
// DuckDB ships native json_* functions, but the engine's value layer
// hands every argument across as a base64-encoded ValueLayout envelope
// (so a column and a bound parameter take the identical wire form), and
// BigQuery's JSON semantics (path syntax, NULL handling, JSON_TYPE
// vocabulary) differ from DuckDB's. Routing through the existing,
// spec-conformant Go bodies preserves both.
func registerDuckDBJSONUDFs(db *sql.DB) error {
	conn, err := db.Conn(context.Background())
	if err != nil {
		return fmt.Errorf("googlesqlite: duckdb json udf conn: %w", err)
	}
	defer conn.Close()

	for _, u := range internal.JSONFamilyUDFs() {
		if err := duckdb.RegisterScalarUDF(conn, u.Name, newJSONScalarUDF(u.Eval)); err != nil {
			return fmt.Errorf("googlesqlite: register %s: %w", u.Name, err)
		}
	}
	if err := duckdb.RegisterScalarUDF(conn, "googlesqlite_to_native_json", &toNativeJSONUDF{}); err != nil {
		return fmt.Errorf("googlesqlite: register googlesqlite_to_native_json: %w", err)
	}
	if err := duckdb.RegisterScalarUDF(conn, "googlesqlite_plain_string", &plainStringUDF{}); err != nil {
		return fmt.Errorf("googlesqlite: register googlesqlite_plain_string: %w", err)
	}
	return nil
}

// plainStringUDF adapts internal.GoogleSQLPlainString to DuckDB's
// scalar-UDF interface: VARCHAR -> VARCHAR, unwrapping a value-layer
// STRING envelope to its bare text (pass-through for anything else).
// The formatter wraps the operand of native STRING -> temporal casts
// in it so bound params / stored STRING cells (which carry the
// envelope at runtime) survive DuckDB's date/time parser.
type plainStringUDF struct{}

func (*plainStringUDF) Config() duckdb.ScalarFuncConfig {
	varchar, _ := duckdb.NewTypeInfo(duckdb.TYPE_VARCHAR)
	return duckdb.ScalarFuncConfig{
		InputTypeInfos: []duckdb.TypeInfo{varchar},
		ResultTypeInfo: varchar,
	}
}

func (*plainStringUDF) Executor() duckdb.ScalarFuncExecutor {
	return duckdb.ScalarFuncExecutor{
		RowExecutor: func(values []driver.Value) (any, error) {
			if len(values) != 1 {
				return nil, fmt.Errorf("googlesqlite_plain_string: want 1 arg, got %d", len(values))
			}
			return internal.GoogleSQLPlainString(values[0])
		},
	}
}

// toNativeJSONUDF adapts internal.GoogleSQLToNativeJSON to DuckDB's
// scalar-UDF interface. The formatter wraps JSON-typed INSERT VALUES /
// UPDATE SET expressions in it so a native JSON column receives raw
// JSON text rather than the value-layer base64 envelope. The argument
// is variadic-ANY (the wrapped expression may be an envelope VARCHAR,
// a natively-encoded param, or a bare scalar); the result is VARCHAR
// that DuckDB casts into the JSON column. NULL short-circuits to NULL
// via DuckDB's default NULL handling.
type toNativeJSONUDF struct{}

func (*toNativeJSONUDF) Config() duckdb.ScalarFuncConfig {
	any_, _ := duckdb.NewTypeInfo(duckdb.TYPE_ANY)
	varchar, _ := duckdb.NewTypeInfo(duckdb.TYPE_VARCHAR)
	return duckdb.ScalarFuncConfig{
		VariadicTypeInfo: any_,
		ResultTypeInfo:   varchar,
	}
}

func (*toNativeJSONUDF) Executor() duckdb.ScalarFuncExecutor {
	return duckdb.ScalarFuncExecutor{
		RowExecutor: func(values []driver.Value) (any, error) {
			if len(values) != 1 {
				return nil, fmt.Errorf("googlesqlite_to_native_json: want 1 arg, got %d", len(values))
			}
			return internal.GoogleSQLToNativeJSON(values[0])
		},
	}
}

// jsonScalarUDF adapts a value-layer JSON-family body to DuckDB's
// scalar-UDF interface. Inputs and output are VARCHAR (the base64
// envelope); arity is variadic over TYPE_ANY so a single registration
// serves the 1-, 2-, and 3-plus-argument JSON functions and tolerates
// arguments the formatter inlines as raw scalars (e.g. integer literals
// in JSON_ARRAY).
type jsonScalarUDF struct {
	eval func(args []any) (any, error)
}

func newJSONScalarUDF(eval func(args []any) (any, error)) *jsonScalarUDF {
	return &jsonScalarUDF{eval: eval}
}

func (*jsonScalarUDF) Config() duckdb.ScalarFuncConfig {
	any_, _ := duckdb.NewTypeInfo(duckdb.TYPE_ANY)
	varchar, _ := duckdb.NewTypeInfo(duckdb.TYPE_VARCHAR)
	return duckdb.ScalarFuncConfig{
		VariadicTypeInfo: any_,
		ResultTypeInfo:   varchar,
		// JSON builtins observe their NULL arguments (e.g.
		// TO_JSON_STRING(NULL) -> the JSON null literal), so opt out of
		// DuckDB's NULL-in/NULL-out short-circuit and let the body decide.
		SpecialNullHandling: true,
	}
}

func (u *jsonScalarUDF) Executor() duckdb.ScalarFuncExecutor {
	return duckdb.ScalarFuncExecutor{
		RowExecutor: func(values []driver.Value) (any, error) {
			args := make([]any, len(values))
			for i, v := range values {
				args[i] = v
			}
			return u.eval(args)
		},
	}
}
