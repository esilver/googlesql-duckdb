package internal

import (
	"testing"

	"github.com/goccy/googlesqlite/internal/value"
)

func TestTryNativeDatetimeFuncPinnedCurrentTime(t *testing.T) {
	const unixNano = "1718452800123456789"
	const utcTS = "make_timestamp(CAST(1718452800123456789 AS BIGINT) // 1000)"
	const instant = "(" + utcTS + " AT TIME ZONE 'UTC')"

	laZone, err := literalFromValue(value.StringValue("America/Los_Angeles"))
	if err != nil {
		t.Fatalf("literalFromValue named zone: %v", err)
	}
	offsetZone, err := literalFromValue(value.StringValue("-08"))
	if err != nil {
		t.Fatalf("literalFromValue offset zone: %v", err)
	}

	tests := []struct {
		name     string
		funcName string
		args     []string
		want     string
	}{
		{
			name:     "current datetime default utc zone",
			funcName: "googlesqlite_current_datetime",
			args:     []string{unixNano},
			want:     "timezone('UTC', " + instant + ")",
		},
		{
			name:     "current date named zone",
			funcName: "googlesqlite_current_date",
			args:     []string{laZone, unixNano},
			want:     "CAST(timezone('America/Los_Angeles', " + instant + ") AS DATE)",
		},
		{
			name:     "current time offset zone",
			funcName: "googlesqlite_current_time",
			args:     []string{offsetZone, unixNano},
			want:     "CAST((" + utcTS + " + INTERVAL '-480 minutes') AS TIME)",
		},
		{
			name:     "current timestamp absolute instant",
			funcName: "googlesqlite_current_timestamp",
			args:     []string{unixNano},
			want:     instant,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := tryNativeDatetimeFunc(tt.funcName, tt.args, nil)
			if !ok {
				t.Fatalf("tryNativeDatetimeFunc returned ok=false")
			}
			if got != tt.want {
				t.Fatalf("SQL mismatch\n got: %s\nwant: %s", got, tt.want)
			}
		})
	}
}
