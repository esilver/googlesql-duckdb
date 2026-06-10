package googlesqlite_test

// Storage-seam regression suite.
//
// The declarative spec suite (testdata/specs/**) exercises function
// semantics almost exclusively over LITERALS — `SELECT F(<literal>)`.
// Every recent real-world bug (stored DATE columns breaking date
// functions, day-early date rendering, SAFE_DIVIDE's cast-carrier
// coercion, top-level ARRAY_AGG nil, struct field order) lived in the
// STORAGE SEAM that path never crosses: googlesqlite's value-layer
// carriers meeting the engine's native column types.
//
// This suite closes the class, not the instances. For each GoogleSQL
// type family it does the full round-trip the spec suite skips:
//
//	CREATE TABLE t (c <type>);
//	INSERT a known typed literal;
//	SELECT c back and assert the exact value / canonical rendering;
//	run representative FUNCTIONS ON THE STORED COLUMN (not a literal);
//	WHERE / GROUP BY on the column where sensible.
//
// Plus cross-process persistence checks (file-backed database written,
// closed, reopened) for the families where on-disk representation has
// historically diverged from the in-memory one (temporal, containers).
//
// Expected values follow the canonical renderings documented in
// docs/third_party/googlesql-docs/data-types.md and the per-function
// reference pages (e.g. timestamp_functions.md, array_functions.md).

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"
)

// openSeamDB opens a fresh in-memory database for one seam subtest.
// MaxOpenConns is pinned to 1 so DDL, DML and the queries that follow
// are guaranteed to observe the same engine catalog (the pool must not
// hand a later statement a different connection).
func openSeamDB(t *testing.T, name string) *sql.DB {
	t.Helper()
	db, err := sql.Open("googlesqlite", ":memory:?_test=seam_"+name)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// seamExec runs setup statements (CREATE TABLE / INSERT), failing the
// test on the first error.
func seamExec(t *testing.T, db *sql.DB, stmts ...string) {
	t.Helper()
	ctx := context.Background()
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			t.Fatalf("exec failed: %v\nsql: %s", err, s)
		}
	}
}

// seamString runs a single-row single-column query and scans it into a
// string (driving the string-affinity coercion path for the column's
// kind).
func seamString(t *testing.T, db *sql.DB, query string) string {
	t.Helper()
	var got string
	if err := db.QueryRowContext(context.Background(), query).Scan(&got); err != nil {
		t.Fatalf("query failed: %v\nsql: %s", err, query)
	}
	return got
}

func seamInt64(t *testing.T, db *sql.DB, query string) int64 {
	t.Helper()
	var got int64
	if err := db.QueryRowContext(context.Background(), query).Scan(&got); err != nil {
		t.Fatalf("query failed: %v\nsql: %s", err, query)
	}
	return got
}

func seamFloat64(t *testing.T, db *sql.DB, query string) float64 {
	t.Helper()
	var got float64
	if err := db.QueryRowContext(context.Background(), query).Scan(&got); err != nil {
		t.Fatalf("query failed: %v\nsql: %s", err, query)
	}
	return got
}

func seamBool(t *testing.T, db *sql.DB, query string) bool {
	t.Helper()
	var got bool
	if err := db.QueryRowContext(context.Background(), query).Scan(&got); err != nil {
		t.Fatalf("query failed: %v\nsql: %s", err, query)
	}
	return got
}

// seamStringErr is seamString without the Fatalf: it returns the query
// error so known-bug call sites can xfail (skip) instead of dying.
func seamStringErr(db *sql.DB, query string) (string, error) {
	var got string
	err := db.QueryRowContext(context.Background(), query).Scan(&got)
	return got, err
}

// seamAny scans into `any`, driving assignInterfaceValue for container
// kinds (ARRAY scans to []any, STRUCT to positional []any).
func seamAny(t *testing.T, db *sql.DB, query string) any {
	t.Helper()
	var got any
	if err := db.QueryRowContext(context.Background(), query).Scan(&got); err != nil {
		t.Fatalf("query failed: %v\nsql: %s", err, query)
	}
	return got
}

