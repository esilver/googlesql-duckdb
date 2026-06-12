package googlesqlite_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	_ "github.com/goccy/googlesqlite"
)

// Issue #21: BigQuery `/` (and SAFE_DIVIDE) over NUMERIC / BIGNUMERIC
// operands lowers to the googlesqlite_divide scalar UDF, which the DuckDB
// backend never registered, so every NUMERIC division failed at bind time
// with "Catalog Error: Scalar Function with name googlesqlite_divide does
// not exist!". These tests pin the registration (raising + safe forms),
// the exact-arithmetic semantics (BigQuery NUMERIC division is exact with
// a raising zero divisor; SAFE_DIVIDE folds the zero divisor to NULL and
// keeps the exact result type), and the native float control paths that
// must stay untouched.

func openDivideTestDB(t *testing.T, name string) *sql.DB {
	t.Helper()
	db, err := sql.Open("googlesqlite", "file:"+name+"?mode=memory&cache=private")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return db
}

func queryOneString(t *testing.T, db *sql.DB, query string) string {
	t.Helper()
	var got string
	if err := db.QueryRowContext(context.Background(), query).Scan(&got); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	return got
}

// TestIssue21_NumericDivide is the exact repro from the issue report.
func TestIssue21_NumericDivide(t *testing.T) {
	t.Parallel()
	db := openDivideTestDB(t, "issue21_repro")
	got := queryOneString(t, db, "SELECT CAST(CAST(2 AS NUMERIC) / 1000 AS STRING)")
	if got != "0.002" {
		t.Fatalf("CAST(2 AS NUMERIC) / 1000 = %q, want %q", got, "0.002")
	}
}

// TestIssue21_BigNumericDivide covers the BIGNUMERIC operand path: the
// operand carrier is the value-layer envelope VARCHAR (BIGNUMERIC exceeds
// DuckDB's 38-digit DECIMAL), so the UDF decode path differs from the
// native duckdb.Decimal NUMERIC carrier.
func TestIssue21_BigNumericDivide(t *testing.T) {
	t.Parallel()
	db := openDivideTestDB(t, "issue21_bignumeric")
	got := queryOneString(t, db, "SELECT CAST(CAST(2 AS BIGNUMERIC) / 1000 AS STRING)")
	if got != "0.002" {
		t.Fatalf("CAST(2 AS BIGNUMERIC) / 1000 = %q, want %q", got, "0.002")
	}
}

// TestIssue21_NumericDivideExactScale pins the exact-arithmetic rendering:
// the quotient is held as an exact rational and renders at NUMERIC scale 9
// with halves rounded away from zero (big.Rat.FloatString semantics,
// matching BigQuery's NUMERIC division result scale and the legacy SQLite
// backend's observable output).
func TestIssue21_NumericDivideExactScale(t *testing.T) {
	t.Parallel()
	db := openDivideTestDB(t, "issue21_scale")
	if got := queryOneString(t, db, "SELECT CAST(CAST(1 AS NUMERIC) / 3 AS STRING)"); got != "0.333333333" {
		t.Fatalf("1/3 = %q, want %q", got, "0.333333333")
	}
	if got := queryOneString(t, db, "SELECT CAST(CAST(2 AS NUMERIC) / 3 AS STRING)"); got != "0.666666667" {
		t.Fatalf("2/3 = %q, want %q", got, "0.666666667")
	}
}

// TestIssue21_NumericDivideByZeroErrors: BigQuery `/` over NUMERIC ERRORS
// on a zero divisor (it must not yield Inf or NULL).
func TestIssue21_NumericDivideByZeroErrors(t *testing.T) {
	t.Parallel()
	db := openDivideTestDB(t, "issue21_divzero")
	var got string
	err := db.QueryRowContext(context.Background(),
		"SELECT CAST(CAST(2 AS NUMERIC) / 0 AS STRING)").Scan(&got)
	if err == nil {
		t.Fatalf("NUMERIC / 0 returned %q, want a division-by-zero error", got)
	}
	if !strings.Contains(strings.ToLower(err.Error()), "zero") {
		t.Fatalf("NUMERIC / 0 error %q does not mention the zero divisor", err)
	}
}

// TestIssue21_SafeDivideNumericZeroIsNull: SAFE_DIVIDE over NUMERIC folds
// the zero divisor to NULL instead of raising.
func TestIssue21_SafeDivideNumericZeroIsNull(t *testing.T) {
	t.Parallel()
	db := openDivideTestDB(t, "issue21_safezero")
	var got sql.NullString
	err := db.QueryRowContext(context.Background(),
		"SELECT CAST(SAFE_DIVIDE(CAST(2 AS NUMERIC), 0) AS STRING)").Scan(&got)
	if err != nil {
		t.Fatalf("SAFE_DIVIDE(NUMERIC, 0): %v", err)
	}
	if got.Valid {
		t.Fatalf("SAFE_DIVIDE(NUMERIC, 0) = %q, want NULL", got.String)
	}
}

