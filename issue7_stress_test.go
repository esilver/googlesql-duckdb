package googlesqlite_test

// Issue 7: analyzer throughput harness for interpreter-vs-compiler
// wazero mode comparison. Each query text is distinct, so every
// iteration is an analysis-cache MISS — this measures the raw
// analyze+format path. Run with ISSUE7_STRESS=1; flip
// GOOGLESQLITE_WASM_COMPILATION_MODE=compiler externally to compare.

import (
	"database/sql"
	"fmt"
	"os"
	"runtime"
	"testing"
	"time"
)

func TestIssue7AnalyzerStress(t *testing.T) {
	if os.Getenv("ISSUE7_STRESS") == "" {
		t.Skip("set ISSUE7_STRESS=1")
	}
	openStart := time.Now()
	db, err := sql.Open("googlesqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	var one int
	if err := db.QueryRow("SELECT 1").Scan(&one); err != nil {
		t.Fatalf("first query: %v", err)
	}
	fmt.Printf("stress: open+init+first-query %v\n", time.Since(openStart))
	if _, err := db.Exec("CREATE TABLE st (id INT64, grp STRING, val FLOAT64)"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := db.Exec("INSERT INTO st VALUES (1, 'a', 1.5), (2, 'b', 2.5)"); err != nil {
		t.Fatalf("insert: %v", err)
	}
	const n = 25
	start := time.Now()
	for i := 0; i < n; i++ {
		q := fmt.Sprintf("SELECT grp, COUNT(*) AS n, SUM(val) AS s FROM st WHERE id > %d GROUP BY grp ORDER BY grp", i)
		rows, err := db.Query(q)
		if err != nil {
			t.Fatalf("query %d: %v", i, err)
		}
		for rows.Next() {
		}
		rows.Close()
	}
	per := time.Since(start) / n
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	fmt.Printf("stress: %d distinct-text queries, avg %v/query (analysis-dominated); heap %d MiB\n",
		n, per, ms.HeapAlloc/(1<<20))
}
