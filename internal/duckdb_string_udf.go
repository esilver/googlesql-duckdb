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
	// DuckDBResultEnvelope declares VARCHAR like the default, but forces
	// EVERY result - including the INT64 / FLOAT64 / BOOL primitives that
	// EncodeValue would hand back as bare Go scalars - into the base64
	// envelope string. It exists for polymorphic functions such as
	// EVAL_JAVASCRIPT whose return type is chosen per call site, so a
	// single registration cannot commit to a native BIGINT/DOUBLE/BOOLEAN
	// result column. The row decoder (DecodeValue + CastValue) re-types
	// the envelope against the statement's output column.
	DuckDBResultEnvelope
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
	"search":             DuckDBResultBoolean,
	"like":               DuckDBResultBoolean,
	"is_true":            DuckDBResultBoolean,
	"is_false":           DuckDBResultBoolean,
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
	// HLL_COUNT.EXTRACT returns the sketch's cardinality as an INT64.
	"hll_count_extract": DuckDBResultBigint,
	"mysql_length":      DuckDBResultBigint,
	"mysql_char_length": DuckDBResultBigint,
	"mysql_abs":         DuckDBResultDouble,
	"mysql_floor":       DuckDBResultDouble,
	"mysql_ceil":        DuckDBResultDouble,
	"mysql_ceiling":     DuckDBResultDouble,
	"mysql_round":       DuckDBResultDouble,

	"mysql_unix_timestamp": DuckDBResultBigint,
	"mysql_day":            DuckDBResultBigint,
	"mysql_month":          DuckDBResultBigint,
	"mysql_year":           DuckDBResultBigint,
	"mysql_hour":           DuckDBResultBigint,
	"mysql_minute":         DuckDBResultBigint,
	"mysql_second":         DuckDBResultBigint,
	"mysql_microsecond":    DuckDBResultBigint,
	"mysql_quarter":        DuckDBResultBigint,
	"mysql_dayofweek":      DuckDBResultBigint,
	"mysql_dayofyear":      DuckDBResultBigint,
	"mysql_dayofmonth":     DuckDBResultBigint,
	"mysql_week":           DuckDBResultBigint,
	"mysql_weekofyear":     DuckDBResultBigint,
	"mysql_weekday":        DuckDBResultBigint,
	"mysql_bit_length":     DuckDBResultBigint,
	"mysql_position":       DuckDBResultBigint,
	"mysql_degrees":        DuckDBResultDouble,
	"mysql_radians":        DuckDBResultDouble,
	"mysql_log2":           DuckDBResultDouble,
	"mysql_truncate":       DuckDBResultDouble,
	"mysql_locate":         DuckDBResultBigint,
	"mysql_ord":            DuckDBResultBigint,
	"mysql_regexp_like":    DuckDBResultBoolean,
	"mysql_strcmp":         DuckDBResultBigint,
	"mysql_to_days":        DuckDBResultBigint,
	"mysql_to_seconds":     DuckDBResultBigint,
	"mysql_period_add":     DuckDBResultBigint,
	"mysql_period_diff":    DuckDBResultBigint,
	"mysql_datediff":       DuckDBResultBigint,
	"mysql_inet_aton":      DuckDBResultBigint,
	"mysql_is_ipv4":        DuckDBResultBoolean,
	"mysql_is_ipv6":        DuckDBResultBoolean,
	"mysql_is_ipv4_compat": DuckDBResultBoolean,
	"mysql_is_ipv4_mapped": DuckDBResultBoolean,
	"mysql_is_uuid":        DuckDBResultBoolean,

	"search_substring":            DuckDBResultBoolean,
	"search_ngrams":               DuckDBResultBoolean,
	"score":                       DuckDBResultDouble,
	"score_ngrams":                DuckDBResultDouble,
	"bit_reverse":                 DuckDBResultBigint,
	"dot_product":                 DuckDBResultDouble,
	"approx_dot_product":          DuckDBResultDouble,
	"approx_euclidean_distance":   DuckDBResultDouble,
	"approx_cosine_distance":      DuckDBResultDouble,
	"get_next_sequence_value":     DuckDBResultBigint,
	"get_internal_sequence_state": DuckDBResultBigint,
	"ai_score":                    DuckDBResultDouble,
	// EVAL_JAVASCRIPT's return type is declared per CREATE FUNCTION, so
	// the bridge must envelope every result (see DuckDBResultEnvelope).
	"eval_javascript": DuckDBResultEnvelope,
}

