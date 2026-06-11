package googlesqlite_test

// Issue 16: the dbt-shaped churn that crashed the v0.2.18 analyzer
// runtime (LanguageOptions finalizer free on the runtime-finalizer
// goroutine + bridge slicing wasm pointers as signed int32 once
// linear memory passed 2 GiB). This stress reproduces the workload
// shape end-to-end through the driver: concurrent workers issuing
// distinct statement texts (every one an analysis-cache miss), DDL
// mutations interleaved so resetCatalog rebuilds the SimpleCatalog
// (the per-node LanguageOptions churn source, now a process
// singleton), and forced GC so finalizers fire while analyses are in
// flight. Must complete with zero errors and zero panics.

import (
	"database/sql"
	"fmt"
	"runtime"
	"sync"
	"testing"
)

func TestIssue16FinalizerChurnStress(t *testing.T) {
	db, err := sql.Open("googlesqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec("CREATE TABLE i16 (id INT64, grp STRING, val FLOAT64)"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := db.Exec("INSERT INTO i16 VALUES (1, 'a', 1.5), (2, 'b', 2.5), (3, 'a', 3.5)"); err != nil {
		t.Fatalf("insert: %v", err)
	}

	const (
		workers = 4
		iters   = 40
	)
	var wg sync.WaitGroup
	errCh := make(chan error, workers)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				// Distinct text per (worker, iteration): always a
				// cache miss, the dbt shape.
				q := fmt.Sprintf(
					"SELECT grp, COUNT(*) + %d AS n, SUM(val) * %d AS s FROM i16 WHERE id != %d GROUP BY grp ORDER BY grp",
					w*1000+i, w+2, i+100)
				rows, err := db.Query(q)
				if err != nil {
					errCh <- fmt.Errorf("w%d i%d query: %w", w, i, err)
					return
				}
				for rows.Next() {
				}
				if err := rows.Close(); err != nil {
					errCh <- fmt.Errorf("w%d i%d close: %w", w, i, err)
					return
				}
				if i%8 == 0 {
					runtime.GC()
				}
			}
		}(w)
	}
	// DDL churn on the main goroutine: every CREATE OR REPLACE runs
	// resetCatalog -> newSimpleCatalog, the path that armed one
	// abandoned LanguageOptions finalizer per dbt node pre-fix.
	for i := 0; i < 12; i++ {
		stmt := fmt.Sprintf("CREATE OR REPLACE TABLE i16_ddl_%d AS SELECT %d AS x", i%3, i)
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("ddl %d: %v", i, err)
		}
		runtime.GC()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
	// Post-churn sanity: the rebuilt catalog still answers.
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM i16").Scan(&n); err != nil {
		t.Fatalf("post-churn query: %v", err)
	}
	if n != 3 {
		t.Fatalf("post-churn COUNT(*) = %d, want 3", n)
	}
}
