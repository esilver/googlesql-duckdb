package googlesqlite

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	internal "github.com/goccy/googlesqlite/internal"
)

func TestDuckDBDSNPreservesMemoryLimitParams(t *testing.T) {
	tests := []struct {
		name string
		dsn  string
		want string
	}{
		{
			name: "file max memory",
			dsn:  "file:/tmp/example-project.db?cache=shared&max_memory=3GB",
			want: "/tmp/example-project.db?max_memory=3GB",
		},
		{
			name: "file memory limit alias",
			dsn:  "file:/tmp/example-project.db?cache=shared&memory_limit=3072MB",
			want: "/tmp/example-project.db?memory_limit=3072MB",
		},
		{
			name: "memory max memory",
			dsn:  "file::memory:?cache=shared&max_memory=512MB",
			want: ":memory:?max_memory=512MB",
		},
		{
			name: "sqlite params stripped",
			dsn:  "file:/tmp/example-project.db?cache=shared&mode=memory",
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := duckdbDSN(tt.dsn); got != tt.want {
				t.Fatalf("duckdbDSN(%q) = %q; want %q", tt.dsn, got, tt.want)
			}
		})
	}
}

// TestConn_IsValid_And_TranslateConnDone covers the two pieces
// database/sql's pool relies on to drop a Conn whose inner *sql.Conn
// was closed under us (typically by a cancelled transaction's
// discardConn rollback): IsValid (cheap, called on every pool fetch)
// and the translateConnDone helper that maps sql.ErrConnDone to
// driver.ErrBadConn and flips the dead flag.
//
// Driving translateConnDone directly with the sentinel - rather than
// trying to time a real cancellation - keeps this test deterministic;
// the end-to-end shape (cancel mid-statement on one pooled conn, the
// pool keeps serving) is exercised by TestRegression_WedgedSlotRecovery
// in regression_test.go.
//
// Ported from upstream goccy/googlesqlite bf326675 (fork issue #12).
func TestConn_IsValid_And_TranslateConnDone(t *testing.T) {
	t.Run("fresh Conn is valid", func(t *testing.T) {
		c := &Conn{}
		if !c.IsValid() {
			t.Fatal("a freshly-allocated Conn must report IsValid()=true; otherwise database/sql would never put us in the pool")
		}
	})

	t.Run("nil error passes through and does not flip dead", func(t *testing.T) {
		c := &Conn{}
		if got := c.translateConnDone(nil); got != nil {
			t.Fatalf("translateConnDone(nil) = %v; want nil", got)
		}
		if !c.IsValid() {
			t.Fatal("Conn was marked dead after a nil error; the flag is only meant for the ErrConnDone path")
		}
	})

	t.Run("unrelated error passes through and does not flip dead", func(t *testing.T) {
		c := &Conn{}
		sentinel := errors.New("boom")
		if got := c.translateConnDone(sentinel); !errors.Is(got, sentinel) {
			t.Fatalf("translateConnDone of an unrelated error mutated it: got %v", got)
		}
		if !c.IsValid() {
			t.Fatal("Conn was marked dead after an unrelated error; only ErrConnDone may flip the flag")
		}
	})

	t.Run("ErrConnDone is mapped to ErrBadConn and flips dead", func(t *testing.T) {
		c := &Conn{}
		out := c.translateConnDone(sql.ErrConnDone)
		if !errors.Is(out, driver.ErrBadConn) {
			t.Fatalf("translateConnDone(sql.ErrConnDone) = %v; want driver.ErrBadConn so database/sql discards-and-retries", out)
		}
		if c.IsValid() {
			t.Fatal("Conn must be marked dead after observing ErrConnDone, so the next pool fetch is skipped via IsValid")
		}
	})

	t.Run("wrapped ErrConnDone is mapped to ErrBadConn", func(t *testing.T) {
		c := &Conn{}
		wrapped := fmt.Errorf("upper-layer context: %w", sql.ErrConnDone)
		out := c.translateConnDone(wrapped)
		if !errors.Is(out, driver.ErrBadConn) {
			t.Fatalf("translateConnDone(wrapped) = %v; want driver.ErrBadConn (errors.Is must traverse Unwrap)", out)
		}
		if c.IsValid() {
			t.Fatal("Conn must be marked dead even when ErrConnDone arrives wrapped")
		}
	})

	t.Run("ErrorGroup carrying ErrConnDone is mapped to ErrBadConn", func(t *testing.T) {
		// ExecContext / QueryContext fold cleanup-time errors into an
		// internal.ErrorGroup; the group's Unwrap() []error is what
		// lets errors.Is reach the buried sentinel. Without that
		// hookup the dead-conn signal would be silently lost when any
		// cleanup error landed on the same path.
		c := &Conn{}
		eg := &internal.ErrorGroup{}
		eg.Add(sql.ErrConnDone)
		eg.Add(errors.New("cleanup also failed"))
		out := c.translateConnDone(eg)
		if !errors.Is(out, driver.ErrBadConn) {
			t.Fatalf("translateConnDone(ErrorGroup) = %v; want driver.ErrBadConn (Unwrap() []error must surface the sentinel)", out)
		}
		if c.IsValid() {
			t.Fatal("Conn must be marked dead when an ErrorGroup carries ErrConnDone")
		}
	})

	t.Run("dead flag is sticky across translate calls", func(t *testing.T) {
		c := &Conn{}
		_ = c.translateConnDone(sql.ErrConnDone)
		if c.IsValid() {
			t.Fatal("setup: expected dead after first ErrConnDone")
		}
		// A subsequent benign error must not un-deaden us: once a Conn
		// has been seen poisoned the flag stays set so the pool never
		// hands it out again.
		_ = c.translateConnDone(nil)
		if c.IsValid() {
			t.Fatal("Conn un-deadened after a nil translate; the flag must be sticky")
		}
		_ = c.translateConnDone(errors.New("unrelated"))
		if c.IsValid() {
			t.Fatal("Conn un-deadened after an unrelated translate; the flag must be sticky")
		}
	})
}

// Regression tests for a panic thrown across the analyzer wasm boundary
// inside BulkInsert's Conn.Raw callback, while the same connection holds
// an open transaction: it must NOT escape into stdlib database/sql.
// Before the containment, stdlib Conn.Raw's deferred release treated the
// panic as driver.ErrBadConn and synchronously called Conn.close, which
// blocks forever on the closemu write lock against the open
// transaction's read lock - a permanent mid-unwind self-deadlock that
// orphaned every caller-held mutex (observed live as a wedged
// bigquery-emulator after one analyzer bad_alloc). Provenance: fork
// issues #19/#20.
//
// The tests are white-box (package googlesqlite) so they can assert
// the poison flag the pool's Validator reads. Every potentially
// deadlocking step runs in a goroutine guarded by a timeout, so a
// regression fails the test in seconds instead of hanging the suite.

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
