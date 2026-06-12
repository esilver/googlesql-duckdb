package value_test

import (
	"testing"

	"cloud.google.com/go/bigquery"
	"github.com/goccy/googlesqlite/internal/value"
)

// TestIntervalValue covers IntervalValue's all-unsupported arithmetic,
// the conversion stubs that return errors, and ToString / ToJSON
// formatting including the negative-month sign-fixup branch.
func TestIntervalValue(t *testing.T) {
	t.Parallel()

	iv := &value.IntervalValue{IntervalValue: &bigquery.IntervalValue{Months: 2, Days: 3}}

	t.Run("arithmetic and comparison unsupported", func(t *testing.T) {
		if _, err := iv.Add(iv); err == nil {
			t.Fatal("Add")
		}
		if _, err := iv.Sub(iv); err == nil {
			t.Fatal("Sub")
		}
		if _, err := iv.Mul(iv); err == nil {
			t.Fatal("Mul")
		}
		if _, err := iv.Div(iv); err == nil {
			t.Fatal("Div")
		}
		if _, err := iv.EQ(iv); err == nil {
			t.Fatal("EQ")
		}
		if _, err := iv.GT(iv); err == nil {
			t.Fatal("GT")
		}
		if _, err := iv.GTE(iv); err == nil {
			t.Fatal("GTE")
		}
		if _, err := iv.LT(iv); err == nil {
			t.Fatal("LT")
		}
		if _, err := iv.LTE(iv); err == nil {
			t.Fatal("LTE")
		}
	})

	t.Run("ToInt64/Float64/Bool/Array/Struct/Time/Rat error", func(t *testing.T) {
		if _, err := iv.ToInt64(); err == nil {
			t.Fatal("ToInt64")
		}
		if _, err := iv.ToFloat64(); err == nil {
			t.Fatal("ToFloat64")
		}
		if _, err := iv.ToBool(); err == nil {
			t.Fatal("ToBool")
		}
		if _, err := iv.ToArray(); err == nil {
			t.Fatal("ToArray")
		}
		if _, err := iv.ToStruct(); err == nil {
			t.Fatal("ToStruct")
		}
		if _, err := iv.ToTime(); err == nil {
			t.Fatal("ToTime")
		}
		if _, err := iv.ToRat(); err == nil {
			t.Fatal("ToRat")
		}
	})

	t.Run("ToString/ToBytes/ToJSON", func(t *testing.T) {
		s, _ := iv.ToString()
		if s == "" {
			t.Fatal("ToString empty")
		}
		b, _ := iv.ToBytes()
		if string(b) != s {
			t.Fatalf("ToBytes mismatch: %s vs %s", b, s)
		}
		j, _ := iv.ToJSON()
		// JSON is the string in quotes
		if len(j) < 2 || j[0] != '"' || j[len(j)-1] != '"' {
			t.Fatalf("ToJSON not quoted: %s", j)
		}
	})

	t.Run("ToString negative months fix-up", func(t *testing.T) {
		// Years==0 && Months<0 triggers the leading-minus branch.
		neg := &value.IntervalValue{IntervalValue: &bigquery.IntervalValue{Months: -1}}
		s, _ := neg.ToString()
		if s == "" || s[0] != '-' {
			t.Fatalf("ToString negative: %q", s)
		}
	})

	t.Run("Format/Interface", func(t *testing.T) {
		if iv.Format('t') == "" {
			t.Fatal("Format empty")
		}
		if iv.Interface() == nil {
			t.Fatal("Interface nil")
		}
	})
}

