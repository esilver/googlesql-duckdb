package internal

import (
	"fmt"
	"sync"

	"github.com/goccy/googlesqlite/internal/value"
)

// DuckDBUDFResultKind names the DuckDB result type a googlesqlite_<name>
// scalar UDF declares. It mirrors the Go type EncodeValue produces for that
// function's return value: IntValue -> int64 (BIGINT), FloatValue -> float64
// (DOUBLE), BoolValue -> bool (BOOLEAN), and every richer type (STRING /
// BYTES / ARRAY / ...) -> the base64 envelope string (VARCHAR).
type DuckDBUDFResultKind int

const (
	// DuckDBResultVarchar is the default: the function returns the
	// value-layer envelope as a VARCHAR.
	DuckDBResultVarchar DuckDBUDFResultKind = iota
	// DuckDBResultBigint covers INT64-returning functions.
	DuckDBResultBigint
	// DuckDBResultDouble covers FLOAT64-returning functions.
	DuckDBResultDouble
	// DuckDBResultBoolean covers BOOL-returning functions.
	DuckDBResultBoolean
)

// DuckDBUDFSpec describes one googlesqlite_<name> scalar UDF the DuckDB
// retarget registers: the canonical builtin name (the formatter emits
// googlesqlite_<Name> / googlesqlite_safe_<Name>) and the declared result
// type.
type DuckDBUDFSpec struct {
	Name       string
	ResultKind DuckDBUDFResultKind
	Volatile   bool
}

// stringFamilyResultKinds records the result type for every string-family
// builtin whose return value is not the default VARCHAR envelope. Functions
// absent from this map default to DuckDBResultVarchar.
var stringFamilyResultKinds = map[string]DuckDBUDFResultKind{
	"ascii":         DuckDBResultBigint,
	"byte_length":   DuckDBResultBigint,
	"char_length":   DuckDBResultBigint,
	"edit_distance": DuckDBResultBigint,
	"instr":         DuckDBResultBigint,
	"length":        DuckDBResultBigint,
	"strpos":        DuckDBResultBigint,
	"unicode":       DuckDBResultBigint,
	"regexp_instr":  DuckDBResultBigint,

	"ends_with":       DuckDBResultBoolean,
	"starts_with":     DuckDBResultBoolean,
	"regexp_contains": DuckDBResultBoolean,
	// REGEXP_MATCH is the legacy boolean form (full-string match), not a
	// substring extractor, so it returns BOOL like CONTAINS/STARTS_WITH.
	"regexp_match": DuckDBResultBoolean,
}

var duckDBNormalResultKinds = map[string]DuckDBUDFResultKind{
	"contains_substr":    DuckDBResultBoolean,
	"like":               DuckDBResultBoolean,
	"keys_keyset_length": DuckDBResultBigint,
	// Hash + bit functions whose pure-Go body returns IntValue must
	// declare BIGINT so the value-layer bridge hands DuckDB a native
	// integer rather than the envelope VARCHAR. FARM_FINGERPRINT and
	// BIT_COUNT return INT64; the BIT_CAST_TO_* family carries its
	// reinterpreted width as INT64 on this layer.
	"farm_fingerprint":   DuckDBResultBigint,
	"bit_count":          DuckDBResultBigint,
	"bit_cast_to_int32":  DuckDBResultBigint,
	"bit_cast_to_int64":  DuckDBResultBigint,
	"bit_cast_to_uint32": DuckDBResultBigint,
	"bit_cast_to_uint64": DuckDBResultBigint,
	// EXTRACT(part FROM INTERVAL ...) returns the requested field as
	// an INT64; the date/timestamp EXTRACT forms never reach this UDF
	// (tryFormatExtract lowers them to native date_part / CAST).
	"extract": DuckDBResultBigint,
}

var duckDBVolatileUDFNames = map[string]struct{}{
	"generate_uuid":               {},
	"keys_new_keyset":             {},
	"keys_add_key_from_raw_bytes": {},
	"keys_rotate_keyset":          {},
	"keys_new_wrapped_keyset":     {},
	"aead_encrypt":                {},
}

