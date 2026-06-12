package googlesqlite_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	_ "github.com/goccy/googlesqlite"
)

// Regression suite for the __gsx_setop_ordinal determinism-train helper
// column (fix24-ordinal-rework).
//
// The set-operation ordinal is an internal helper column that UNION ALL
// rendering appends so downstream order anchors (left-join row_number,
// window row_id, aggregate input ordinals) can reproduce GoogleSQL's
// branch order deterministically. The invariant this suite pins:
//
//	The helper column must NEVER escape into a fixed-column consumer
//	(INSERT ... SELECT, MERGE USING, recursive-CTE union branches,
//	scalar/IN subqueries), and exposure decisions must be derived from
//	the resolved AST, not from substring matches over rendered SQL.

func openOrdinalTestDB(t *testing.T, name string) *sql.DB {
	t.Helper()
	db, err := sql.Open("googlesqlite", "file:"+name+"?mode=memory&cache=private")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return db
}

func ordinalExec(t *testing.T, db *sql.DB, stmts ...string) {
	t.Helper()
	for _, s := range stmts {
		if _, err := db.ExecContext(context.Background(), s); err != nil {
			t.Fatalf("exec %q: %v", s, err)
		}
	}
}

func ordinalQueryInts(t *testing.T, db *sql.DB, query string) []int64 {
	t.Helper()
	rows, err := db.QueryContext(context.Background(), query)
	if err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("scan %q: %v", query, err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows %q: %v", query, err)
	}
	return out
}

func ordinalQueryString(t *testing.T, db *sql.DB, query string) string {
	t.Helper()
	var got string
	if err := db.QueryRowContext(context.Background(), query).Scan(&got); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	return got
}

// --- Bug 1: INSERT ... SELECT with a bare UNION ALL query root -------------

// TestOrdinalRegression_InsertSelectBareUnion is the verified bug-1 repro:
// the resolved INSERT query root is the SetOperationScan itself (no
// ProjectScan above it), so the rendered union exposed the helper ordinal
// as a second column and DuckDB rejected the INSERT with "expected 1
// columns but 2 values were supplied".
func TestOrdinalRegression_InsertSelectBareUnion(t *testing.T) {
	t.Parallel()
	db := openOrdinalTestDB(t, "ord_insert_bare")
	ordinalExec(t, db,
		"CREATE TABLE t1 (a INT64)",
		"INSERT INTO t1 SELECT 1 UNION ALL SELECT 2",
	)
	got := ordinalQueryInts(t, db, "SELECT a FROM t1 ORDER BY a")
	want := []int64{1, 2}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("t1 rows = %v, want %v", got, want)
	}
}

// TestOrdinalRegression_InsertSelectBareUnionMultiColumn covers the
// multi-column variant of bug 1 (the ordinal lands after ALL projected
// columns, so the off-by-one must not silently shift values either).
func TestOrdinalRegression_InsertSelectBareUnionMultiColumn(t *testing.T) {
	t.Parallel()
	db := openOrdinalTestDB(t, "ord_insert_bare_multi")
	ordinalExec(t, db,
		"CREATE TABLE t2 (a INT64, b STRING)",
		"INSERT INTO t2 (a, b) SELECT 1, 'x' UNION ALL SELECT 2, 'y'",
	)
	got := ordinalQueryString(t, db,
		"SELECT STRING_AGG(CONCAT(CAST(a AS STRING), ':', b), ',' ORDER BY a) FROM t2")
	if got != "1:x,2:y" {
		t.Fatalf("t2 rows = %q, want %q", got, "1:x,2:y")
	}
}

// TestOrdinalRegression_InsertSelectWrappedUnion pins the already-passing
// wrapped form (a ProjectScan above the union strips the helper column).
func TestOrdinalRegression_InsertSelectWrappedUnion(t *testing.T) {
	t.Parallel()
	db := openOrdinalTestDB(t, "ord_insert_wrapped")
	ordinalExec(t, db,
		"CREATE TABLE t3 (a INT64)",
		"INSERT INTO t3 SELECT * FROM (SELECT 1 UNION ALL SELECT 2)",
	)
	got := ordinalQueryInts(t, db, "SELECT a FROM t3 ORDER BY a")
	if fmt.Sprint(got) != fmt.Sprint([]int64{1, 2}) {
		t.Fatalf("t3 rows = %v, want [1 2]", got)
	}
}