// TestIssue21_SafeDivideNumericNormal: the non-zero SAFE_DIVIDE path over
// NUMERIC returns the exact NUMERIC quotient (not a DOUBLE approximation).
func TestIssue21_SafeDivideNumericNormal(t *testing.T) {
	t.Parallel()
	db := openDivideTestDB(t, "issue21_safenormal")
	got := queryOneString(t, db, "SELECT CAST(SAFE_DIVIDE(CAST(2 AS NUMERIC), 1000) AS STRING)")
	if got != "0.002" {
		t.Fatalf("SAFE_DIVIDE(CAST(2 AS NUMERIC), 1000) = %q, want %q", got, "0.002")
	}
}

// TestIssue21_IfErrorNumericDivide: the safe-eval wrap (IFERROR) routes the
// raising $divide through googlesqlite_safe_divide, so a zero divisor folds
// into the fallback arm instead of aborting the statement.
func TestIssue21_IfErrorNumericDivide(t *testing.T) {
	t.Parallel()
	db := openDivideTestDB(t, "issue21_iferror")
	got := queryOneString(t, db,
		"SELECT IFERROR(CAST(CAST(2 AS NUMERIC) / 0 AS STRING), 'caught')")
	if got != "caught" {
		t.Fatalf("IFERROR(NUMERIC / 0, 'caught') = %q, want %q", got, "caught")
	}
}

// TestIssue21_SumCoalesceDivide is the dbt staging-model shape from the
// dbt build: money math COALESCE(CAST(col AS NUMERIC), 0) / 1000
// aggregated with SUM.
func TestIssue21_SumCoalesceDivide(t *testing.T) {
	t.Parallel()
	db := openDivideTestDB(t, "issue21_dbt")
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, "CREATE TABLE issue21_money (c INT64)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx,
		"INSERT INTO issue21_money (c) VALUES (2000), (3000), (NULL)"); err != nil {
		t.Fatal(err)
	}
	got := queryOneString(t, db,
		"SELECT CAST(SUM(COALESCE(CAST(c AS NUMERIC), 0) / 1000) AS STRING) FROM issue21_money")
	if got != "5" {
		t.Fatalf("SUM(COALESCE(CAST(c AS NUMERIC), 0) / 1000) = %q, want %q", got, "5")
	}
}

// TestIssue21_IfNullNumericCarrierMix: IFNULL is the same dual-carrier
// shape as the COALESCE in the dbt models (envelope VARCHAR from
// googlesqlite_cast vs the native DECIMAL(38,9) literal), so its
// NUMERIC-kind arguments take the same normalization.
func TestIssue21_IfNullNumericCarrierMix(t *testing.T) {
	t.Parallel()
	db := openDivideTestDB(t, "issue21_ifnull")
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, "CREATE TABLE issue21_ifnull_t (c INT64)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx,
		"INSERT INTO issue21_ifnull_t (c) VALUES (2000), (NULL)"); err != nil {
		t.Fatal(err)
	}
	got := queryOneString(t, db,
		"SELECT CAST(SUM(IFNULL(CAST(c AS NUMERIC), 0) / 1000) AS STRING) FROM issue21_ifnull_t")
	if got != "2" {
		t.Fatalf("SUM(IFNULL(CAST(c AS NUMERIC), 0) / 1000) = %q, want %q", got, "2")
	}
}

// TestIssue21_FloatDivideControl pins the native float lowering: `/` over
// non-NUMERIC operands stays on DuckDB's native DOUBLE division and must
// not be disturbed by the UDF registration.
func TestIssue21_FloatDivideControl(t *testing.T) {
	t.Parallel()
	db := openDivideTestDB(t, "issue21_floatctl")
	var got float64
	if err := db.QueryRowContext(context.Background(),
		"SELECT 2.0 / 1000").Scan(&got); err != nil {
		t.Fatalf("float divide control: %v", err)
	}
	if got != 0.002 {
		t.Fatalf("2.0 / 1000 = %v, want 0.002", got)
	}
	// SAFE_DIVIDE over non-NUMERIC operands keeps the native DOUBLE
	// CASE lowering: NULL on zero, plain division otherwise.
	var safe sql.NullFloat64
	if err := db.QueryRowContext(context.Background(),
		"SELECT SAFE_DIVIDE(1, 0)").Scan(&safe); err != nil {
		t.Fatalf("SAFE_DIVIDE(1, 0): %v", err)
	}
	if safe.Valid {
		t.Fatalf("SAFE_DIVIDE(1, 0) = %v, want NULL", safe.Float64)
	}
	if err := db.QueryRowContext(context.Background(),
		"SELECT SAFE_DIVIDE(10.0, 4)").Scan(&got); err != nil {
		t.Fatalf("SAFE_DIVIDE(10.0, 4): %v", err)
	}
	if got != 2.5 {
		t.Fatalf("SAFE_DIVIDE(10.0, 4) = %v, want 2.5", got)
	}
}

// TestIssue21_NumericAddControl pins that NUMERIC `+` keeps its native
// exact-DECIMAL lowering (the divide fix must not widen the UDF fallback).
func TestIssue21_NumericAddControl(t *testing.T) {
	t.Parallel()
	db := openDivideTestDB(t, "issue21_addctl")
	got := queryOneString(t, db,
		"SELECT CAST(CAST(1 AS NUMERIC) + CAST(2 AS NUMERIC) AS STRING)")
	if got != "3" {
		t.Fatalf("NUMERIC 1 + 2 = %q, want %q", got, "3")
	}
}
