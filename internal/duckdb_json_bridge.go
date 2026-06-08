package internal

import (
	"sync"

	jsonfn "github.com/goccy/googlesqlite/internal/functions/json"
	"github.com/goccy/googlesqlite/internal/functions/longtail"
	"github.com/goccy/googlesqlite/internal/functions/timestamp"
	"github.com/goccy/googlesqlite/internal/value"
)

// jsonFamilyFuncs is the BigQuery JSON-function surface the DuckDB
// backend serves through registered googlesqlite_* scalar UDFs. The
// formatter lowers e.g. JSON_QUERY(...) to googlesqlite_json_query(...),
// and registerDuckDBJSONUDFs installs a UDF under that exact name whose
// body decodes the value-layer-encoded arguments, delegates to the
// shared pure-Go implementation, and re-encodes the result.
//
// Keeping this table local (rather than reusing the full normalFuncs
// list) scopes the DuckDB JSON retarget to its own family without
// disturbing the other function categories that still route through the
// predecessor's SQLite adapter.
var jsonFamilyFuncs = []*funcInfo{
	{Name: "json_extract", BindFunc: jsonfn.BindJsonExtract},
	{Name: "json_extract_scalar", BindFunc: jsonfn.BindJsonExtractScalar},
	{Name: "json_extract_array", BindFunc: jsonfn.BindJsonExtractArray},
	{Name: "json_extract_string_array", BindFunc: jsonfn.BindJsonExtractStringArray},
	{Name: "json_query", BindFunc: jsonfn.BindJsonQuery},
	{Name: "json_value", BindFunc: jsonfn.BindJsonValue},
	{Name: "json_query_array", BindFunc: jsonfn.BindJsonQueryArray},
	{Name: "json_value_array", BindFunc: jsonfn.BindJsonValueArray},
	{Name: "parse_json", BindFunc: jsonfn.BindParseJson},
	{Name: "to_json", BindFunc: jsonfn.BindToJson},
	{Name: "to_json_string", BindFunc: jsonfn.BindToJsonString},
	{Name: "json_type", BindFunc: jsonfn.BindJsonType},
	{Name: "json_strip_nulls", BindFunc: jsonfn.BindJsonStripNulls},
	{Name: "json_remove", BindFunc: jsonfn.BindJsonRemove},
	{Name: "json_set", BindFunc: jsonfn.BindJsonSet},
	{Name: "json_keys", BindFunc: jsonfn.BindJsonKeys},
	{Name: "json_array", BindFunc: jsonfn.JSON_ARRAY},
	{Name: "json_object", BindFunc: jsonfn.JSON_OBJECT},
	{Name: "json_contains", BindFunc: jsonfn.JSON_CONTAINS},
	{Name: "json_flatten", BindFunc: jsonfn.JSON_FLATTEN},
	{Name: "json_path_exists", BindFunc: jsonfn.JSON_PATH_EXISTS},

	// Strict scalar accessors (BOOL/INT64/... taking a JSON arg).
	{Name: "bool", BindFunc: bindBool},
	{Name: "int64", BindFunc: bindInt64},
	{Name: "double", BindFunc: bindDouble},
	{Name: "float64", BindFunc: bindDouble},
	{Name: "int32", BindFunc: jsonfn.BindInt32},
	{Name: "uint32", BindFunc: jsonfn.BindUint32},
	{Name: "uint64", BindFunc: jsonfn.BindUint64},
	{Name: "float32", BindFunc: jsonfn.BindFloat},
	{Name: "string", BindFunc: timestamp.BindString},

	// LAX scalar accessors.
	{Name: "lax_int64", BindFunc: jsonfn.BindLaxInt64},
	{Name: "lax_float64", BindFunc: jsonfn.BindLaxFloat64},
	{Name: "lax_bool", BindFunc: jsonfn.BindLaxBool},
	{Name: "lax_string", BindFunc: jsonfn.BindLaxString},
	{Name: "lax_int32", BindFunc: jsonfn.BindLaxInt32},
	{Name: "lax_uint32", BindFunc: jsonfn.BindLaxUint32},
	{Name: "lax_uint64", BindFunc: jsonfn.BindLaxUint64},
	{Name: "lax_float32", BindFunc: jsonfn.BindLaxFloat},

	// Strict array accessors.
	{Name: "bool_array", BindFunc: jsonfn.BindBoolArray},
	{Name: "int32_array", BindFunc: jsonfn.BindInt32Array},
	{Name: "int64_array", BindFunc: jsonfn.BindInt64Array},
	{Name: "uint32_array", BindFunc: jsonfn.BindUint32Array},
	{Name: "uint64_array", BindFunc: jsonfn.BindUint64Array},
	{Name: "float32_array", BindFunc: jsonfn.BindFloatArray},
	{Name: "double_array", BindFunc: jsonfn.BindDoubleArray},
	{Name: "float64_array", BindFunc: jsonfn.BindDoubleArray},
	{Name: "string_array", BindFunc: jsonfn.BindStringArray},

	// LAX array accessors.
	{Name: "lax_bool_array", BindFunc: jsonfn.BindLaxBoolArray},
	{Name: "lax_int32_array", BindFunc: jsonfn.BindLaxInt32Array},
	{Name: "lax_int64_array", BindFunc: jsonfn.BindLaxInt64Array},
	{Name: "lax_uint32_array", BindFunc: jsonfn.BindLaxUint32Array},
	{Name: "lax_uint64_array", BindFunc: jsonfn.BindLaxUint64Array},
	{Name: "lax_float32_array", BindFunc: jsonfn.BindLaxFloatArray},
	{Name: "lax_double_array", BindFunc: jsonfn.BindLaxDoubleArray},
	{Name: "lax_float64_array", BindFunc: jsonfn.BindLaxDoubleArray},
	{Name: "lax_string_array", BindFunc: jsonfn.BindLaxStringArray},

	// longtail JSON mutators / converters.
	{Name: "safe_to_json", BindFunc: longtail.BindSafeToJson},
	{Name: "json_array_append", BindFunc: longtail.BindJsonArrayAppend},
	{Name: "json_array_insert", BindFunc: longtail.BindJsonArrayInsert},
}

