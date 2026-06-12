package date

import (
	"time"

	"github.com/goccy/googlesqlite/internal/functions/helper"
	"github.com/goccy/googlesqlite/internal/value"
)

func DATE_FROM_UNIX_DATE(unixdate int64) (value.Value, error) {
	// A unix date is a day count from the epoch, independent of any
	// time zone. Build the date in UTC: time.Unix renders in the local
	// zone, which shifts the calendar day for any process west of UTC
	// (epoch day 0 became 1969-12-31 in EST).
	t := time.Unix(int64(time.Duration(unixdate)*24*time.Hour/time.Second), 0).UTC()
	return value.DateValue(t), nil
}

var BindDateFromUnixDate = helper.Scalar1(func(a value.Value) (value.Value, error) {
	unixdate, err := a.ToInt64()
	if err != nil {
		return nil, err
	}
	return DATE_FROM_UNIX_DATE(unixdate)
})