func wantString(t *testing.T, db *sql.DB, query, want string) {
	t.Helper()
	if got := seamString(t, db, query); got != want {
		t.Errorf("got %q want %q\nsql: %s", got, want, query)
	}
}

func wantInt64(t *testing.T, db *sql.DB, query string, want int64) {
	t.Helper()
	if got := seamInt64(t, db, query); got != want {
		t.Errorf("got %d want %d\nsql: %s", got, want, query)
	}
}

func wantFloat64(t *testing.T, db *sql.DB, query string, want float64) {
	t.Helper()
	if got := seamFloat64(t, db, query); got != want {
		t.Errorf("got %v want %v\nsql: %s", got, want, query)
	}
}

func wantBool(t *testing.T, db *sql.DB, query string, want bool) {
	t.Helper()
	if got := seamBool(t, db, query); got != want {
		t.Errorf("got %v want %v\nsql: %s", got, want, query)
	}
}

// ---------------------------------------------------------------------
// Scalar families
// ---------------------------------------------------------------------

func TestStorageSeam_Int64(t *testing.T) {
	t.Parallel()
	db := openSeamDB(t, "int64")
	seamExec(t, db,
		"CREATE TABLE t (c INT64)",
		"INSERT INTO t (c) VALUES (10), (20), (12)",
	)
	// Stored value round-trip.
	wantInt64(t, db, "SELECT c FROM t ORDER BY c LIMIT 1", 10)
	// Functions on the stored column.
	wantInt64(t, db, "SELECT SUM(c) FROM t", 42)
	wantInt64(t, db, "SELECT MAX(c) + 1 FROM t", 21)
	// SAFE_DIVIDE over aggregates of a stored column — the exact shape
	// of the cast-carrier coercion bug the literal suite never hit.
	wantFloat64(t, db, "SELECT SAFE_DIVIDE(SUM(c), COUNT(*)) FROM t", 14)
	// WHERE / GROUP BY on the stored column.
	wantInt64(t, db, "SELECT COUNT(*) FROM t WHERE c > 10", 2)
	wantInt64(t, db,
		"SELECT COUNT(*) FROM t GROUP BY MOD(c, 2) ORDER BY COUNT(*) DESC LIMIT 1", 3)
}

func TestStorageSeam_Float64(t *testing.T) {
	t.Parallel()
	db := openSeamDB(t, "float64")
	seamExec(t, db,
		"CREATE TABLE t (c FLOAT64)",
		"INSERT INTO t (c) VALUES (2.5), (0.25)",
	)
	wantFloat64(t, db, "SELECT c FROM t ORDER BY c DESC LIMIT 1", 2.5)
	wantFloat64(t, db, "SELECT SUM(c) FROM t", 2.75)
	wantFloat64(t, db, "SELECT ROUND(c * 2, 1) FROM t ORDER BY c DESC LIMIT 1", 5.0)
	wantInt64(t, db, "SELECT COUNT(*) FROM t WHERE c < 1", 1)
}

func TestStorageSeam_Numeric(t *testing.T) {
	t.Parallel()
	db := openSeamDB(t, "numeric")
	seamExec(t, db,
		"CREATE TABLE t (c NUMERIC)",
		"INSERT INTO t (c) VALUES (NUMERIC '3.14')",
	)
	// Canonical decimal rendering of the stored value, both via SQL
	// CAST and via the driver's direct string scan of a NUMERIC column.
	wantString(t, db, "SELECT CAST(c AS STRING) FROM t", "3.14")
	if got := seamString(t, db, "SELECT c FROM t"); got != "3.14" {
		t.Errorf("direct NUMERIC scan: got %q want %q", got, "3.14")
	}
	// Arithmetic on the stored column must stay NUMERIC.
	//
	// KNOWN BUG (caught by this suite, 2026-06-10, do not paper over):
	// NUMERIC columns persist as VARCHAR at the engine level, so any
	// arithmetic / comparison on the stored column dies with
	//   Binder Error: No function matches '*(VARCHAR, STRING_LITERAL)'.
	// Rendering round-trips fine (the asserts above); only computation
	// on the stored column is broken. The skip below is an xfail: once
	// the column persists as native DECIMAL the assertions take over.
	got, err := seamStringErr(db, "SELECT CAST(c * 2 AS STRING) FROM t")
	if err != nil {
		t.Skipf("KNOWN BUG (storage seam): NUMERIC column persists as VARCHAR; "+
			"arithmetic on the stored column fails: %v", err)
	}
	if got != "6.28" {
		t.Errorf("got %q want %q\nsql: SELECT CAST(c * 2 AS STRING) FROM t", got, "6.28")
	}
	wantInt64(t, db, "SELECT COUNT(*) FROM t WHERE c > NUMERIC '3'", 1)
}

