package interval

import (
	"fmt"

	"cloud.google.com/go/bigquery"
	"github.com/goccy/googlesqlite/internal/functions/helper"
	"github.com/goccy/googlesqlite/internal/value"
)

func INTERVAL(v int64, part string) (value.Value, error) {
	v32, err := helper.SafeInt32(v)
	if err != nil {
		return nil, err
	}
	switch part {
	case "YEAR":
		return &value.IntervalValue{IntervalValue: &bigquery.IntervalValue{Years: v32}}, nil
	case "MONTH":
		return &value.IntervalValue{IntervalValue: &bigquery.IntervalValue{Months: v32}}, nil
	case "DAY":
		return &value.IntervalValue{IntervalValue: &bigquery.IntervalValue{Days: v32}}, nil
	case "HOUR":
		return &value.IntervalValue{IntervalValue: &bigquery.IntervalValue{Hours: v32}}, nil
	case "MINUTE":
		return &value.IntervalValue{IntervalValue: &bigquery.IntervalValue{Minutes: v32}}, nil
	case "SECOND":
		return &value.IntervalValue{IntervalValue: &bigquery.IntervalValue{Seconds: v32}}, nil
	case "MILLISECOND":
		return subSecondInterval(v, 1000)
	case "MICROSECOND":
		return subSecondInterval(v, 1000000)
	case "NANOSECOND":
		return &value.IntervalValue{IntervalValue: &bigquery.IntervalValue{SubSecondNanos: v32}}, nil
	}
	return nil, fmt.Errorf("unexpected interval part: %s", part)
}

// subSecondInterval builds an interval from a sub-second unit count,
// splitting whole seconds out so SubSecondNanos stays within one second
// (perUnit is the number of units per second).
func subSecondInterval(v, perUnit int64) (value.Value, error) {
	secs, err := helper.SafeInt32(v / perUnit)
	if err != nil {
		return nil, err
	}
	nanos, err := helper.SafeInt32((v % perUnit) * (1000000000 / perUnit))
	if err != nil {
		return nil, err
	}
	return &value.IntervalValue{IntervalValue: &bigquery.IntervalValue{Seconds: secs, SubSecondNanos: nanos}}, nil
}

func BindInterval(args ...value.Value) (value.Value, error) {
	v, err := args[0].ToInt64()
	if err != nil {
		return nil, err
	}
	part, err := args[1].ToString()
	if err != nil {
		return nil, err
	}
	return INTERVAL(v, part)
}
