package value

import (
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/bigquery"
)

type IntervalValue struct {
	*bigquery.IntervalValue
}

func (iv *IntervalValue) Add(v Value) (Value, error) {
	return nil, fmt.Errorf("unsupported add operator for interval value")
}

func (iv *IntervalValue) Sub(v Value) (Value, error) {
	return nil, fmt.Errorf("unsupported sub operator for interval value")
}

func (iv *IntervalValue) Mul(v Value) (Value, error) {
	return nil, fmt.Errorf("unsupported mul operator for interval value")
}

func (iv *IntervalValue) Div(v Value) (Value, error) {
	return nil, fmt.Errorf("unsupported div operator for interval value")
}

func (iv *IntervalValue) EQ(v Value) (bool, error) {
	return false, fmt.Errorf("unsupported eq operator for interval value")
}

func (iv *IntervalValue) GT(v Value) (bool, error) {
	return false, fmt.Errorf("unsupported gt operator for interval value")
}

func (iv *IntervalValue) GTE(v Value) (bool, error) {
	return false, fmt.Errorf("unsupporte gte operator for interval value")
}

func (iv *IntervalValue) LT(v Value) (bool, error) {
	return false, fmt.Errorf("unsupported lt operator for interval value")
}

func (iv *IntervalValue) LTE(v Value) (bool, error) {
	return false, fmt.Errorf("unsupported lte operator for interval value")
}

func (iv *IntervalValue) ToInt64() (int64, error) {
	return 0, fmt.Errorf("unsupported int64 cast for interval value")
}

// ToString renders the interval in BigQuery's canonical text form
// "[sign]Y-M [sign]D [sign]H:M:S[.F]". Each of the three groups
// (year-month, day, time) carries its own sign; components inside a
// signed group are printed as absolute values, and fractional seconds
// come from abs(SubSecondNanos) with trailing zeros trimmed.
//
// cloud.google.com/go/bigquery's IntervalValue.String keys the
// year-month sign off Years and the time sign off Hours, so values
// like {Months: -2} or {Minutes: -45} lose their sign (and negative
// SubSecondNanos render garbage like "0:0:0.-5"); render here instead.
func (iv *IntervalValue) ToString() (string, error) {
	src := iv.IntervalValue
	if !src.IsCanonical() {
		src = src.Canonicalize()
	}
	var b strings.Builder
	if src.Years < 0 || src.Months < 0 {
		b.WriteByte('-')
	}
	fmt.Fprintf(&b, "%d-%d %d ", intervalAbs(src.Years), intervalAbs(src.Months), src.Days)
	if src.Hours < 0 || src.Minutes < 0 || src.Seconds < 0 || src.SubSecondNanos < 0 {
		b.WriteByte('-')
	}
	fmt.Fprintf(&b, "%d:%d:%d", intervalAbs(src.Hours), intervalAbs(src.Minutes), intervalAbs(src.Seconds))
	if src.SubSecondNanos != 0 {
		mant := strings.TrimRight(fmt.Sprintf("%09d", intervalAbs(src.SubSecondNanos)), "0")
		b.WriteByte('.')
		b.WriteString(mant)
	}
	return b.String(), nil
}

func intervalAbs(v int32) int32 {
	if v < 0 {
		return -v
	}
	return v
}

func (iv *IntervalValue) ToBytes() ([]byte, error) {
	s, err := iv.ToString()
	if err != nil {
		return nil, err
	}
	return []byte(s), nil
}

func (iv *IntervalValue) ToFloat64() (float64, error) {
	return 0, fmt.Errorf("unsupported float64 cast for interval value")
}

func (iv *IntervalValue) ToBool() (bool, error) {
	return false, fmt.Errorf("unsupported bool cast for interval value")
}

func (iv *IntervalValue) ToArray() (*ArrayValue, error) {
	return nil, fmt.Errorf("unsupported array cast for interval value")
}

func (iv *IntervalValue) ToStruct() (*StructValue, error) {
	return nil, fmt.Errorf("unsupported struct cast for interval value")
}

func (iv *IntervalValue) ToJSON() (string, error) {
	s, err := iv.ToString()
	if err != nil {
		return "", err
	}
	return strconv.Quote(s), nil
}

func (iv *IntervalValue) ToTime() (time.Time, error) {
	return time.Time{}, fmt.Errorf("unsupported time cast for interval value")
}

func (iv *IntervalValue) ToRat() (*big.Rat, error) {
	return nil, fmt.Errorf("unsupported numeric cast for interval value")
}

func (iv *IntervalValue) Format(verb rune) string {
	s, err := iv.ToString()
	if err != nil {
		return ""
	}
	return s
}

func (iv *IntervalValue) Interface() any {
	s, err := iv.ToString()
	if err != nil {
		return nil
	}
	return s
}
