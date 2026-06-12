package googlesqlite_test

import (
	"context"
	"database/sql"
	"testing"

	_ "github.com/goccy/googlesqlite"
)

// Issue #22: a CASE expression mixing the native DECIMAL(38,9) carrier
// (NUMERIC division results since #21, native literals) with the
// value-layer envelope VARCHAR carrier (googlesqlite_cast / UDF results)
// in sibling branches failed at bind time with "Binder Error: Cannot mix
// values of type DECIMAL(38,9) and VARCHAR in CASE expression". The
// v0.2.25 carrier harmonization covered COALESCE / IFNULL only; these
// tests pin the same numericStorageReadSQL normalization for the rest of
// the multi-branch family: CASE (searched + value-match, branch results
// AND value/comparand operands), IF, NULLIF, GREATEST, LEAST, IFERROR,
// and the already-normalized IN-list control.

func openIssue22DB(t *testing.T, name string) *sql.DB {
	t.Helper()
	db, err := sql.Open("googlesqlite", "file:"+name+"?mode=memory&cache=private")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return db
}

func issue22QueryString(t *testing.T, db *sql.DB, query string) string {
	t.Helper()
	var got string
	if err := db.QueryRowContext(context.Background(), query).Scan(&got); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	return got
}

// TestIssue22_CaseSearchedCarrierMix is the exact repro from the issue
// report: the THEN branch carries the division result, the ELSE branch a
// CAST literal — the two NUMERIC carrier forms.
func TestIssue22_CaseSearchedCarrierMix(t *testing.T) {
	t.Parallel()
	db := openIssue22DB(t, "issue22_repro")
	got := issue22QueryString(t, db,
		"SELECT CAST(CASE WHEN TRUE THEN CAST(2 AS NUMERIC) / 1000 ELSE CAST(0 AS NUMERIC) END AS STRING)")
	if got != "0.002" {
		t.Fatalf("searched CASE carrier mix = %q, want %q", got, "0.002")
	}
}

// TestIssue22_CaseSearchedReversedBranchOrder swaps the carriers across
// the branches (envelope-producing division in ELSE, CAST literal in
// THEN) so the unification cannot depend on branch position.
func TestIssue22_CaseSearchedReversedBranchOrder(t *testing.T) {
	t.Parallel()
	db := openIssue22DB(t, "issue22_reversed")
	got := issue22QueryString(t, db,
		"SELECT CAST(CASE WHEN FALSE THEN CAST(0 AS NUMERIC) ELSE CAST(2 AS NUMERIC) / 1000 END AS STRING)")
	if got != "0.002" {
		t.Fatalf("reversed-branch CASE carrier mix = %q, want %q", got, "0.002")
	}
}

// TestIssue22_CaseValueMatchResultMix covers the value-match (simple)
// CASE form with mixed-carrier branch results.
func TestIssue22_CaseValueMatchResultMix(t *testing.T) {
	t.Parallel()
	db := openIssue22DB(t, "issue22_valmatch")
	got := issue22QueryString(t, db,
		"SELECT CAST(CASE 'a' WHEN 'a' THEN CAST(2 AS NUMERIC) / 1000 ELSE CAST(0 AS NUMERIC) END AS STRING)")
	if got != "0.002" {
		t.Fatalf("value-match CASE result carrier mix = %q, want %q", got, "0.002")
	}
}

// TestIssue22_CaseValueMatchComparandMix exercises the OTHER slot of the
// value-match form: the CASE value is the envelope-carried division
// result while the WHEN comparand is a cast NUMERIC, so the equality
// the binder builds between them must also be carrier-unified (and must
// compare by VALUE, not by envelope text).
func TestIssue22_CaseValueMatchComparandMix(t *testing.T) {
	t.Parallel()
	db := openIssue22DB(t, "issue22_comparand")
	got := issue22QueryString(t, db,
		"SELECT CASE CAST(2 AS NUMERIC) / 1000 WHEN CAST(0.002 AS NUMERIC) THEN 'hit' ELSE 'miss' END")
	if got != "hit" {
		t.Fatalf("value-match CASE comparand carrier mix = %q, want %q", got, "hit")
	}
}

