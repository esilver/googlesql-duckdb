package googlesqlite_test

import (
	"context"
	"database/sql"
	"testing"

	_ "github.com/goccy/googlesqlite"
)

func openWindowAggregateRegressionDB(t *testing.T, name string) *sql.DB {
	t.Helper()
	db, err := sql.Open("googlesqlite", "file:"+name+"?mode=memory&cache=private")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return db
}

func windowAggregateRegressionString(t *testing.T, db *sql.DB, query string) string {
	t.Helper()
	var got string
	if err := db.QueryRowContext(context.Background(), query).Scan(&got); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	return got
}

func TestWindowAggregateRegression_ArrayAggDistinctOver(t *testing.T) {
	t.Parallel()
	db := openWindowAggregateRegressionDB(t, "win_array_distinct")
	got := windowAggregateRegressionString(t, db, `
		SELECT TO_JSON_STRING(ARRAY_AGG(DISTINCT x) OVER (PARTITION BY g))
		FROM (SELECT 1 AS g, 1 AS x UNION ALL SELECT 1, 1 UNION ALL SELECT 1, 2)
		LIMIT 1`)
	if got != "[1,2]" {
		t.Fatalf("window ARRAY_AGG(DISTINCT) = %q, want [1,2]", got)
	}
}

func TestWindowAggregateRegression_StringAggDistinctOver(t *testing.T) {
	t.Parallel()
	db := openWindowAggregateRegressionDB(t, "win_string_distinct")
	got := windowAggregateRegressionString(t, db, `
		SELECT STRING_AGG(DISTINCT s) OVER ()
		FROM (SELECT 'a' AS s UNION ALL SELECT 'a' UNION ALL SELECT 'b')
		LIMIT 1`)
	if got != "a,b" {
		t.Fatalf("window STRING_AGG(DISTINCT) = %q, want a,b", got)
	}
}

func TestWindowAggregateRegression_PercentileContRespectNulls(t *testing.T) {
	t.Parallel()
	db := openWindowAggregateRegressionDB(t, "win_percentile_nulls")
	var got sql.NullFloat64
	if err := db.QueryRowContext(context.Background(), `
		SELECT PERCENTILE_CONT(x, 0 RESPECT NULLS) OVER()
		FROM UNNEST([NULL, 1.0]) AS x
		LIMIT 1`).Scan(&got); err != nil {
		t.Fatalf("PERCENTILE_CONT RESPECT NULLS: %v", err)
	}
	if got.Valid {
		t.Fatalf("PERCENTILE_CONT RESPECT NULLS = %v, want NULL", got.Float64)
	}
}

func TestWindowAggregateRegression_NestedMarkerAggregateNoOrdinalLeak(t *testing.T) {
	t.Parallel()
	db := openWindowAggregateRegressionDB(t, "win_nested_marker")
	got := windowAggregateRegressionString(t, db, `
		SELECT TO_JSON_STRING(ARRAY_AGG(a))
		FROM (
			SELECT (
				SELECT APPROX_QUANTILES(v, 1)[OFFSET(0)]
				FROM UNNEST([3, 1, 2]) AS v
			) AS a
		)`)
	if got != "[3]" {
		t.Fatalf("nested marker aggregate = %q, want [3]", got)
	}
}

func TestWindowAggregateRegression_ExtractWeekWeekday(t *testing.T) {
	t.Parallel()
	db := openWindowAggregateRegressionDB(t, "win_extract_weekday")
	var got int64
	if err := db.QueryRowContext(context.Background(),
		"SELECT EXTRACT(WEEK(MONDAY) FROM DATE '2026-01-01')").Scan(&got); err != nil {
		t.Fatalf("EXTRACT WEEK(MONDAY): %v", err)
	}
	if got != 0 {
		t.Fatalf("EXTRACT WEEK(MONDAY) = %d, want 0", got)
	}
}

func TestWindowAggregateRegression_ArrayConcatAggLimitIgnoresNullArrays(t *testing.T) {
	t.Parallel()
	db := openWindowAggregateRegressionDB(t, "win_array_concat_limit")
	got := windowAggregateRegressionString(t, db, `
		SELECT TO_JSON_STRING(ARRAY_CONCAT_AGG(arr LIMIT 1))
		FROM UNNEST([
			STRUCT(CAST(NULL AS ARRAY<INT64>) AS arr),
			STRUCT([1, 2] AS arr),
			STRUCT([3, 4] AS arr)
		])`)
	if got != "[1,2]" {
		t.Fatalf("ARRAY_CONCAT_AGG LIMIT over NULL array = %q, want [1,2]", got)
	}
}
