package googlesqlite_test

// Issue #6 coverage: the BulkInsert fast path extended to complex column
// types. One storage-parity test per type (twin tables, same Go values
// through the per-row INSERT path and through BulkInsert, RAW stored cells
// compared byte-for-byte), the value-aware NULL gate, the instrumentation
// counter/hook, and the JSON-column throughput benchmark (the issue's 400x
// gap).

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/goccy/googlesqlite"
)

// bulkRawRows runs query directly on the inner engine connection (bypassing
// the dialect layer) and returns the stringified rows.
func bulkRawRows(t testing.TB, ctx context.Context, conn *sql.Conn, query string) [][]string {
	t.Helper()
	var out [][]string
	if err := conn.Raw(func(dc any) error {
		rows, err := dc.(*googlesqlite.Conn).RawEngineQuery(ctx, query)
		out = rows
		return err
	}); err != nil {
		t.Fatalf("raw %s: %v", query, err)
	}
	return out
}

// TestBulkInsertStorageParityComplexTypes is the issue-#6 parity bar: for
// every complex type on the extended fast path, rows written through
// BulkInsert must be BYTE-IDENTICAL IN STORAGE to the same rows written
// through the per-row INSERT path.
func TestBulkInsertStorageParityComplexTypes(t *testing.T) {
	db, err := sql.Open("googlesqlite", ":memory:?_test=bulkinsert_parity")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	cases := []struct {
		name     string
		colType  string
		blob     bool   // raw-compare via hex() (BLOB storage)
		readback string // dialect-path projection over `c` ("" = TO_JSON_STRING(`c`))
		valueSQL string // per-row VALUES expression for `c` ("" = plain ?)
		values   []any  // one per row; nil rows included on purpose
	}{
		{"numeric", "NUMERIC", false, "", "", []any{"123.45", "-0.000000001", "123.450", nil}},
		{"bignumeric", "BIGNUMERIC", false, "", "", []any{"99999999999999999999999999999.123456789", nil}},
		// JSON's per-row reference is PARSE_JSON(?) — the bigquery-emulator
		// binding shape and the issue-#14 parity bar: a bound string is the
		// JSON TEXT (parsed into a JSON value), not a JSON scalar string.
		{"json", "JSON", false, "", "PARSE_JSON(?)", []any{`{"b": 2,  "a": [1, null]}`, `"plain string"`, `[1,2,3]`, nil}},
		// TO_JSON_STRING over TIME is a pre-existing dialect gap (identical
		// for both paths); read back through CAST instead.
		{"time", "TIME", false, "CAST(`c` AS STRING)", "", []any{"12:34:56.789012", "00:00:00", nil}},
		{"bytes", "BYTES", true, "", "", []any{[]byte{0x01, 0x02, 'h', 'i'}, []byte{}, nil}},
		{"arr_i", "ARRAY<INT64>", false, "", "", []any{[]int64{1, 2, 3}, []int64{}, nil}},
		{"arr_s", "ARRAY<STRING>", false, "", "", []any{[]string{"x", "it's"}, nil}},
		{"strct", "STRUCT<a INT64, b STRING>", false, "", "", []any{map[string]any{"a": int64(7), "b": "q"}, map[string]any{"b": "only-b"}, nil}},
		{"strct_nest", "STRUCT<a INT64, s STRUCT<x FLOAT64, y ARRAY<INT64>>>", false, "", "",
			[]any{map[string]any{"a": int64(1), "s": map[string]any{"x": 2.5, "y": []int64{9, 8}}}, nil}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			perRow, bulk := "p_"+tc.name, "b_"+tc.name
			for _, tbl := range []string{perRow, bulk} {
				if _, err := conn.ExecContext(ctx,
					fmt.Sprintf("CREATE TABLE `%s` (`id` INT64, `c` %s)", tbl, tc.colType)); err != nil {
					t.Fatalf("create %s: %v", tbl, err)
				}
			}

			// Per-row path: one parameterized INSERT per row (the exact
			// fallback the emulator-style caller runs today).
			for i, v := range tc.values {
				expr := tc.valueSQL
				if expr == "" || v == nil {
					expr = "?" // NULL binds plainly on both paths
				}
				if _, err := conn.ExecContext(ctx,
					fmt.Sprintf("INSERT `%s` (`id`, `c`) VALUES (?, %s)", perRow, expr), int64(i), v); err != nil {
					t.Fatalf("per-row insert %d (%v): %v", i, v, err)
				}
			}

			// Bulk path: same values, one batch.
			rows := make([][]any, len(tc.values))
			for i, v := range tc.values {
				rows[i] = []any{int64(i), v}
			}
			n, err := googlesqlite.BulkInsert(ctx, conn, []string{bulk}, []string{"id", "c"}, rows)
			if err != nil {
				t.Fatalf("BulkInsert: %v", err)
			}
			if n != int64(len(rows)) {
				t.Fatalf("BulkInsert returned %d, want %d", n, len(rows))
			}

			// RAW storage comparison: column type, cell typeof, cell text
			// (hex for BLOB) must match byte-for-byte.
			probe := `SELECT typeof(c), CAST(c AS VARCHAR) FROM "%s" ORDER BY id`
			if tc.blob {
				probe = `SELECT typeof(c), hex(c) FROM "%s" ORDER BY id`
			}
			want := bulkRawRows(t, ctx, conn, fmt.Sprintf(probe, perRow))
			got := bulkRawRows(t, ctx, conn, fmt.Sprintf(probe, bulk))
			if !reflect.DeepEqual(got, want) {
				t.Errorf("raw storage mismatch:\n per-row: %v\n bulk:    %v", want, got)
			}

			// Read-back correctness through the normal query path.
			proj := tc.readback
			if proj == "" {
				proj = "TO_JSON_STRING(`c`)"
			}
			q := func(tbl string) []string {
				t.Helper()
				rs, err := conn.QueryContext(ctx, "SELECT "+proj+" FROM `"+tbl+"` ORDER BY `id`")
				if err != nil {
					t.Fatalf("readback %s: %v", tbl, err)
				}
				defer rs.Close()
				var out []string
				for rs.Next() {
					var s sql.NullString
					if err := rs.Scan(&s); err != nil {
						t.Fatalf("scan: %v", err)
					}
					out = append(out, fmt.Sprintf("%v|%s", s.Valid, s.String))
				}
				if err := rs.Err(); err != nil {
					t.Fatal(err)
				}
				return out
			}
			if w, g := q(perRow), q(bulk); !reflect.DeepEqual(g, w) {
				t.Errorf("readback mismatch:\n per-row: %v\n bulk:    %v", w, g)
			}
		})
	}
}

