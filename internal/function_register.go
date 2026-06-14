package internal

import (
	"fmt"
	"sync"

	"github.com/goccy/googlesqlite/internal/functions/window"
	"github.com/goccy/googlesqlite/internal/value"
)

type nameAndFunc struct {
	Name string
	Func any
}

var (
	funcMapMu          sync.RWMutex
	registerFuncOnce   sync.Once
	normalFuncMap      = map[string][]*nameAndFunc{}
	aggregateFuncMap   = map[string][]*nameAndFunc{}
	windowFuncMap      = map[string][]*nameAndFunc{}
	currentTimeFuncMap = map[string]struct{}{
		"current_date":      {},
		"current_datetime":  {},
		"current_time":      {},
		"current_timestamp": {},
	}
)

// populateFuncMaps fills the normal / aggregate / window function maps.
// It is run exactly once, guarded by registerFuncOnce, and touches only
// the global maps (no connection required), so the DuckDB path
// (EnsureFuncMaps) can drive it.
func populateFuncMaps() {
	for _, info := range normalFuncs {
		setupNormalFuncMap(info)
	}
	for _, info := range aggregateFuncs {
		setupAggregateFuncMap(info)
	}
	for _, info := range windowFuncs {
		setupWindowFuncMap(info)
	}
	// Replace the predecessor's per-row full-scan emulation for
	// the window forms of these aggregators with native
	// incremental implementations. The formatter emits these
	// through OVER syntax in customNativeWindowFuncMap, so DuckDB
	// drives Step/Done over the active frame instead of the
	// O(N²) correlated subquery the predecessor required.
	windowFuncMap["array_agg"] = []*nameAndFunc{
		{Name: "googlesqlite_window_array_agg", Func: window.NewArrayAggWindowNative()},
	}
	windowFuncMap["string_agg"] = []*nameAndFunc{
		{Name: "googlesqlite_window_string_agg", Func: window.NewStringAggWindowNative()},
	}
	windowFuncMap["countif"] = []*nameAndFunc{
		{Name: "googlesqlite_window_countif", Func: window.NewCountifWindowNative()},
	}
	windowFuncMap["count_star"] = []*nameAndFunc{
		{Name: "googlesqlite_window_count_star", Func: window.NewCountStarWindowNative()},
	}
	windowFuncMap["any_value"] = []*nameAndFunc{
		{Name: "googlesqlite_window_any_value", Func: window.NewAnyValueWindowNative()},
	}
	windowFuncMap["corr"] = []*nameAndFunc{
		{Name: "googlesqlite_window_corr", Func: window.NewCorrWindowNative()},
	}
	windowFuncMap["covar_pop"] = []*nameAndFunc{
		{Name: "googlesqlite_window_covar_pop", Func: window.NewCovarPopWindowNative()},
	}
	windowFuncMap["covar_samp"] = []*nameAndFunc{
		{Name: "googlesqlite_window_covar_samp", Func: window.NewCovarSampWindowNative()},
	}
	windowFuncMap["stddev_pop"] = []*nameAndFunc{
		{Name: "googlesqlite_window_stddev_pop", Func: window.NewStddevPopWindowNative()},
	}
	windowFuncMap["stddev_samp"] = []*nameAndFunc{
		{Name: "googlesqlite_window_stddev_samp", Func: window.NewStddevSampWindowNative()},
	}
	windowFuncMap["var_pop"] = []*nameAndFunc{
		{Name: "googlesqlite_window_var_pop", Func: window.NewVarPopWindowNative()},
	}
	windowFuncMap["var_samp"] = []*nameAndFunc{
		{Name: "googlesqlite_window_var_samp", Func: window.NewVarSampWindowNative()},
	}
	// DISTINCT-aware window aggregators (SUM/COUNT/AVG). These
	// don't replace existing entries — they're in addition,
	// keyed under their custom registration names so they're
	// looked up by the formatter's distinctAwareNativeWindowFuncs
	// path rather than via predecessor name.
	windowFuncMap["sum_distinct"] = []*nameAndFunc{
		{Name: "googlesqlite_window_sum_distinct", Func: window.NewSumDistinctWindowNative()},
	}
	windowFuncMap["count_distinct"] = []*nameAndFunc{
		{Name: "googlesqlite_window_count_distinct", Func: window.NewCountDistinctWindowNative()},
	}
	windowFuncMap["avg_distinct"] = []*nameAndFunc{
		{Name: "googlesqlite_window_avg_distinct", Func: window.NewAvgDistinctWindowNative()},
	}
	windowFuncMap["percentile_cont"] = []*nameAndFunc{
		{Name: "googlesqlite_window_percentile_cont", Func: window.NewPercentileContWindowNative()},
	}
	windowFuncMap["percentile_disc"] = []*nameAndFunc{
		{Name: "googlesqlite_window_percentile_disc", Func: window.NewPercentileDiscWindowNative()},
	}
	// Native window forms for the boolean / bitwise aggregators
	// that previously fell through to the predecessor's
	// correlated-subquery emulation.
	windowFuncMap["logical_or"] = []*nameAndFunc{
		{Name: "googlesqlite_window_logical_or", Func: window.NewLogicalOrWindowNative()},
	}
	windowFuncMap["logical_and"] = []*nameAndFunc{
		{Name: "googlesqlite_window_logical_and", Func: window.NewLogicalAndWindowNative()},
	}
	windowFuncMap["bit_and"] = []*nameAndFunc{
		{Name: "googlesqlite_window_bit_and", Func: window.NewBitAndAggWindowNative()},
	}
	windowFuncMap["bit_or"] = []*nameAndFunc{
		{Name: "googlesqlite_window_bit_or", Func: window.NewBitOrAggWindowNative()},
	}
	windowFuncMap["bit_xor"] = []*nameAndFunc{
		{Name: "googlesqlite_window_bit_xor", Func: window.NewBitXorAggWindowNative()},
	}
	windowFuncMap["array_concat_agg"] = []*nameAndFunc{
		{Name: "googlesqlite_window_array_concat_agg", Func: window.NewArrayConcatAggWindowNative()},
	}
}

