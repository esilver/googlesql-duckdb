// Package value is googlesqlite's GoogleSQL value layer. It models each
// GoogleSQL value as a typed Value (see value.go) and defines the
// on-the-wire envelope used to carry the types DuckDB cannot store
// natively with BigQuery semantics.
//
// The envelope is a base64-encoded JSON object {type, value} (the
// ValueType header plus a string body in codec.go), stored in DuckDB as
// a flat string and decoded back on the way out. It exists because
// DuckDB has no native carrier that preserves GoogleSQL semantics for
// NUMERIC / BIGNUMERIC, ARRAY, STRUCT, the temporal types (DATE,
// DATETIME, TIME, TIMESTAMP, INTERVAL), RANGE, GEOGRAPHY, and JSON, so
// those types ride the envelope. INT64, FLOAT64, BOOL, STRING, and BYTES
// round-trip through DuckDB's own column types instead. encoder.go and
// decoder.go are the two ends of that conversion.
package value

import (
	"fmt"
	"math"
	"math/big"
	"reflect"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/bigquery"
)

type Value interface {
	Add(Value) (Value, error)
	Sub(Value) (Value, error)
	Mul(Value) (Value, error)
	Div(Value) (Value, error)
	EQ(Value) (bool, error)
	GT(Value) (bool, error)
	GTE(Value) (bool, error)
	LT(Value) (bool, error)
	LTE(Value) (bool, error)
	ToInt64() (int64, error)
	ToString() (string, error)
	ToBytes() ([]byte, error)
	ToFloat64() (float64, error)
	ToBool() (bool, error)
	ToArray() (*ArrayValue, error)
	ToStruct() (*StructValue, error)
	ToJSON() (string, error)
	ToTime() (time.Time, error)
	ToRat() (*big.Rat, error)
	Format(verb rune) string
	Interface() any
}

func isDate(date string) bool {
	return dateRe.MatchString(date)
}

func isDatetime(datetime string) bool {
	return datetimeRe.MatchString(datetime)
}

func isTime(v string) bool {
	return timeRe.MatchString(v)
}

func isTimestamp(timestamp string) bool {
	loc, err := toLocation("")
	if err != nil {
		return false
	}
	if _, err := parseTimestamp(timestamp, loc); err != nil {
		return false
	}
	return true
}

func parseDate(date string) (time.Time, error) {
	return time.Parse("2006-01-02", date)
}

func parseDatetime(datetime string) (time.Time, error) {
	if t, err := time.Parse(datetimeFormat, datetime); err == nil {
		return t, nil
	}
	return time.Parse("2006-01-02 15:04:05.999999", datetime)
}

func parseTime(t string) (time.Time, error) {
	return time.Parse("15:04:05.999999", t)
}

func parseTimestamp(timestamp string, loc *time.Location) (time.Time, error) {
	if t, err := time.ParseInLocation("2006-01-02T15:04:05.999999999Z07:00", timestamp, loc); err == nil {
		return t, nil
	}
	if t, err := time.ParseInLocation("2006-01-02T15:04:05.999999999-07:00", timestamp, loc); err == nil {
		return t, nil
	}
	if t, err := time.ParseInLocation("2006-01-02T15:04:05.999999999-07", timestamp, loc); err == nil {
		return t, nil
	}
	if t, err := time.ParseInLocation("2006-01-02T15:04:05.999999999 MST", timestamp, loc); err == nil {
		return t, nil
	}
	if t, err := time.ParseInLocation("2006-01-02T15:04:05", timestamp, loc); err == nil {
		return t, nil
	}
	if t, err := time.ParseInLocation("2006-01-02 15:04:05.999999999Z07:00", timestamp, loc); err == nil {
		return t, nil
	}
	if t, err := time.ParseInLocation("2006-01-02 15:04:05.999999999-07:00", timestamp, loc); err == nil {
		return t, nil
	}
	if t, err := time.ParseInLocation("2006-01-02 15:04:05.999999999-07", timestamp, loc); err == nil {
		return t, nil
	}
	if t, err := time.ParseInLocation("2006-01-02 15:04:05.999999999 MST", timestamp, loc); err == nil {
		return t, nil
	}
	if t, err := time.ParseInLocation("2006-01-02 15:04:05.999999999", timestamp, loc); err == nil {
		return t, nil
	}
	if t, err := time.ParseInLocation("2006-01-02", timestamp, loc); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("failed to parse timestamp. unexpected format %s", timestamp)
}

func DateFromInt64Value(v int64) (time.Time, error) {
	return time.Unix(0, 0).UTC().Add(time.Duration(v) * 24 * time.Hour), nil
}

func TimestampFromFloatValue(f float64) (time.Time, error) {
	secs := math.Trunc(f)
	micros := math.Trunc((f-secs)*1e6 + 0.5)
	return time.Unix(int64(secs), int64(micros)*1000).UTC(), nil
}