var duckDBVolatileUDFNames = map[string]struct{}{
	"generate_uuid": {},
	// NEW_UUID is GoogleSQL's V1.4 alias for GENERATE_UUID: a random
	// UUID per call, so it must stay volatile (never constant-folded).
	"new_uuid":                    {},
	"keys_new_keyset":             {},
	"keys_add_key_from_raw_bytes": {},
	"keys_rotate_keyset":          {},
	"keys_new_wrapped_keyset":     {},
	"aead_encrypt":                {},
	"mysql_curdate":               {},
	"mysql_current_date":          {},
	"mysql_now":                   {},
	"mysql_current_timestamp":     {},
	"mysql_sysdate":               {},
	"mysql_utc_date":              {},
	"mysql_utc_timestamp":         {},
	"mysql_localtime":             {},
	"mysql_localtimestamp":        {},
	"pending_commit_timestamp":    {},
	"get_next_sequence_value":     {},
	// EVAL_JAVASCRIPT executes arbitrary user JS (CREATE TEMP FUNCTION
	// ... LANGUAGE js), which can be non-deterministic (Math.random(),
	// Date.now()). Without this entry DuckDB constant-folds the call and
	// every row sees one value.
	"eval_javascript": {},
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

var spannerDuckDBUDFNames = []string{
	"mysql_length",
	"mysql_char_length",
	"mysql_lower",
	"mysql_upper",
	"mysql_trim",
	"mysql_ltrim",
	"mysql_rtrim",
	"mysql_concat",
	"mysql_reverse",
	"mysql_md5",
	"mysql_sha1",
	"mysql_abs",
	"mysql_floor",
	"mysql_ceil",
	"mysql_ceiling",
	"mysql_round",
	"mysql_coalesce",
	"mysql_curdate",
	"mysql_current_date",
	"mysql_now",
	"mysql_current_timestamp",
	"mysql_unix_timestamp",
	"mysql_day",
	"mysql_month",
	"mysql_year",
	"mysql_hour",
	"mysql_minute",
	"mysql_second",
	"mysql_microsecond",
	"mysql_quarter",
	"mysql_dayofweek",
	"mysql_dayofyear",
	"mysql_dayofmonth",
	"mysql_week",
	"mysql_weekofyear",
	"mysql_weekday",
	"mysql_bit_length",
	"mysql_hex",
	"mysql_space",
	"mysql_position",
	"mysql_degrees",
	"mysql_radians",
	"mysql_log2",
	"mysql_truncate",
	"mysql_char",
	"mysql_concat_ws",
	"mysql_insert",
	"mysql_locate",
	"mysql_mid",
	"mysql_oct",
	"mysql_ord",
	"mysql_quote",
	"mysql_regexp_like",
	"mysql_strcmp",
	"mysql_substring_index",
	"mysql_unhex",
	"mysql_dayname",
	"mysql_monthname",
	"mysql_from_days",
	"mysql_to_days",
	"mysql_to_seconds",
	"mysql_makedate",
	"mysql_from_unixtime",
	"mysql_sysdate",
	"mysql_utc_date",
	"mysql_utc_timestamp",
	"mysql_period_add",
	"mysql_period_diff",
	"mysql_date_format",
	"mysql_str_to_date",
	"mysql_datediff",
	"mysql_localtime",
	"mysql_localtimestamp",
	"mysql_inet_aton",
	"mysql_inet_ntoa",
	"mysql_inet6_aton",
	"mysql_inet6_ntoa",
	"mysql_is_ipv4",
	"mysql_is_ipv6",
	"mysql_is_ipv4_compat",
	"mysql_is_ipv4_mapped",
	"mysql_is_uuid",
	"mysql_bin_to_uuid",
	"mysql_uuid_to_bin",
	"mysql_sha2",
	"mysql_json_quote",
	"mysql_json_unquote",
	"tokenize_fulltext",
	"tokenize_substring",
	"tokenize_ngrams",
	"tokenize_number",
	"tokenize_bool",
	"tokenize_json",
	"token",
	"tokenlist_concat",
	"search_substring",
	"search_ngrams",
	"score",
	"score_ngrams",
	"snippet",
	"debug_tokenlist",
	"pending_commit_timestamp",
	"bit_reverse",
	"dot_product",
	"approx_dot_product",
	"approx_euclidean_distance",
	"approx_cosine_distance",
	"zstd_compress",
	"zstd_decompress_to_bytes",
	"zstd_decompress_to_string",
	"get_next_sequence_value",
	"get_internal_sequence_state",
	"mysql_lcase",
	"mysql_ucase",
	"mysql_adddate",
	"mysql_subdate",
	"ai_if",
	"ai_classify",
	"ai_score",
	"ml_predict",
}

// duckDBNormalUDFNames is the broader normal-function surface registered on
// DuckDB through the generic value-layer scalar bridge. It includes the string
// family plus BigQuery scalars whose bodies already live in normalFuncs.
var duckDBNormalUDFNames = append(append(append([]string{}, stringFamilyUDFNames...), spannerDuckDBUDFNames...),
	"contains_substr",
	"like",
	"make_array",
	"subscript",
	"session_user",
	"generate_uuid",
	"new_uuid",
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
	// EXTRACT(DATE FROM TIMESTAMP/DATETIME) can lower to the legacy normal
	// helper name in paths that cannot use a DuckDB-native date expression.
	"extract_date",
	// HLL_COUNT.EXTRACT(sketch) reads a serialized HyperLogLog++ sketch and
	// returns its INT64 cardinality. The analyzer lowers it to the bare leaf
	// `extract`, which collides with SQL's EXTRACT keyword, so the formatter
	// remaps that leaf to googlesqlite_hll_count_extract (duckDBExtractFuncRemap)
	// and the body is the same hll.BindHllCountExtract registered in normalFuncs.
	"hll_count_extract",
	// SEARCH and the ObjectRef (OBJ.*) family: pure-Go value-layer bodies
	// (textfn.BindSearch, extras.BindObj*). SEARCH returns a native BOOL;
	// the OBJ.* constructors/accessors return richer values (ObjectRef JSON,
	// URLs) that stay on the envelope VARCHAR and re-type at decode.
	"search",
	"obj_make_ref",
	"obj_fetch_metadata",
	"obj_get_access_url",
	"obj_get_read_url",
	// JSON field access (`doc.field`): the analyzer resolves it to
	// ResolvedGetJsonField, which GetJsonFieldNode lowers to
	// googlesqlite_get_json_field(expr, 'fieldName'). The body
	// (jsonfn.BindJsonField) was always in normalFuncs but the name was
	// missing from this DuckDB registration list, so the first stored-
	// JSON-column field access raised "Scalar Function with name
	// googlesqlite_get_json_field does not exist" (unmasked once JSON
	// INSERTs started succeeding). Result is a JSON value (envelope
	// VARCHAR) re-typed by the consumer (INT64()/JSON_VALUE()/decoder).
	"get_json_field",
	// NUMERIC / BIGNUMERIC division (issue #21): the formatter's
	// tryNativeOperator deliberately refuses to claim $divide natively when
	// either operand is NUMERIC or BIGNUMERIC (BigQuery `/` over NUMERIC is
	// EXACT and errors on a zero divisor, while the native float lowering
	// computes in DOUBLE and yields Inf), so the default emit produces
	// googlesqlite_divide / googlesqlite_safe_divide — names the legacy
	// SQLite backend registered via normalFuncs (mathfn.OP_DIV) but the
	// DuckDB bridge set was missing, failing every NUMERIC division at bind
	// time with a Catalog Error. The body is the same exact-arithmetic
	// implementation: operands arrive as exact carriers (a native
	// DECIMAL(38,9) becomes a NumericValue via the duckdb.Decimal
	// foreign-decoder hook; a BIGNUMERIC envelope decodes through the
	// value layer), NumericValue.Div computes the exact rational and
	// raises on a zero divisor, and the result re-encodes as the
	// NUMERIC / BIGNUMERIC envelope VARCHAR. The googlesqlite_safe_divide
	// companion (registered automatically by registerNormalFuncUDFs) folds
	// the zero-divisor error to NULL for SAFE_DIVIDE and the IFERROR /
	// ISERROR / NULLIFERROR safe-eval wraps.
	"divide",
	// Aggregate option marker helpers emitted by aggregate/window formatting
	// when DISTINCT / IGNORE NULLS / LIMIT / ORDER BY must be represented as
	// value-layer options.
	"distinct",
	"ignore_nulls",
	"limit",
	"order_by",
	// Boolean predicate helpers emitted for IS TRUE / IS FALSE when the
	// operator cannot be lowered natively.
	"is_true",
	"is_false",
	// JavaScript UDF evaluator (goja). FunctionSpec bodies for
	// `LANGUAGE js` CREATE FUNCTIONs are hand-built calls to
	// googlesqlite_eval_javascript(code, retType[, argNames, args...]).
	// The pure-Go body has always lived in normalFuncs; the name was
	// simply missing from this DuckDB registration list, so every JS UDF
	// invocation failed at bind time with a Catalog Error.
	"eval_javascript",
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

// CallNormalFuncEnvelope is CallNormalFunc for functions registered with
// DuckDBResultEnvelope: the result is always re-encoded as the base64
// envelope string (even INT64 / FLOAT64 / BOOL primitives) so the declared
// VARCHAR result type holds for every per-call return type.
func CallNormalFuncEnvelope(name string, safe bool, args ...any) (any, error) {
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
			return nil, nil
		}
		return nil, err
	}
	encoded, isNull, err := value.EncodeValueString(ret)
	if err != nil {
		if safe {
			return nil, nil
		}
		return nil, err
	}
	if isNull {
		return nil, nil
	}
	return encoded, nil
}
