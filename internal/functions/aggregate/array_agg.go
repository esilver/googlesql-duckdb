package aggregate

import (
	"fmt"
	"sync"

	"github.com/goccy/googlesqlite/internal/functions/helper"
	"github.com/goccy/googlesqlite/internal/value"
)

type ARRAY_AGG struct {
	once   sync.Once
	opt    *helper.Option
	seen   bool
	values []*helper.OrderedValue
}

func (f *ARRAY_AGG) Step(v value.Value, opt *helper.Option) error {
	if v == nil {
		return fmt.Errorf("ARRAY_AGG: input value must be not null")
	}
	f.once.Do(func() { f.opt = opt })
	f.seen = true

	if limit := aggregateLimit(opt); limit != nil && *limit <= 0 {
		f.values = f.values[:0]
		return nil
	}
	if limit := aggregateLimit(opt); limit != nil && len(opt.OrderBy) == 0 && len(f.values) >= *limit {
		return nil
	}

	f.values = append(f.values, &helper.OrderedValue{
		OrderBy: opt.OrderBy,
		Value:   v,
	})
	f.trimToLimit(opt)
	return nil
}

func (f *ARRAY_AGG) Done() (value.Value, error) {
	// ARRAY_AGG over zero input rows is SQL NULL, not an empty array.
	if !f.seen {
		return nil, nil
	}
	f.values = helper.SortAggregatedValues(f.values, f.opt)
	f.trimToLimit(f.opt)
	values := make([]value.Value, 0, len(f.values))
	for _, v := range f.values {
		values = append(values, v.Value)
	}
	return &value.ArrayValue{
		Values: values,
	}, nil
}

func (f *ARRAY_AGG) trimToLimit(opt *helper.Option) {
	limit := aggregateLimit(opt)
	if limit == nil || len(f.values) <= *limit {
		return
	}
	if len(opt.OrderBy) > 0 {
		f.values = helper.SortAggregatedValues(f.values, opt)
	}
	f.values = f.values[:*limit]
}

func aggregateLimit(opt *helper.Option) *int {
	if opt == nil || opt.Limit == nil {
		return nil
	}
	if *opt.Limit > int64(int(^uint(0)>>1)) {
		max := int(^uint(0) >> 1)
		return &max
	}
	limit := int(*opt.Limit)
	return &limit
}
