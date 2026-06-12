// Command t2dbg runs ONE GoogleSQL query (argv[1]) through the googlesqlite
// driver and prints the result rows — a minimal bisection tool for debugging
// backend issues (used to reduce the pure-Go HLL_COUNT panic).
package main

import (
	"database/sql"
	"fmt"
	"os"

	_ "github.com/goccy/googlesqlite" // registers the "googlesqlite" driver
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: t2dbg <query> [setup...]")
		os.Exit(2)
	}
	db, err := sql.Open("googlesqlite", ":memory:")
	if err != nil {
		panic(err)
	}
	defer db.Close()

	for _, setup := range os.Args[2:] {
		if _, err := db.Exec(setup); err != nil {
			// Keep going: lets a deliberately failing statement precede the main
			// query, to probe error-path state poisoning (tx-in-tx cascade).
			fmt.Printf("SETUP ERROR (continuing): %v\n", err)
		}
	}

	rows, err := db.Query(os.Args[1])
	if err != nil {
		fmt.Printf("QUERY ERROR: %v\n", err)
		os.Exit(1)
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	n := 0
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			fmt.Printf("SCAN ERROR: %v\n", err)
			os.Exit(1)
		}
		for i, v := range vals {
			if b, ok := v.([]byte); ok {
				if len(b) > 80 {
					vals[i] = fmt.Sprintf("%s...(%d bytes)", b[:80], len(b))
				} else {
					vals[i] = string(b)
				}
			} else if s, ok := v.(string); ok && len(s) > 80 {
				vals[i] = fmt.Sprintf("%s...(%d chars)", s[:80], len(s))
			}
		}
		fmt.Printf("row: %v\n", vals)
		n++
	}
	if err := rows.Err(); err != nil {
		fmt.Printf("ROWS ERROR: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("OK: %d row(s)\n", n)
}