// ensureDuckDBFuncMapOnce guards the DuckDB-path population so it runs once.
var ensureDuckDBFuncMapOnce sync.Once

// EnsureFuncMaps populates normalFuncMap with exactly the builtins the DuckDB
// retarget has registered as googlesqlite_<name> scalar UDFs. The formatter's
// getFuncNameAndArgs lowers a builtin to its googlesqlite_<name> shell only
// when the name is present in normalFuncMap; populating just the registered
// subset keeps every other builtin on its bare DuckDB-native lowering (so an
// unported function does not lower to a UDF that was never created).
func EnsureFuncMaps() {
	ensureDuckDBFuncMapOnce.Do(func() {
		funcMapMu.Lock()
		defer funcMapMu.Unlock()
		for _, info := range normalFuncs {
			if _, ported := duckdbPortedFuncNames[info.Name]; ported {
				setupNormalFuncMap(info)
			}
		}
	})
}

// duckdbPortedFuncNames is the set of canonical builtin names whose
// googlesqlite_<name> scalar UDFs are registered against DuckDB. It is the
// single source of truth shared by EnsureFuncMaps (which decides what the
// formatter lowers to a UDF) and the root-package registration (which decides
// what UDF exists), so the two can never drift apart.
var duckdbPortedFuncNames = func() map[string]struct{} {
	specs := DuckDBNormalUDFSpecs()
	m := make(map[string]struct{}, len(specs))
	for _, spec := range specs {
		m[spec.Name] = struct{}{}
	}
	return m
}()

func setupNormalFuncMap(info *funcInfo) {
	normalFuncMap[info.Name] = append(normalFuncMap[info.Name], &nameAndFunc{
		Name: fmt.Sprintf("googlesqlite_%s", info.Name),
		Func: func(args ...any) (any, error) {
			values, err := value.ConvertArgs(args...)
			if err != nil {
				return nil, err
			}
			ret, err := info.BindFunc(values...)
			if err != nil {
				return nil, err
			}
			return EncodeValue(ret)
		},
	}, &nameAndFunc{
		Name: fmt.Sprintf("googlesqlite_safe_%s", info.Name),
		Func: func(args ...any) (any, error) {
			values, err := value.ConvertArgs(args...)
			if err != nil {
				return nil, err
			}
			ret, err := info.BindFunc(values...)
			if err != nil {
				// Note, this should only suppress semantic errors based on the
				// input data. See
				// https://github.com/google/googlesql/blob/master/docs/resolved_ast.md#resolvedfunctioncallbase
				return nil, nil
			}
			return EncodeValue(ret)
		},
	})
}

func setupAggregateFuncMap(info *aggregateFuncInfo) {
	aggregateFuncMap[info.Name] = append(aggregateFuncMap[info.Name], &nameAndFunc{
		Name: fmt.Sprintf("googlesqlite_%s", info.Name),
		Func: info.BindFunc(),
	})
}

func setupWindowFuncMap(info *windowFuncInfo) {
	windowFuncMap[info.Name] = append(windowFuncMap[info.Name], &nameAndFunc{
		Name: fmt.Sprintf("googlesqlite_window_%s", info.Name),
		Func: info.BindFunc(),
	})
}