var jsonFamilyMapOnce sync.Once

// ensureJSONFamilyInFormatterMap inserts every JSON-family name into
// normalFuncMap so getFuncNameAndArgs emits the googlesqlite_-prefixed
// UDF name. The formatter only consults the map for membership (it never
// invokes the stored body on the DuckDB path), so a nil body is fine; we
// still attach the canonical encode/decode wrapper for parity with the
// SQLite registration shape.
func ensureJSONFamilyInFormatterMap() {
	jsonFamilyMapOnce.Do(func() {
		funcMapMu.Lock()
		defer funcMapMu.Unlock()
		for _, info := range jsonFamilyFuncs {
			if _, ok := normalFuncMap[info.Name]; ok {
				continue
			}
			normalFuncMap[info.Name] = []*nameAndFunc{{
				Name: "googlesqlite_" + info.Name,
			}}
		}
	})
}

// JSONFamilyUDF describes one DuckDB scalar UDF the root package
// registers: the prefixed catalog name plus the body that decodes the
// value-layer arguments, runs the shared implementation, and re-encodes
// the result as the canonical base64 envelope string (VARCHAR result).
type JSONFamilyUDF struct {
	Name string
	Eval func(args []any) (any, error)
}

// JSONFamilyUDFs returns the googlesqlite_<name> and
// googlesqlite_safe_<name> scalar UDFs for every JSON-family function,
// ready for registration on a DuckDB handle. It also populates
// normalFuncMap as a side effect so the formatter and the registered
// UDFs stay in lockstep.
func JSONFamilyUDFs() []JSONFamilyUDF {
	ensureJSONFamilyInFormatterMap()
	out := make([]JSONFamilyUDF, 0, len(jsonFamilyFuncs)*2)
	for _, info := range jsonFamilyFuncs {
		bind := info.BindFunc
		eval := func(args []any) (any, error) {
			values, err := value.ConvertArgs(args...)
			if err != nil {
				return nil, err
			}
			ret, err := bind(values...)
			if err != nil {
				return nil, err
			}
			s, ok, err := value.EncodeValueAsString(ret)
			if err != nil {
				return nil, err
			}
			if !ok {
				return nil, nil
			}
			return s, nil
		}
		out = append(out, JSONFamilyUDF{Name: "googlesqlite_" + info.Name, Eval: eval})
		// The safe_ variant folds a runtime error to NULL (SAFE.fn / use
		// inside IFERROR). Reuse the same body but swallow errors.
		safeEval := func(args []any) (any, error) {
			res, err := eval(args)
			if err != nil {
				return nil, nil
			}
			return res, nil
		}
		out = append(out, JSONFamilyUDF{Name: "googlesqlite_safe_" + info.Name, Eval: safeEval})
	}
	return out
}
