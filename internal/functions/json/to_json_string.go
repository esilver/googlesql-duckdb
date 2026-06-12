package json

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/goccy/googlesqlite/internal/value"
)

func TO_JSON_STRING(v value.Value, prettyPrint bool) (value.Value, error) {
	if v == nil {
		// BigQuery surfaces TO_JSON_STRING(NULL) as the JSON null
		// literal rather than SQL NULL.
		return value.StringValue("null"), nil
	}
	s, err := toJSONLiteral(v)
	if err != nil {
		return nil, err
	}
	return value.StringValue(s), nil
}

func toJSONLiteral(v value.Value) (string, error) {
	if v == nil {
		return "null", nil
	}
	switch vv := v.(type) {
	case *value.SafeValue:
		return toJSONLiteral(vv.Value)
	case value.DateValue, value.DatetimeValue, value.TimeValue, value.TimestampValue:
		s, err := v.ToString()
		if err != nil {
			return "", err
		}
		return strconv.Quote(s), nil
	case *value.ArrayValue:
		elems := make([]string, 0, len(vv.Values))
		for _, elem := range vv.Values {
			s, err := toJSONLiteral(elem)
			if err != nil {
				return "", err
			}
			elems = append(elems, s)
		}
		return "[" + strings.Join(elems, ",") + "]", nil
	case *value.StructValue:
		fields := make([]string, 0, len(vv.Keys))
		for i, key := range vv.Keys {
			s, err := toJSONLiteral(vv.Values[i])
			if err != nil {
				return "", err
			}
			fields = append(fields, strconv.Quote(key)+":"+s)
		}
		return "{" + strings.Join(fields, ",") + "}", nil
	}
	return v.ToJSON()
}

func BindToJsonString(args ...value.Value) (value.Value, error) {
	if len(args) != 1 && len(args) != 2 {
		return nil, fmt.Errorf("TO_JSON_STRING: invalid number of arguments: got %d, want 1 or 2", len(args))
	}
	var prettyPrint bool
	if len(args) == 2 {
		if args[1] != nil {
			b, err := args[1].ToBool()
			if err != nil {
				return nil, err
			}
			prettyPrint = b
		}
	}
	return TO_JSON_STRING(args[0], prettyPrint)
}
