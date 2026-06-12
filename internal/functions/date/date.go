package date

import (
	"fmt"
	"time"

	"github.com/goccy/googlesqlite/internal/functions/helper"
	"github.com/goccy/googlesqlite/internal/value"
)

func DATE(args ...value.Value) (value.Value, error) {
	if len(args) == 3 {
		year, err := args[0].ToInt64()
		if err != nil {
			return nil, err
		}
		month, err := args[1].ToInt64()
		if err != nil {
			return nil, err
		}
		day, err := args[2].ToInt64()
		if err != nil {
			return nil, err
		}
		yearInt, err := helper.SafeInt(year)
		if err != nil {
			return nil, err
		}
		monthInt, err := helper.SafeInt(month)
		if err != nil {
			return nil, err
		}
		dayInt, err := helper.SafeInt(day)
		if err != nil {
			return nil, err
		}
		t := time.Date(yearInt, time.Month(monthInt), dayInt, 0, 0, 0, 0, time.UTC)
		if t.Year() != yearInt || int(t.Month()) != monthInt || t.Day() != dayInt {
			return nil, fmt.Errorf("DATE: date field out of range: %04d-%02d-%02d", yearInt, monthInt, dayInt)
		}
		return value.DateValue(t), nil
	} else if len(args) == 2 {
		t, err := args[0].ToTime()
		if err != nil {
			return nil, err
		}
		zone, err := args[1].ToString()
		if err != nil {
			return nil, err
		}
		loc, err := value.ToLocation(zone)
		if err != nil {
			return nil, err
		}
		return value.DateValue(t.In(loc)), nil
	} else {
		t, err := args[0].ToTime()
		if err != nil {
			return nil, err
		}
		return value.DateValue(t), nil
	}
}

// BindDate short-circuits to NULL when any argument is NULL; DATE
// itself performs the arity dispatch.
var BindDate = helper.ScalarN(DATE)
