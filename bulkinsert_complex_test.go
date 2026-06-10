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
		values   []any  // one per row; nil rows included on purpose
	}{
		{"numeric", "NUMERIC", false, "", []any{"123.45", "-0.000000001", "123.450", nil}},
		{"bignumeric", "BIGNUMERIC", false, "", []any{"99999999999999999999999999999.123456789", nil}},
		{"json", "JSON", false, "", []any{`{"b": 2,  "a": [1, null]}`, `"plain string"`, `[1,2,3]`, nil}},
		// TO_JSON_STRING over TIME is a pre-existing dialect gap (identical
		// for both paths); read back through CAST instead.
		{"time", "TIME", false, "CAST(`c` AS STRING)", []any{"12:34:56.789012", "00:00:00", nil}},
		{"bytes", "BYTES", true, "", []any{[]byte{0x01, 0x02, 'h', 'i'}, []byte{}, nil}},
		{"arr_i", "ARRAY<INT64>", false, "", []any{[]int64{1, 2, 3}, []int64{}, nil}},
		{"arr_s", "ARRAY<STRING>", false, "", []any{[]string{"x", "it's"}, nil}},
		{"strct", "STRUCT<a INT64, b STRING>", false, "", []any{map[string]any{"a": int64(7), "b": "q"}, map[string]any{"b": "only-b"}, nil}},
		{"strct_nest", "STRUCT<a INT64, s STRUCT<x FLOAT64, y ARRAY<INT64>>>", false, "",
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
				if _, err := conn.ExecContext(ctx,
					fmt.Sprintf("INSERT `%s` (`id`, `c`) VALUES (?, ?)", perRow), int64(i), v); err != nil {
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
