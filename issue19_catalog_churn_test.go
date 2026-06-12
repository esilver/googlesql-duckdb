package googlesqlite_test

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

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"
)

func TestIssue19_DbtCreateOrReplaceChurn(t *testing.T) {
	if testing.Short() {
		t.Skip("multi-second churn loop")
	}
	// Deliberately not parallel: this test asserts on wall time.
	db, err := sql.Open("googlesqlite", ":memory:?_test=issue19_churn")
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
	t.Logf("issue19 churn: first-10 avg %v, last-10 avg %v, total %v",
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
