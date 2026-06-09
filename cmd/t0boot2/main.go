// Command t0boot2 exercises more BigQuery scalar functions (including ones that
// lower to googlesqlite_* UDFs registered through duckdbcompat) plus a
// CREATE/INSERT/COUNT round-trip to validate cross-connection catalog
// visibility (the SWAP-BLUEPRINT §4 T0 gating risk).
package main

import (
	"database/sql"
	"fmt"
	"os"

	_ "github.com/goccy/googlesqlite"
)

func main() {
	db, err := sql.Open("googlesqlite", ":memory:")
	if err != nil {
		fmt.Printf("FATAL sql.Open: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	scalar := []string{
		`SELECT SAFE_DIVIDE(10, 0)`,
		`SELECT STARTS_WITH('duckdb', 'duck')`,
		`SELECT ENDS_WITH('duckdb', 'db')`,
		`SELECT REVERSE('abc')`,
		`SELECT REPEAT('ab', 3)`,
		`SELECT FORMAT('%d-%s', 7, 'x')`,
		`SELECT CHAR_LENGTH('héllo')`,
		`SELECT LPAD('7', 3, '0')`,
		`SELECT SAFE_CAST('42' AS INT64)`,
		`SELECT POW(2, 10)`,
		`SELECT SQRT(16.0)`,
		`SELECT GREATEST(3, 9, 1)`,
		`SELECT IFNULL(NULL, 'fallback')`,
		`SELECT COALESCE(NULL, NULL, 'c')`,
	}
	fails := 0
	for _, q := range scalar {
		var v sql.NullString
		if err := db.QueryRow(q).Scan(&v); err != nil {
			fmt.Printf("[FAIL] %-40s -> ERROR: %v\n", q, err)
			fails++
			continue
		}
		out := "NULL"
		if v.Valid {
			out = v.String
		}
		fmt.Printf("[ OK ] %-40s -> %s\n", q, out)
	}

	// Cross-connection / DDL round trip on the pooled *sql.DB.
	fmt.Println("\n-- DDL / cross-connection round trip --")
	if _, err := db.Exec(`CREATE TABLE t (id INT64, name STRING)`); err != nil {
		fmt.Printf("[FAIL] CREATE TABLE -> %v\n", err)
		fails++
	} else {
		fmt.Println("[ OK ] CREATE TABLE t")
	}
	if _, err := db.Exec(`INSERT INTO t (id, name) VALUES (1, 'a'), (2, 'b'), (3, 'c')`); err != nil {
		fmt.Printf("[FAIL] INSERT -> %v\n", err)
		fails++
	} else {
		fmt.Println("[ OK ] INSERT 3 rows")
	}
	var n int64
	if err := db.QueryRow(`SELECT COUNT(*) FROM t`).Scan(&n); err != nil {
		fmt.Printf("[FAIL] COUNT(*) -> %v\n", err)
		fails++
	} else {
		fmt.Printf("[ OK ] SELECT COUNT(*) FROM t -> %d\n", n)
	}
	var name string
	if err := db.QueryRow(`SELECT UPPER(name) FROM t WHERE id = 2`).Scan(&name); err != nil {
		fmt.Printf("[FAIL] SELECT UPPER(name) WHERE id=2 -> %v\n", err)
		fails++
	} else {
		fmt.Printf("[ OK ] SELECT UPPER(name) FROM t WHERE id=2 -> %s\n", name)
	}

	fmt.Printf("\n%d failed\n", fails)
	if fails > 0 {
		os.Exit(2)
	}
}
