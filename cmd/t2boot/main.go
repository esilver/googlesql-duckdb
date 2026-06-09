// Command t2boot is the T2 boot smoke-test for the pure-Go (CGO_ENABLED=0)
// DuckDB aggregate-UDF bridge (duckdb_aggregate_udf_purego.go). It opens the
// googlesqlite BigQuery emulator on an in-memory database — which registers
// the 32 aggregate specs through duckdbconverge/duckdb.RegisterAggregateBandConn
// — and runs known-good BigQuery-dialect aggregate queries copied from this
// repo's spec corpus (testdata/specs/**), printing each query and its result
// next to the corpus' expected value.
//
// Coverage: ANY_VALUE(x HAVING MAX|MIN y) (having_any_value),
// KLL_QUANTILES.INIT_INT64 + MERGE_INT64, HLL_COUNT.INIT + MERGE (+ the
// EXTRACT scalar over per-group INIT sketches), ST_CENTROID_AGG /
// ST_UNION_AGG over geography, and the WITH ANONYMIZATION ANON_COUNT(*)
// noiseless (epsilon=1e20) example from
// testdata/specs/googlesql/functions/aggregate/differential_privacy/anon_count.yaml.
package main

import (
	"database/sql"
	"fmt"
	"os"
	"sort"
	"strings"

	_ "github.com/goccy/googlesqlite" // registers the "googlesqlite" driver
)

// aggCase is one smoke query: optional setup DDL/DML, the query, and the
// expected result rows (each row's columns joined with " | "). Rows compare
// order-insensitively when unordered is set (GROUP BY output order is not
// guaranteed).
type aggCase struct {
	name      string
	setup     []string
	query     string
	expected  []string
	unordered bool
}

func cases() []aggCase {
	return []aggCase{
		// testdata/specs/googlesql/functions/aggregate/any_value.yaml —
		// "HAVING MAX picks the value associated with the largest sold count".
		{
			name: "ANY_VALUE(fruit HAVING MAX sold)",
			query: `
WITH Store AS (
  SELECT 20 AS sold, "apples" AS fruit
  UNION ALL SELECT 30, "pears"
  UNION ALL SELECT 30, "bananas"
  UNION ALL SELECT 10, "oranges"
)
SELECT ANY_VALUE(fruit HAVING MAX sold) IN ("pears", "bananas") AS ok
FROM Store`,
			expected: []string{"true"},
		},
		// any_value.yaml — "HAVING MIN picks the value associated with the
		// smallest sold count" (deterministic: 10 is unique).
		{
			name: "ANY_VALUE(fruit HAVING MIN sold)",
			query: `
WITH Store AS (
  SELECT 20 AS sold, "apples" AS fruit
  UNION ALL SELECT 30, "pears"
  UNION ALL SELECT 30, "bananas"
  UNION ALL SELECT 10, "oranges"
)
SELECT ANY_VALUE(fruit HAVING MIN sold) AS a_lowest_selling_fruit
FROM Store`,
			expected: []string{"oranges"},
		},
		// testdata/specs/bigquery/functions/kll/kll_quantiles_init_int64.yaml —
		// "kll_quantiles_init_int64 basic pipeline".
		{
			name:     "KLL_QUANTILES.INIT_INT64",
			query:    `SELECT BYTE_LENGTH(KLL_QUANTILES.INIT_INT64(x)) > 0 FROM UNNEST([1,2,3]) AS x`,
			expected: []string{"true"},
		},
		// testdata/specs/bigquery/functions/kll/kll_quantiles_merge_int64.yaml —
		// "merge_int64 returns ARRAY of quantiles" (4 quantiles => 5 cut points).
		{
			name:     "KLL_QUANTILES.MERGE_INT64",
			query:    `SELECT ARRAY_LENGTH(KLL_QUANTILES.MERGE_INT64(s, 4)) FROM (SELECT KLL_QUANTILES.INIT_INT64(x) AS s FROM UNNEST([1,2,3]) AS x)`,
			expected: []string{"5"},
		},
		// query_test.go "hll_count.merge" — INIT per country, MERGE folds the
		// sketches back to the distinct-customer count.
		{
			name: "HLL_COUNT.INIT + HLL_COUNT.MERGE",
			query: `
SELECT HLL_COUNT.MERGE(hll_sketch) AS distinct_customers_with_open_invoice
FROM
(
    SELECT
      country,
      HLL_COUNT.INIT(customer_id) AS hll_sketch
    FROM
      UNNEST(
        ARRAY<STRUCT<country STRING, customer_id STRING, invoice_id STRING>>[
          ('UA', 'customer_id_1', 'invoice_id_11'),
          ('BR', 'customer_id_3', 'invoice_id_31'),
          ('CZ', 'customer_id_2', 'invoice_id_22'),
          ('CZ', 'customer_id_2', 'invoice_id_23'),
          ('BR', 'customer_id_3', 'invoice_id_31'),
          ('UA', 'customer_id_2', 'invoice_id_24')])
    GROUP BY country
)`,
			expected: []string{"3"},
		},
		// query_test.go "hll_count.extract" — the EXTRACT scalar over per-group
		// INIT sketches (exercises the grouped aggregate + the scalar UDF).
		{
			name: "HLL_COUNT.INIT per group + HLL_COUNT.EXTRACT",
			query: `
SELECT
  country,
  HLL_COUNT.EXTRACT(HLL_sketch) AS distinct_customers_with_open_invoice
FROM
  (
    SELECT
      country,
      HLL_COUNT.INIT(customer_id) AS hll_sketch
    FROM
      UNNEST(
        ARRAY<STRUCT<country STRING, customer_id STRING, invoice_id STRING>>[
          ('UA', 'customer_id_1', 'invoice_id_11'),
          ('BR', 'customer_id_3', 'invoice_id_31'),
          ('CZ', 'customer_id_2', 'invoice_id_22'),
          ('CZ', 'customer_id_2', 'invoice_id_23'),
          ('BR', 'customer_id_3', 'invoice_id_31'),
          ('UA', 'customer_id_2', 'invoice_id_24')])
    GROUP BY country
  )`,
			expected:  []string{"BR | 1", "CZ | 1", "UA | 2"},
			unordered: true,
		},
		// testdata/specs/bigquery/functions/geography/st_centroid_agg.yaml —
		// "centroid of two antipodal-free points".
		{
			name:     "ST_CENTROID_AGG over two ST_GEOGPOINTs",
			query:    `SELECT ST_ASTEXT(ST_CENTROID_AGG(g)) FROM UNNEST([ST_GEOGPOINT(0,0), ST_GEOGPOINT(2,2)]) AS g`,
			expected: []string{"POINT (1 1)"},
		},
		// internal/functions/geography/sql_driven_test.go
		// TestStUnionAggMultipleLines — three chained linestrings merge into one.
		{
			name: "ST_UNION_AGG over chained linestrings",
			query: `
SELECT ST_ASTEXT(ST_UNION_AGG(items)) FROM UNNEST([
  ST_GEOGFROMTEXT('LINESTRING(0 0, 1 1)'),
  ST_GEOGFROMTEXT('LINESTRING(1 1, 2 2)'),
  ST_GEOGFROMTEXT('LINESTRING(2 2, 3 3)')]) AS items`,
			expected: []string{"LINESTRING (3 3, 2 2, 1 1, 0 0)"},
		},
		// testdata/specs/googlesql/functions/aggregate/differential_privacy/anon_count.yaml —
		// "upstream example 2 — ANON_COUNT(*) without noise" (epsilon=1e20 is
		// deterministic; the eps=10 examples are flagged nondeterministic in the
		// corpus and are skipped here).
		{
			name: "WITH ANONYMIZATION ANON_COUNT(*) (epsilon=1e20)",
			setup: []string{
				`CREATE TABLE professors (id INT64, item STRING, quantity INT64) OPTIONS(anonymization_userid_column='id')`,
				`INSERT INTO professors VALUES
  (101, "pencil", 24),
  (123, "pen", 16),
  (123, "pencil", 10),
  (123, "pencil", 38),
  (101, "pen", 19),
  (101, "pen", 23),
  (130, "scissors", 8),
  (150, "pencil", 72)`,
			},
			query: `
SELECT
  WITH ANONYMIZATION
    OPTIONS(epsilon=1e20, delta=.01, max_groups_contributed=1)
    item,
    ANON_COUNT(*) AS times_requested
FROM professors
GROUP BY item`,
			expected:  []string{"scissors | 1", "pencil | 4", "pen | 3"},
			unordered: true,
		},
	}
}