// TestIntervalCanonicalRendering hand-derives BigQuery's canonical text
// form for negative intervals: per-group sign (year-month, day, time),
// absolute component values inside a signed group, and fractional
// seconds from abs(SubSecondNanos) with trailing zeros trimmed.
func TestIntervalCanonicalRendering(t *testing.T) {
	t.Parallel()

	cases := []struct {
		desc string
		iv   *bigquery.IntervalValue
		want string
	}{
		{
			desc: "INTERVAL -45 MINUTE",
			iv:   &bigquery.IntervalValue{Minutes: -45},
			want: "0-0 0 -0:45:0",
		},
		{
			desc: "INTERVAL -2 MONTH",
			iv:   &bigquery.IntervalValue{Months: -2},
			want: "-0-2 0 0:0:0",
		},
		{
			desc: "INTERVAL -500 MILLISECOND",
			iv:   &bigquery.IntervalValue{SubSecondNanos: -500000000},
			want: "0-0 0 -0:0:0.5",
		},
		{
			desc: "INTERVAL '1-2 -3 -4:30:0'",
			iv:   &bigquery.IntervalValue{Years: 1, Months: 2, Days: -3, Hours: -4, Minutes: -30},
			want: "1-2 -3 -4:30:0",
		},
		{
			desc: "positive mixed with fraction",
			iv:   &bigquery.IntervalValue{Years: 1, Months: 2, Days: 3, Hours: 4, Minutes: 5, Seconds: 6, SubSecondNanos: 789000000},
			want: "1-2 3 4:5:6.789",
		},
		{
			desc: "negative sub-second with seconds",
			iv:   &bigquery.IntervalValue{Seconds: -1, SubSecondNanos: -250000000},
			want: "0-0 0 -0:0:1.25",
		},
		{
			desc: "negative years and months",
			iv:   &bigquery.IntervalValue{Years: -1, Months: -2},
			want: "-1-2 0 0:0:0",
		},
	}
	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			v := &value.IntervalValue{IntervalValue: tc.iv}
			got, err := v.ToString()
			if err != nil {
				t.Fatalf("ToString: %v", err)
			}
			if got != tc.want {
				t.Fatalf("ToString = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestIntervalEnvelopeRoundTrip ensures the canonical text survives an
// encode/decode cycle through the value envelope sign-intact. The old
// decode path used cloud.google.com/go/bigquery.ParseInterval, which
// reads "-0-2 0 0:0:0" as +2 months and "0-0 0 -0:45:0" as +45 minutes.
func TestIntervalEnvelopeRoundTrip(t *testing.T) {
	t.Parallel()

	cases := []struct {
		desc string
		iv   *bigquery.IntervalValue
	}{
		{"minus 45 minutes", &bigquery.IntervalValue{Minutes: -45}},
		{"minus 2 months", &bigquery.IntervalValue{Months: -2}},
		{"minus 500 milliseconds", &bigquery.IntervalValue{SubSecondNanos: -500000000}},
		{"mixed sign groups", &bigquery.IntervalValue{Years: 1, Months: 2, Days: -3, Hours: -4, Minutes: -30}},
		{"all positive", &bigquery.IntervalValue{Years: 1, Months: 2, Days: 3, Hours: 4, Minutes: 5, Seconds: 6, SubSecondNanos: 789000000}},
		{"negative day only", &bigquery.IntervalValue{Days: -7}},
	}
	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			orig := &value.IntervalValue{IntervalValue: tc.iv}
			encoded, err := value.EncodeValue(orig)
			if err != nil {
				t.Fatalf("EncodeValue: %v", err)
			}
			decoded, err := value.DecodeValue(encoded)
			if err != nil {
				t.Fatalf("DecodeValue: %v", err)
			}
			got, ok := decoded.(*value.IntervalValue)
			if !ok {
				t.Fatalf("decoded type = %T, want *value.IntervalValue", decoded)
			}
			if *got.IntervalValue != *tc.iv {
				t.Fatalf("round-trip mismatch: got %+v, want %+v", *got.IntervalValue, *tc.iv)
			}
			wantText, err := orig.ToString()
			if err != nil {
				t.Fatalf("orig.ToString: %v", err)
			}
			gotText, err := got.ToString()
			if err != nil {
				t.Fatalf("decoded.ToString: %v", err)
			}
			if gotText != wantText {
				t.Fatalf("round-trip text = %q, want %q", gotText, wantText)
			}
		})
	}
}

// TestParseIntervalSignCorrect exercises the sign-correct canonical
// parser directly, including forms the upstream library mis-signs.
func TestParseIntervalSignCorrect(t *testing.T) {
	t.Parallel()

	cases := []struct {
		text string
		want bigquery.IntervalValue
	}{
		{"-0-2 0 0:0:0", bigquery.IntervalValue{Months: -2}},
		{"0-0 0 -0:45:0", bigquery.IntervalValue{Minutes: -45}},
		{"0-0 0 -0:0:0.5", bigquery.IntervalValue{SubSecondNanos: -500000000}},
		{"1-2 -3 -4:30:0", bigquery.IntervalValue{Years: 1, Months: 2, Days: -3, Hours: -4, Minutes: -30}},
		{"-1-2 3 4:5:6.789", bigquery.IntervalValue{Years: -1, Months: -2, Days: 3, Hours: 4, Minutes: 5, Seconds: 6, SubSecondNanos: 789000000}},
		{"0-0 1 0:0:0", bigquery.IntervalValue{Days: 1}},
		{"+1-0 +2 +3:0:0", bigquery.IntervalValue{Years: 1, Days: 2, Hours: 3}},
	}
	for _, tc := range cases {
		t.Run(tc.text, func(t *testing.T) {
			got, err := value.ParseInterval(tc.text)
			if err != nil {
				t.Fatalf("ParseInterval(%q): %v", tc.text, err)
			}
			if *got.IntervalValue != tc.want {
				t.Fatalf("ParseInterval(%q) = %+v, want %+v", tc.text, *got.IntervalValue, tc.want)
			}
		})
	}

	for _, bad := range []string{"", "garbage", "1-2", "1-2 3", "1-2 3 4:5", "x-2 3 4:5:6", "1-2 3 4:5:6.x"} {
		if _, err := value.ParseInterval(bad); err == nil {
			t.Fatalf("ParseInterval(%q): expected error", bad)
		}
	}
}