// TestOrdinalRegression_InsertSelectUnionInCTE pins the CTE form: the
// WithRefScan re-exposes the ordinal inside the query, and the INSERT
// splice must still end up with exactly the insert column list.
func TestOrdinalRegression_InsertSelectUnionInCTE(t *testing.T) {
	t.Parallel()
	db := openOrdinalTestDB(t, "ord_insert_cte")
	ordinalExec(t, db,
		"CREATE TABLE t4 (a INT64)",
		"INSERT INTO t4 WITH u AS (SELECT 1 AS a UNION ALL SELECT 2) SELECT a FROM u",
	)
	got := ordinalQueryInts(t, db, "SELECT a FROM t4 ORDER BY a")
	if fmt.Sprint(got) != fmt.Sprint([]int64{1, 2}) {
		t.Fatalf("t4 rows = %v, want [1 2]", got)
	}
}

// --- Bug 2: chained LEFT JOINs over a UNION ALL left input -----------------

// TestOrdinalRegression_ChainedLeftJoinOverUnion is the verified bug-2
// repro: the second LEFT JOIN's left input is itself a left join that
// PROJECTS its column list (no ordinal), but the rendered-SQL substring
// probe saw the nested ordinal literal and emitted
// `ORDER BY "__gsx_setop_ordinal"` over a scope that doesn't expose it.
func TestOrdinalRegression_ChainedLeftJoinOverUnion(t *testing.T) {
	t.Parallel()
	db := openOrdinalTestDB(t, "ord_chain_lj")
	ordinalExec(t, db,
		"CREATE TABLE tx (k INT64, v STRING)",
		"CREATE TABLE ty (k INT64, w STRING)",
		"INSERT INTO tx (k, v) VALUES (1, 'v1'), (2, 'v2')",
		"INSERT INTO ty (k, w) VALUES (2, 'w2')",
	)
	got := ordinalQueryString(t, db, `
		SELECT STRING_AGG(CONCAT(CAST(u.a AS STRING), ':', IFNULL(x.v, '-'), ':', IFNULL(y.w, '-')), ',' ORDER BY u.a)
		FROM (SELECT 1 AS a UNION ALL SELECT 2) u
		LEFT JOIN tx x ON u.a = x.k
		LEFT JOIN ty y ON u.a = y.k`)
	want := "1:v1:-,2:v2:w2"
	if got != want {
		t.Fatalf("chained left join = %q, want %q", got, want)
	}
}

// TestOrdinalRegression_ChainedLeftJoinUnionInCTE: the same shape with the
// union behind a CTE reference (the WithRefScan carries the ordinal, the
// first left join strips it, the second must not order by it).
func TestOrdinalRegression_ChainedLeftJoinUnionInCTE(t *testing.T) {
	t.Parallel()
	db := openOrdinalTestDB(t, "ord_chain_lj_cte")
	ordinalExec(t, db,
		"CREATE TABLE ctx2 (k INT64, v STRING)",
		"CREATE TABLE cty2 (k INT64, w STRING)",
		"INSERT INTO ctx2 (k, v) VALUES (1, 'v1')",
		"INSERT INTO cty2 (k, w) VALUES (1, 'w1'), (2, 'w2')",
	)
	got := ordinalQueryString(t, db, `
		WITH u AS (SELECT 1 AS a UNION ALL SELECT 2)
		SELECT STRING_AGG(CONCAT(CAST(u.a AS STRING), ':', IFNULL(x.v, '-'), ':', IFNULL(y.w, '-')), ',' ORDER BY u.a)
		FROM u
		LEFT JOIN ctx2 x ON u.a = x.k
		LEFT JOIN cty2 y ON u.a = y.k`)
	want := "1:v1:w1,2:-:w2"
	if got != want {
		t.Fatalf("chained left join (cte union) = %q, want %q", got, want)
	}
}

