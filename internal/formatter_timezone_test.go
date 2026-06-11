package internal

import (
	"testing"

	googlesql "github.com/goccy/go-googlesql"
	"github.com/goccy/googlesqlite/internal/value"
)

func TestDuckDBTimezoneConstructorLowering(t *testing.T) {
	strLit := func(s string) string {
		t.Helper()
		lit, err := literalFromValue(value.StringValue(s))
		if err != nil {
			t.Fatalf("literalFromValue(%q): %v", s, err)
		}
		return lit
	}

	ts := `TIMESTAMPTZ '2008-12-25 15:30:00+00'`
	dt := `TIMESTAMP '2008-12-25 15:30:00'`
	zone := strLit("America/Los_Angeles")
	tests := []struct {
		name     string
		funcName string
		args     []string
		kinds    []googlesql.TypeKind
		want     string
	}{
		{
			name:     "timestamp with zone",
			funcName: "timestamp",
			args:     []string{strLit("2008-12-25 15:30:00"), zone},
			kinds:    []googlesql.TypeKind{googlesql.TypeKindTypeString, googlesql.TypeKindTypeString},
			want:     "timezone('America/Los_Angeles', CAST(CAST('2008-12-25 15:30:00' AS TIMESTAMP) AS TIMESTAMP))",
		},
		{
			name:     "time from timestamp with zone",
			funcName: "time",
			args:     []string{ts, zone},
			kinds:    []googlesql.TypeKind{googlesql.TypeKindTypeTimestamp, googlesql.TypeKindTypeString},
			want:     "CAST(timezone('America/Los_Angeles', CAST(TIMESTAMPTZ '2008-12-25 15:30:00+00' AS TIMESTAMP WITH TIME ZONE)) AS TIME)",
		},
		{
			name:     "time from datetime",
			funcName: "time",
			args:     []string{dt},
			kinds:    []googlesql.TypeKind{googlesql.TypeKindTypeDatetime},
			want:     "CAST(TIMESTAMP '2008-12-25 15:30:00' AS TIME)",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := tryNativeDatetimeFunc(tt.funcName, tt.args, tt.kinds)
			if !ok {
				t.Fatalf("tryNativeDatetimeFunc returned ok=false")
			}
			if got != tt.want {
				t.Fatalf("SQL mismatch\n got: %s\nwant: %s", got, tt.want)
			}
		})
	}
}

func TestDuckDBTimezoneDateDatetimeConstructorLowering(t *testing.T) {
	zone, err := literalFromValue(value.StringValue("America/Los_Angeles"))
	if err != nil {
		t.Fatalf("literalFromValue zone: %v", err)
	}
	ts := `TIMESTAMPTZ '2008-12-25 05:30:00+00'`
	tests := []struct {
		name     string
		funcName string
		args     []string
		kinds    []googlesql.TypeKind
		want     string
	}{
		{
			name:     "date timestamp with zone",
			funcName: "date",
			args:     []string{ts, zone},
			kinds:    []googlesql.TypeKind{googlesql.TypeKindTypeTimestamp, googlesql.TypeKindTypeString},
			want:     "CAST(timezone('America/Los_Angeles', CAST(TIMESTAMPTZ '2008-12-25 05:30:00+00' AS TIMESTAMP WITH TIME ZONE)) AS DATE)",
		},
		{
			name:     "datetime timestamp with zone",
			funcName: "datetime",
			args:     []string{ts, zone},
			kinds:    []googlesql.TypeKind{googlesql.TypeKindTypeTimestamp, googlesql.TypeKindTypeString},
			want:     "timezone('America/Los_Angeles', CAST(TIMESTAMPTZ '2008-12-25 05:30:00+00' AS TIMESTAMP WITH TIME ZONE))",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok, err := (&FunctionCallNode{}).tryFormatDateConstructor(tt.funcName, tt.args, tt.kinds)
			if err != nil {
				t.Fatalf("tryFormatDateConstructor: %v", err)
			}
			if !ok {
				t.Fatalf("tryFormatDateConstructor returned ok=false")
			}
			if got != tt.want {
				t.Fatalf("SQL mismatch\n got: %s\nwant: %s", got, tt.want)
			}
		})
	}
}

func TestDuckDBTimestampTruncTimezoneLowering(t *testing.T) {
	zone, err := literalFromValue(value.StringValue("Pacific/Auckland"))
	if err != nil {
		t.Fatalf("literalFromValue zone: %v", err)
	}
	partLit := func(part string) string {
		t.Helper()
		lit, err := literalFromValue(value.StringValue(part))
		if err != nil {
			t.Fatalf("literalFromValue(%q): %v", part, err)
		}
		return lit
	}
	ts := `TIMESTAMPTZ '2017-11-06 00:00:00+12'`
	local := "timezone('Pacific/Auckland', CAST(" + ts + " AS TIMESTAMP WITH TIME ZONE))"
	tests := []struct {
		name string
		part string
		want string
	}{
		{
			name: "day",
			part: "DAY",
			want: "timezone('Pacific/Auckland', CAST(date_trunc('day', " + local + ") AS TIMESTAMP))",
		},
		{
			name: "week monday",
			part: "WEEK_MONDAY",
			want: "timezone('Pacific/Auckland', CAST(CAST((CAST(" + local + " AS DATE) - (((dayofweek(CAST(" + local + " AS DATE)) - 1 + 7) % 7) * INTERVAL '1 day')) AS TIMESTAMP) AS TIMESTAMP))",
		},
		{
			name: "quarter",
			part: "QUARTER",
			want: "timezone('Pacific/Auckland', CAST(date_trunc('quarter', " + local + ") AS TIMESTAMP))",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := tryNativeDatetimeFunc("timestamp_trunc", []string{ts, partLit(tt.part), zone}, nil)
			if !ok {
				t.Fatalf("tryNativeDatetimeFunc returned ok=false")
			}
			if got != tt.want {
				t.Fatalf("SQL mismatch\n got: %s\nwant: %s", got, tt.want)
			}
		})
	}
}