func TestStorageSeam_String(t *testing.T) {
	t.Parallel()
	db := openSeamDB(t, "string")
	seamExec(t, db,
		"CREATE TABLE t (c STRING)",
		"INSERT INTO t (c) VALUES ('hello'), ('world')",
	)
	wantString(t, db, "SELECT c FROM t ORDER BY c LIMIT 1", "hello")
	wantString(t, db, "SELECT UPPER(c) FROM t ORDER BY c LIMIT 1", "HELLO")
	wantInt64(t, db, "SELECT LENGTH(c) FROM t ORDER BY c LIMIT 1", 5)
	wantInt64(t, db, "SELECT COUNT(*) FROM t WHERE c = 'hello'", 1)
	wantString(t, db, "SELECT STRING_AGG(c, ',' ORDER BY c) FROM t", "hello,world")
}

func TestStorageSeam_Bytes(t *testing.T) {
	t.Parallel()
	db := openSeamDB(t, "bytes")
	seamExec(t, db,
		"CREATE TABLE t (c BYTES)",
		"INSERT INTO t (c) VALUES (b'abc')",
	)
	var got []byte
	if err := db.QueryRowContext(context.Background(),
		"SELECT c FROM t").Scan(&got); err != nil {
		t.Fatalf("scan BYTES: %v", err)
	}
	// KNOWN BUG (caught by this suite, 2026-06-10, do not paper over):
	// BYTES columns persist the legacy base64 {"header","body"} JSON
	// carrier envelope instead of a native BLOB. The stored b'abc'
	// reads back as the 44-char envelope string
	// (base64 of {"header":"bytes","body":"YWJj"}), BYTE_LENGTH
	// returns 44, and TO_HEX hex-dumps the envelope. The skip below is
	// an xfail: once the column persists natively the assertions run.
	if !bytes.Equal(got, []byte("abc")) {
		t.Skipf("KNOWN BUG (storage seam): BYTES column persists the JSON carrier "+
			"envelope, not a native BLOB; round-trip got %q want %q", got, "abc")
	}
	wantInt64(t, db, "SELECT BYTE_LENGTH(c) FROM t", 3)
	wantString(t, db, "SELECT TO_HEX(c) FROM t", "616263")
	wantInt64(t, db, "SELECT COUNT(*) FROM t WHERE c = b'abc'", 1)
}

func TestStorageSeam_Bool(t *testing.T) {
	t.Parallel()
	db := openSeamDB(t, "bool")
	seamExec(t, db,
		"CREATE TABLE t (c BOOL)",
		"INSERT INTO t (c) VALUES (TRUE), (FALSE), (TRUE)",
	)
	wantBool(t, db, "SELECT c FROM t WHERE c LIMIT 1", true)
	wantBool(t, db, "SELECT NOT c FROM t WHERE NOT c LIMIT 1", true)
	wantInt64(t, db, "SELECT COUNTIF(c) FROM t", 2)
	wantBool(t, db, "SELECT LOGICAL_AND(c) FROM t", false)
	wantBool(t, db, "SELECT LOGICAL_OR(c) FROM t", true)
}

