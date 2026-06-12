package googlesqlite

import (
	"database/sql"
	"database/sql/driver"
	"fmt"

	duckdb "github.com/goccy/googlesqlite/internal/duckdbgo"

	internal "github.com/goccy/googlesqlite/internal"
)

// structValueLayerUDFNames is the set of canonical builtin names whose
// googlesqlite_<name> scalar UDFs back the STRUCT value layer. The
// formatter emits these names directly (MakeStructNode, GetStructFieldNode,
// the UPDATE field-set rewrite, and REPLACE_FIELDS), so the bodies must be
// registered as duckdb-go scalar UDFs. Every name has a matching entry in
// internal.normalFuncs, so internal.CallNormalFunc reaches the shared
// pure-Go body that operates over the value-layer envelope.
//
// The result is the value-layer envelope (a VARCHAR holding the
// base64-encoded JSON of a StructValue). The row decoder
// (Rows.assignInterfaceValue) reads that envelope back and materialises a
// native Go []any for the STRUCT column, so the surface the test harness
// sees is a native struct, not the envelope string.
var structValueLayerUDFNames = []string{
	// MAKE_STRUCT(name1, val1, name2, val2, ...) -> the resolved-tree
	// MakeStructNode emits googlesqlite_make_struct with interleaved
	// field-name string literals and field-value expressions.
	"make_struct",
	// GETSTRUCTFIELD(struct, idx) -> field access by 0-based position.
	"get_struct_field",
	// UPDATE rewrite of `col.field = value` lowers to a whole-column
	// replacement through googlesqlite_struct_with_field_set.
	"struct_with_field_set",
	// REPLACE_FIELDS(proto/struct, (value, path), ...) lowers each pair
	// to a nested googlesqlite_replace_fields call.
	"replace_fields",
}

// registerStructUDFs installs the STRUCT value-layer scalar UDFs onto a
// DuckDB connection. Each UDF takes a variadic ANY argument list (so the
// mix of envelope-VARCHAR field values, native-int field indices, and
// bare string literals all pass through unmolested) and delegates to
// internal.CallNormalFunc, which decodes each argument to a value.Value,
// runs the builtin's pure-Go body, and re-encodes the StructValue result
// to the value-layer envelope. The googlesqlite_safe_<name> companion is
// not emitted by the formatter for these, so only the raising form is
// registered.
func registerStructUDFs(conn *sql.Conn) error {
	for _, name := range structValueLayerUDFNames {
		udf := &structFuncUDF{name: name}
		if err := duckdb.RegisterScalarUDF(conn, "googlesqlite_"+name, udf); err != nil {
			return fmt.Errorf("googlesqlite: register googlesqlite_%s: %w", name, err)
		}
	}
	return nil
}

// structFuncUDF adapts one STRUCT value-layer builtin to DuckDB's
// scalar-UDF interface. The variadic ANY input lets a single registration
// serve every arity the builtin accepts (MAKE_STRUCT with any field count,
// GET_STRUCT_FIELD's fixed (struct, idx) pair, the 3-arg field-set, and
// REPLACE_FIELDS' 4-arg primitive). SpecialNullHandling keeps NULL
// arguments flowing into the body so the per-function NULL semantics
// (e.g. GET_STRUCT_FIELD of a missing field is NULL, STRUCT_WITH_FIELD_SET
// of a NULL struct is NULL) are decided by the shared body rather than by
// DuckDB's blanket NULL-in/NULL-out default.
type structFuncUDF struct {
	name string
}

func (u *structFuncUDF) Config() duckdb.ScalarFuncConfig {
	anyInfo, _ := duckdb.NewTypeInfo(duckdb.TYPE_ANY)
	varchar, _ := duckdb.NewTypeInfo(duckdb.TYPE_VARCHAR)
	return duckdb.ScalarFuncConfig{
		VariadicTypeInfo:    anyInfo,
		ResultTypeInfo:      varchar,
		SpecialNullHandling: true,
	}
}

func (u *structFuncUDF) Executor() duckdb.ScalarFuncExecutor {
	name := u.name
	return duckdb.ScalarFuncExecutor{
		RowExecutor: func(values []driver.Value) (any, error) {
			args := make([]any, len(values))
			for i, v := range values {
				args[i] = v
			}
			return internal.CallValueLayerFunc(name, args...)
		},
	}
}
