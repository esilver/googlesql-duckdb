package googlesqlite_test

// Issue #8 coverage.
//
// Part 1 — CTAS speed: CREATE TABLE AS SELECT already lowers to ONE native
// engine statement (CREATE TABLE "t" AS <formatted select>); the ~2ms/row
// cost was googlesqlite_cast resolving its two JSON-encoded *Type arguments
// through the wasm type factory on EVERY row (json.Unmarshal +
// MakeSimpleType + Kind(), each wasm call stack-walking for goroutine
// pinning). bindCast now caches the resolved googlesql type node per JSON
// type literal, making the per-row cast body wasm-free. The parity bar is
// the same as issue #6: rows stored by CTAS must be byte-identical in
// storage to the same values written through the per-row INSERT path.
//
// Part 2 — DROP TABLE IF EXISTS on a missing table must succeed silently
// (BigQuery semantics) instead of failing with "failed to delete table
// spec"; DROP TABLE without IF EXISTS must keep failing.

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/goccy/googlesqlite"
)

// ctasRawRows runs query directly on the inner engine connection
// (bypassing the dialect layer) and returns the stringified rows.
func ctasRawRows(t testing.TB, ctx context.Context, conn *sql.Conn, query string) [][]string {
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

// TestCTASStorageParityWithPerRowInsert is the issue-#8 parity bar: a
// table created via CREATE TABLE AS SELECT must hold storage cells
// byte-identical to a twin table created with typed DDL and filled with
// the same values through the per-row INSERT path. The SELECT exercises
// the cached-cast hot path (CONCAT + CAST to STRING / NUMERIC /
// TIMESTAMP) alongside native INT64 / FLOAT64 / BOOL columns.
func TestCTASStorageParityWithPerRowInsert(t *testing.T) {
	db, err := sql.Open("googlesqlite", ":memory:?_test=ctas_parity")
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

	const nRows = 25
	selectBody := fmt.Sprintf(
		"SELECT n AS id, CONCAT('user_', CAST(n AS STRING)) AS name, MOD(n, 7) AS bucket, "+
			"CAST(n AS FLOAT64) / 4 AS score, MOD(n, 2) = 0 AS even, "+
			"CAST(CAST(n AS STRING) AS NUMERIC) AS num, "+
			"TIMESTAMP_ADD(TIMESTAMP '2026-01-01 00:00:00+00', INTERVAL n SECOND) AS ts "+
			"FROM UNNEST(GENERATE_ARRAY(1, %d)) AS n", nRows)

	if _, err := conn.ExecContext(ctx, "CREATE TABLE ctas_t AS "+selectBody); err != nil {
		t.Fatalf("CTAS: %v", err)
	}

	// Twin table: typed DDL matching the CTAS catalog types, filled row
	// by row through the per-row INSERT path with the same values.
	if _, err := conn.ExecContext(ctx,
		"CREATE TABLE perrow_t (id INT64, name STRING, bucket INT64, score FLOAT64, even BOOL, num NUMERIC, ts TIMESTAMP)"); err != nil {
		t.Fatalf("CREATE TABLE perrow_t: %v", err)
	}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for n := 1; n <= nRows; n++ {
		if _, err := conn.ExecContext(ctx,
			"INSERT INTO perrow_t VALUES (?, ?, ?, ?, ?, ?, ?)",
			int64(n),
			fmt.Sprintf("user_%d", n),
			int64(n%7),
			float64(n)/4,
			n%2 == 0,
			fmt.Sprintf("%d", n),
			base.Add(time.Duration(n)*time.Second),
		); err != nil {
			t.Fatalf("per-row INSERT n=%d: %v", n, err)
		}
	}

	// Dialect-level readback must match.
	readback := func(table string) [][]string {
		rows, err := conn.QueryContext(ctx, fmt.Sprintf(
			"SELECT id, name, bucket, score, even, CAST(num AS STRING), CAST(ts AS STRING) FROM %s ORDER BY id", table))
		if err != nil {
			t.Fatalf("readback %s: %v", table, err)
		}
		defer rows.Close()
		var out [][]string
		for rows.Next() {
			cols := make([]any, 7)
			for i := range cols {
				cols[i] = new(sql.NullString)
			}
			if err := rows.Scan(cols...); err != nil {
				t.Fatalf("scan %s: %v", table, err)
			}
			row := make([]string, 7)
			for i, c := range cols {
				ns := c.(*sql.NullString)
				if ns.Valid {
					row[i] = ns.String
				} else {
					row[i] = "<NULL>"
				}
			}
			out = append(out, row)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows %s: %v", table, err)
		}
		return out
	}
	gotCTAS, gotPerRow := readback("ctas_t"), readback("perrow_t")
	if len(gotCTAS) != nRows {
		t.Fatalf("ctas_t row count = %d, want %d", len(gotCTAS), nRows)
	}
	if !reflect.DeepEqual(gotCTAS, gotPerRow) {
		t.Errorf("dialect readback mismatch:\nctas:   %v\nperrow: %v", gotCTAS, gotPerRow)
	}

	// Storage parity: RAW stored cells, byte for byte.
	rawCTAS := ctasRawRows(t, ctx, conn, `SELECT * FROM "ctas_t" ORDER BY 1`)
	rawPerRow := ctasRawRows(t, ctx, conn, `SELECT * FROM "perrow_t" ORDER BY 1`)
	if !reflect.DeepEqual(rawCTAS, rawPerRow) {
		t.Errorf("RAW storage mismatch between CTAS and per-row path:\nctas:   %v\nperrow: %v", rawCTAS, rawPerRow)
	}
}

// TestCTASBulkThroughput is the issue-#8 measurement guard: 10k rows of
// the issue's exact CTAS shape (was ~21s = ~2ms/row) must complete well
// under a second now that the cast hot path is cached. The bound is
// deliberately loose (5s) so slow CI machines do not flake, while still
// catching any regression back to the 2ms/row regime (10k rows would
// take >20s).
func TestCTASBulkThroughput(t *testing.T) {
	if testing.Short() {
		t.Skip("timing test")
	}
	db, err := sql.Open("googlesqlite", ":memory:?_test=ctas_throughput")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()

	start := time.Now()
	if _, err := db.ExecContext(ctx,
		"CREATE TABLE big10k AS SELECT n AS id, CONCAT('user_', CAST(n AS STRING)) AS name, MOD(n,100) AS bucket, RAND() AS score FROM UNNEST(GENERATE_ARRAY(1, 10000)) AS n"); err != nil {
		t.Fatalf("CTAS 10k: %v", err)
	}
	elapsed := time.Since(start)
	var count int64
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM big10k").Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 10000 {
		t.Fatalf("row count = %d, want 10000", count)
	}
	t.Logf("CTAS 10k rows in %v (%.4f ms/row)", elapsed, float64(elapsed.Milliseconds())/10000)
	if elapsed > 5*time.Second {
		t.Errorf("CTAS 10k took %v; per-row cast regression (issue #8 was ~21s)", elapsed)
	}
}

