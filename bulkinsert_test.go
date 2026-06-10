package googlesqlite_test

// Black-box tests for the BulkInsert fast path (bulkinsert.go +
// internal/bulkinsert.go): correctness incl. NULLs, storage-representation
// parity with the per-row INSERT path, the ErrBulkInsertUnsupported fallback
// contract, and the 100k-row throughput benchmark that motivated the API
// (duckdb-go-pure#1).

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/goccy/googlesqlite"
)

func TestBulkInsertCorrectness(t *testing.T) {
	db, err := sql.Open("googlesqlite", ":memory:?_test=bulkinsert_correctness")
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

	if _, err := conn.ExecContext(ctx, "CREATE TABLE `t` (`i` INT64, `f` FLOAT64, `s` STRING, `b` BOOL, `ts` TIMESTAMP)"); err != nil {
		t.Fatal(err)
	}
	ts := time.Date(2026, 6, 10, 12, 34, 56, 789000000, time.UTC)
	rows := [][]any{
		{int64(42), 3.14, "hello world", true, ts},
		{nil, nil, nil, nil, nil},
		{int64(-7), float64(0), "", false, ts.Add(24 * time.Hour)},
		{int64(1), 2.5, "it's quoted", true, nil},
	}
	n, err := googlesqlite.BulkInsert(ctx, conn, []string{"t"}, []string{"i", "f", "s", "b", "ts"}, rows)
	if err != nil {
		t.Fatal(err)
	}
	if n != int64(len(rows)) {
		t.Fatalf("BulkInsert returned %d, want %d", n, len(rows))
	}

	// One row through the SLOW path for representation-parity comparison.
	if _, err := conn.ExecContext(ctx, "INSERT `t` (`i`,`f`,`s`,`b`,`ts`) VALUES (100, 1.5, 'hello world', true, TIMESTAMP '2026-06-10 12:34:56.789+00')"); err != nil {
		t.Fatal(err)
	}

	countWhere := func(where string) int64 {
		t.Helper()
		var cnt int64
		if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM `t` WHERE "+where).Scan(&cnt); err != nil {
			t.Fatalf("WHERE %s: %v", where, err)
		}
		return cnt
	}
	// Envelope/representation parity: predicates must match BOTH the bulk
	// rows and the slow-path row.
	if got := countWhere("s = 'hello world'"); got != 2 {
		t.Errorf("WHERE s='hello world' count = %d, want 2 (one bulk, one slow-path)", got)
	}
	if got := countWhere("ts = TIMESTAMP '2026-06-10 12:34:56.789+00'"); got != 2 {
		t.Errorf("WHERE ts=... count = %d, want 2", got)
	}
	if got := countWhere("i IS NULL"); got != 1 {
		t.Errorf("WHERE i IS NULL count = %d, want 1", got)
	}
	if got := countWhere("b"); got != 3 {
		t.Errorf("WHERE b count = %d, want 3", got)
	}
	var sum int64
	if err := conn.QueryRowContext(ctx, "SELECT SUM(i) FROM `t`").Scan(&sum); err != nil {
		t.Fatal(err)
	}
	if sum != 136 {
		t.Errorf("SUM(i) = %d, want 136", sum)
	}
	var up string
	if err := conn.QueryRowContext(ctx, "SELECT UPPER(s) FROM `t` WHERE i = 42").Scan(&up); err != nil {
		t.Fatal(err)
	}
	if up != "HELLO WORLD" {
		t.Errorf("UPPER(s) on bulk row = %q, want %q", up, "HELLO WORLD")
	}
}

