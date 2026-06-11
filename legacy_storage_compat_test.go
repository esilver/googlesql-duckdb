package googlesqlite_test

// Legacy-database compatibility for the native-storage migration (compat
// mode (b) of the migration spec): a file database whose tables were
// created BEFORE the migration — NUMERIC cells as value-layer base64
// envelopes in TEXT columns, BYTES cells as envelope ASCII in BLOB
// columns — must OPEN and READ correctly after it, while NEW tables in
// the same database use the native storage (DECIMAL(38,9) / payload
// BLOB).
//
// The fixture is built through the current binary with the test-only
// legacy-storage knob (SetForceLegacyStorageForTest): CREATE TABLE
// renders the legacy column types and stamps StorageVersion 0 into the
// catalog spec, and BulkInsert encodes the byte-identical legacy cell
// forms (the same pure-Go encoder the v0.2.16 write path used).

import (
	"context"
	"database/sql"
	"encoding/base64"
	"os"
	"testing"

	"github.com/goccy/googlesqlite"
)

func TestLegacyStorageCompat(t *testing.T) {
	// NOT parallel: the legacy-storage knob is process-global.
	dir, err := os.MkdirTemp("", "legacy-compat-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	path := dir + "/legacy.db"
	ctx := context.Background()

	// ---- Writer stand-in: a pre-migration binary creating the DB. ----
	func() {
		googlesqlite.SetForceLegacyStorageForTest(true)
		defer googlesqlite.SetForceLegacyStorageForTest(false)
		db, err := sql.Open("googlesqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		db.SetMaxOpenConns(1)
		conn, err := db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if _, err := conn.ExecContext(ctx,
			"CREATE TABLE legacy (n NUMERIC, b BYTES, s STRING)"); err != nil {
			t.Fatalf("legacy CREATE TABLE: %v", err)
		}
		// BulkInsert under the legacy knob writes the v0.2.16 cell forms:
		// NUMERIC -> TEXT envelope, BYTES -> envelope-ASCII BLOB,
		// STRING -> TEXT envelope.
		rows := [][]any{
			{"3.14", []byte("abc"), "hello"},
			{"10", []byte("zz"), "world"},
			{nil, nil, nil},
		}
		if _, err := googlesqlite.BulkInsert(ctx, conn,
			[]string{"legacy"}, []string{"n", "b", "s"}, rows); err != nil {
			t.Fatalf("legacy BulkInsert: %v", err)
		}
	}()

	// ---- Reader: a post-migration binary over the same file. ----
	db, err := sql.Open("googlesqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	// Plain reads round-trip the legacy envelope cells.
	wantString(t, db, "SELECT CAST(n AS STRING) FROM legacy WHERE s = 'hello'", "3.14")
	wantString(t, db, "SELECT s FROM legacy WHERE s = 'hello'", "hello")
	var got []byte
	if err := db.QueryRowContext(ctx,
		"SELECT b FROM legacy WHERE s = 'hello'").Scan(&got); err != nil {
		t.Fatalf("legacy BYTES scan: %v", err)
	}
	// The scan contract renders BYTES as the payload's base64 text
	// (BigQuery's wire form), regardless of which cell convention —
	// legacy envelope or native BLOB — holds it.
	if want := base64.StdEncoding.EncodeToString([]byte("abc")); string(got) != want {
		t.Errorf("legacy BYTES round-trip: got %q want %q", got, want)
	}
	wantInt64(t, db, "SELECT COUNT(*) FROM legacy", 3)

	// Engine ops over legacy NUMERIC cells keep working through the
	// envelope-tolerant read form (the COALESCE/TRY_CAST fallback arm).
	wantString(t, db, "SELECT CAST(SUM(n) AS STRING) FROM legacy", "13.14")
	wantString(t, db, "SELECT CAST(MIN(n) AS STRING) FROM legacy", "3.14")
	wantString(t, db, "SELECT CAST(MAX(n) AS STRING) FROM legacy", "10")
	wantInt64(t, db, "SELECT COUNT(*) FROM legacy WHERE n > NUMERIC '3.5'", 1)
	wantString(t, db, "SELECT CAST(n * 2 AS STRING) FROM legacy WHERE s = 'hello'", "6.28")
	// BYTES functions decode the legacy envelope-ASCII cells.
	wantInt64(t, db, "SELECT BYTE_LENGTH(b) FROM legacy WHERE s = 'hello'", 3)
	wantString(t, db, "SELECT TO_HEX(b) FROM legacy WHERE s = 'hello'", "616263")
	wantString(t, db, "SELECT CAST(MIN(b) AS STRING) FROM legacy", "abc")

	// New tables in the same database take the NATIVE storage and the
	// native fast paths.
	seamExec(t, db,
		"CREATE TABLE modern (n NUMERIC, b BYTES)",
		"INSERT INTO modern (n, b) VALUES (NUMERIC '2.5', b'xy'), (NUMERIC '4', b'aa')",
	)
	wantString(t, db, "SELECT CAST(SUM(n) AS STRING) FROM modern", "6.5")
	wantString(t, db, "SELECT CAST(n * 2 AS STRING) FROM modern WHERE n = NUMERIC '2.5'", "5")
	wantString(t, db, "SELECT CAST(MIN(b) AS STRING) FROM modern", "aa")
	var modernBytes []byte
	if err := db.QueryRowContext(ctx,
		"SELECT b FROM modern WHERE n = NUMERIC '4'").Scan(&modernBytes); err != nil {
		t.Fatalf("modern BYTES scan: %v", err)
	}
	if want := base64.StdEncoding.EncodeToString([]byte("aa")); string(modernBytes) != want {
		t.Errorf("modern BYTES round-trip: got %q want %q", modernBytes, want)
	}

	// Cross-table read: legacy and native NUMERIC compare by value in
	// one plan (the tolerant read normalizes both sides to DECIMAL).
	wantInt64(t, db,
		"SELECT COUNT(*) FROM legacy l, modern m WHERE l.n > m.n", 3)
}