// TestBulkInsertValueAwareNullGate: a column whose type stays off the fast
// path (GEOGRAPHY) no longer rejects on type alone — a batch whose values for
// it are all NULL loads bulk; the first non-NULL value falls back with
// ErrBulkInsertUnsupported, counted and reported through the hook.
func TestBulkInsertValueAwareNullGate(t *testing.T) {
	db, err := sql.Open("googlesqlite", ":memory:?_test=bulkinsert_nullgate")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "CREATE TABLE `tg` (`id` INT64, `g` GEOGRAPHY)"); err != nil {
		t.Fatal(err)
	}

	// All-NULL batch for the GEOGRAPHY column: fast path.
	if _, err := googlesqlite.BulkInsert(ctx, conn, []string{"tg"}, []string{"id", "g"},
		[][]any{{int64(1), nil}, {int64(2), nil}}); err != nil {
		t.Fatalf("all-NULL GEOGRAPHY batch should bulk-load, got: %v", err)
	}
	var cnt int64
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM `tg` WHERE `g` IS NULL").Scan(&cnt); err != nil {
		t.Fatal(err)
	}
	if cnt != 2 {
		t.Errorf("NULL geography rows = %d, want 2", cnt)
	}

	// Omitted GEOGRAPHY column: also fast path (NULL fill).
	if _, err := googlesqlite.BulkInsert(ctx, conn, []string{"tg"}, []string{"id"}, [][]any{{int64(3)}}); err != nil {
		t.Fatalf("omitted GEOGRAPHY column should bulk-load, got: %v", err)
	}

	// Non-NULL value: fall back, instrumented.
	var got []googlesqlite.BulkInsertUnsupportedEvent
	googlesqlite.SetBulkInsertUnsupportedHook(func(ev googlesqlite.BulkInsertUnsupportedEvent) {
		got = append(got, ev)
	})
	defer googlesqlite.SetBulkInsertUnsupportedHook(nil)
	before := googlesqlite.BulkInsertUnsupportedCount()
	_, err = googlesqlite.BulkInsert(ctx, conn, []string{"tg"}, []string{"id", "g"},
		[][]any{{int64(4), "POINT(1 2)"}})
	if !errors.Is(err, googlesqlite.ErrBulkInsertUnsupported) {
		t.Fatalf("non-NULL GEOGRAPHY: err = %v, want ErrBulkInsertUnsupported", err)
	}
	if d := googlesqlite.BulkInsertUnsupportedCount() - before; d != 1 {
		t.Errorf("unsupported counter delta = %d, want 1", d)
	}
	if len(got) != 1 || got[0].Column != "g" || got[0].Type != "GEOGRAPHY" || got[0].Table == "" {
		t.Errorf("hook events = %+v, want one event for column g type GEOGRAPHY", got)
	}
	// The failed batch left nothing behind.
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM `tg`").Scan(&cnt); err != nil {
		t.Fatal(err)
	}
	if cnt != 3 {
		t.Errorf("row count after rejected batch = %d, want 3", cnt)
	}
}

