package googlesqlite_test

// Tests for the issue-7 analysis cache: repeat queries hit the cache
// and return identical results, DDL between repeats invalidates (the
// schema change is reflected), and different parameter shapes key
// separately instead of colliding.

import (
	"database/sql"
	"testing"

	"github.com/goccy/googlesqlite"
)

func cacheTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("googlesqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// TestAnalysisCacheRepeatQuery: the same query text twice returns
// identical results, and the second run is a cache hit.
func TestAnalysisCacheRepeatQuery(t *testing.T) {
	db := cacheTestDB(t)
	if _, err := db.Exec("CREATE TABLE cache_t1 (id INT64, name STRING)"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := db.Exec("INSERT INTO cache_t1 VALUES (1, 'a'), (2, 'b')"); err != nil {
		t.Fatalf("insert: %v", err)
	}
	const q = "SELECT id, name FROM cache_t1 ORDER BY id"
	read := func() []string {
		rows, err := db.Query(q)
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var id int64
			var name string
			if err := rows.Scan(&id, &name); err != nil {
				t.Fatalf("scan: %v", err)
			}
			out = append(out, name)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows: %v", err)
		}
		return out
	}
	first := read()
	hits0, _ := googlesqlite.AnalysisCacheCounters()
	second := read()
	hits1, _ := googlesqlite.AnalysisCacheCounters()
	if len(first) != 2 || len(second) != 2 || first[0] != second[0] || first[1] != second[1] {
		t.Fatalf("repeat query results differ: %v vs %v", first, second)
	}
	if hits1 <= hits0 {
		t.Fatalf("expected a cache hit on the repeated query (hits %d -> %d)", hits0, hits1)
	}
}

// TestAnalysisCacheDDLInvalidation: DDL between two runs of the same
// text must invalidate the cached plan — the second run sees the new
// schema (a SELECT * that would return stale columns otherwise).
func TestAnalysisCacheDDLInvalidation(t *testing.T) {
	db := cacheTestDB(t)
	if _, err := db.Exec("CREATE TABLE cache_t2 (a INT64)"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := db.Exec("INSERT INTO cache_t2 VALUES (7)"); err != nil {
		t.Fatalf("insert: %v", err)
	}
	const q = "SELECT * FROM cache_t2"
	cols := func() []string {
		rows, err := db.Query(q)
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		defer rows.Close()
		cs, err := rows.Columns()
		if err != nil {
			t.Fatalf("columns: %v", err)
		}
		return cs
	}
	before := cols()
	if len(before) != 1 || before[0] != "a" {
		t.Fatalf("unexpected pre-DDL columns: %v", before)
	}
	// Run the text twice so it is definitely cached before the DDL.
	_ = cols()
	// DROP + CREATE with a different schema. (CREATE OR REPLACE TABLE
	// keeps the old resolver-side SimpleTable handle even without the
	// cache — pre-existing upstream behavior — so it can't be used to
	// probe invalidation.)
	if _, err := db.Exec("DROP TABLE cache_t2"); err != nil {
		t.Fatalf("drop: %v", err)
	}
	if _, err := db.Exec("CREATE TABLE cache_t2 (a INT64, b STRING)"); err != nil {
		t.Fatalf("recreate: %v", err)
	}
	after := cols()
	if len(after) != 2 || after[0] != "a" || after[1] != "b" {
		t.Fatalf("DDL did not invalidate the analysis cache: columns %v", after)
	}
}

// TestAnalysisCacheParamShapes: the same text with different
// parameter shapes must not collide.
func TestAnalysisCacheParamShapes(t *testing.T) {
	db := cacheTestDB(t)
	const q = "SELECT @v AS x"
	var asInt int64
	if err := db.QueryRow(q, sql.Named("v", int64(42))).Scan(&asInt); err != nil {
		t.Fatalf("int shape: %v", err)
	}
	if asInt != 42 {
		t.Fatalf("int shape: got %d", asInt)
	}
	var asStr string
	if err := db.QueryRow(q, sql.Named("v", "hello")).Scan(&asStr); err != nil {
		t.Fatalf("string shape: %v", err)
	}
	if asStr != "hello" {
		t.Fatalf("string shape: got %q", asStr)
	}
	// Repeat both shapes (cache-hit paths) and ensure values bind.
	if err := db.QueryRow(q, sql.Named("v", int64(7))).Scan(&asInt); err != nil || asInt != 7 {
		t.Fatalf("repeat int shape: %v (got %d)", err, asInt)
	}
	if err := db.QueryRow(q, sql.Named("v", "bye")).Scan(&asStr); err != nil || asStr != "bye" {
		t.Fatalf("repeat string shape: %v (got %q)", err, asStr)
	}
}

// TestAnalysisCacheDMLRepeat: repeated INSERT text with fresh args
// lands distinct rows (cached DML rebinds arguments per call).
func TestAnalysisCacheDMLRepeat(t *testing.T) {
	db := cacheTestDB(t)
	if _, err := db.Exec("CREATE TABLE cache_t3 (id INT64, v STRING)"); err != nil {
		t.Fatalf("create: %v", err)
	}
	const ins = "INSERT INTO cache_t3 (id, v) VALUES (?, ?)"
	for i, v := range []string{"x", "y", "z"} {
		if _, err := db.Exec(ins, int64(i), v); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	rows, err := db.Query("SELECT id, v FROM cache_t3 ORDER BY id")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	want := []string{"x", "y", "z"}
	i := 0
	for rows.Next() {
		var id int64
		var v string
		if err := rows.Scan(&id, &v); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if id != int64(i) || v != want[i] {
			t.Fatalf("row %d: got (%d,%q) want (%d,%q)", i, id, v, i, want[i])
		}
		i++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if i != 3 {
		t.Fatalf("expected 3 rows, got %d", i)
	}
}