func TimestampFromInt64Value(v int64) (time.Time, error) {
	sec := v / int64(time.Millisecond)
	msec := v - sec*int64(time.Millisecond)
	return time.Unix(sec, msec*int64(time.Millisecond)).UTC(), nil
}

// parseInterval parses BigQuery's canonical interval text form
// "[sign]Y-M [sign]D [sign]H:M:S[.F]" with sign-correct group handling.
//
// cloud.google.com/go/bigquery.ParseInterval keys the year-month group
// sign off Years < 0 and the time group sign off Hours < 0, so "-0-2"
// decodes to +2 months and "-0:45:0" to +45 minutes ("-0" parses to 0).
// This parser applies a group's leading sign to every component of the
// group, so canonical text produced by IntervalValue.ToString
// round-trips sign-intact.
func parseInterval(v string) (*IntervalValue, error) {
	if v == "" {
		return nil, fmt.Errorf("interval value is empty")
	}
	fields := strings.Fields(v)
	if len(fields) != 3 {
		return nil, fmt.Errorf("failed to parse interval %q: expected canonical \"Y-M D H:M:S[.F]\" form", v)
	}
	years, months, err := parseIntervalYearMonth(fields[0])
	if err != nil {
		return nil, fmt.Errorf("failed to parse interval %q: %w", v, err)
	}
	daySign, dayBody := splitIntervalSign(fields[1])
	days, err := parseIntervalComponent("days", dayBody)
	if err != nil {
		return nil, fmt.Errorf("failed to parse interval %q: %w", v, err)
	}
	hours, minutes, seconds, nanos, err := parseIntervalTime(fields[2])
	if err != nil {
		return nil, fmt.Errorf("failed to parse interval %q: %w", v, err)
	}
	return &IntervalValue{IntervalValue: &bigquery.IntervalValue{
		Years:          years,
		Months:         months,
		Days:           daySign * days,
		Hours:          hours,
		Minutes:        minutes,
		Seconds:        seconds,
		SubSecondNanos: nanos,
	}}, nil
}

// splitIntervalSign strips a single leading +/- and returns the group
// sign (+1 or -1) and the remaining text.
func splitIntervalSign(s string) (int32, string) {
	switch {
	case strings.HasPrefix(s, "-"):
		return -1, s[1:]
	case strings.HasPrefix(s, "+"):
		return 1, s[1:]
	}
	return 1, s
}

// parseIntervalComponent parses an unsigned decimal component.
func parseIntervalComponent(part, s string) (int32, error) {
	if s == "" {
		return 0, fmt.Errorf("missing %s component", part)
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, fmt.Errorf("invalid %s component %q", part, s)
		}
	}
	n, err := strconv.ParseInt(s, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid %s component %q: %w", part, s, err)
	}
	return int32(n), nil
}

// parseIntervalYearMonth parses "[sign]Y-M"; the group sign applies to
// both years and months.
func parseIntervalYearMonth(s string) (years, months int32, err error) {
	sign, body := splitIntervalSign(s)
	parts := strings.Split(body, "-")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("invalid year-month group %q", s)
	}
	years, err = parseIntervalComponent("years", parts[0])
	if err != nil {
		return 0, 0, err
	}
	months, err = parseIntervalComponent("months", parts[1])
	if err != nil {
		return 0, 0, err
	}
	return sign * years, sign * months, nil
}

// parseIntervalTime parses "[sign]H:M:S[.F]"; the group sign applies to
// hours, minutes, seconds, and fractional nanoseconds. The fraction is
// a mantissa: it is right-padded to nanosecond precision (and truncated
// past nine digits).
func parseIntervalTime(s string) (hours, minutes, seconds, nanos int32, err error) {
	sign, body := splitIntervalSign(s)
	frac := ""
	if dot := strings.IndexByte(body, '.'); dot >= 0 {
		frac = body[dot+1:]
		body = body[:dot]
	}
	parts := strings.Split(body, ":")
	if len(parts) != 3 {
		return 0, 0, 0, 0, fmt.Errorf("invalid time group %q", s)
	}
	hours, err = parseIntervalComponent("hours", parts[0])
	if err != nil {
		return 0, 0, 0, 0, err
	}
	minutes, err = parseIntervalComponent("minutes", parts[1])
	if err != nil {
		return 0, 0, 0, 0, err
	}
	seconds, err = parseIntervalComponent("seconds", parts[2])
	if err != nil {
		return 0, 0, 0, 0, err
	}
	if frac != "" {
		mant := frac
		if len(mant) > 9 {
			mant = mant[:9]
		}
		padded := mant + strings.Repeat("0", 9-len(mant))
		nanos, err = parseIntervalComponent("subseconds", padded)
		if err != nil {
			return 0, 0, 0, 0, err
		}
	}
	return sign * hours, sign * minutes, sign * seconds, sign * nanos, nil
}

func isNullValue(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	if _, ok := v.([]byte); ok {
		if rv.IsNil() {
			return true
		}
	}
	return false
}
