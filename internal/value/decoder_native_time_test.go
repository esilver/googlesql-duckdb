package value_test

import (
	"testing"
	"time"

	"github.com/goccy/googlesqlite/internal/value"
)

// TestDecodeValueNativeTime covers the engine-native bare time.Time carrier
// (bigquery-emulator issue #17): the duckdb-go UDF codec hands every native
// temporal vector cell to value-layer UDF arguments as a UTC time.Time with
// the logical type erased, so DecodeValue infers the temporal kind from the
// carrier's shape.
func TestDecodeValueNativeTime(t *testing.T) {
	t.Parallel()

	date := time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC)
	clock := time.Date(1970, 1, 1, 12, 34, 56, 789000000, time.UTC)
	instant := time.Date(2026, 6, 10, 12, 34, 56, 789000000, time.UTC)

	t.Run("midnight_decodes_as_date", func(t *testing.T) {
		v, err := value.DecodeValue(date)
		if err != nil {
			t.Fatal(err)
		}
		dv, ok := v.(value.DateValue)
		if !ok {
			t.Fatalf("got %T, want value.DateValue", v)
		}
		if !time.Time(dv).Equal(date) {
			t.Fatalf("got %v, want %v", time.Time(dv), date)
		}
	})

	t.Run("epoch_day_clock_decodes_as_time", func(t *testing.T) {
		v, err := value.DecodeValue(clock)
		if err != nil {
			t.Fatal(err)
		}
		tv, ok := v.(value.TimeValue)
		if !ok {
			t.Fatalf("got %T, want value.TimeValue", v)
		}
		if !time.Time(tv).Equal(clock) {
			t.Fatalf("got %v, want %v", time.Time(tv), clock)
		}
	})

	t.Run("instant_decodes_as_datetime", func(t *testing.T) {
		v, err := value.DecodeValue(instant)
		if err != nil {
			t.Fatal(err)
		}
		dv, ok := v.(value.DatetimeValue)
		if !ok {
			t.Fatalf("got %T, want value.DatetimeValue", v)
		}
		if !time.Time(dv).Equal(instant) {
			t.Fatalf("got %v, want %v", time.Time(dv), instant)
		}
	})

	// Comparisons across the inferred kinds funnel through ToTime, so a
	// BETWEEN over any temporal column stays exact regardless of which
	// kind the inference picked for each operand.
	t.Run("cross_kind_comparison_exact", func(t *testing.T) {
		lo, _ := value.DecodeValue(date)                                       // DateValue
		hi, _ := value.DecodeValue(date.Add(13 * time.Hour))                   // DatetimeValue
		mid, _ := value.DecodeValue(date.Add(12*time.Hour + 34*time.Minute))   // DatetimeValue
		out, _ := value.DecodeValue(date.Add(14 * time.Hour))                  // DatetimeValue
		if got, err := mid.GTE(lo); err != nil || !got {
			t.Fatalf("mid >= lo: got %v err %v, want true", got, err)
		}
		if got, err := mid.LTE(hi); err != nil || !got {
			t.Fatalf("mid <= hi: got %v err %v, want true", got, err)
		}
		if got, err := out.LTE(hi); err != nil || got {
			t.Fatalf("out <= hi: got %v err %v, want false", got, err)
		}
	})

	// ConvertArgs (the UDF-argument seam where the issue #17 failure
	// surfaced) must accept a bare time.Time too.
	t.Run("convert_args", func(t *testing.T) {
		vals, err := value.ConvertArgs(date, instant, clock)
		if err != nil {
			t.Fatal(err)
		}
		if len(vals) != 3 {
			t.Fatalf("got %d values, want 3", len(vals))
		}
	})
}
