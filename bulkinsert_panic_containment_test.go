package googlesqlite

// Regression tests for issue #20: a panic thrown across the analyzer
// wasm boundary inside BulkInsert's Conn.Raw callback, while the same
// connection holds an open transaction, must NOT escape into stdlib
// database/sql. Before the containment, stdlib Conn.Raw's deferred
// release treated the panic as driver.ErrBadConn and synchronously
// called Conn.close, which blocks forever on the closemu write lock
// against the open transaction's read lock — a permanent mid-unwind
// self-deadlock that orphaned every caller-held mutex (observed live
// as a wedged bigquery-emulator after one analyzer bad_alloc, #19).
//
// The tests are white-box (package googlesqlite) so they can assert
// the poison flag the pool's Validator reads. Every potentially
// deadlocking step runs in a goroutine guarded by a timeout, so a
// regression fails the test in seconds instead of hanging the suite.

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"
	"time"

	internal "github.com/goccy/googlesqlite/internal"
)

// containmentStepTimeout bounds each step that deadlocked before the
// fix. Generous against loaded CI boxes, tiny against a real
// regression (which previously hung forever).
const containmentStepTimeout = 60 * time.Second

// runWithTimeout runs fn in a goroutine and returns its error, or
// fails the test if fn does not return within containmentStepTimeout.
func runWithTimeout(t *testing.T, step string, fn func() error) error {
	t.Helper()
	ch := make(chan error, 1)
	go func() { ch <- fn() }()
	select {
	case err := <-ch:
		return err
	case <-time.After(containmentStepTimeout):
		t.Fatalf("issue #20 regression: %s did not return within %v — the panic escaped into stdlib Conn.Raw and parked in Conn.close against the open transaction's closemu read lock", step, containmentStepTimeout)
		return nil // unreachable
	}
}

// TestBulkInsertBoundaryPanicInsideOpenTx synthesizes the exact live
// wedge shape: open transaction on a *sql.Conn, package-level
// BulkInsert on the SAME conn, panic at internal.BulkInsert entry
// (where the analyzer abort surfaced). Asserts the full containment
// contract: BulkInsert returns an ordinary error (non-ErrBadConn, no
// hang), the transaction rollback completes, the Conn is poisoned so
// the pool's Validator discards it, and the next statement on the db
// runs on a fresh conn.
func TestBulkInsertBoundaryPanicInsideOpenTx(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("googlesqlite", ":memory:?_test=bulkinsert_panic_containment")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// Capture the driver Conn so the poison flag is observable after
	// the unwind.
	var gc *Conn
	if err := conn.Raw(func(dc any) error {
		gc, _ = dc.(*Conn)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if gc == nil {
		t.Fatal("driver connection is not a *googlesqlite.Conn")
	}

	// Step 1 of the wedge: the open transaction holds closemu.RLock
	// inside database/sql until the tx ends.
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Step 2+3: BulkInsert on the same conn; the hook stands in for
	// the analyzer abort (bad_alloc surfacing as a Go panic) at the
	// position the live dump showed — inside internal.BulkInsert under
	// the Raw callback.
	internal.SetBulkInsertEntryHookForTest(func() {
		panic("synthetic analyzer abort: bad_alloc across the wasm boundary")
	})
	defer internal.SetBulkInsertEntryHookForTest(nil)

	bulkErr := runWithTimeout(t, "BulkInsert with a panic at the wasm boundary", func() error {
		_, err := BulkInsert(ctx, conn, []string{"p", "d", "t"}, []string{"a"}, [][]any{{int64(1)}})
		return err
	})
	if bulkErr == nil {
		t.Fatal("BulkInsert returned nil; the synthetic boundary panic must surface as an error")
	}
	if errors.Is(bulkErr, driver.ErrBadConn) {
		t.Fatalf("BulkInsert returned an error satisfying driver.ErrBadConn (%v); stdlib Raw reacts to that with a synchronous Conn.close, which deadlocks against the open transaction", bulkErr)
	}
	if !strings.Contains(bulkErr.Error(), "panic") || !strings.Contains(bulkErr.Error(), "bad_alloc") {
		t.Fatalf("BulkInsert error does not carry the recovered panic value: %v", bulkErr)
	}

	// The recovery must poison the Conn immediately: the analyzer/wasm
	// state may be unusable after the abort.
	if !gc.dead.Load() {
		t.Fatal("Conn was not poisoned by the boundary-panic recovery; the pool would hand the broken analyzer state to the next caller")
	}

	// The transaction must unwind normally through Rollback — before
	// the containment this was impossible (the panic was parked inside
	// Raw's deferred release).
	internal.SetBulkInsertEntryHookForTest(nil)
	if err := runWithTimeout(t, "Tx.Rollback after the contained panic", tx.Rollback); err != nil {
		t.Fatalf("rollback after the contained panic failed: %v", err)
	}

	// Pool-discard contract: the Validator must report the poisoned
	// Conn invalid so database/sql prunes it at put-back/fetch.
	if gc.IsValid() {
		t.Fatal("IsValid() = true on the poisoned Conn; the pool would reuse it")
	}

	// Returning the conn must not wedge either, and the NEXT statement
	// on the db must run (on a fresh conn from the pool).
	if err := runWithTimeout(t, "sql.Conn.Close after the contained panic", conn.Close); err != nil {
		t.Fatalf("conn.Close: %v", err)
	}
	var one int64
	if err := runWithTimeout(t, "next statement after the contained panic", func() error {
		return db.QueryRowContext(ctx, "SELECT 1").Scan(&one)
	}); err != nil {
		t.Fatalf("next statement after the contained panic failed: %v", err)
	}
	if one != 1 {
		t.Fatalf("SELECT 1 = %d", one)
	}
}

// TestConnBulkInsertPanicIsContainedWithoutTx covers the method-level
// seam on its own: Conn.BulkInsert (the entry the bigquery-emulator
// calls inside its own Raw callback) must convert a boundary panic to
// an error and poison the conn even with no transaction open.
func TestConnBulkInsertPanicIsContainedWithoutTx(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("googlesqlite", ":memory:?_test=bulkinsert_panic_no_tx")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	internal.SetBulkInsertEntryHookForTest(func() {
		panic("synthetic analyzer abort outside a transaction")
	})
	defer internal.SetBulkInsertEntryHookForTest(nil)

	var gc *Conn
	cbErr := runWithTimeout(t, "Conn.BulkInsert with a panic at the wasm boundary", func() error {
		return conn.Raw(func(dc any) error {
			var ok bool
			gc, ok = dc.(*Conn)
			if !ok {
				t.Errorf("driver connection is %T, not *googlesqlite.Conn", dc)
				return nil
			}
			_, err := gc.BulkInsert(ctx, []string{"p", "d", "t"}, []string{"a"}, [][]any{{int64(1)}})
			return err
		})
	})
	if cbErr == nil {
		t.Fatal("Conn.BulkInsert returned nil; the synthetic boundary panic must surface as an error")
	}
	if errors.Is(cbErr, driver.ErrBadConn) {
		t.Fatalf("Conn.BulkInsert surfaced driver.ErrBadConn (%v) out of a Raw callback", cbErr)
	}
	if gc == nil || !gc.dead.Load() {
		t.Fatal("Conn was not poisoned by the method-level recovery")
	}
}