// runQuery executes q and renders every result row as its columns (scanned as
// nullable strings) joined with " | ".
func runQuery(db *sql.DB, q string) ([]string, error) {
	rows, err := db.Query(q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out []string
	for rows.Next() {
		vals := make([]sql.NullString, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		parts := make([]string, len(cols))
		for i, v := range vals {
			if v.Valid {
				parts[i] = v.String
			} else {
				parts[i] = "NULL"
			}
		}
		out = append(out, strings.Join(parts, " | "))
	}
	return out, rows.Err()
}

// match compares got against want, order-insensitively when unordered.
func match(got, want []string, unordered bool) bool {
	if len(got) != len(want) {
		return false
	}
	g := append([]string(nil), got...)
	w := append([]string(nil), want...)
	if unordered {
		sort.Strings(g)
		sort.Strings(w)
	}
	for i := range g {
		if g[i] != w[i] {
			return false
		}
	}
	return true
}

func main() {
	db, err := sql.Open("googlesqlite", ":memory:")
	if err != nil {
		fmt.Printf("FATAL sql.Open: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	fails := 0
	for _, c := range cases() {
		setupOK := true
		for _, s := range c.setup {
			if _, err := db.Exec(s); err != nil {
				fmt.Printf("[FAIL] %s -> setup ERROR: %v\n", c.name, err)
				fails++
				setupOK = false
				break
			}
		}
		if !setupOK {
			continue
		}
		got, err := runQuery(db, c.query)
		if err != nil {
			fmt.Printf("[FAIL] %s -> ERROR: %v\n", c.name, err)
			fails++
			continue
		}
		status := "[ OK ]"
		if !match(got, c.expected, c.unordered) {
			status = "[FAIL]"
			fails++
		}
		fmt.Printf("%s %-48s -> %s (want %s)\n",
			status, c.name, strings.Join(got, " ; "), strings.Join(c.expected, " ; "))
	}

	total := len(cases())
	fmt.Printf("\n%d/%d aggregate queries OK, %d failed\n", total-fails, total, fails)
	if fails > 0 {
		os.Exit(2)
	}
}
