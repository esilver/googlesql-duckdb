package googlesqlite_test

// Issue #15 coverage: a googlesqlite_cast targeting a native-representable
// scalar kind (BOOL / INT64 / FLOAT64; temporals were already native) must
// produce a NATIVE value in the lowered SQL, so any branching construct
// (COALESCE / IF / IFNULL / CASE / NULLIF) can mix it with a native-literal
// sibling branch without DuckDB's "Cannot mix values of type VARCHAR and
// BOOLEAN" binder error. The suite covers the class, not the instance:
// the issue's four probes, the {construct} x {kind} matrix, the dbt
// staging-view idiom over a STORED JSON column, the silent lexicographic
// MIN/MAX/ORDER BY hazard of the old VARCHAR representation, and twin-table
// storage parity for INSERTed cast results.

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"testing"
)

// TestCastNativeBranch_IssueProbes runs the four driver-level probes from
// issue #15 verbatim.
func TestCastNativeBranch_IssueProbes(t *testing.T) {
	t.Parallel()
	db := openSeamDB(t, "cast_native_probes")
	wantBool(t, db, `SELECT CAST(JSON_VALUE('{"a":true}', '$.a') AS BOOL)`, true)
	wantBool(t, db, `SELECT COALESCE(CAST('true' AS BOOL), false)`, true)
	wantBool(t, db, `SELECT COALESCE(CAST(JSON_VALUE('{"a":true}', '$.a') AS BOOL), false)`, true)
	wantBool(t, db, `SELECT NOT COALESCE(CAST(JSON_VALUE('{"a":false}','$.a') AS BOOL), false)`, true)
}

// TestCastNativeBranch_Matrix exercises {COALESCE, IF, IFNULL, CASE WHEN}
// x {BOOL, INT64, FLOAT64, DATE}, each mixing a JSON_VALUE-sourced CAST
// with a native literal sibling. For every cell it checks both arms: the
// cast arm selected (value must equal the JSON payload) and the literal
// arm selected (the default must flow out). The assertion itself compares
// the construct's result against a native literal with `=`, which also
// fails to bind when the representations disagree.
func TestCastNativeBranch_Matrix(t *testing.T) {
	t.Parallel()
	db := openSeamDB(t, "cast_native_matrix")

	kinds := []struct {
		name    string
		doc     string // JSON doc with key v
		target  string // CAST target type
		def     string // native literal default (sibling branch)
		wantSQL string // native literal equal to CAST(JSON_VALUE(doc,'$.v') AS target)
	}{
		{"BOOL", `'{"v":true}'`, "BOOL", "false", "true"},
		{"INT64", `'{"v":7}'`, "INT64", "0", "7"},
		{"FLOAT64", `'{"v":1.5}'`, "FLOAT64", "0.0", "1.5"},
		{"DATE", `'{"v":"2024-03-01"}'`, "DATE", "DATE '2000-01-01'", "DATE '2024-03-01'"},
	}
	constructs := []struct {
		name string
		// castArm yields the cast-arm value; litArm yields the default.
		castArm, litArm string // fmt: %[1]s = cast expr, %[2]s = default literal
	}{
		{"COALESCE", "COALESCE(%[1]s, %[2]s)", "COALESCE(%[3]s, %[2]s)"},
		{"IF", "IF(TRUE, %[1]s, %[2]s)", "IF(FALSE, %[1]s, %[2]s)"},
		{"IFNULL", "IFNULL(%[1]s, %[2]s)", "IFNULL(%[3]s, %[2]s)"},
		{"CASE", "CASE WHEN TRUE THEN %[1]s ELSE %[2]s END", "CASE WHEN FALSE THEN %[1]s ELSE %[2]s END"},
	}
	for _, k := range kinds {
		castExpr := fmt.Sprintf("CAST(JSON_VALUE(%s, '$.v') AS %s)", k.doc, k.target)
		// nullCast is the same cast over a missing key: a NULL of the
		// target type, so COALESCE/IFNULL fall through to the default.
		nullCast := fmt.Sprintf("CAST(JSON_VALUE(%s, '$.missing') AS %s)", k.doc, k.target)
		for _, c := range constructs {
			name := c.name + "_" + k.name
			t.Run(name, func(t *testing.T) {
				cast := fmt.Sprintf(c.castArm, castExpr, k.def, nullCast)
				wantBool(t, db, fmt.Sprintf("SELECT (%s) = %s", cast, k.wantSQL), true)
				lit := fmt.Sprintf(c.litArm, castExpr, k.def, nullCast)
				wantBool(t, db, fmt.Sprintf("SELECT (%s) = %s", lit, k.def), true)
			})
		}
	}
	// NULLIF is the remaining branching builtin from the issue's class
	// list; it unifies the cast arm with the comparison literal.
	wantBool(t, db, `SELECT NULLIF(CAST(JSON_VALUE('{"v":true}','$.v') AS BOOL), false)`, true)
	wantInt64(t, db, `SELECT IFNULL(NULLIF(CAST(JSON_VALUE('{"v":7}','$.v') AS INT64), 7), -1)`, -1)
}