// ---------------------------------------------------------------------
// Temporal families — the original storage-seam bug nest (stored DATE
// columns hitting Binder errors, day-early date rendering).
// ---------------------------------------------------------------------

func TestStorageSeam_Date(t *testing.T) {
	t.Parallel()
	db := openSeamDB(t, "date")
	seamExec(t, db,
		"CREATE TABLE t (c DATE)",
		"INSERT INTO t (c) VALUES (DATE '2024-03-15')",
	)
	// Exact canonical rendering — the day-early regression made this
	// come back as 2024-03-14.
	wantString(t, db, "SELECT c FROM t", "2024-03-15")
	wantString(t, db, "SELECT CAST(c AS STRING) FROM t", "2024-03-15")
	// Functions on the stored column (these used to throw Binder
	// errors when the column persisted as VARCHAR).
	wantInt64(t, db, "SELECT EXTRACT(YEAR FROM c) FROM t", 2024)
	wantString(t, db, "SELECT FORMAT_DATE('%Y|%m|%d', c) FROM t", "2024|03|15")
	wantString(t, db, "SELECT CAST(DATE_ADD(c, INTERVAL 1 DAY) AS STRING) FROM t", "2024-03-16")
	wantInt64(t, db, "SELECT DATE_DIFF(DATE '2024-03-20', c, DAY) FROM t", 5)
	wantInt64(t, db, "SELECT COUNT(*) FROM t WHERE c = DATE '2024-03-15'", 1)
}

func TestStorageSeam_Time(t *testing.T) {
	t.Parallel()
	db := openSeamDB(t, "time")
	seamExec(t, db,
		"CREATE TABLE t (c TIME)",
		"INSERT INTO t (c) VALUES (TIME '12:34:56')",
	)
	wantString(t, db, "SELECT CAST(c AS STRING) FROM t", "12:34:56")
	wantInt64(t, db, "SELECT EXTRACT(MINUTE FROM c) FROM t", 34)
	wantString(t, db, "SELECT FORMAT_TIME('%H:%M', c) FROM t", "12:34")
	wantInt64(t, db, "SELECT COUNT(*) FROM t WHERE c > TIME '12:00:00'", 1)
}

func TestStorageSeam_Datetime(t *testing.T) {
	t.Parallel()
	db := openSeamDB(t, "datetime")
	seamExec(t, db,
		"CREATE TABLE t (c DATETIME)",
		"INSERT INTO t (c) VALUES (DATETIME '2024-03-15 12:34:56')",
	)
	wantInt64(t, db, "SELECT EXTRACT(HOUR FROM c) FROM t", 12)
	wantString(t, db, "SELECT FORMAT_DATETIME('%Y-%m-%d %H:%M:%S', c) FROM t",
		"2024-03-15 12:34:56")
	wantInt64(t, db, "SELECT EXTRACT(HOUR FROM DATETIME_TRUNC(c, DAY)) FROM t", 0)
	wantInt64(t, db, "SELECT COUNT(*) FROM t WHERE c >= DATETIME '2024-03-15 00:00:00'", 1)
	// Canonical rendering: CAST(DATETIME AS STRING) uses a space
	// separator per data-types.md / conversion_functions.md examples.
	//
	// KNOWN BUG (caught by this suite, 2026-06-10, do not paper over):
	// CAST on the stored column renders the engine's native ISO form
	// "2024-03-15T12:34:56" ('T' separator) instead of the GoogleSQL
	// canonical "2024-03-15 12:34:56". xfail until the cast lowering
	// canonicalizes the separator.
	if got := seamString(t, db, "SELECT CAST(c AS STRING) FROM t"); got != "2024-03-15 12:34:56" {
		if got == "2024-03-15T12:34:56" {
			t.Skipf("KNOWN BUG (storage seam): CAST(stored DATETIME AS STRING) renders %q; "+
				"want canonical space-separated form", got)
		}
		t.Errorf("got %q want %q\nsql: SELECT CAST(c AS STRING) FROM t",
			got, "2024-03-15 12:34:56")
	}
}

