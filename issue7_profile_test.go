package googlesqlite_test

// Issue 7 instrumentation: CPU-profile the dialect path (analyze ->
// format -> engine exec -> decode) for a repeated GROUP BY over a
// 10k-row table, mirroring the REST measurements in the issue.
//
// Run with:
//
//	ISSUE7_PROFILE=1 CGO_ENABLED=0 go test -vet=off -run TestIssue7Profile -timeout 1200s .
//
// Writes /tmp/issue7/dialect.pprof and prints per-phase wall times.

import (
	"database/sql"
	"fmt"
	"os"
	"runtime/pprof"
	"testing"
	"time"
)

func issue7Drain(t *testing.T, db *sql.DB, q string) int {
	t.Helper()
	rows, err := db.Query(q)
	if err != nil {
		t.Fatalf("query %q: %v", q, err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatalf("columns: %v", err)
	}
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	n := 0
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatalf("scan: %v", err)
		}
		n++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	return n
}

func TestIssue7Profile(t *testing.T) {
	if os.Getenv("ISSUE7_PROFILE") == "" {
		t.Skip("set ISSUE7_PROFILE=1 to run the issue-7 profile harness")
	}
	db, err := sql.Open("googlesqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	setupStart := time.Now()
	if _, err := db.Exec(`CREATE TABLE issue7 AS
SELECT id,
       CONCAT('g', CAST(MOD(id, 10) AS STRING)) AS grp,
       CAST(id AS FLOAT64) AS val
FROM UNNEST(GENERATE_ARRAY(1, 10000)) AS id`); err != nil {
		t.Fatalf("setup: %v", err)
	}
	fmt.Printf("issue7: setup (CTAS 10k rows) %v\n", time.Since(setupStart))

	const groupBy = `SELECT grp, COUNT(*) AS n, SUM(val) AS s, AVG(val) AS a
FROM issue7 GROUP BY grp ORDER BY grp`
	const selfJoin = `SELECT a.grp, COUNT(*) AS n
FROM issue7 a JOIN issue7 b ON a.id = b.id
GROUP BY a.grp ORDER BY a.grp`

	// First-query latency (cold analyzer path for this text).
	first := time.Now()
	n := issue7Drain(t, db, groupBy)
	fmt.Printf("issue7: first GROUP BY %v (%d rows)\n", time.Since(first), n)
	first = time.Now()
	n = issue7Drain(t, db, selfJoin)
	fmt.Printf("issue7: first self-join %v (%d rows)\n", time.Since(first), n)

	// Steady-state repeats, timed.
	const iters = 50
	rep := time.Now()
	for i := 0; i < iters; i++ {
		issue7Drain(t, db, groupBy)
	}
	fmt.Printf("issue7: repeat GROUP BY avg %v over %d iters\n", time.Since(rep)/iters, iters)
	rep = time.Now()
	for i := 0; i < iters; i++ {
		issue7Drain(t, db, selfJoin)
	}
	fmt.Printf("issue7: repeat self-join avg %v over %d iters\n", time.Since(rep)/iters, iters)

	// CPU profile over a repeat-heavy loop: the split between
	// go-googlesql (analysis), googlesqlite/internal (formatting),
	// and duckdb-go-pure (engine) apportions the per-query cost.
	if err := os.MkdirAll("/tmp/issue7", 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	pf, err := os.Create("/tmp/issue7/dialect.pprof")
	if err != nil {
		t.Fatalf("create profile: %v", err)
	}
	defer pf.Close()
	if err := pprof.StartCPUProfile(pf); err != nil {
		t.Fatalf("start profile: %v", err)
	}
	profStart := time.Now()
	const profIters = 30
	for i := 0; i < profIters; i++ {
		issue7Drain(t, db, groupBy)
		issue7Drain(t, db, selfJoin)
	}
	pprof.StopCPUProfile()
	fmt.Printf("issue7: profiled %d iter-pairs in %v (avg %v/query)\n",
		profIters, time.Since(profStart), time.Since(profStart)/(2*profIters))
}