// stringFamilyUDFNames is the canonical builtin names of the string-family
// functions ported to DuckDB scalar UDFs. Each name has a matching entry in
// normalFuncs (so a BindFunc exists) and a string spec under
// testdata/specs/googlesql/functions/string.
var stringFamilyUDFNames = []string{
	"ascii",
	"byte_length",
	"char_length",
	"chr",
	"code_points_to_bytes",
	"code_points_to_string",
	"collate",
	"concat",
	"edit_distance",
	"ends_with",
	"format",
	"from_base32",
	"from_base64",
	"from_hex",
	"initcap",
	"instr",
	"left",
	"length",
	"lpad",
	"lower",
	"ltrim",
	"normalize",
	"normalize_and_casefold",
	"regexp_contains",
	"regexp_extract",
	"regexp_extract_all",
	"regexp_instr",
	"regexp_replace",
	"replace",
	"repeat",
	"reverse",
	"right",
	"rpad",
	"rtrim",
	"safe_convert_bytes_to_string",
	"soundex",
	"split",
	"split_substr",
	"starts_with",
	"strpos",
	"substr",
	"to_base32",
	"to_base64",
	"to_code_points",
	"to_hex",
	"translate",
	"trim",
	"unicode",
	"upper",
	"regexp_extract_groups",
	"regexp_match",
	// TO_JSON_STRING is the JSON-family renderer the string specs wrap
	// around their ARRAY/BYTES-returning functions (TO_CODE_POINTS,
	// CODE_POINTS_TO_BYTES, REGEXP_EXTRACT_ALL) to obtain a comparable
	// scalar. It returns a STRING (JSON text), so the generic VARCHAR
	// bridge handles it; registering it here unblocks those string cases.
	"to_json_string",
}

// duckDBNormalUDFNames is the broader normal-function surface registered on
// DuckDB through the generic value-layer scalar bridge. It includes the string
// family plus BigQuery scalars whose bodies already live in normalFuncs.
var duckDBNormalUDFNames = append(append([]string{}, stringFamilyUDFNames...),
	"contains_substr",
	"like",
	"make_array",
	"generate_uuid",
	"typeof",
	"date_bucket",
	"datetime_bucket",
	"timestamp_bucket",
	"text_analyze",
	"bag_of_words",
	"keys_new_keyset",
	"keys_add_key_from_raw_bytes",
	"keys_keyset_length",
	"keys_keyset_to_json",
	"keys_keyset_from_json",
	"keys_rotate_keyset",
	"keys_keyset_chain",
	"keys_new_wrapped_keyset",
	"keys_rewrap_keyset",
	"keys_rotate_wrapped_keyset",
	"aead_encrypt",
	"aead_decrypt_bytes",
	"aead_decrypt_string",
	"deterministic_encrypt",
	"deterministic_decrypt_bytes",
	"deterministic_decrypt_string",
	"dlp_key_chain",
	"dlp_deterministic_encrypt",
	"dlp_deterministic_decrypt",
	// Hash family: MD5 / SHA* / HIGHWAY_FINGERPRINT128 return BYTES
	// (envelope VARCHAR, consumed by TO_HEX / BYTE_LENGTH which are
	// already value-layer UDFs); FARM_FINGERPRINT returns INT64.
	// Routing these through the value-layer bridge decodes the BYTES
	// envelope to the raw payload before hashing, instead of hashing
	// the base64 envelope string DuckDB's native md5/sha256 would see.
	"md5",
	"sha1",
	"sha256",
	"sha512",
	"farm_fingerprint",
	"highway_fingerprint128",
	// Bit family: BIT_CAST_TO_* reinterpret the integer bit pattern;
	// BIT_COUNT popcounts an INT64 / BYTES input. All return INT64.
	"bit_cast_to_int32",
	"bit_cast_to_int64",
	"bit_cast_to_uint32",
	"bit_cast_to_uint64",
	"bit_count",
	// Numeric parsers: PARSE_NUMERIC / PARSE_BIGNUMERIC return a
	// NUMERIC / BIGNUMERIC value (envelope VARCHAR), CAST to STRING by
	// the spec.
	"parse_numeric",
	"parse_bignumeric",
	// Interval constructors / normalizers return an INTERVAL value
	// (envelope VARCHAR); the specs only assert IS NOT NULL.
	"make_interval",
	"justify_days",
	"justify_hours",
	"justify_interval",
	// EXTRACT(part FROM INTERVAL ...) is routed here by tryFormatExtract
	// (no DuckDB-native date_part overload accepts the interval
	// envelope). The extracted interval field is an INT64.
	"extract",
)

