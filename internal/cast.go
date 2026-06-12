package internal

import (
	"fmt"
	"sync"

	"github.com/goccy/go-json"

	googlesql "github.com/goccy/go-googlesql"

	"github.com/goccy/googlesqlite/internal/value"
)

func CAST(expr value.Value, fromType, toType *Type, isSafeCast bool) (value.Value, error) {
	from, err := fromType.ToGoogleSQLType()
	if err != nil {
		return nil, fmt.Errorf("failed to get googlesql type from cast base type: %w", err)
	}
	to, err := toType.ToGoogleSQLType()
	if err != nil {
		return nil, fmt.Errorf("failed to get googlesql type from cast target type: %w", err)
	}
	return castResolved(expr, from, fromType.kindAs(), to, toType.kindAs(), isSafeCast)
}

// castResolved is the body of CAST with both googlesql type nodes (and
// their kinds) already resolved. The kind comes from the serialized
// *Type rather than a node.Kind() wasm round-trip, so the per-row path
// stays wasm-free for every scalar cast.
func castResolved(expr value.Value, from googlesql.Googlesql_TypeNode, fromKind googlesql.TypeKind, to googlesql.Googlesql_TypeNode, toKind googlesql.TypeKind, isSafeCast bool) (value.Value, error) {
	fromValue, err := castValueWithKind(fromKind, from, expr)
	if err != nil {
		if isSafeCast {
			return nil, nil
		}
		return nil, err
	}
	casted, err := castValueWithKind(toKind, to, fromValue)
	if err != nil {
		if isSafeCast {
			return nil, nil
		}
		return nil, err
	}
	return casted, nil
}

// resolvedCastType caches the googlesql type node (and its kind) built
// from one JSON-serialized *Type literal. The formatter embeds the same
// two JSON type strings into every googlesqlite_cast call site, and the
// engine evaluates that call once per row: without the cache each row
// pays json.Unmarshal + ToGoogleSQLType (one or more MakeSimpleType /
// MakeArrayType wasm round-trips, each of which stack-walks for
// goroutine pinning) — ~2ms/row, the entire cost behind issue #8's
// "CTAS is 2ms/row". Direct PROTO casts cache the stable protobuf
// full name instead of a catalog-scoped *ProtoType handle; repeated
// short-lived catalogs can otherwise leave the cache holding stale proto
// handles.
type resolvedCastType struct {
	typ       *Type
	node      googlesql.Googlesql_TypeNode
	kind      googlesql.TypeKind
	protoName string
}

var castTypeCache sync.Map // JSON type string -> *resolvedCastType

func resolveCastType(jsonEncodedType string) (*resolvedCastType, error) {
	if cached, ok := castTypeCache.Load(jsonEncodedType); ok {
		return cached.(*resolvedCastType), nil
	}
	var typ Type
	if err := json.Unmarshal([]byte(jsonEncodedType), &typ); err != nil {
		return nil, err
	}
	kind := typ.kindAs()
	protoName := ""
	var node googlesql.Googlesql_TypeNode
	if kind == googlesql.TypeKindTypeProto {
		protoName = protoNameFromDebug(typ.Name)
	} else {
		var err error
		node, err = typ.ToGoogleSQLType()
		if err != nil {
			return nil, fmt.Errorf("failed to get googlesql type from cast type: %w", err)
		}
	}
	resolved := &resolvedCastType{typ: &typ, node: node, kind: kind, protoName: protoName}
	castTypeCache.Store(jsonEncodedType, resolved)
	return resolved, nil
}

func castValueWithResolvedType(rt *resolvedCastType, v value.Value) (value.Value, error) {
	if rt == nil {
		return v, nil
	}
	if rt.kind == googlesql.TypeKindTypeProto {
		if sv, ok := v.(value.StringValue); ok && rt.protoName != "" {
			bytes, err := parseProtoTextLiteral(rt.protoName, string(sv))
			if err != nil {
				return nil, fmt.Errorf("CAST to PROTO<%s>: %w", rt.protoName, err)
			}
			return value.BytesValue(bytes), nil
		}
		return v, nil
	}
	return castValueWithKind(rt.kind, rt.node, v)
}

func castResolvedTypes(expr value.Value, fromType, toType *resolvedCastType, isSafeCast bool) (value.Value, error) {
	fromValue, err := castValueWithResolvedType(fromType, expr)
	if err != nil {
		if isSafeCast {
			return nil, nil
		}
		return nil, err
	}
	casted, err := castValueWithResolvedType(toType, fromValue)
	if err != nil {
		if isSafeCast {
			return nil, nil
		}
		return nil, err
	}
	return casted, nil
}

func bindCast(args ...value.Value) (value.Value, error) {
	if len(args) != 4 {
		return nil, fmt.Errorf("CAST: invalid number of arguments: got %d, want 4", len(args))
	}
	jsonEncodedFromType, err := args[1].ToString()
	if err != nil {
		return nil, err
	}
	jsonEncodedToType, err := args[2].ToString()
	if err != nil {
		return nil, err
	}
	fromType, err := resolveCastType(jsonEncodedFromType)
	if err != nil {
		return nil, err
	}
	toType, err := resolveCastType(jsonEncodedToType)
	if err != nil {
		return nil, err
	}
	isSafeCast, err := args[3].ToBool()
	if err != nil {
		return nil, err
	}
	return castResolvedTypes(args[0], fromType, toType, isSafeCast)
}
