package googlesqlite_test

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

// TestInterruptedTxDoesNotPoisonPool regresses the post-interrupt pool
// hygiene behind the bigquery-emulator statement watchdog: a runaway
// statement inside an explicit transaction is interrupted mid-flight
// (duckdb-go-pure >= v0.3.5). The driver must still allow the caller's
// rollback to cleanly end the explicit transaction and must not return
// a poisoned connection to the pool. The next caller is on a dedicated
// sql.Conn, where database/sql cannot retry, so every follow-up probe
// must succeed.
func TestInterruptedTxDoesNotPoisonPool(t *testing.T) {
	t.Parallel()
	db, err := sql.Open("googlesqlite", ":memory:?_test=interrupt_pool")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	bg := context.Background()
	conn, err := db.Conn(bg)
	if err != nil {
		t.Fatalf("Conn: %v", err)
	}

	// A multi-second single statement, far over the 200ms budget.
	slow := "SELECT COUNT(DISTINCT CONCAT(CAST(a AS STRING), '-', CAST(b AS STRING))) " +
		"FROM UNNEST(GENERATE_ARRAY(1, 10000)) a, UNNEST(GENERATE_ARRAY(1, 10000)) b"
	ctx, cancel := context.WithTimeout(bg, 200*time.Millisecond)
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		cancel()
		t.Fatalf("BeginTx: %v", err)
	}
	start := time.Now()
	rows, err := tx.QueryContext(ctx, slow)
	if err == nil {
		rows.Close()
		cancel()
		t.Fatal("runaway statement succeeded; expected the budget to interrupt it")
	}
	if took := time.Since(start); took > 30*time.Second {
		t.Fatalf("statement returned after %s; the interrupt did not fire mid-flight", took)
	}
	_ = tx.Rollback()
	cancel()
	if err := conn.Close(); err != nil {
		t.Logf("conn.Close: %v", err)
	}

	// Every immediate follow-up probe on a dedicated conn must succeed:
	// the first one is the request that used to eat the poisoned conn.
	for i := 0; i < 3; i++ {
		probe, err := db.Conn(bg)
		if err != nil {
			t.Fatalf("probe %d: Conn: %v", i, err)
		}
		var v int
		if err := probe.QueryRowContext(bg, "SELECT 17").Scan(&v); err != nil {
			t.Fatalf("probe %d: SELECT 17: %v", i, err)
		}
		if v != 17 {
			t.Fatalf("probe %d: got %d, want 17", i, v)
		}
		probe.Close()
	}
}