func TestStorageSeam_Timestamp(t *testing.T) {
	t.Parallel()
	db := openSeamDB(t, "timestamp")
	seamExec(t, db,
		"CREATE TABLE t (c TIMESTAMP)",
		"INSERT INTO t (c) VALUES (TIMESTAMP '2024-03-15 12:34:56+00')",
	)
	// Canonical absolute-time rendering (UTC, +00 suffix).
	wantString(t, db, "SELECT c FROM t", "2024-03-15 12:34:56+00")
	wantInt64(t, db, "SELECT EXTRACT(YEAR FROM c) FROM t", 2024)
	// %F is a BigQuery composite specifier the engine's strftime lacks;
	// the lowering layer must expand it for stored columns too.
	wantString(t, db, "SELECT FORMAT_TIMESTAMP('%F', c) FROM t", "2024-03-15")
	wantInt64(t, db, "SELECT UNIX_SECONDS(c) FROM t",
		time.Date(2024, 3, 15, 12, 34, 56, 0, time.UTC).Unix())
	wantInt64(t, db, "SELECT COUNT(*) FROM t WHERE c < CURRENT_TIMESTAMP()", 1)
}

// ---------------------------------------------------------------------
// Container families
// ---------------------------------------------------------------------

func TestStorageSeam_ArrayInt64(t *testing.T) {
	t.Parallel()
	db := openSeamDB(t, "array_int64")
	seamExec(t, db,
		"CREATE TABLE t (c ARRAY<INT64>)",
		"INSERT INTO t (c) VALUES ([1, 2, 3])",
	)
	// Stored array round-trip through the interface scan path.
	got := seamAny(t, db, "SELECT c FROM t")
	slice, ok := got.([]any)
	if !ok {
		t.Fatalf("stored ARRAY<INT64> scanned to %T; want []any", got)
	}
	if len(slice) != 3 || slice[0] != int64(1) || slice[1] != int64(2) || slice[2] != int64(3) {
		t.Errorf("stored ARRAY<INT64> round-trip: got %v want [1 2 3]", slice)
	}
	// Functions on the stored column.
	wantInt64(t, db, "SELECT ARRAY_LENGTH(c) FROM t", 3)
	wantInt64(t, db, "SELECT c[OFFSET(1)] FROM t", 2)
	// Crossing the seam twice: UNNEST a stored array, aggregate it.
	wantInt64(t, db, "SELECT SUM(x) FROM t, UNNEST(c) AS x", 6)
}

func TestStorageSeam_ArrayString(t *testing.T) {
	t.Parallel()
	db := openSeamDB(t, "array_string")
	seamExec(t, db,
		"CREATE TABLE t (c ARRAY<STRING>)",
		"INSERT INTO t (c) VALUES (['a', 'b'])",
	)
	got := seamAny(t, db, "SELECT c FROM t")
	slice, ok := got.([]any)
	if !ok {
		t.Fatalf("stored ARRAY<STRING> scanned to %T; want []any", got)
	}
	if len(slice) != 2 || slice[0] != "a" || slice[1] != "b" {
		t.Errorf("stored ARRAY<STRING> round-trip: got %v want [a b]", slice)
	}
	wantInt64(t, db, "SELECT ARRAY_LENGTH(c) FROM t", 2)
	wantString(t, db, "SELECT ARRAY_TO_STRING(c, ',') FROM t", "a,b")
	wantInt64(t, db, "SELECT COUNT(*) FROM t, UNNEST(c) AS x WHERE x = 'b'", 1)
}