// StringFamilyUDFSpecs returns the DuckDB scalar UDF specs for the string
// family. The root package consumes these to register one variadic UDF per
// name (and its googlesqlite_safe_ companion) against duckdb-go.
func StringFamilyUDFSpecs() []DuckDBUDFSpec {
	specs := make([]DuckDBUDFSpec, 0, len(stringFamilyUDFNames))
	for _, name := range stringFamilyUDFNames {
		specs = append(specs, DuckDBUDFSpec{
			Name:       name,
			ResultKind: stringFamilyResultKinds[name],
		})
	}
	return specs
}

// DuckDBNormalUDFSpecs returns the normal scalar UDF specs registered on the
// DuckDB backend. The result kind mirrors EncodeValue's native output where
// callers need a BOOL / INT64 in SQL; richer results remain VARCHAR envelopes.
func DuckDBNormalUDFSpecs() []DuckDBUDFSpec {
	specs := make([]DuckDBUDFSpec, 0, len(duckDBNormalUDFNames))
	for _, name := range duckDBNormalUDFNames {
		kind, ok := duckDBNormalResultKinds[name]
		if !ok {
			kind = stringFamilyResultKinds[name]
		}
		_, volatile := duckDBVolatileUDFNames[name]
		specs = append(specs, DuckDBUDFSpec{
			Name:       name,
			ResultKind: kind,
			Volatile:   volatile,
		})
	}
	return specs
}

// normalBindFuncIndex maps a canonical builtin name to its pure-Go body so
// the DuckDB bridge can invoke it. It is built once under bindFuncIndexOnce
// (DuckDB executes UDFs concurrently across parallel connections, so a lazy
// unsynchronised build races with reads). After the once completes the map
// is read-only and safe for concurrent lookup.
var (
	bindFuncIndexOnce   sync.Once
	normalBindFuncIndex map[string]BindFunction
)

func buildBindFuncIndex() {
	normalBindFuncIndex = make(map[string]BindFunction, len(normalFuncs))
	for _, info := range normalFuncs {
		// The first registration for a name wins, matching the
		// formatter's lookup which keys on the canonical name.
		if _, exists := normalBindFuncIndex[info.Name]; !exists {
			normalBindFuncIndex[info.Name] = info.BindFunc
		}
	}
}

func bindFuncForName(name string) (BindFunction, bool) {
	bindFuncIndexOnce.Do(buildBindFuncIndex)
	fn, ok := normalBindFuncIndex[name]
	return fn, ok
}

// CallNormalFunc is the DuckDB-facing bridge for a googlesqlite_<name>
// scalar UDF. It mirrors setupNormalFuncMap's SQLite body exactly: decode
// every value-layer-encoded argument back into a value.Value, invoke the
// builtin's pure-Go BindFunc, and re-encode the result into the over-the-wire
// form (a native int64 / float64 / bool, or a base64 envelope string). When
// safe is true a body error folds to NULL instead of propagating, matching
// the googlesqlite_safe_<name> SQLite registration.
func CallNormalFunc(name string, safe bool, args ...any) (any, error) {
	fn, ok := bindFuncForName(name)
	if !ok {
		return nil, fmt.Errorf("googlesqlite: no registered body for function %q", name)
	}
	values, err := value.ConvertArgs(args...)
	if err != nil {
		if safe {
			return nil, nil
		}
		return nil, err
	}
	ret, err := fn(values...)
	if err != nil {
		if safe {
			// Suppress only data-dependent semantic errors, matching the
			// SAFE-variant contract in setupNormalFuncMap.
			return nil, nil
		}
		return nil, err
	}
	return EncodeValue(ret)
}
