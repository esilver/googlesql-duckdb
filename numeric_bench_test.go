package googlesqlite_test

// BenchmarkNumericAggregateStored measures SUM over a stored NUMERIC
// column, 100k rows — the native-storage migration's headline perf gate.
// Pre-migration the column was a TEXT envelope and every aggregated row
// paid a googlesqlite_plain_string UDF decode; post-migration the column
// is DECIMAL(38,9) and the engine's vectorized decimal sum runs with no
// per-row UDF (the tolerant read's TRY_CAST arm claims every row).

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/goccy/googlesqlite"
)

func BenchmarkNumericAggregateStored(b *testing.B) {
	db, err := sql.Open("googlesqlite", ":memory:?_test=numeric_agg_bench")
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		b.Fatal(err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "CREATE TABLE nums (c NUMERIC)"); err != nil {
		b.Fatal(err)
	}
	const rows = 100_000
	batch := make([][]any, rows)
	for i := range batch {
		batch[i] = []any{fmt.Sprintf("%d.%09d", i%1000, i)}
	}
	if _, err := googlesqlite.BulkInsert(ctx, conn, []string{"nums"}, []string{"c"}, batch); err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var sum string
		if err := conn.QueryRowContext(ctx,
			"SELECT CAST(SUM(c) AS STRING) FROM nums").Scan(&sum); err != nil {
			b.Fatal(err)
		}
		if sum == "" {
			b.Fatal("empty sum")
		}
	}
}