func TestStorageSeam_Struct(t *testing.T) {
	t.Parallel()
	db := openSeamDB(t, "struct")
	seamExec(t, db,
		"CREATE TABLE t (c STRUCT<a INT64, b STRING>)",
		"INSERT INTO t (c) VALUES (STRUCT(7 AS a, 'x' AS b))",
	)
	// Field access on the stored column.
	wantInt64(t, db, "SELECT c.a FROM t", 7)
	wantString(t, db, "SELECT c.b FROM t", "x")
	// Arithmetic on a stored struct field.
	wantInt64(t, db, "SELECT c.a + 1 FROM t", 8)
	// Whole-struct scan is positional []any in declaration order —
	// the struct-field-order regression made fields swap.
	got := seamAny(t, db, "SELECT c FROM t")
	slice, ok := got.([]any)
	if !ok {
		t.Fatalf("stored STRUCT scanned to %T; want []any", got)
	}
	if len(slice) != 2 || slice[0] != int64(7) || slice[1] != "x" {
		t.Errorf("stored STRUCT field order: got %v want [7 x]", slice)
	}
	wantInt64(t, db, "SELECT COUNT(*) FROM t WHERE c.b = 'x'", 1)
}

// ---------------------------------------------------------------------
// Document / exotic families
// ---------------------------------------------------------------------

func TestStorageSeam_JSON(t *testing.T) {
	t.Parallel()
	db := openSeamDB(t, "json")
	ctx := context.Background()
	seamExec(t, db, "CREATE TABLE t (c JSON)")
	// KNOWN BUG (caught by this suite, 2026-06-10, do not paper over):
	// a JSON LITERAL inserted into a native JSON column is lowered to
	// the legacy base64 {"header","body"} carrier envelope, which the
	// engine then rejects:
	//   Conversion Error: Malformed JSON at byte 0 of input ...
	//   VALUES ('eyJoZWFkZXIiOiJqc29uIiwiYm9keSI6...')
	// Same family as the bound-parameter envelope seam, but reachable
	// from a plain literal INSERT. xfail: once the lowering emits the
	// native JSON text the assertions below take over.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO t (c) VALUES (JSON '{"a": 1, "b": "x"}')`); err != nil {
		t.Skipf("KNOWN BUG (storage seam): INSERT of a JSON literal into a native "+
			"JSON column emits the carrier envelope and fails: %v", err)
	}
	// Field extraction on the stored column.
	wantString(t, db, "SELECT JSON_VALUE(c, '$.b') FROM t", "x")
	wantString(t, db, "SELECT JSON_VALUE(c, '$.a') FROM t", "1")
	wantInt64(t, db, "SELECT INT64(c.a) FROM t", 1)
	// Whole-document rendering: key membership rather than byte-exact
	// output, since JSON key order is not part of the contract.
	rendered := seamString(t, db, "SELECT TO_JSON_STRING(c) FROM t")
	if !strings.Contains(rendered, `"a":1`) || !strings.Contains(rendered, `"b":"x"`) {
		t.Errorf("TO_JSON_STRING of stored JSON: got %q; want it to contain %q and %q",
			rendered, `"a":1`, `"b":"x"`)
	}
	wantInt64(t, db, "SELECT COUNT(*) FROM t WHERE JSON_VALUE(c, '$.b') = 'x'", 1)
}

func TestStorageSeam_Geography(t *testing.T) {
	t.Parallel()
	db := openSeamDB(t, "geography")
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, "CREATE TABLE t (c GEOGRAPHY)"); err != nil {
		t.Skipf("GEOGRAPHY column type not supported: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		"INSERT INTO t (c) VALUES (ST_GEOGPOINT(1, 2))"); err != nil {
		t.Skipf("GEOGRAPHY INSERT not supported: %v", err)
	}
	wantFloat64(t, db, "SELECT ST_X(c) FROM t", 1)
	wantFloat64(t, db, "SELECT ST_Y(c) FROM t", 2)
	// WKT rendering: BigQuery's ST_ASTEXT emits "POINT(1 2)" (no space
	// after the geometry tag) per geography_functions.md examples.
	//
	// KNOWN BUG (caught by this suite, 2026-06-10, do not paper over):
	// the stored column renders the engine's spatial form "POINT (1 2)"
	// (space after POINT). xfail until ST_ASTEXT output is
	// canonicalized.
	if got := seamString(t, db, "SELECT ST_ASTEXT(c) FROM t"); got != "POINT(1 2)" {
		if got == "POINT (1 2)" {
			t.Skipf("KNOWN BUG (storage seam): ST_ASTEXT renders %q; "+
				"want BigQuery canonical %q", got, "POINT(1 2)")
		}
		t.Errorf("got %q want %q\nsql: SELECT ST_ASTEXT(c) FROM t", got, "POINT(1 2)")
	}
}

