package googlesqlite_test

// Regression coverage for bigquery-emulator issue #17: a BETWEEN predicate
// over an engine-native temporal column failed with "unexpected value type:
// time.Time". $between lowers to the value-layer googlesqlite_between UDF,
// whose ANY-typed arguments arrive from the engine as bare Go time.Time for
// every native temporal vector (DATE / TIME / TIMESTAMP / DATETIME) — the
// value decoder had no time.Time case. The fix infers the temporal kind from
// the carrier shape (internal/value decodeNativeTime).

import (
	"context"
	"database/sql"
	"testing"

	"github.com/goccy/googlesqlite"
)

// openIssue17Conn returns a fresh in-memory connection for one subtest.
func openIssue17Conn(t *testing.T, tag string) (*sql.Conn, context.Context) {
	t.Helper()
	db, err := sql.Open("googlesqlite", ":memory:?_test=issue17_"+tag)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn, ctx
}

// TestIssue17DateBetweenJoinCSVLoadShape reproduces the exact issue shape:
// DATE cells loaded as raw ISO strings through BulkInsert (the emulator's
// CSV fast path writes them as engine-native DATE), then a join + aggregate
// query with a `BETWEEN DATE '...' AND DATE '...'` predicate.
func TestIssue17DateBetweenJoinCSVLoadShape(t *testing.T) {
	conn, ctx := openIssue17Conn(t, "date_join")

	if _, err := conn.ExecContext(ctx, "CREATE TABLE `events` (`id` INT64, `event_date` DATE, `publisher_id` INT64, `amount` FLOAT64)"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, "CREATE TABLE `publishers` (`publisher_id` INT64, `region` STRING, `tier` STRING)"); err != nil {
		t.Fatal(err)
	}
	eventRows := [][]any{
		{int64(1), "2026-06-01", int64(10), 1.5},
		{int64(2), "2026-06-10", int64(10), 2.5},
		{int64(3), "2026-06-25", int64(20), 4.0},
	}
	if _, err := googlesqlite.BulkInsert(ctx, conn, []string{"events"}, []string{"id", "event_date", "publisher_id", "amount"}, eventRows); err != nil {
		t.Fatal(err)
	}
	pubRows := [][]any{
		{int64(10), "us", "gold"},
		{int64(20), "eu", "silver"},
	}
	if _, err := googlesqlite.BulkInsert(ctx, conn, []string{"publishers"}, []string{"publisher_id", "region", "tier"}, pubRows); err != nil {
		t.Fatal(err)
	}

	// Control: the same join without the predicate (worked before the fix).
	var n int64
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM `events` e JOIN `publishers` p ON e.publisher_id = p.publisher_id").Scan(&n); err != nil {
		t.Fatalf("control join: %v", err)
	}
	if n != 3 {
		t.Fatalf("control join count = %d, want 3", n)
	}

	rows, err := conn.QueryContext(ctx, "SELECT p.region, p.tier, COUNT(*) AS row_count, SUM(e.amount) AS revenue, AVG(e.amount) AS avg_amount FROM `events` e JOIN `publishers` p ON e.publisher_id = p.publisher_id WHERE e.event_date BETWEEN DATE '2026-06-01' AND DATE '2026-06-20' GROUP BY p.region, p.tier ORDER BY revenue DESC")
	if err != nil {
		t.Fatalf("join+BETWEEN query: %v", err)
	}
	defer rows.Close()
	groups := 0
	for rows.Next() {
		var region, tier string
		var cnt int64
		var rev, avg float64
		if err := rows.Scan(&region, &tier, &cnt, &rev, &avg); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if region != "us" || tier != "gold" || cnt != 2 || rev != 4.0 || avg != 2.0 {
			t.Errorf("group = (%s,%s,%d,%v,%v), want (us,gold,2,4,2)", region, tier, cnt, rev, avg)
		}
		groups++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows err: %v", err)
	}
	if groups != 1 {
		t.Fatalf("got %d groups, want 1", groups)
	}
}

