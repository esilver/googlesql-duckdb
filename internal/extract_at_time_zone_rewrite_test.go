package internal

import "testing"

func TestApplyExtractAtTimeZoneRewriteRewritesNestedCalls(t *testing.T) {
	query := `SELECT EXTRACT(YEAR FROM EXTRACT(DATE FROM ts AT TIME ZONE "America/Los_Angeles"))`
	want := `SELECT EXTRACT(YEAR FROM DATE(ts, "America/Los_Angeles"))`
	if got := applyExtractAtTimeZoneRewrite(query); got != want {
		t.Fatalf("rewrite = %q; want %q", got, want)
	}
}