// TestIssue22_NestedCaseCarrierMix nests a mixed-carrier CASE inside
// another CASE whose sibling branch is a third NUMERIC carrier shape.
func TestIssue22_NestedCaseCarrierMix(t *testing.T) {
	t.Parallel()
	db := openIssue22DB(t, "issue22_nested")
	got := issue22QueryString(t, db,
		`SELECT CAST(
			CASE WHEN TRUE THEN
				CASE WHEN FALSE THEN CAST(0 AS NUMERIC) ELSE CAST(2 AS NUMERIC) / 1000 END
			ELSE CAST(5 AS NUMERIC) END
		AS STRING)`)
	if got != "0.002" {
		t.Fatalf("nested CASE carrier mix = %q, want %q", got, "0.002")
	}
}

// TestIssue22_UnitDispatchShape is a representative shape from the issue:
// unit dispatch via value-match CASE whose branches mix the
// division result, a CAST(0 AS NUMERIC) sibling, and a typed NULL,
// aggregated under SUM over a small table.
func TestIssue22_UnitDispatchShape(t *testing.T) {
	t.Parallel()
	db := openIssue22DB(t, "issue22_dbt")
	ctx := context.Background()
	if _, err := db.ExecContext(ctx,
		"CREATE TABLE issue22_charges (unit_kind STRING, cnt INT64, rate NUMERIC)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO issue22_charges (unit_kind, cnt, rate) VALUES
			('per_thousand', 2000, CAST(1 AS NUMERIC)),
			('per_thousand', 4000, CAST(0.5 AS NUMERIC)),
			('flat', 7, CAST(10 AS NUMERIC)),
			('unknown', 5, CAST(1 AS NUMERIC))`); err != nil {
		t.Fatal(err)
	}
	// per_thousand rows: 2000*1/1000 = 2, 4000*0.5/1000 = 2; flat row: 0;
	// unknown row: NULL (ignored by SUM). Total 4.
	got := issue22QueryString(t, db,
		`SELECT CAST(SUM(
			CASE unit_kind
				WHEN 'per_thousand' THEN cnt * rate / 1000
				WHEN 'flat' THEN CAST(0 AS NUMERIC)
				ELSE CAST(NULL AS NUMERIC)
			END
		) AS STRING) FROM issue22_charges`)
	if got != "4" {
		t.Fatalf("unit-dispatch CASE under SUM = %q, want %q", got, "4")
	}
}

// TestIssue22_IfCarrierMix: IF(cond, native-division result,
// envelope/cast NUMERIC) binds both result branches together exactly
// like CASE and takes the same normalization.
func TestIssue22_IfCarrierMix(t *testing.T) {
	t.Parallel()
	db := openIssue22DB(t, "issue22_if")
	got := issue22QueryString(t, db,
		"SELECT CAST(IF(TRUE, CAST(2 AS NUMERIC) / 1000, CAST(0 AS NUMERIC)) AS STRING)")
	if got != "0.002" {
		t.Fatalf("IF carrier mix = %q, want %q", got, "0.002")
	}
	got = issue22QueryString(t, db,
		"SELECT CAST(IF(FALSE, CAST(0 AS NUMERIC), CAST(2 AS NUMERIC) / 1000) AS STRING)")
	if got != "0.002" {
		t.Fatalf("IF reversed carrier mix = %q, want %q", got, "0.002")
	}
}

// TestIssue22_NullIfCarrierMix: NULLIF binds its two operands under one
// type (and compares them by value), so the carrier mix must unify both
// for the bind and for the equality semantics.
func TestIssue22_NullIfCarrierMix(t *testing.T) {
	t.Parallel()
	db := openIssue22DB(t, "issue22_nullif")
	got := issue22QueryString(t, db,
		"SELECT CAST(NULLIF(CAST(2 AS NUMERIC) / 1000, CAST(0 AS NUMERIC)) AS STRING)")
	if got != "0.002" {
		t.Fatalf("NULLIF carrier mix = %q, want %q", got, "0.002")
	}
	// Equal-by-value across carriers must fold to NULL.
	var folded sql.NullString
	if err := db.QueryRowContext(context.Background(),
		"SELECT CAST(NULLIF(CAST(2 AS NUMERIC) / 1000, CAST(0.002 AS NUMERIC)) AS STRING)").Scan(&folded); err != nil {
		t.Fatalf("NULLIF equal-by-value: %v", err)
	}
	if folded.Valid {
		t.Fatalf("NULLIF(0.002, 0.002) = %q, want NULL", folded.String)
	}
}

// TestIssue22_GreatestLeastCarrierMix: GREATEST / LEAST bind every
// operand under one common type AND order them by value, so the
// envelope form must be normalized (an envelope VARCHAR would compare
// lexicographically — silently wrong even when it binds).
func TestIssue22_GreatestLeastCarrierMix(t *testing.T) {
	t.Parallel()
	db := openIssue22DB(t, "issue22_greatest")
	got := issue22QueryString(t, db,
		"SELECT CAST(GREATEST(CAST(2 AS NUMERIC) / 1000, CAST(0 AS NUMERIC)) AS STRING)")
	if got != "0.002" {
		t.Fatalf("GREATEST carrier mix = %q, want %q", got, "0.002")
	}
	got = issue22QueryString(t, db,
		"SELECT CAST(LEAST(CAST(2 AS NUMERIC) / 1000, CAST(0 AS NUMERIC)) AS STRING)")
	if got != "0" {
		t.Fatalf("LEAST carrier mix = %q, want %q", got, "0")
	}
}

// TestIssue22_InListCarrierMix: the $in lowering already normalizes
// NUMERIC probe and list members (tryNativeOperator); this pins that the
// envelope-carried division result IN-matches by value.
func TestIssue22_InListCarrierMix(t *testing.T) {
	t.Parallel()
	db := openIssue22DB(t, "issue22_in")
	var got bool
	if err := db.QueryRowContext(context.Background(),
		"SELECT CAST(2 AS NUMERIC) / 1000 IN (CAST(0 AS NUMERIC), CAST(0.002 AS NUMERIC))").Scan(&got); err != nil {
		t.Fatalf("IN-list carrier mix: %v", err)
	}
	if !got {
		t.Fatalf("0.002 IN (0, 0.002) = false, want true")
	}
}

// TestIssue22_BetweenCarrierMix: BETWEEN was swept as a friend that
// binds its three operands together; it is already safe — the analyzer
// lowers it to the comparison operators, whose NUMERIC operands were
// normalized in the #15-era comparison work. Pinned here.
func TestIssue22_BetweenCarrierMix(t *testing.T) {
	t.Parallel()
	db := openIssue22DB(t, "issue22_between")
	var got bool
	if err := db.QueryRowContext(context.Background(),
		"SELECT CAST(2 AS NUMERIC) / 1000 BETWEEN CAST(0 AS NUMERIC) AND CAST(1 AS NUMERIC)").Scan(&got); err != nil {
		t.Fatalf("BETWEEN carrier mix: %v", err)
	}
	if !got {
		t.Fatalf("0.002 BETWEEN 0 AND 1 = false, want true")
	}
}

// TestIssue22_IfErrorCarrierMix: IFERROR lowers to a CASE whose try arm
// (safe-evaluated division → envelope VARCHAR) and catch arm (cast
// NUMERIC) must agree on one engine type, same class as CASE branches.
func TestIssue22_IfErrorCarrierMix(t *testing.T) {
	t.Parallel()
	db := openIssue22DB(t, "issue22_iferror")
	got := issue22QueryString(t, db,
		"SELECT CAST(IFERROR(CAST(2 AS NUMERIC) / 1000, CAST(0 AS NUMERIC)) AS STRING)")
	if got != "0.002" {
		t.Fatalf("IFERROR carrier mix = %q, want %q", got, "0.002")
	}
	got = issue22QueryString(t, db,
		"SELECT CAST(IFERROR(CAST(2 AS NUMERIC) / 0, CAST(7 AS NUMERIC)) AS STRING)")
	if got != "7" {
		t.Fatalf("IFERROR catch arm = %q, want %q", got, "7")
	}
}

// TestIssue22_BigNumericCaseMix: BIGNUMERIC stays on the envelope
// carrier everywhere (it exceeds DuckDB's DECIMAL ceiling), so the
// branches never mix forms — pinned across the swept members.
func TestIssue22_BigNumericCaseMix(t *testing.T) {
	t.Parallel()
	db := openIssue22DB(t, "issue22_bignumeric")
	got := issue22QueryString(t, db,
		"SELECT CAST(CASE WHEN TRUE THEN CAST(2 AS BIGNUMERIC) / 1000 ELSE CAST(0 AS BIGNUMERIC) END AS STRING)")
	if got != "0.002" {
		t.Fatalf("BIGNUMERIC searched CASE = %q, want %q", got, "0.002")
	}
	got = issue22QueryString(t, db,
		"SELECT CAST(IF(TRUE, CAST(2 AS BIGNUMERIC) / 1000, CAST(0 AS BIGNUMERIC)) AS STRING)")
	if got != "0.002" {
		t.Fatalf("BIGNUMERIC IF = %q, want %q", got, "0.002")
	}
	got = issue22QueryString(t, db,
		"SELECT CAST(NULLIF(CAST(2 AS BIGNUMERIC) / 1000, CAST(0 AS BIGNUMERIC)) AS STRING)")
	if got != "0.002" {
		t.Fatalf("BIGNUMERIC NULLIF = %q, want %q", got, "0.002")
	}
}

// TestIssue22_Controls pins the three OK queries from the issue report
// plus a plain DOUBLE CASE: none of these may be disturbed by the
// normalization.
func TestIssue22_Controls(t *testing.T) {
	t.Parallel()
	db := openIssue22DB(t, "issue22_controls")
	// NULL-branch CASE (unified before the fix and must stay unified).
	got := issue22QueryString(t, db,
		"SELECT CAST(CASE WHEN TRUE THEN CAST(2 AS NUMERIC) / 1000 ELSE CAST(NULL AS NUMERIC) END AS STRING)")
	if got != "0.002" {
		t.Fatalf("NULL-branch CASE control = %q, want %q", got, "0.002")
	}
	// COALESCE form (the v0.2.25 fix).
	got = issue22QueryString(t, db,
		"SELECT CAST(COALESCE(CAST(2 AS NUMERIC) / 1000, CAST(NULL AS NUMERIC)) AS STRING)")
	if got != "0.002" {
		t.Fatalf("COALESCE control = %q, want %q", got, "0.002")
	}
	// No-division value-match CASE.
	got = issue22QueryString(t, db,
		"SELECT CAST(CASE 'a' WHEN 'a' THEN CAST(1 AS NUMERIC) ELSE CAST(NULL AS NUMERIC) END AS STRING)")
	if got != "1" {
		t.Fatalf("no-division value-match control = %q, want %q", got, "1")
	}
	// Plain DOUBLE CASE stays on the native lowering, untouched.
	var f float64
	if err := db.QueryRowContext(context.Background(),
		"SELECT CASE WHEN TRUE THEN 2.0 / 1000 ELSE 0.0 END").Scan(&f); err != nil {
		t.Fatalf("DOUBLE CASE control: %v", err)
	}
	if f != 0.002 {
		t.Fatalf("DOUBLE CASE control = %v, want 0.002", f)
	}
	// Plain string CASE / IF / NULLIF / GREATEST untouched.
	if got := issue22QueryString(t, db, "SELECT IF(TRUE, 'x', 'y')"); got != "x" {
		t.Fatalf("string IF control = %q, want %q", got, "x")
	}
	if got := issue22QueryString(t, db, "SELECT GREATEST('a', 'b')"); got != "b" {
		t.Fatalf("string GREATEST control = %q, want %q", got, "b")
	}
}