// TestOrdinalRegression_SingleLeftJoinOverUnionOrder pins the feature the
// anchor exists for: a single LEFT JOIN over a union preserves the union's
// branch order in its output.
func TestOrdinalRegression_SingleLeftJoinOverUnionOrder(t *testing.T) {
	t.Parallel()
	db := openOrdinalTestDB(t, "ord_single_lj")
	ordinalExec(t, db,
		"CREATE TABLE slj (k INT64, v STRING)",
		"INSERT INTO slj (k, v) VALUES (1, 'one'), (2, 'two'), (3, 'three')",
	)
	rows, err := db.QueryContext(context.Background(), `
		SELECT u.a, s.v
		FROM (SELECT 3 AS a UNION ALL SELECT 1 UNION ALL SELECT 2) u
		LEFT JOIN slj s ON u.a = s.k`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var order []int64
	for rows.Next() {
		var a int64
		var v sql.NullString
		if err := rows.Scan(&a, &v); err != nil {
			t.Fatal(err)
		}
		order = append(order, a)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(order) != fmt.Sprint([]int64{3, 1, 2}) {
		t.Fatalf("left-join row order = %v, want union branch order [3 1 2]", order)
	}
}

// --- Bug 3: safe-eval scalar subquery double LIMIT --------------------------

// TestOrdinalRegression_IfErrorScalarSubqueryWithLimit is the verified
// bug-3 repro: the IFERROR safe-eval arm appended `LIMIT 1` to a value
// subquery whose rendered SQL already ends in LIMIT, producing
// `... LIMIT 1 LIMIT 1` (Parser Error).
func TestOrdinalRegression_IfErrorScalarSubqueryWithLimit(t *testing.T) {
	t.Parallel()
	db := openOrdinalTestDB(t, "ord_iferror_limit")
	got := ordinalQueryInts(t, db, "SELECT IFERROR((SELECT 5 LIMIT 1), -1)")
	if fmt.Sprint(got) != fmt.Sprint([]int64{5}) {
		t.Fatalf("IFERROR((SELECT 5 LIMIT 1), -1) = %v, want [5]", got)
	}
}

// TestOrdinalRegression_IfErrorScalarSubqueryOrderByLimit covers the
// ORDER BY ... LIMIT shape of the same bug.
func TestOrdinalRegression_IfErrorScalarSubqueryOrderByLimit(t *testing.T) {
	t.Parallel()
	db := openOrdinalTestDB(t, "ord_iferror_oblimit")
	got := ordinalQueryInts(t, db,
		"SELECT IFERROR((SELECT x FROM UNNEST([3, 1, 2]) AS x ORDER BY x LIMIT 1), -1)")
	if fmt.Sprint(got) != fmt.Sprint([]int64{1}) {
		t.Fatalf("IFERROR(ordered limited subquery, -1) = %v, want [1]", got)
	}
}

// TestOrdinalRegression_IfErrorScalarSubqueryMultiRowFolds pins the
// semantics the safe-eval guard exists for: a multi-row scalar subquery
// must fold to the fallback, not raise.
func TestOrdinalRegression_IfErrorScalarSubqueryMultiRowFolds(t *testing.T) {
	t.Parallel()
	db := openOrdinalTestDB(t, "ord_iferror_multirow")
	got := ordinalQueryInts(t, db,
		"SELECT IFERROR((SELECT x FROM UNNEST([1, 2]) AS x), -1)")
	if fmt.Sprint(got) != fmt.Sprint([]int64{-1}) {
		t.Fatalf("IFERROR(multi-row scalar subquery, -1) = %v, want [-1]", got)
	}
}

// TestOrdinalRegression_IfErrorScalarSubqueryUnion: a bare-union scalar
// subquery inside IFERROR exercises both the ordinal projection guard and
// the safe-eval wrap at once (two rows -> fallback).
func TestOrdinalRegression_IfErrorScalarSubqueryUnion(t *testing.T) {
	t.Parallel()
	db := openOrdinalTestDB(t, "ord_iferror_union")
	got := ordinalQueryInts(t, db,
		"SELECT IFERROR((SELECT 1 UNION ALL SELECT 2), -1)")
	if fmt.Sprint(got) != fmt.Sprint([]int64{-1}) {
		t.Fatalf("IFERROR(bare union scalar subquery, -1) = %v, want [-1]", got)
	}
}

// --- Family: other fixed-column / single-value consumers -------------------

// TestOrdinalRegression_InOverBareUnion pins IN (subquery) over a bare
// union: the value subquery must project exactly one column even though
// the union scan exposes the helper ordinal.
func TestOrdinalRegression_InOverBareUnion(t *testing.T) {
	t.Parallel()
	db := openOrdinalTestDB(t, "ord_in_union")
	got := ordinalQueryInts(t, db,
		"SELECT IF(2 IN (SELECT 1 UNION ALL SELECT 2), 1, 0)")
	if fmt.Sprint(got) != fmt.Sprint([]int64{1}) {
		t.Fatalf("2 IN (bare union) = %v, want [1]", got)
	}
}

// TestOrdinalRegression_ArraySubqueryUnionOrder pins ARRAY(bare union):
// the helper ordinal must not leak into the array elements AND the array
// must keep the union's branch order.
func TestOrdinalRegression_ArraySubqueryUnionOrder(t *testing.T) {
	t.Parallel()
	db := openOrdinalTestDB(t, "ord_array_union")
	got := ordinalQueryString(t, db,
		"SELECT TO_JSON_STRING(ARRAY(SELECT 3 UNION ALL SELECT 1 UNION ALL SELECT 2))")
	if got != "[3,1,2]" {
		t.Fatalf("ARRAY(bare union) = %q, want %q", got, "[3,1,2]")
	}
}

// TestOrdinalRegression_MergeUsingUnion: MERGE with a union source. The
// merged-table materialization (`SELECT DISTINCT * FROM source FULL OUTER
// JOIN target`) is a fixed-shape consumer: the helper ordinal must not
// become a merged-table column (it would distort DISTINCT pair collapsing).
func TestOrdinalRegression_MergeUsingUnion(t *testing.T) {
	t.Parallel()
	db := openOrdinalTestDB(t, "ord_merge_union")
	ordinalExec(t, db,
		"CREATE TABLE mt (k INT64, v STRING)",
		"INSERT INTO mt (k, v) VALUES (1, 'old')",
		`MERGE mt t USING (SELECT 1 AS k, 'new' AS v UNION ALL SELECT 2, 'two') s ON t.k = s.k
		 WHEN MATCHED THEN UPDATE SET v = s.v
		 WHEN NOT MATCHED THEN INSERT (k, v) VALUES (s.k, s.v)`,
	)
	got := ordinalQueryString(t, db,
		"SELECT STRING_AGG(CONCAT(CAST(k AS STRING), ':', v), ',' ORDER BY k) FROM mt")
	want := "1:new,2:two"
	if got != want {
		t.Fatalf("MERGE USING (union) result = %q, want %q", got, want)
	}
}

// TestOrdinalRegression_RecursiveCTEUnionSeed: a recursive CTE whose
// non-recursive term is itself a UNION ALL. The recursive scan's branches
// are positional UNION arms; a leaked ordinal column would make the seed
// arm wider than the recursive arm.
func TestOrdinalRegression_RecursiveCTEUnionSeed(t *testing.T) {
	t.Parallel()
	db := openOrdinalTestDB(t, "ord_recursive_seed")
	got := ordinalQueryString(t, db, `
		WITH RECURSIVE r AS (
			(SELECT 1 AS x UNION ALL SELECT 5)
			UNION ALL
			SELECT x + 1 FROM r WHERE x < 3
		)
		SELECT STRING_AGG(CAST(x AS STRING), ',' ORDER BY x) FROM r`)
	want := "1,2,3,5"
	if got != want {
		t.Fatalf("recursive CTE with union seed = %q, want %q", got, want)
	}
}

// --- Order anchors derived structurally -------------------------------------

// TestOrdinalRegression_WindowOverFilteredUnion: detection must recurse
// through pass-through scans (here a FilterScan) so the window row_id
// anchor keeps ordering by the union ordinal instead of silently falling
// back to an unanchored ROW_NUMBER() OVER ().
func TestOrdinalRegression_WindowOverFilteredUnion(t *testing.T) {
	t.Parallel()
	db := openOrdinalTestDB(t, "ord_window_filter")
	rows, err := db.QueryContext(context.Background(), `
		SELECT a, ROW_NUMBER() OVER () AS rn
		FROM (SELECT 3 AS a UNION ALL SELECT 1 UNION ALL SELECT 2)
		WHERE a > 0`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[int64]int64{}
	for rows.Next() {
		var a, rn int64
		if err := rows.Scan(&a, &rn); err != nil {
			t.Fatal(err)
		}
		got[a] = rn
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := map[int64]int64{3: 1, 1: 2, 2: 3}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("ROW_NUMBER over filtered union = %v, want %v (union branch order)", got, want)
	}
}

// TestOrdinalRegression_NestedWithUnionAnchor: an outer union CTE
// referenced inside an inner WITH subtree. The inner WITH scope must
// inherit the outer scope's ordinal-carrying CTE set so the window anchor
// (and the WithRefScan projection) stay consistent.
func TestOrdinalRegression_NestedWithUnionAnchor(t *testing.T) {
	t.Parallel()
	db := openOrdinalTestDB(t, "ord_nested_with")
	rows, err := db.QueryContext(context.Background(), `
		WITH u AS (SELECT 3 AS a UNION ALL SELECT 1 UNION ALL SELECT 2)
		SELECT a, rn FROM (
			WITH v AS (SELECT a, ROW_NUMBER() OVER () AS rn FROM u)
			SELECT a, rn FROM v
		)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[int64]int64{}
	for rows.Next() {
		var a, rn int64
		if err := rows.Scan(&a, &rn); err != nil {
			t.Fatal(err)
		}
		got[a] = rn
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := map[int64]int64{3: 1, 1: 2, 2: 3}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("nested-WITH window over outer union CTE = %v, want %v", got, want)
	}
}

// TestOrdinalRegression_StringAggOverUnionOrder: ARRAY_AGG/STRING_AGG
// stamp an input ordinal (`__gsx_agg_rn`) to reproduce input order; over a
// union input that stamp must anchor on the set-operation ordinal.
func TestOrdinalRegression_StringAggOverUnionOrder(t *testing.T) {
	t.Parallel()
	db := openOrdinalTestDB(t, "ord_stringagg_union")
	got := ordinalQueryString(t, db,
		"SELECT STRING_AGG(CAST(a AS STRING), ',') FROM (SELECT 3 AS a UNION ALL SELECT 1 UNION ALL SELECT 2)")
	if got != "3,1,2" {
		t.Fatalf("STRING_AGG over union = %q, want %q (union branch order)", got, "3,1,2")
	}
}