// TestBulkInsertStagedJoinsOpenTransaction: the staged (cast-in-flush) path
// must join an open transaction exactly like the direct path.
func TestBulkInsertStagedJoinsOpenTransaction(t *testing.T) {
	db, err := sql.Open("googlesqlite", ":memory:?_test=bulkinsert_staged_tx")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "CREATE TABLE `tj` (`id` INT64, `j` JSON)"); err != nil {
		t.Fatal(err)
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := googlesqlite.BulkInsert(ctx, conn, []string{"tj"}, []string{"id", "j"},
		[][]any{{int64(1), `{"k":1}`}}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	var cnt int64
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM `tj`").Scan(&cnt); err != nil {
		t.Fatal(err)
	}
	if cnt != 0 {
		t.Errorf("count after rollback = %d, want 0 (staged bulk insert must join the open tx)", cnt)
	}
	// And committed work persists (including the omitted-JSON-column shape,
	// which used to be unsupported even all-NULL).
	if _, err := googlesqlite.BulkInsert(ctx, conn, []string{"tj"}, []string{"id"}, [][]any{{int64(7)}}); err != nil {
		t.Fatalf("omitted JSON column should bulk-load now: %v", err)
	}
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM `tj` WHERE `j` IS NULL").Scan(&cnt); err != nil {
		t.Fatal(err)
	}
	if cnt != 1 {
		t.Errorf("NULL json rows = %d, want 1", cnt)
	}
}

// BenchmarkBulkInsertJSON100k: the issue-#6 motivating shape — a table with a
// JSON column, which previously fell back to ~0.24 ms/row per-row INSERTs.
// Run with -benchtime=1x; ms/row is reported as a custom metric.
func BenchmarkBulkInsertJSON100k(b *testing.B) {
	const rowsN = 100_000
	ctx := context.Background()
	fpath := filepath.Join(b.TempDir(), "bulkbenchjson.db")
	db, err := sql.Open("googlesqlite", fmt.Sprintf("file:%s?cache=shared", fpath))
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("CREATE TABLE `t` (`id` INT64, `doc` JSON)"); err != nil {
		b.Fatal(err)
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		b.Fatal(err)
	}
	defer conn.Close()
	if _, err := googlesqlite.BulkInsert(ctx, conn, []string{"t"}, []string{"id", "doc"},
		[][]any{{int64(0), `{"warm":true}`}}); err != nil {
		b.Fatal(err)
	}
	rows := make([][]any, rowsN)
	for i := range rows {
		rows[i] = []any{int64(i), fmt.Sprintf(`{"k":%d,"s":"v%d"}`, i, i)}
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := googlesqlite.BulkInsert(ctx, conn, []string{"t"}, []string{"id", "doc"}, rows); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(b.Elapsed().Milliseconds())/float64(b.N)/float64(rowsN), "ms/row")
}