// TestIssue17TemporalBetween covers the BETWEEN shape over each native
// temporal column kind written through the reference INSERT path (typed
// literals), including a CTAS'd DATE table. Each subtest counts rows
// matching a BETWEEN window over typed literals.
func TestIssue17TemporalBetween(t *testing.T) {
	for _, tc := range []struct {
		name   string
		ddl    string
		insert string
		query  string
		want   int64
	}{
		{
			name:   "date",
			ddl:    "CREATE TABLE `t` (`d` DATE)",
			insert: "INSERT `t` (`d`) VALUES (DATE '2026-06-01'), (DATE '2026-06-10'), (DATE '2026-06-25'), (NULL)",
			query:  "SELECT COUNT(*) FROM `t` WHERE `d` BETWEEN DATE '2026-06-01' AND DATE '2026-06-20'",
			want:   2,
		},
		{
			name: "date_ctas",
			ddl:  "CREATE TABLE `t` AS SELECT DATE '2026-06-05' AS d UNION ALL SELECT DATE '2026-06-30'",
			// DATE literal comparison on a CTAS'd table (control from the
			// issue brief) plus the BETWEEN shape.
			query: "SELECT COUNT(*) FROM `t` WHERE `d` BETWEEN DATE '2026-06-01' AND DATE '2026-06-20' AND `d` = DATE '2026-06-05'",
			want:  1,
		},
		{
			name:   "timestamp",
			ddl:    "CREATE TABLE `t` (`ts` TIMESTAMP)",
			insert: "INSERT `t` (`ts`) VALUES (TIMESTAMP '2026-06-01 08:30:00+00'), (TIMESTAMP '2026-06-10 23:59:59+00'), (TIMESTAMP '2026-06-25 00:00:00+00')",
			query:  "SELECT COUNT(*) FROM `t` WHERE `ts` BETWEEN TIMESTAMP '2026-06-01 00:00:00+00' AND TIMESTAMP '2026-06-20 12:00:00+00'",
			want:   2,
		},
		{
			name:   "datetime",
			ddl:    "CREATE TABLE `t` (`dt` DATETIME)",
			insert: "INSERT `t` (`dt`) VALUES (DATETIME '2026-06-01 08:30:00'), (DATETIME '2026-06-10 23:59:59'), (DATETIME '2026-06-25 00:00:00')",
			query:  "SELECT COUNT(*) FROM `t` WHERE `dt` BETWEEN DATETIME '2026-06-01 00:00:00' AND DATETIME '2026-06-20 12:00:00'",
			want:   2,
		},
		{
			name:   "time",
			ddl:    "CREATE TABLE `t` (`tm` TIME)",
			insert: "INSERT `t` (`tm`) VALUES (TIME '08:30:00'), (TIME '12:15:30'), (TIME '23:59:59')",
			query:  "SELECT COUNT(*) FROM `t` WHERE `tm` BETWEEN TIME '08:00:00' AND TIME '13:00:00'",
			want:   2,
		},
		{
			name: "time_midnight_bound",
			// TIME '00:00:00' decodes through the exact-midnight (DATE)
			// shape; comparisons must still be exact via ToTime.
			ddl:    "CREATE TABLE `t` (`tm` TIME)",
			insert: "INSERT `t` (`tm`) VALUES (TIME '00:00:00'), (TIME '00:00:01'), (TIME '12:00:00')",
			query:  "SELECT COUNT(*) FROM `t` WHERE `tm` BETWEEN TIME '00:00:00' AND TIME '01:00:00'",
			want:   2,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn, ctx := openIssue17Conn(t, "between_"+tc.name)
			if _, err := conn.ExecContext(ctx, tc.ddl); err != nil {
				t.Fatal(err)
			}
			if tc.insert != "" {
				if _, err := conn.ExecContext(ctx, tc.insert); err != nil {
					t.Fatal(err)
				}
			}
			var got int64
			if err := conn.QueryRowContext(ctx, tc.query).Scan(&got); err != nil {
				t.Fatalf("query: %v", err)
			}
			if got != tc.want {
				t.Fatalf("count = %d, want %d", got, tc.want)
			}
		})
	}
}
