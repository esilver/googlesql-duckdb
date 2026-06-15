package googlesqlite_test

// Tests for the analyzer analysis cache: repeat queries hit the cache
// and return identical results, DDL between repeats invalidates (the
// schema change is reflected), and different parameter shapes key
// separately instead of colliding.

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"runtime"
	"runtime/pprof"
	"sync"
	"testing"
	"time"

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

// Instrumentation that CPU-profiles the dialect path (analyze ->
// format -> engine exec -> decode) for a repeated GROUP BY over a
// 10k-row table, mirroring the REST measurements in issue 7.
//
// Run with:
//
//	ANALYZER_PROFILE=1 CGO_ENABLED=0 go test -vet=off -run TestAnalyzerProfile -timeout 1200s .
//
// Writes /tmp/analyzer/dialect.pprof and prints per-phase wall times.

func analyzerDrain(t *testing.T, db *sql.DB, q string) int {
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

func TestAnalyzerProfile(t *testing.T) {
	if os.Getenv("ANALYZER_PROFILE") == "" {
		t.Skip("set ANALYZER_PROFILE=1 to run the analyzer profile harness")
	}
	db, err := sql.Open("googlesqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	setupStart := time.Now()
	if _, err := db.Exec(`CREATE TABLE analyzer AS
SELECT id,
       CONCAT('g', CAST(MOD(id, 10) AS STRING)) AS grp,
       CAST(id AS FLOAT64) AS val
FROM UNNEST(GENERATE_ARRAY(1, 10000)) AS id`); err != nil {
		t.Fatalf("setup: %v", err)
	}
	fmt.Printf("analyzer: setup (CTAS 10k rows) %v\n", time.Since(setupStart))

	const groupBy = `SELECT grp, COUNT(*) AS n, SUM(val) AS s, AVG(val) AS a
FROM analyzer GROUP BY grp ORDER BY grp`
	const selfJoin = `SELECT a.grp, COUNT(*) AS n
FROM analyzer a JOIN analyzer b ON a.id = b.id
GROUP BY a.grp ORDER BY a.grp`

	// First-query latency (cold analyzer path for this text).
	first := time.Now()
	n := analyzerDrain(t, db, groupBy)
	fmt.Printf("analyzer: first GROUP BY %v (%d rows)\n", time.Since(first), n)
	first = time.Now()
	n = analyzerDrain(t, db, selfJoin)
	fmt.Printf("analyzer: first self-join %v (%d rows)\n", time.Since(first), n)

	// Steady-state repeats, timed.
	const iters = 50
	rep := time.Now()
	for i := 0; i < iters; i++ {
		analyzerDrain(t, db, groupBy)
	}
	fmt.Printf("analyzer: repeat GROUP BY avg %v over %d iters\n", time.Since(rep)/iters, iters)
	rep = time.Now()
	for i := 0; i < iters; i++ {
		analyzerDrain(t, db, selfJoin)
	}
	fmt.Printf("analyzer: repeat self-join avg %v over %d iters\n", time.Since(rep)/iters, iters)

	// CPU profile over a repeat-heavy loop: the split between
	// go-googlesql (analysis), googlesqlite/internal (formatting),
	// and duckdb-go-pure (engine) apportions the per-query cost.
	if err := os.MkdirAll("/tmp/analyzer", 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	pf, err := os.Create("/tmp/analyzer/dialect.pprof")
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
		analyzerDrain(t, db, groupBy)
		analyzerDrain(t, db, selfJoin)
	}
	pprof.StopCPUProfile()
	fmt.Printf("analyzer: profiled %d iter-pairs in %v (avg %v/query)\n",
		profIters, time.Since(profStart), time.Since(profStart)/(2*profIters))
}

// Analyzer throughput harness for interpreter-vs-compiler wazero mode
// comparison (issue 7). Each query text is distinct, so every
// iteration is an analysis-cache MISS — this measures the raw
// analyze+format path. Run with ANALYZER_STRESS=1; flip
// GOOGLESQLITE_WASM_COMPILATION_MODE=compiler externally to compare.

func TestAnalyzerStress(t *testing.T) {
	if os.Getenv("ANALYZER_STRESS") == "" {
		t.Skip("set ANALYZER_STRESS=1")
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

// The dbt-shaped churn that crashed the v0.2.18 analyzer runtime
// (issue 16): LanguageOptions finalizer free on the runtime-finalizer
// goroutine + bridge slicing wasm pointers as signed int32 once
// linear memory passed 2 GiB. This stress reproduces the workload
// shape end-to-end through the driver: concurrent workers issuing
// distinct statement texts (every one an analysis-cache miss), DDL
// mutations interleaved so resetCatalog rebuilds the SimpleCatalog
// (the per-node LanguageOptions churn source, now a process
// singleton), and forced GC so finalizers fire while analyses are in
// flight. Must complete with zero errors and zero panics.

func TestFinalizerChurnStress(t *testing.T) {
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

// Acceptance test for issue #19: per-statement catalog rebuilds
// exhausted the analyzer wasm heap during a dbt build (bad_alloc at
// node 70 of a large build) after per-test cost climbed 9s -> 17s on empty
// tables.
//
// The dbt shape: every model is CREATE OR REPLACE TABLE, the catalog
// grows past 100 specs across several datasets, and each DDL is
// followed by analyzed queries (schema tests such as not_null). The
// unfixed code rebuilt the WHOLE analyzer catalog on every replace —
// re-registering the full builtin + BigQuery + Spanner extension
// surface once per sub-catalog — so per-iteration cost climbed with
// catalog size and the analyzer linear memory filled until bad_alloc.
//
// Acceptance criteria:
//  (a) the loop completes (no bad_alloc / wasm trap);
//  (b) per-iteration wall time stays roughly flat: the average of
//      the last 10 iterations must be < 3x the average of the first
//      10 (with an absolute floor so a fast machine can't flake the
//      ratio on sub-millisecond noise).

func TestDbtCreateOrReplaceChurn(t *testing.T) {
	if testing.Short() {
		t.Skip("multi-second churn loop")
	}
	// Deliberately not parallel: this test asserts on wall time.
	db, err := sql.Open("googlesqlite", ":memory:?_test=catalog_churn")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("Conn: %v", err)
	}
	defer conn.Close()

	const (
		datasets   = 7   // distinct datasets, like the dbt run's fixtures
		fixtures   = 110 // pre-created tables: past the 108 specs at the production abort
		iterations = 150 // CREATE OR REPLACE + analyzed query loop
		models     = 35  // distinct model tables cycled by the loop
	)

	// Pre-create the fixture tables so the catalog already holds a
	// production-sized spec set before the churn starts.
	for i := 0; i < fixtures; i++ {
		stmt := fmt.Sprintf(
			"CREATE TABLE proj.ds%d.fixture_%d (id INT64, payload STRING)",
			i%datasets, i,
		)
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("fixture %d: %v", i, err)
		}
	}

	durations := make([]time.Duration, 0, iterations)
	for i := 0; i < iterations; i++ {
		ds := i % datasets
		model := fmt.Sprintf("proj.ds%d.model_%d", ds, i%models)
		// Every 50th-ish iteration replaces with a CHANGED schema to
		// exercise the (now rare) full-rebuild path too; the indices
		// are chosen to stay out of the first-10/last-10 timing
		// windows because a rebuild is legitimately costlier.
		schema := "(id INT64, val FLOAT64)"
		if i%50 == 20 {
			schema = fmt.Sprintf("(id INT64, val FLOAT64, extra_%d STRING)", i)
		}
		start := time.Now()
		if _, err := conn.ExecContext(ctx,
			"CREATE OR REPLACE TABLE "+model+" "+schema); err != nil {
			t.Fatalf("iteration %d: CREATE OR REPLACE: %v", i, err)
		}
		// A not_null-style schema test. The alias is unique per
		// iteration so every statement is a distinct text — analysis
		// cache misses, like dbt's per-test SQL.
		var n int64
		q := fmt.Sprintf("SELECT COUNT(*) AS c_%d FROM %s WHERE id IS NULL", i, model)
		if err := conn.QueryRowContext(ctx, q).Scan(&n); err != nil {
			t.Fatalf("iteration %d: not_null probe: %v", i, err)
		}
		if n != 0 {
			t.Fatalf("iteration %d: not_null probe returned %d, want 0", i, n)
		}
		durations = append(durations, time.Since(start))
	}

	avg := func(ds []time.Duration) time.Duration {
		var sum time.Duration
		for _, d := range ds {
			sum += d
		}
		return sum / time.Duration(len(ds))
	}
	first10 := avg(durations[:10])
	last10 := avg(durations[len(durations)-10:])
	t.Logf("catalog churn: first-10 avg %v, last-10 avg %v, total %v",
		first10, last10, avg(durations)*time.Duration(len(durations)))

	// (b) flat per-iteration cost. The unfixed code climbs steeply:
	// every iteration rebuilds a 110+ spec catalog, so last-10 is
	// many multiples of first-10 (in production 9s -> 17s and then
	// bad_alloc). Allow 3x for noise, with a 100ms absolute floor.
	if last10 > 3*first10 && last10 > 100*time.Millisecond {
		t.Fatalf("per-iteration cost climbs: first-10 avg %v, last-10 avg %v (> 3x)",
			first10, last10)
	}

	// Correctness guard: the schema-changing replaces must have gone
	// through the full rebuild — the analyzer must resolve the new
	// column on the last changed model (i=120 -> model_15 on ds1).
	var extra sql.NullString
	if err := conn.QueryRowContext(ctx,
		"SELECT extra_120 FROM proj.ds1.model_15 LIMIT 1").Scan(&extra); err != nil &&
		err != sql.ErrNoRows {
		t.Fatalf("changed-schema column did not resolve after replace: %v", err)
	}
}