// BenchmarkPerRowInsertJSON1k measures the fallback this issue removes (the
// "before" number): per-row parameterized INSERTs into the same JSON-column
// table. 1k rows keeps the wall clock sane at ~0.24 ms/row.
func BenchmarkPerRowInsertJSON1k(b *testing.B) {
	const rowsN = 1_000
	ctx := context.Background()
	fpath := filepath.Join(b.TempDir(), "perrowbenchjson.db")
	db, err := sql.Open("googlesqlite", fmt.Sprintf("file:%s?cache=shared", fpath))
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("CREATE TABLE `t` (`id` INT64, `doc` JSON)"); err != nil {
		b.Fatal(err)
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		b.Fatal(err)
	}
	defer conn.Close()
	stmt, err := conn.PrepareContext(ctx, "INSERT `t` (`id`, `doc`) VALUES (?, ?)")
	if err != nil {
		b.Fatal(err)
	}
	defer stmt.Close()
	if _, err := stmt.ExecContext(ctx, int64(0), `{"warm":true}`); err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for r := 0; r < rowsN; r++ {
			if _, err := stmt.ExecContext(ctx, int64(r), fmt.Sprintf(`{"k":%d,"s":"v%d"}`, r, r)); err != nil {
				b.Fatal(err)
			}
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(b.Elapsed().Milliseconds())/float64(b.N)/float64(rowsN), "ms/row")
}

// TestBulkInsertArrayStructNormalizedShape pins the bqe TestTable hang's
// root cause: an ARRAY<STRUCT> cell arriving in the bigquery-emulator's
// normalized RECORD form — each struct element a []map of single-key maps
// (one per field). bulkCastStruct merged that shape for TOP-LEVEL struct
// columns, but bulkCastValue's ARRAY branch passed elements through
// untyped, so each element persisted as a nested ARRAY envelope of
// mini-structs; the scan side then died with "failed to convert struct
// from array" (surfacing as an HTTP 500 retry loop in the emulator).
// The ARRAY branch now recurses with the declared element type.
func TestBulkInsertArrayStructNormalizedShape(t *testing.T) {
	db, err := sql.Open("googlesqlite", ":memory:?_test=bulk_arrstruct")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx,
		"CREATE TABLE `t` (`id` INT64, `recs` ARRAY<STRUCT<a INT64, b STRING>>)"); err != nil {
		t.Fatal(err)
	}
	rows := [][]any{{
		int64(1),
		[]any{
			[]map[string]any{{"a": int64(7)}, {"b": "x"}},
			[]map[string]any{{"a": int64(8)}, {"b": "y"}},
		},
	}}
	if n, err := googlesqlite.BulkInsert(ctx, conn, []string{"t"}, []string{"id", "recs"}, rows); err != nil || n != 1 {
		t.Fatalf("BulkInsert: n=%d err=%v", n, err)
	}
	var got string
	if err := conn.QueryRowContext(ctx,
		"SELECT TO_JSON_STRING(recs) FROM `t` WHERE id = 1").Scan(&got); err != nil {
		t.Fatalf("scan ARRAY<STRUCT> back: %v", err)
	}
	want := `[{"a":7,"b":"x"},{"a":8,"b":"y"}]`
	if got != want {
		t.Fatalf("round-trip = %s, want %s", got, want)
	}
}

