package json

import (
	"fmt"

	"github.com/goccy/googlesqlite/internal/value"
)

func JSON_TYPE(v value.JsonValue) (value.Value, error) {
	return value.StringValue(v.Type()), nil
}

func BindJsonType(args ...value.Value) (value.Value, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("JSON_TYPE: invalid number of arguments: got %d, want 1", len(args))
	}
	if args[0] == nil {
		return nil, nil // JSON_TYPE(NULL) is NULL
	}
	jv, ok := args[0].(value.JsonValue)
	if !ok {
		// A stored JSON cell can arrive engine-native: DuckDB scans a
		// JSON object / array into the matching Go shape, so ConvertArgs
		// hands a StructValue / ArrayValue (or a StringValue for a JSON
		// string cell) rather than a JsonValue. The analyzer already
		// guaranteed those arguments are JSON-typed — render their JSON
		// form and classify that. Anything else (a bare INT64 / FLOAT64 /
		// BOOL ...) is a type error, matching JSON_TYPE's
		// JSON-argument-only signature.
		switch args[0].(type) {
		case *value.StructValue, *value.ArrayValue, value.StringValue:
			s, err := args[0].ToJSON()
			if err != nil {
				return nil, fmt.Errorf("JSON_TYPE: failed to convert %T to JSON value", args[0])
			}
			jv = value.JsonValue(s)
		default:
			return nil, fmt.Errorf("JSON_TYPE: argument must be a JSON value, got %T", args[0])
		}
	}
	return JSON_TYPE(jv)
}
