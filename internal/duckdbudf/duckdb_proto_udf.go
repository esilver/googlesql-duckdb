package duckdbudf

import (
	"database/sql"
	"database/sql/driver"
	"fmt"

	duckdb "github.com/goccy/googlesqlite/internal/duckdbgo"

	internal "github.com/goccy/googlesqlite/internal"
)

// protoValueLayerUDFNames is the set of canonical proto-family builtin
// names whose googlesqlite_<name> scalar UDFs back the proto value
// surface. The formatter (internal/formatter.go) lowers proto
// constructors and accessors directly to these names:
//
//   - new MessageName(v AS f, ...)            -> googlesqlite_make_proto
//   - proto.field / GETPROTOFIELD             -> googlesqlite_get_proto_field
//   - repeated-field access                    -> googlesqlite_get_proto_field_repeated
//   - FILTER_FIELDS(proto, +/-paths)          -> googlesqlite_filter_fields
//   - FROM_PROTO / TO_PROTO over wrappers      -> googlesqlite_from_proto / _to_proto
//   - PROTO_MAP_CONTAINS_KEY / PROTO_MODIFY_MAP
//   - ENUM_VALUE_DESCRIPTOR_PROTO
//
// Each name has a matching BindFunc entry in internal.normalFuncs
// (internal/function_register_normal.go), so the proto UDF shell reaches
// the pure-Go proto body via internal.CallProtoFunc.
//
// replace_fields is deliberately omitted: REPLACE_FIELDS is already
// registered through structValueLayerUDFNames (it operates over both
// STRUCT and PROTO inputs and resolves to the same protofn.BindReplaceFields
// body), so registering it here would be a duplicate-name error.
//
// The result of every proto builtin is the value-layer envelope (a
// VARCHAR holding the base64-encoded JSON of the value). Proto wire
// payloads come back as a BytesValue inside that envelope; scalar
// accessors like GET_PROTO_FIELD return the field's resolved value
// (INT64 / STRING / BOOL / ...) inside the same envelope. The row
// decoder (Rows.assignInterfaceValue) reads the envelope back and
// re-types it against the resolved column type, exactly as it does for
// the STRUCT accessors.
var protoValueLayerUDFNames = []string{
	"make_proto",
	"get_proto_field",
	"get_proto_field_repeated",
	"filter_fields",
	"from_proto",
	"to_proto",
	"proto_map_contains_key",
	"proto_modify_map",
	"enum_value_descriptor_proto",
}

// registerProtoUDFs installs the proto value-layer scalar UDFs onto a
// DuckDB connection. The proto builtins share the STRUCT builtins'
// "decode args -> run shared body -> re-encode to the value-layer
// envelope" contract, but route through internal.CallProtoFunc so the
// FROM_PROTO / TO_PROTO identity-passthrough over a native DATE /
// TIMESTAMP literal (which arrives as a bare Go time.Time) is re-wrapped
// before the value decoder sees it.
func registerProtoUDFs(conn *sql.Conn) error {
	for _, name := range protoValueLayerUDFNames {
		udf := &protoFuncUDF{name: name}
		if err := duckdb.RegisterScalarUDF(conn, "googlesqlite_"+name, udf); err != nil {
			return fmt.Errorf("googlesqlite: register googlesqlite_%s: %w", name, err)
		}
	}
	return nil
}

// protoFuncUDF adapts one proto-family builtin to DuckDB's scalar-UDF
// interface. The variadic ANY input lets one registration serve every
// arity each proto builtin accepts (MAKE_PROTO's (tag, kind, value)
// triples, the fixed (proto, num, kind, default) of GET_PROTO_FIELD, and
// the variadic key/value tail of PROTO_MODIFY_MAP). SpecialNullHandling
// keeps NULL arguments flowing into the body so each builtin's per-arg
// NULL semantics (e.g. GET_PROTO_FIELD of a NULL proto is NULL,
// PROTO_MODIFY_MAP of a NULL key is an error) are decided by the shared
// body rather than by DuckDB's blanket NULL-in/NULL-out default.
type protoFuncUDF struct {
	name string
}

func (u *protoFuncUDF) Config() duckdb.ScalarFuncConfig {
	anyInfo, _ := duckdb.NewTypeInfo(duckdb.TYPE_ANY)
	varchar, _ := duckdb.NewTypeInfo(duckdb.TYPE_VARCHAR)
	return duckdb.ScalarFuncConfig{
		VariadicTypeInfo:    anyInfo,
		ResultTypeInfo:      varchar,
		SpecialNullHandling: true,
	}
}

func (u *protoFuncUDF) Executor() duckdb.ScalarFuncExecutor {
	name := u.name
	return duckdb.ScalarFuncExecutor{
		RowExecutor: func(values []driver.Value) (any, error) {
			args := make([]any, len(values))
			for i, v := range values {
				args[i] = v
			}
			return internal.CallProtoFunc(name, args...)
		},
	}
}
