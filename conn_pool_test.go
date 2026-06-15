package googlesqlite_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/goccy/googlesqlite"
)

// TestCancelledTxDoesNotPoisonPooledConn is the regression guard for the
// bigquery-emulator #478 "connection is already closed" / "driver: bad
// connection" cascade.
//
// The emulator runs each request as db.Conn(ctx) + BeginTx(ctx) +
// query(ctx). Before the fix, BeginTx handed the request context to the
// inner *sql.Conn's transaction; when the request was cancelled
// mid-statement database/sql's tx watcher ran Rollback(discardConn=true)
// and closed the inner conn asynchronously. Our own operation had
// already returned context.Canceled (not sql.ErrConnDone), so the Conn
// was returned to the pool still looking healthy. The next caller drew
// that poisoned Conn and saw sql.ErrConnDone -> driver.ErrBadConn surface
// as a 500 — and because the emulator uses dedicated db.Conn() handles,
// database/sql performs no bad-conn retry, so the error reached the
// client.
//
// The fix decouples the inner transaction from the request context, so a
// cancellation can never close the inner conn out from under us. This
// test drives a burst of concurrently-cancelled transactions and then
// asserts that every subsequent probe on a freshly drawn connection
// succeeds — no ErrBadConn, no ErrConnDone.
func TestCancelledTxDoesNotPoisonPooledConn(t *testing.T) {
	db, err := sql.Open("googlesqlite", ":memory:?cache=shared&_test=cancel_poison")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec("CREATE TABLE t (id INT64)"); err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}

	// A query heavy enough that a few-millisecond deadline reliably
	// cancels it mid-statement.
	const slow = "SELECT COUNT(*) FROM (SELECT x FROM UNNEST(GENERATE_ARRAY(1,4000000)) x)"

	var poisoned int64
	for round := 0; round < 8; round++ {
		// Burst of concurrent BeginTx+slow-query, each cancelled in flight,
		// mirroring the emulator's db.Conn(ctx)+BeginTx(ctx) request shape.
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
				defer cancel()
				conn, err := db.Conn(ctx)
				if err != nil {
					return
				}
				defer conn.Close()
				tx, err := conn.BeginTx(ctx, nil)
				if err != nil {
					return
				}
				_, _ = tx.QueryContext(ctx, slow)
				_ = tx.Rollback()
			}()
		}
		wg.Wait()

		// Probe exactly as the emulator's next request would: a dedicated
		// connection (no bad-conn retry) running a trivial query.
		for p := 0; p < 25; p++ {
			conn, err := db.Conn(context.Background())
			if err != nil {
				if isPoison(err) {
					atomic.AddInt64(&poisoned, 1)
					t.Errorf("round %d probe %d: db.Conn: %v", round, p, err)
				}
				continue
			}
			var n int
			err = conn.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM t").Scan(&n)
			if err != nil && isPoison(err) {
				atomic.AddInt64(&poisoned, 1)
				t.Errorf("round %d probe %d: %v", round, p, err)
			}
			conn.Close()
		}
	}
	if poisoned > 0 {
		t.Fatalf("%d probe(s) hit a poisoned connection after concurrent cancellations; the pool must never serve a conn whose inner handle was closed by a cancelled tx", poisoned)
	}
}

func TestBeginTxDuckDBOptionsAndCanceledContext(t *testing.T) {
	db, err := sql.Open("googlesqlite", ":memory:?_test=begin_tx_duckdb_options")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	if _, err := db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true}); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("read-only BeginTx error = %v; want read-only rejection", err)
	}
	if _, err := db.BeginTx(context.Background(), &sql.TxOptions{Isolation: sql.LevelSerializable}); err == nil || !strings.Contains(err.Error(), "unsupported isolation") {
		t.Fatalf("isolation BeginTx error = %v; want unsupported isolation rejection", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := db.BeginTx(ctx, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled BeginTx error = %v; want context.Canceled", err)
	}
}

func isPoison(err error) bool {
	return errors.Is(err, driver.ErrBadConn) ||
		errors.Is(err, sql.ErrConnDone) ||
		err.Error() == "driver: bad connection" ||
		err.Error() == "sql: connection is already closed"
}

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