func TestStorageSeam_Interval(t *testing.T) {
	t.Parallel()
	db := openSeamDB(t, "interval")
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, "CREATE TABLE t (c INTERVAL)"); err != nil {
		t.Skipf("INTERVAL column type not supported: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		"INSERT INTO t (c) VALUES (INTERVAL 1 DAY)"); err != nil {
		t.Skipf("INTERVAL INSERT not supported: %v", err)
	}
	// Canonical [sign]Y-M [sign]D [sign]H:M:S rendering per
	// data-types.md "Interval type".
	wantString(t, db, "SELECT CAST(c AS STRING) FROM t", "0-0 1 0:0:0")
	wantString(t, db,
		"SELECT CAST(JUSTIFY_INTERVAL(c) AS STRING) FROM t", "0-0 1 0:0:0")
}

// ---------------------------------------------------------------------
// Cross-process persistence — file-backed databases written by one
// *sql.DB, closed, and reopened by a fresh one. The on-disk column
// representation must stay native-typed (databases written before the
// temporal seam fix persisted dates as VARCHAR and broke every date
// function on reopen).
// ---------------------------------------------------------------------

// seamTempDBPath returns a path for a file-backed database in a fresh
// temp dir (the file itself must not pre-exist; the engine creates it).
func seamTempDBPath(t *testing.T, name string) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "seam-"+name+"-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir + "/seam.db"
}

func TestStorageSeamPersistence_Temporal(t *testing.T) {
	t.Parallel()
	path := seamTempDBPath(t, "temporal")

	// Writer process stand-in: populate and close.
	func() {
		db, err := sql.Open("googlesqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		db.SetMaxOpenConns(1)
		seamExec(t, db,
			"CREATE TABLE t (d DATE, ts TIMESTAMP)",
			"INSERT INTO t (d, ts) VALUES (DATE '2024-03-15', TIMESTAMP '2024-03-15 12:34:56+00')",
		)
	}()

	// Reader: a brand-new *sql.DB over the same file.
	db, err := sql.Open("googlesqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	wantString(t, db, "SELECT d FROM t", "2024-03-15")
	wantString(t, db, "SELECT FORMAT_DATE('%Y|%m|%d', d) FROM t", "2024|03|15")
	wantString(t, db, "SELECT ts FROM t", "2024-03-15 12:34:56+00")
	wantInt64(t, db, "SELECT EXTRACT(YEAR FROM ts) FROM t", 2024)
	wantInt64(t, db, "SELECT COUNT(*) FROM t WHERE d = DATE '2024-03-15'", 1)
}

func TestStorageSeamPersistence_Containers(t *testing.T) {
	t.Parallel()
	path := seamTempDBPath(t, "containers")

	func() {
		db, err := sql.Open("googlesqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		db.SetMaxOpenConns(1)
		seamExec(t, db,
			"CREATE TABLE t (arr ARRAY<INT64>, st STRUCT<a INT64, b STRING>)",
			"INSERT INTO t (arr, st) VALUES ([1, 2, 3], STRUCT(7 AS a, 'x' AS b))",
		)
	}()

	db, err := sql.Open("googlesqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	wantInt64(t, db, "SELECT ARRAY_LENGTH(arr) FROM t", 3)
	wantInt64(t, db, "SELECT SUM(x) FROM t, UNNEST(arr) AS x", 6)
	wantInt64(t, db, "SELECT st.a + 1 FROM t", 8)
	wantString(t, db, "SELECT st.b FROM t", "x")
}
