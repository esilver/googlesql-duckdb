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
		// A stored JSON cell arrives engine-native: DuckDB scans a JSON
		// object / array / scalar into the matching Go shape, so ConvertArgs
		// hands a StructValue / ArrayValue / scalar Value rather than a
		// JsonValue. The analyzer already guaranteed the argument is
		// JSON-typed — render its JSON form and classify that.
		s, err := args[0].ToJSON()
		if err != nil {
			return nil, fmt.Errorf("JSON_TYPE: failed to convert %T to JSON value", args[0])
		}
		jv = value.JsonValue(s)
	}
	return JSON_TYPE(jv)
}
