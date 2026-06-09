// Command t0boot is the T0 boot smoke-test for the duckdbcompat (pure-Go,
// CGO_ENABLED=0) DuckDB backend swap. It opens the googlesqlite BigQuery
// emulator on an in-memory database and runs SELECT 1 plus a handful of
// BigQuery scalar string/math queries, printing each query and its result.
package main

import (
	"database/sql"
	"fmt"
	"os"

	_ "github.com/goccy/googlesqlite" // registers the "googlesqlite" driver
)

func main() {
	db, err := sql.Open("googlesqlite", ":memory:")
	if err != nil {
		fmt.Printf("FATAL sql.Open: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	// A query plus the destination scanned as a string for uniform printing.
	queries := []string{
		`SELECT 1`,
		`SELECT 1 + 1`,
		`SELECT UPPER('duck')`,
		`SELECT CONCAT('a', 'b', 'c')`,
		`SELECT LENGTH('hello')`,
		`SELECT LOWER('LOUD')`,
		`SELECT SUBSTR('bigquery', 1, 3)`,
		`SELECT ABS(-7)`,
		`SELECT ROUND(3.14159, 2)`,
		`SELECT MOD(10, 3)`,
		`SELECT SAFE_DIVIDE(10, 0)`,
		`SELECT 'a' || 'b'`,
		`SELECT REPLACE('aaa', 'a', 'b')`,
		`SELECT TRIM('  hi  ')`,
	}

	fails := 0
	for _, q := range queries {
		var v sql.NullString
		err := db.QueryRow(q).Scan(&v)
		if err != nil {
			fmt.Printf("[FAIL] %-32s -> ERROR: %v\n", q, err)
			fails++
			continue
		}
		out := "NULL"
		if v.Valid {
			out = v.String
		}
		fmt.Printf("[ OK ] %-32s -> %s\n", q, out)
	}

	fmt.Printf("\n%d/%d queries OK, %d failed\n", len(queries)-fails, len(queries), fails)
	if fails > 0 {
		os.Exit(2)
	}
}
