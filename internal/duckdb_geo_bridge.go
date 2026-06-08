package internal

import (
	"strings"

	"github.com/goccy/googlesqlite/internal/value"
)

// GeoUDFResultType classifies the concrete DuckDB result type a
// geography scalar UDF must declare. DuckDB requires a concrete result
// type per scalar function (TYPE_ANY is rejected), so the root-package
// shell needs to know whether a given function yields a base64 value-
// layer envelope (VARCHAR), or a native double / bigint / boolean that
// EncodeValue passes through as a Go primitive.
type GeoUDFResultType int

const (
	// GeoResultVarchar covers every function whose EncodeValue output is
	// a string: GEOGRAPHY (base64 envelope), STRING / JSON / BYTES
	// results (also base64 envelope). The value decoder reverses the
	// envelope on the result-scan path.
	GeoResultVarchar GeoUDFResultType = iota
	// GeoResultDouble: ST_X / ST_DISTANCE / ST_AREA etc. EncodeValue
	// returns a Go float64.
	GeoResultDouble
	// GeoResultBigint: ST_DIMENSION / ST_NUMPOINTS etc. EncodeValue
	// returns a Go int64.
	GeoResultBigint
	// GeoResultBoolean: ST_CONTAINS / ST_ISEMPTY etc. EncodeValue
	// returns a Go bool.
	GeoResultBoolean
	// GeoResultStringRaw covers the string serializers (ST_ASTEXT,
	// ST_ASGEOJSON, ST_ASKML, ST_GEOMETRYTYPE, ST_GEOHASH). Their result
	// is a user-facing STRING that may be wrapped by a native DuckDB
	// string function (LENGTH(ST_GEOHASH(...)), so the UDF must return
	// the raw decoded text, not the base64 value-layer envelope, for the
	// outer function to see the real string.
	GeoResultStringRaw
	// GeoResultBytesRaw covers ST_ASBINARY: the WKB is a user-facing
	// BYTES value consumed by native byte functions (TO_HEX(...)), so the
	// UDF returns the raw []byte rather than the base64 envelope.
	GeoResultBytesRaw
)

// geoResultTypeByName pins each geography / S2 scalar function to the
// concrete Go type its pure-Go body returns through EncodeValue. The
// default (absent) entry is GeoResultVarchar, which is correct for
// every GEOGRAPHY-producing constructor and every string serializer;
// only the numeric / boolean accessors and predicates need an explicit
// override here.
var geoResultTypeByName = map[string]GeoUDFResultType{
	// Double-valued measurements and accessors.
	"st_x":                    GeoResultDouble,
	"st_y":                    GeoResultDouble,
	"st_distance":             GeoResultDouble,
	"st_area":                 GeoResultDouble,
	"st_length":               GeoResultDouble,
	"st_perimeter":            GeoResultDouble,
	"st_azimuth":              GeoResultDouble,
	"st_angle":                GeoResultDouble,
	"st_maxdistance":          GeoResultDouble,
	"st_hausdorffdistance":    GeoResultDouble,
	"st_linelocatepoint":      GeoResultDouble,
	// Integer-valued accessors.
	"st_dimension":     GeoResultBigint,
	"st_numpoints":     GeoResultBigint,
	"st_npoints":       GeoResultBigint,
	"st_numgeometries": GeoResultBigint,
	// S2 cell id is an INT64 cell identifier.
	"s2_cellidfrompoint": GeoResultBigint,
	// String serializers: return raw text so a native string function
	// wrapping the call (e.g. LENGTH) sees the real string.
	"st_astext":       GeoResultStringRaw,
	"st_asgeojson":    GeoResultStringRaw,
	"st_askml":        GeoResultStringRaw,
	"st_geometrytype": GeoResultStringRaw,
	"st_geohash":      GeoResultStringRaw,
	// WKB serializer: return raw bytes for native byte functions.
	"st_asbinary": GeoResultBytesRaw,
	// Boolean predicates.
	"st_isempty":          GeoResultBoolean,
	"st_iscollection":     GeoResultBoolean,
	"st_isclosed":         GeoResultBoolean,
	"st_isring":           GeoResultBoolean,
	"st_intersects":       GeoResultBoolean,
	"st_contains":         GeoResultBoolean,
	"st_within":           GeoResultBoolean,
	"st_covers":           GeoResultBoolean,
	"st_coveredby":        GeoResultBoolean,
	"st_disjoint":         GeoResultBoolean,
	"st_touches":          GeoResultBoolean,
	"st_equals":           GeoResultBoolean,
	"st_dwithin":          GeoResultBoolean,
	"st_hausdorffdwithin": GeoResultBoolean,
	"st_intersectsbox":    GeoResultBoolean,
}