// TestBulkInsertJSONObjectParity is the issue-#14 acceptance bar. BulkInsert's
// cast-in-flush used to feed a Go string bound to a JSON column through
// StringValue.ToJSON (strconv.Quote), so the flush's CAST(c AS JSON) stored a
// double-encoded JSON STRING where the per-row reference path — INSERT with
// PARSE_JSON(?), the bigquery-emulator binding shape — stored the parsed
// value. The bar, verbatim from the issue-#6 verification thread:
//
//   - the 5,000-row parity check at ZERO differing rows, compared via
//     TO_JSON_STRING over the whole row (JSON_VALUE masked the bug — it
//     unwraps a JSON scalar string, so field extraction passed either way);
//   - JSON_TYPE(doc) = 'object' on a bulk-loaded row;
//   - and (failure parity) invalid JSON text must error on the bulk path the
//     way per-row PARSE_JSON errors, not get silently stored as a quoted
//     scalar string.
func TestBulkInsertJSONObjectParity(t *testing.T) {
	db, err := sql.Open("googlesqlite", ":memory:?_test=bulk_json_parity14")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	for _, tbl := range []string{"p", "b"} {
		if _, err := conn.ExecContext(ctx,
			fmt.Sprintf("CREATE TABLE `%s` (`id` INT64, `doc` JSON)", tbl)); err != nil {
			t.Fatalf("create %s: %v", tbl, err)
		}
	}

	// 5,000 rows over the JSON kinds that diverge under double-encoding:
	// objects, arrays, scalar strings, numbers, booleans, JSON null.
	const rowCount = 5000
	doc := func(i int) string {
		switch i % 6 {
		case 0:
			return fmt.Sprintf(`{"k":%d,"tags":["a","b"],"nested":{"v":%d}}`, i, i*2)
		case 1:
			return fmt.Sprintf(`[%d,null,"s-%d"]`, i, i)
		case 2:
			return fmt.Sprintf(`"plain string %d"`, i)
		case 3:
			return fmt.Sprintf(`%d.5`, i)
		case 4:
			return `true`
		default:
			return `null`
		}
	}

	// Per-row reference path: INSERT with PARSE_JSON(?).
	for i := 0; i < rowCount; i++ {
		if _, err := conn.ExecContext(ctx,
			"INSERT `p` (`id`, `doc`) VALUES (?, PARSE_JSON(?))", int64(i), doc(i)); err != nil {
			t.Fatalf("per-row insert %d: %v", i, err)
		}
	}

	// Bulk path: the same raw JSON texts, one batch.
	rows := make([][]any, rowCount)
	for i := 0; i < rowCount; i++ {
		rows[i] = []any{int64(i), doc(i)}
	}
	if n, err := googlesqlite.BulkInsert(ctx, conn, []string{"b"}, []string{"id", "doc"}, rows); err != nil || n != rowCount {
		t.Fatalf("BulkInsert: n=%d err=%v", n, err)
	}

	// Whole-row TO_JSON_STRING parity, row by row: ZERO differing rows.
	fetch := func(tbl string) []string {
		t.Helper()
		rs, err := conn.QueryContext(ctx,
			fmt.Sprintf("SELECT TO_JSON_STRING(t) FROM `%s` AS t ORDER BY t.id", tbl))
		if err != nil {
			t.Fatalf("TO_JSON_STRING over %s: %v", tbl, err)
		}
		defer rs.Close()
		out := make([]string, 0, rowCount)
		for rs.Next() {
			var s string
			if err := rs.Scan(&s); err != nil {
				t.Fatalf("scan: %v", err)
			}
			out = append(out, s)
		}
		if err := rs.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}
	want, got := fetch("p"), fetch("b")
	if len(want) != rowCount || len(got) != rowCount {
		t.Fatalf("row counts: per-row=%d bulk=%d, want %d", len(want), len(got), rowCount)
	}
	differing := 0
	for i := range want {
		if want[i] != got[i] {
			if differing == 0 {
				t.Errorf("first differing row %d:\n per-row: %s\n bulk:    %s", i, want[i], got[i])
			}
			differing++
		}
	}
	if differing != 0 {
		t.Errorf("TO_JSON_STRING parity: %d differing rows, want 0", differing)
	}

	// JSON_TYPE on a loaded object row must be 'object', not 'string' —
	// on the bulk table AND the per-row reference table.
	for _, tbl := range []string{"b", "p"} {
		var jt string
		if err := conn.QueryRowContext(ctx,
			fmt.Sprintf("SELECT JSON_TYPE(`doc`) FROM `%s` WHERE `id` = 0", tbl)).Scan(&jt); err != nil {
			t.Fatalf("JSON_TYPE over %s: %v", tbl, err)
		}
		if jt != "object" {
			t.Errorf("JSON_TYPE(doc) on %s row = %q, want %q", tbl, jt, "object")
		}
	}

	// Failure parity: invalid JSON text errors on BOTH paths (the bulk path
	// used to quote it into a valid JSON scalar string and store it).
	const badJSON = `{not json`
	if _, err := conn.ExecContext(ctx,
		"INSERT `p` (`id`, `doc`) VALUES (?, PARSE_JSON(?))", int64(rowCount), badJSON); err == nil {
		t.Error("per-row PARSE_JSON of invalid JSON text: want error, got nil")
	}
	if _, err := googlesqlite.BulkInsert(ctx, conn, []string{"b"}, []string{"id", "doc"},
		[][]any{{int64(rowCount), badJSON}}); err == nil {
		t.Error("BulkInsert of invalid JSON text: want error, got nil")
	}
	var afterBad int64
	if err := conn.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM `b` WHERE `id` = ?", int64(rowCount)).Scan(&afterBad); err != nil {
		t.Fatal(err)
	}
	if afterBad != 0 {
		t.Errorf("invalid-JSON bulk row was stored (%d rows), want rejected", afterBad)
	}
}
