package googlesqlite_test

import (
	"context"
	"database/sql"
	"testing"
)

// mergeTestConn opens an isolated in-memory database and returns a
// single pinned connection plus a must-exec helper.
func mergeTestConn(t *testing.T, tag string) (*sql.Conn, context.Context, func(string)) {
	t.Helper()
	db, err := sql.Open("googlesqlite", ":memory:?_test="+tag)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("Conn: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	mustExec := func(q string) {
		t.Helper()
		if _, err := conn.ExecContext(ctx, q); err != nil {
			t.Fatalf("exec %q: %v", q, err)
		}
	}
	return conn, ctx, mustExec
}

// readKV scans (k INT64, v STRING) rows into a map.
func readKV(t *testing.T, ctx context.Context, conn *sql.Conn, table string) map[int64]string {
	t.Helper()
	rows, err := conn.QueryContext(ctx, "SELECT k, v FROM "+table+" ORDER BY k")
	if err != nil {
		t.Fatalf("query %s: %v", table, err)
	}
	defer rows.Close()
	got := map[int64]string{}
	for rows.Next() {
		var k int64
		var v string
		if err := rows.Scan(&k, &v); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got[k] = v
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	return got
}

// TestMergeUsingComputedSubquery is the verbatim repro from
// googlesqlite issue #17: a MERGE whose USING clause is a computed
// subquery used to lower the source unparenthesized (`... FROM SELECT
// ...`), making the engine raise a Parser Error and poisoning the
// surrounding transaction. Both WHEN arms are exercised: row k=1
// exists (MATCHED -> UPDATE) and the same statement re-run against a
// missing key takes the NOT MATCHED -> INSERT arm.
func TestMergeUsingComputedSubquery(t *testing.T) {
	t.Parallel()
	conn, ctx, mustExec := mergeTestConn(t, "merge_subq_issue17")
	mustExec("CREATE TABLE t (k INT64, v STRING)")
	mustExec("INSERT INTO t (k, v) VALUES (1, 'old')")

	// MATCHED path: k=1 exists, v becomes 'x'.
	mustExec(`MERGE t USING (SELECT 1 AS k, 'x' AS v) s ON t.k = s.k
WHEN MATCHED THEN UPDATE SET v = s.v
WHEN NOT MATCHED THEN INSERT (k, v) VALUES (s.k, s.v)`)
	if got := readKV(t, ctx, conn, "t"); len(got) != 1 || got[1] != "x" {
		t.Fatalf("after MATCHED merge: got %v, want {1:x}", got)
	}

	// NOT MATCHED path: k=2 is new, row is inserted.
	mustExec(`MERGE t USING (SELECT 2 AS k, 'y' AS v) s ON t.k = s.k
WHEN MATCHED THEN UPDATE SET v = s.v
WHEN NOT MATCHED THEN INSERT (k, v) VALUES (s.k, s.v)`)
	if got := readKV(t, ctx, conn, "t"); len(got) != 2 || got[1] != "x" || got[2] != "y" {
		t.Fatalf("after NOT MATCHED merge: got %v, want {1:x 2:y}", got)
	}
}

// TestMergeUsingSubqueryOverTable covers a multi-row computed subquery
// (projection over a staged table) feeding both WHEN arms in a single
// MERGE, plus a reversed ON ordering (s.k = t.k) which exercises the
// source/target column classification.
func TestMergeUsingSubqueryOverTable(t *testing.T) {
	t.Parallel()
	conn, ctx, mustExec := mergeTestConn(t, "merge_subq_table")
	mustExec("CREATE TABLE tgt (k INT64, v STRING)")
	mustExec("CREATE TABLE staged (k INT64, base STRING)")
	mustExec("INSERT INTO tgt (k, v) VALUES (1, 'old1'), (3, 'keep3')")
	mustExec("INSERT INTO staged (k, base) VALUES (1, 'a'), (2, 'b')")

	// Computed column: v derived via CONCAT in the USING subquery.
	mustExec(`MERGE tgt USING (SELECT k, CONCAT(base, '!') AS v FROM staged) s ON s.k = tgt.k
WHEN MATCHED THEN UPDATE SET v = s.v
WHEN NOT MATCHED THEN INSERT (k, v) VALUES (s.k, s.v)`)
	got := readKV(t, ctx, conn, "tgt")
	want := map[int64]string{1: "a!", 2: "b!", 3: "keep3"}
	if len(got) != len(want) {
		t.Fatalf("rows = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("k=%d: got %q, want %q", k, got[k], v)
		}
	}
}

// TestMergeUsingFilteredAliasShadowingSubquery is the bqe wedge-stress
// MERGE shape: the USING subquery projects a computed column whose
// alias SHADOWS the base column it filters on (`SELECT 13 + k AS k ...
// WHERE k <= 2`). The first INSERT-arm lowering re-formatted the source
// without column IDs, so the filter's column ref picked up the aliased
// projection (`... WHERE ((13 + k) AS k <= 2)`) — a Parser Error.
func TestMergeUsingFilteredAliasShadowingSubquery(t *testing.T) {
	t.Parallel()
	conn, ctx, mustExec := mergeTestConn(t, "merge_subq_shadow")
	mustExec("CREATE TABLE sink (k INT64, v STRING)")
	mustExec("CREATE TABLE seed (k INT64, v STRING)")
	mustExec("INSERT INTO sink (k, v) VALUES (14, 'old14')")
	mustExec("INSERT INTO seed (k, v) VALUES (1, 'a'), (2, 'b'), (3, 'c')")

	// Source rows: k in {14, 15} (filter keeps seed k <= 2, then +13).
	// k=14 exists in sink (MATCHED -> UPDATE), k=15 is new (INSERT).
	mustExec(`MERGE sink t USING (SELECT 13 + k AS k, CONCAT(v, '_m') AS v FROM seed WHERE k <= 2) s
ON t.k = s.k
WHEN MATCHED THEN UPDATE SET v = s.v
WHEN NOT MATCHED THEN INSERT (k, v) VALUES (s.k, s.v)`)
	got := readKV(t, ctx, conn, "sink")
	want := map[int64]string{14: "a_m", 15: "b_m"}
	if len(got) != len(want) {
		t.Fatalf("rows = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("k=%d: got %q, want %q", k, got[k], v)
		}
	}
}

// TestMergeUsingSubqueryNotMatchedBySource covers the WHEN NOT MATCHED
// BY SOURCE arm (DELETE) with a computed-subquery source.
func TestMergeUsingSubqueryNotMatchedBySource(t *testing.T) {
	t.Parallel()
	conn, ctx, mustExec := mergeTestConn(t, "merge_subq_bysource")
	mustExec("CREATE TABLE tgt (k INT64, v STRING)")
	mustExec("INSERT INTO tgt (k, v) VALUES (1, 'old1'), (9, 'stale')")

	mustExec(`MERGE tgt USING (SELECT 1 AS k, 'fresh' AS v) s ON tgt.k = s.k
WHEN MATCHED THEN UPDATE SET v = s.v
WHEN NOT MATCHED BY SOURCE THEN DELETE`)
	got := readKV(t, ctx, conn, "tgt")
	if len(got) != 1 || got[1] != "fresh" {
		t.Fatalf("got %v, want {1:fresh}", got)
	}
}
