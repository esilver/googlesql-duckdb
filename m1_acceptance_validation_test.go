package googlesqlite_test

import (
	"context"
	"database/sql"
	"testing"

	_ "github.com/goccy/googlesqlite"
)

// TestM1Acceptance_EventsReduction runs a candidate validation query: a
// self-contained reduction of stg_sample__events_model joined
// against a json_value staging shape. It exercises the
// M1 common constructs: =, AND/OR, count, sum, coalesce, cast, json_value,
// and a date function (date_sub/current_date), over in-query literal rows so
// no example.duckdb table is required.
func TestM1Acceptance_EventsReduction(t *testing.T) {
	db, err := sql.Open("googlesqlite", ":memory:?_test=m1_example")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	const q = `
WITH source AS (
              SELECT DATE '2026-06-07' AS event_day, 'events_source_a' AS source_system, 'impression' AS event_name, 2 AS event_count, JSON '{"id":"L1","fee":"3"}' AS payload
    UNION ALL SELECT DATE '2026-06-07',              'events_source_a',                  'click',                    5,                JSON '{"id":"L2","fee":"7"}'
    UNION ALL SELECT DATE '2020-01-01',              'events_source_a',                  'impression',               9,                JSON '{"id":"L3","fee":"1"}'
    UNION ALL SELECT DATE '2026-06-07',              'other_system',                     'impression',               4,                JSON '{"id":"L4","fee":"4"}'
)
SELECT
  source_system,
  COUNT(*)                                            AS rule_rows,
  SUM(event_count)                                    AS total_events,
  COALESCE(CAST(NULL AS STRING), 'USD')               AS currency,
  COUNT(DISTINCT JSON_VALUE(payload, '$.id'))         AS distinct_labels
FROM source
WHERE source_system = 'events_source_a'
  AND event_day >= DATE_SUB(DATE '2026-06-08', INTERVAL 1 DAY)
  AND (event_name = 'impression' OR event_name = 'click')
GROUP BY source_system
`
	rows, err := db.QueryContext(context.Background(), q)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()

	type row struct {
		system   string
		ruleRows int64
		total    int64
		currency string
		labels   int64
	}
	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.system, &r.ruleRows, &r.total, &r.currency, &r.labels); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}

	// Expected: only the 2 'events_source_a' rows on 2026-06-07 with
	// event_name in (impression, click) survive (the 2020 row fails the
	// date filter, the other_system row fails the = filter).
	want := row{system: "events_source_a", ruleRows: 2, total: 7, currency: "USD", labels: 2}
	if len(got) != 1 {
		t.Fatalf("got %d rows, want 1: %+v", len(got), got)
	}
	if got[0] != want {
		t.Fatalf("got %+v want %+v", got[0], want)
	}
}