// TestDropTableIfExists covers issue-#8 part 2: all four combinations of
// {with, without} IF EXISTS x {existing, missing} table, plus the VIEW
// form.
func TestDropTableIfExists(t *testing.T) {
	t.Parallel()
	db, err := sql.Open("googlesqlite", ":memory:?_test=drop_if_exists")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()

	// IF EXISTS + missing: silent success (BigQuery semantics).
	if _, err := db.ExecContext(ctx, "DROP TABLE IF EXISTS never_created"); err != nil {
		t.Errorf("DROP TABLE IF EXISTS on missing table: %v", err)
	}
	// Repeat to prove it stays idempotent.
	if _, err := db.ExecContext(ctx, "DROP TABLE IF EXISTS never_created"); err != nil {
		t.Errorf("second DROP TABLE IF EXISTS on missing table: %v", err)
	}
	// Without IF EXISTS + missing: must error.
	if _, err := db.ExecContext(ctx, "DROP TABLE never_created"); err == nil {
		t.Errorf("DROP TABLE on missing table succeeded; want error")
	}

	// IF EXISTS + existing: drops the table and its catalog spec.
	if _, err := db.ExecContext(ctx, "CREATE TABLE die_t (k INT64)"); err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO die_t VALUES (1)"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}
	if _, err := db.ExecContext(ctx, "DROP TABLE IF EXISTS die_t"); err != nil {
		t.Errorf("DROP TABLE IF EXISTS on existing table: %v", err)
	}
	if _, err := db.QueryContext(ctx, "SELECT k FROM die_t"); err == nil {
		t.Errorf("SELECT after DROP TABLE IF EXISTS succeeded; want error")
	}
	// The spec must be gone from the catalog: recreating must work.
	if _, err := db.ExecContext(ctx, "CREATE TABLE die_t (k INT64)"); err != nil {
		t.Errorf("re-CREATE after DROP TABLE IF EXISTS: %v", err)
	}
	// Without IF EXISTS + existing: still works.
	if _, err := db.ExecContext(ctx, "DROP TABLE die_t"); err != nil {
		t.Errorf("DROP TABLE on existing table: %v", err)
	}

	// VIEW form: same catalog seam.
	if _, err := db.ExecContext(ctx, "DROP VIEW IF EXISTS never_view"); err != nil {
		t.Errorf("DROP VIEW IF EXISTS on missing view: %v", err)
	}
}