// TestCastNativeBranch_StoredJSONStagingIdiom drives the exact dbt staging
// pattern from the issue through storage: a JSON payload column, a staging
// VIEW projecting NOT COALESCE(CAST(JSON_VALUE(...) AS BOOL), false), and
// a SELECT through the view.
func TestCastNativeBranch_StoredJSONStagingIdiom(t *testing.T) {
	t.Parallel()
	db := openSeamDB(t, "cast_native_dbt")
	seamExec(t, db,
		"CREATE TABLE events (id INT64, payload JSON)",
		`INSERT INTO events (id, payload) VALUES `+
			`(1, JSON '{"flag": true}'), `+
			`(2, JSON '{"flag": false}'), `+
			`(3, JSON '{"other": 1}'), `+
			`(4, NULL)`,
		"CREATE VIEW stg_events AS SELECT id, NOT COALESCE(CAST(JSON_VALUE(payload, '$.flag') AS BOOL), false) AS inverted FROM events",
	)
	rows, err := db.QueryContext(context.Background(),
		"SELECT id, inverted FROM stg_events ORDER BY id")
	if err != nil {
		t.Fatalf("query staging view: %v", err)
	}
	defer rows.Close()
	got := map[int64]bool{}
	for rows.Next() {
		var id int64
		var inv bool
		if err := rows.Scan(&id, &inv); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got[id] = inv
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := map[int64]bool{1: false, 2: true, 3: true, 4: true}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("staging view rows: got %v want %v", got, want)
	}
	// The WHERE-clause form the model's downstream filter uses.
	wantInt64(t, db,
		"SELECT COUNT(*) FROM events WHERE COALESCE(CAST(JSON_VALUE(payload, '$.flag') AS BOOL), false)", 1)
}

// TestCastNativeBranch_OrderingIsNumeric guards the silent half of the
// class: the old envelope-VARCHAR cast result made MIN/MAX/ORDER BY
// compare LEXICOGRAPHICALLY (max of 9 vs 10 returned 9). With the native
// representation the comparison is numeric.
func TestCastNativeBranch_OrderingIsNumeric(t *testing.T) {
	t.Parallel()
	db := openSeamDB(t, "cast_native_order")
	seamExec(t, db,
		"CREATE TABLE t (c STRING)",
		`INSERT INTO t (c) VALUES ('{"n": 9}'), ('{"n": 10}')`,
	)
	wantInt64(t, db, "SELECT MAX(CAST(JSON_VALUE(c, '$.n') AS INT64)) FROM t", 10)
	wantInt64(t, db, "SELECT MIN(CAST(JSON_VALUE(c, '$.n') AS INT64)) FROM t", 9)
	wantInt64(t, db, "SELECT CAST(JSON_VALUE(c, '$.n') AS INT64) AS n FROM t ORDER BY n DESC LIMIT 1", 10)
	wantFloat64(t, db, "SELECT MAX(CAST(JSON_VALUE(c, '$.n') AS FLOAT64)) FROM t", 10)
}

// TestCastNativeBranch_FloatSpecials: NaN / ±Inf cast results ride the
// textual seam between the cast UDF and the wrapping native CAST; they
// must survive it (json.Marshal alone rejects them).
func TestCastNativeBranch_FloatSpecials(t *testing.T) {
	t.Parallel()
	db := openSeamDB(t, "cast_native_floatspecials")
	wantBool(t, db, `SELECT IS_NAN(COALESCE(CAST(JSON_VALUE('{"v":"NaN"}','$.v') AS FLOAT64), 0.0))`, true)
	wantBool(t, db, `SELECT IS_INF(COALESCE(CAST(JSON_VALUE('{"v":"inf"}','$.v') AS FLOAT64), 0.0))`, true)
	wantBool(t, db, `SELECT COALESCE(CAST(JSON_VALUE('{"v":"-inf"}','$.v') AS FLOAT64), 0.0) < 0`, true)
}

// TestCastNativeBranch_StorageParity: an INSERT whose VALUES expression is
// a native-representable CAST result must store byte-identically to the
// same value inserted as a plain native literal (twin tables, raw cells
// compared via the engine connection — the v0.2.11 parity infrastructure).
func TestCastNativeBranch_StorageParity(t *testing.T) {
	db, err := sql.Open("googlesqlite", ":memory:?_test=cast_native_parity")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	cases := []struct {
		name    string
		colType string
		literal string // plain native literal insert
		cast    string // the same value as a JSON_VALUE-sourced CAST
	}{
		{"bool", "BOOL", "true", `CAST(JSON_VALUE('{"v":true}','$.v') AS BOOL)`},
		{"int64", "INT64", "42", `CAST(JSON_VALUE('{"v":42}','$.v') AS INT64)`},
		{"float64", "FLOAT64", "1.5", `CAST(JSON_VALUE('{"v":1.5}','$.v') AS FLOAT64)`},
		{"date", "DATE", "DATE '2024-03-01'", `CAST(JSON_VALUE('{"v":"2024-03-01"}','$.v') AS DATE)`},
		// STRING keeps the envelope storage contract: the cast result
		// must remain byte-identical to the literal's stored envelope.
		{"string", "STRING", "'hi'", `CAST(JSON_VALUE('{"v":"hi"}','$.v') AS STRING)`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lit, cst := "lit_"+tc.name, "cst_"+tc.name
			for tbl, expr := range map[string]string{lit: tc.literal, cst: tc.cast} {
				if _, err := conn.ExecContext(ctx,
					fmt.Sprintf("CREATE TABLE `%s` (`c` %s)", tbl, tc.colType)); err != nil {
					t.Fatalf("create %s: %v", tbl, err)
				}
				if _, err := conn.ExecContext(ctx,
					fmt.Sprintf("INSERT INTO `%s` (`c`) VALUES (%s)", tbl, expr)); err != nil {
					t.Fatalf("insert %s: %v", tbl, err)
				}
			}
			probe := `SELECT typeof(c), CAST(c AS VARCHAR) FROM "%s"`
			want := bulkRawRows(t, ctx, conn, fmt.Sprintf(probe, lit))
			got := bulkRawRows(t, ctx, conn, fmt.Sprintf(probe, cst))
			if !reflect.DeepEqual(got, want) {
				t.Errorf("raw storage mismatch:\n literal: %v\n cast:    %v", want, got)
			}
		})
	}
}
