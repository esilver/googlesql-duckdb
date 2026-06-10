package googlesqlite_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

// Regression tests for issue #10: after CREATE OR REPLACE TABLE with a
// different schema, the analyzer's resolver kept seeing the OLD
// SimpleTable handle for the table (plain DROP+CREATE worked). The
// catalog layer replaced the *TableSpec, but the wasm-side
// SimpleCatalog registration wasn't refreshed on the REPLACE path —
// addTableSpec's exists-branch only swapped Go-side map entries.

// TestCreateOrReplaceTableSchemaChange is the canonical repro:
// CREATE TABLE t (a INT64); CREATE OR REPLACE TABLE t (b STRING);
// SELECT b FROM t must resolve against the NEW column set, and the
// old column must be gone.
func TestCreateOrReplaceTableSchemaChange(t *testing.T) {
	t.Parallel()
	db, err := sql.Open("googlesqlite", ":memory:?_test=cor_schema_change")
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

	if _, err := conn.ExecContext(ctx, "CREATE TABLE cor_t (a INT64)"); err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}
	if _, err := conn.ExecContext(ctx, "INSERT INTO cor_t (a) VALUES (1)"); err != nil {
		t.Fatalf("INSERT old schema: %v", err)
	}
	// Make sure the old schema actually resolved once before the
	// replace, so we know a stale handle would otherwise be reused.
	var a int64
	if err := conn.QueryRowContext(ctx, "SELECT a FROM cor_t").Scan(&a); err != nil {
		t.Fatalf("SELECT a (old schema): %v", err)
	}

	if _, err := conn.ExecContext(ctx, "CREATE OR REPLACE TABLE cor_t (b STRING)"); err != nil {
		t.Fatalf("CREATE OR REPLACE TABLE: %v", err)
	}

	// New column must resolve.
	if _, err := conn.ExecContext(ctx, "INSERT INTO cor_t (b) VALUES ('hello')"); err != nil {
		t.Fatalf("INSERT new schema: %v", err)
	}
	var b string
	if err := conn.QueryRowContext(ctx, "SELECT b FROM cor_t").Scan(&b); err != nil {
		t.Fatalf("SELECT b FROM cor_t after CREATE OR REPLACE: %v", err)
	}
	if b != "hello" {
		t.Fatalf("SELECT b = %q, want %q", b, "hello")
	}

	// Old column must NOT resolve anymore.
	var stale any
	err = conn.QueryRowContext(ctx, "SELECT a FROM cor_t").Scan(&stale)
	if err == nil {
		t.Fatalf("SELECT a FROM cor_t succeeded after replace; resolver still sees the old schema (got %v)", stale)
	}
	if !strings.Contains(strings.ToLower(err.Error()), "unrecognized name") &&
		!strings.Contains(strings.ToLower(err.Error()), "not found") {
		t.Logf("note: stale-column error text was: %v", err)
	}
}

// TestCreateOrReplaceTableSameSchema: replacing with an identical
// schema must keep working and reset the rows (the storage table is
// dropped and recreated).
func TestCreateOrReplaceTableSameSchema(t *testing.T) {
	t.Parallel()
	db, err := sql.Open("googlesqlite", ":memory:?_test=cor_same_schema")
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

	if _, err := conn.ExecContext(ctx, "CREATE TABLE cor_same (x INT64)"); err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}
	if _, err := conn.ExecContext(ctx, "INSERT INTO cor_same (x) VALUES (42)"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}
	if _, err := conn.ExecContext(ctx, "CREATE OR REPLACE TABLE cor_same (x INT64)"); err != nil {
		t.Fatalf("CREATE OR REPLACE TABLE (same schema): %v", err)
	}
	var n int64
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM cor_same").Scan(&n); err != nil {
		t.Fatalf("SELECT COUNT(*): %v", err)
	}
	if n != 0 {
		t.Fatalf("COUNT(*) after same-schema replace = %d, want 0 (table recreated)", n)
	}
	if _, err := conn.ExecContext(ctx, "INSERT INTO cor_same (x) VALUES (7)"); err != nil {
		t.Fatalf("INSERT after replace: %v", err)
	}
	var x int64
	if err := conn.QueryRowContext(ctx, "SELECT x FROM cor_same").Scan(&x); err != nil {
		t.Fatalf("SELECT x after replace: %v", err)
	}
	if x != 7 {
		t.Fatalf("SELECT x = %d, want 7", x)
	}
}

// TestCreateOrReplaceTableNonexistent: OR REPLACE on a table that does
// not exist yet must act as a plain CREATE.
func TestCreateOrReplaceTableNonexistent(t *testing.T) {
	t.Parallel()
	db, err := sql.Open("googlesqlite", ":memory:?_test=cor_nonexistent")
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

	if _, err := conn.ExecContext(ctx, "CREATE OR REPLACE TABLE cor_fresh (v STRING)"); err != nil {
		t.Fatalf("CREATE OR REPLACE TABLE (nonexistent): %v", err)
	}
	if _, err := conn.ExecContext(ctx, "INSERT INTO cor_fresh (v) VALUES ('ok')"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}
	var v string
	if err := conn.QueryRowContext(ctx, "SELECT v FROM cor_fresh").Scan(&v); err != nil {
		t.Fatalf("SELECT v: %v", err)
	}
	if v != "ok" {
		t.Fatalf("SELECT v = %q, want %q", v, "ok")
	}
}

// TestCreateOrReplaceViewSchemaChange: CREATE OR REPLACE VIEW shares
// AddNewTableSpec with the table path, so a view replaced with a
// different output column set must also resolve fresh.
func TestCreateOrReplaceViewSchemaChange(t *testing.T) {
	t.Parallel()
	db, err := sql.Open("googlesqlite", ":memory:?_test=cor_view_change")
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

	if _, err := conn.ExecContext(ctx, "CREATE TABLE cor_vbase (a INT64, b STRING)"); err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}
	if _, err := conn.ExecContext(ctx, "INSERT INTO cor_vbase (a, b) VALUES (5, 'five')"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}
	if _, err := conn.ExecContext(ctx, "CREATE VIEW cor_v AS SELECT a FROM cor_vbase"); err != nil {
		t.Fatalf("CREATE VIEW: %v", err)
	}
	var a int64
	if err := conn.QueryRowContext(ctx, "SELECT a FROM cor_v").Scan(&a); err != nil {
		t.Fatalf("SELECT a FROM cor_v: %v", err)
	}

	if _, err := conn.ExecContext(ctx, "CREATE OR REPLACE VIEW cor_v AS SELECT b FROM cor_vbase"); err != nil {
		t.Fatalf("CREATE OR REPLACE VIEW: %v", err)
	}
	var b string
	if err := conn.QueryRowContext(ctx, "SELECT b FROM cor_v").Scan(&b); err != nil {
		t.Fatalf("SELECT b FROM cor_v after CREATE OR REPLACE VIEW: %v", err)
	}
	if b != "five" {
		t.Fatalf("SELECT b = %q, want %q", b, "five")
	}
	var stale any
	if err := conn.QueryRowContext(ctx, "SELECT a FROM cor_v").Scan(&stale); err == nil {
		t.Fatalf("SELECT a FROM cor_v succeeded after replace; resolver still sees the old view schema (got %v)", stale)
	}
}