// geographyScalarNameSet is the set of normalFuncs names the formatter
// must route to a googlesqlite_st_* / googlesqlite_s2_* DuckDB UDF.
// Built once from the same normalFuncs table the runtime registers, so
// the formatter lowering and the registered UDF surface can never drift.
var geographyScalarNameSet = func() map[string]struct{} {
	m := map[string]struct{}{}
	for _, info := range normalFuncs {
		if strings.HasPrefix(info.Name, "st_") || strings.HasPrefix(info.Name, "s2_") {
			m[info.Name] = struct{}{}
		}
	}
	return m
}()

// isGeographyScalarFunc reports whether name is a geography / S2 scalar
// builtin backed by a registered googlesqlite_<name> DuckDB UDF.
func isGeographyScalarFunc(name string) bool {
	_, ok := geographyScalarNameSet[name]
	return ok
}

// GeoScalarFunc is one geography scalar function ready to register as a
// DuckDB UDF: the storage name the formatter emits (googlesqlite_<name>),
// a row executor that decodes the value-layer args, runs the pure-Go
// body, and encodes the result, and the concrete DuckDB result type.
type GeoScalarFunc struct {
	StorageName string
	ResultType  GeoUDFResultType
	Exec        func(args []any) (any, error)
}

// encodeGeoResult renders a geography body result into the Go value the
// DuckDB UDF returns, matching the declared result type. STRING and
// BYTES serializers return their raw decoded form (so a native function
// wrapping the call sees the real string / bytes); every other category
// passes through EncodeValue, whose output (Go primitive for INT64 /
// FLOAT64 / BOOL, base64 envelope string for GEOGRAPHY) already matches
// the declared DOUBLE / BIGINT / BOOLEAN / VARCHAR result type.
func encodeGeoResult(ret value.Value, rt GeoUDFResultType) (any, error) {
	if ret == nil {
		return nil, nil
	}
	switch rt {
	case GeoResultStringRaw:
		return ret.ToString()
	case GeoResultBytesRaw:
		return ret.ToBytes()
	default:
		return EncodeValue(ret)
	}
}

// GeographyScalarFuncs returns the geography + S2 scalar functions from
// the normalFuncs table (every entry whose name starts with st_ or s2_),
// each wrapped so the DuckDB UDF shell in the root package can register
// it. The wrapper reuses the exact decode -> body -> encode path the
// SQLite registration used (value.ConvertArgs + BindFunc + EncodeValue),
// so the value-layer envelope semantics are identical on both engines.
func GeographyScalarFuncs() []GeoScalarFunc {
	out := make([]GeoScalarFunc, 0, 80)
	for _, info := range normalFuncs {
		if !strings.HasPrefix(info.Name, "st_") && !strings.HasPrefix(info.Name, "s2_") {
			continue
		}
		bind := info.BindFunc
		rt := geoResultTypeByName[info.Name]
		out = append(out, GeoScalarFunc{
			StorageName: "googlesqlite_" + info.Name,
			ResultType:  rt,
			Exec: func(args []any) (any, error) {
				values, err := value.ConvertArgs(args...)
				if err != nil {
					return nil, err
				}
				ret, err := bind(values...)
				if err != nil {
					return nil, err
				}
				return encodeGeoResult(ret, rt)
			},
		})
	}
	return out
}