func TestBulkInsertJoinsOpenTransaction(t *testing.T) {
	db, err := sql.Open("googlesqlite", ":memory:?_test=bulkinsert_tx")
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
	if _, err := conn.ExecContext(ctx, "CREATE TABLE `t` (`id` INT64)"); err != nil {
		t.Fatal(err)
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := googlesqlite.BulkInsert(ctx, conn, []string{"t"}, []string{"id"}, [][]any{{int64(1)}, {int64(2)}}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	var cnt int64
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM `t`").Scan(&cnt); err != nil {
		t.Fatal(err)
	}
	if cnt != 0 {
		t.Errorf("count after rollback = %d, want 0 (bulk insert must join the open tx)", cnt)
	}
}

func TestBulkInsertUnsupportedShapes(t *testing.T) {
	db, err := sql.Open("googlesqlite", ":memory:?_test=bulkinsert_unsupported")
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

	// Targeted column outside the fast path's kind set.
	if _, err := conn.ExecContext(ctx, "CREATE TABLE `tj` (`j` JSON)"); err != nil {
		t.Fatal(err)
	}
	if _, err := googlesqlite.BulkInsert(ctx, conn, []string{"tj"}, []string{"j"}, [][]any{{`{"a":1}`}}); !errors.Is(err, googlesqlite.ErrBulkInsertUnsupported) {
		t.Errorf("JSON column: err = %v, want errors.Is(_, ErrBulkInsertUnsupported)", err)
	}

	// Omitted column with a DEFAULT (the slow path splices defaults).
	if _, err := conn.ExecContext(ctx, "CREATE TABLE `td` (`id` INT64, `d` INT64 DEFAULT 7)"); err != nil {
		t.Fatal(err)
	}
	if _, err := googlesqlite.BulkInsert(ctx, conn, []string{"td"}, []string{"id"}, [][]any{{int64(1)}}); !errors.Is(err, googlesqlite.ErrBulkInsertUnsupported) {
		t.Errorf("omitted DEFAULT column: err = %v, want errors.Is(_, ErrBulkInsertUnsupported)", err)
	}

	// Unknown table is a hard error, not Unsupported.
	if _, err := googlesqlite.BulkInsert(ctx, conn, []string{"nope"}, []string{"x"}, [][]any{{int64(1)}}); err == nil || errors.Is(err, googlesqlite.ErrBulkInsertUnsupported) {
		t.Errorf("unknown table: err = %v, want a non-Unsupported error", err)
	}

	// Unsupported never leaves partial data behind.
	var cnt int64
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM `tj`").Scan(&cnt); err != nil {
		t.Fatal(err)
	}
	if cnt != 0 {
		t.Errorf("tj count = %d, want 0", cnt)
	}
}

// BenchmarkBulkInsert100k measures the issue duckdb-go-pure#1 phase-B shape:
// one 100_000-row batch into a 3-column file-backed table. Run with
// -benchtime=1x; ms/row is reported as a custom metric.
func BenchmarkBulkInsert100k(b *testing.B) {
	const rowsN = 100_000
	ctx := context.Background()
	fpath := filepath.Join(b.TempDir(), "bulkbench.db")
	db, err := sql.Open("googlesqlite", fmt.Sprintf("file:%s?cache=shared", fpath))
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("CREATE TABLE `t` (`id` INT64, `grp` INT64, `val` FLOAT64)"); err != nil {
		b.Fatal(err)
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		b.Fatal(err)
	}
	defer conn.Close()
	// Warm both the engine and the appender path outside timing.
	if _, err := googlesqlite.BulkInsert(ctx, conn, []string{"t"}, []string{"id", "grp", "val"}, [][]any{{int64(0), int64(0), 0.0}}); err != nil {
		b.Fatal(err)
	}
	rows := make([][]any, rowsN)
	for i := range rows {
		rows[i] = []any{int64(i), int64(i % 1000), float64(i) / 7.0}
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := googlesqlite.BulkInsert(ctx, conn, []string{"t"}, []string{"id", "grp", "val"}, rows); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(b.Elapsed().Milliseconds())/float64(b.N)/float64(rowsN), "ms/row")

	var total int64
	if err := db.QueryRow("SELECT COUNT(*) FROM `t`").Scan(&total); err != nil {
		b.Fatal(err)
	}
	if want := int64(1 + b.N*rowsN); total != want {
		b.Fatalf("count after bench = %d, want %d", total, want)
	}
}
