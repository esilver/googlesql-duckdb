package aggregate

import (
	"math"
	"math/rand"

	"github.com/goccy/googlesqlite/internal/functions/helper"
	"github.com/goccy/googlesqlite/internal/value"
)

// This file backs the BigQuery / ZetaSQL differential-privacy
// aggregates on both engine backends. The analyzer's
// RewriteAnonymization stage lowers a SELECT WITH ANONYMIZATION /
// WITH DIFFERENTIAL_PRIVACY query into a two-level plan: an inner
// per-privacy-unit partial aggregate (sum / count / avg / array_agg
// grouped by the user-id column) feeding an OUTER differential-privacy
// aggregate over those partials. These constructors implement the
// outer aggregate.
//
// Argument layout the formatter emits for the outer call (see
// internal/formatter.go AggregateFunctionCall + the DP / anon scan
// FormatSQL methods):
//
//	[partial-value] [positional...] [contribution-bounds] [epsilon] [delta]
//
//   - partial-value is the per-user partial: a scalar for SUM / AVG /
//     COUNT(->SUM), or an ARRAY of the per-user values for VAR_POP /
//     STDDEV_POP / PERCENTILE_CONT / QUANTILES.
//   - positional args precede the bounds only for PERCENTILE_CONT (the
//     0.5 percentile) and QUANTILES (the bucket count).
//   - contribution-bounds is EITHER a STRUCT<lo, hi> (the
//     DIFFERENTIAL_PRIVACY surface's contribution_bounds_per_* named
//     arg) OR two trailing scalar bounds lo, hi (the ANONYMIZATION
//     surface's CLAMPED BETWEEN lo AND hi). Either, or neither, may be
//     present.
//   - epsilon / delta are appended by the formatter from the scan's
//     OPTIONS so every body can size noise from the privacy budget. The
//     high-epsilon "without noise" examples (epsilon=1e20) drive the
//     noise scale to ~0, so the deterministic exact-value assertions
//     hold.

// flattenToFloats extracts every numeric leaf from a value, walking
// through arrays. When a value is an ArrayValue, each element is
// recursively flattened. Non-numeric leaves are skipped silently.
// Used by the partial-then-final DP aggregates (variance / stddev /
// percentile / quantile) where the per-privacy-unit values arrive as
// an ARRAY the final aggregate consumes.
func flattenToFloats(v value.Value) []float64 {
	if v == nil {
		return nil
	}
	if arr, ok := v.(*value.ArrayValue); ok {
		var out []float64
		for _, el := range arr.Values {
			out = append(out, flattenToFloats(el)...)
		}
		return out
	}
	if f, err := v.ToFloat64(); err == nil {
		return []float64{f}
	}
	return nil
}

// laplaceSample returns a sample from Laplace(0, scale) using a
// uniform-random inverse-CDF transform. Used by the differential
// privacy aggregates to add calibrated noise that satisfies
// (epsilon, delta)-differential privacy. A non-positive scale (the
// high-epsilon path collapses sensitivity/epsilon to ~0) yields 0 so
// the deterministic "without noise" examples match exactly.
func laplaceSample(scale float64) float64 {
	if scale <= 0 || math.IsNaN(scale) {
		return 0
	}
	u := rand.Float64() - 0.5
	if u < 0 {
		return scale * math.Log(1+2*u)
	}
	return -scale * math.Log(1-2*u)
}

// dpNoiseEpsilonFloor is the smallest epsilon below which we still add
// noise. Above the eps where sensitivity/eps underflows to numerical
// zero (the epsilon=1e20 "without noise" examples) the noise vanishes
// on its own; this floor only guards the divide.
const dpNoiseEpsilonFloor = 1e-12

// dpArgs is the parsed outer-aggregate argument shape: the per-user
// value, any leading positional args (percentile fraction / bucket
// count), the contribution bounds, and the privacy budget.
type dpArgs struct {
	val        value.Value
	positional []value.Value
	lo, hi     float64
	hasBounds  bool
	eps, delta float64
}

// clamp restricts x to [lo, hi] when bounds were supplied; otherwise it
// returns x unchanged (an absent CLAMP / contribution_bounds means no
// clamping, matching ZetaSQL where the bound is then implicit).
func (a dpArgs) clamp(x float64) float64 {
	if !a.hasBounds {
		return x
	}
	if x < a.lo {
		return a.lo
	}
	if x > a.hi {
		return a.hi
	}
	return x
}

// parseDPArgs splits one Step argument tuple into dpArgs. nLead is the
// number of leading positional args that sit between the value and the
// bounds (1 for PERCENTILE_CONT / QUANTILES, 0 otherwise).
//
// Both bounds conventions are handled. A STRUCT anywhere after the
// value supplies (lo, hi) directly. Otherwise the trailing scalars (the
// args after the leading positionals, minus the epsilon/delta pair) are
// read as the (lo, hi) pair. epsilon / delta are taken from the last
// two scalars when the tuple is long enough to carry them.
func parseDPArgs(args []value.Value, nLead int) dpArgs {
	out := dpArgs{eps: 1, delta: 0, lo: math.Inf(-1), hi: math.Inf(1)}
	if len(args) == 0 {
		return out
	}
	out.val = args[0]
	rest := args[1:]

	// A STRUCT<lo, hi> bound (DIFFERENTIAL_PRIVACY surface). When
	// present, drop it from rest so it isn't mistaken for a positional
	// or scalar bound.
	structIdx := -1
	for i, a := range rest {
		if sv, ok := a.(*value.StructValue); ok && sv != nil {
			out.lo, out.hi = dpStructBounds(sv)
			out.hasBounds = true
			structIdx = i
			break
		}
	}
	if structIdx >= 0 {
		rest = append(append([]value.Value{}, rest[:structIdx]...), rest[structIdx+1:]...)
	}

	// epsilon / delta are the last two scalars the formatter appended.
	// Require enough room beyond the leading positional args before
	// peeling them so a bare two-arg call (value, eps) is not misread.
	if len(rest) >= nLead+2 {
		if f, err := rest[len(rest)-2].ToFloat64(); err == nil {
			out.eps = f
		}
		if f, err := rest[len(rest)-1].ToFloat64(); err == nil {
			out.delta = f
		}
		rest = rest[:len(rest)-2]
	}

	// Leading positional args (percentile fraction / bucket count).
	for i := 0; i < nLead && i < len(rest); i++ {
		out.positional = append(out.positional, rest[i])
	}
	if nLead <= len(rest) {
		rest = rest[nLead:]
	} else {
		rest = nil
	}

	// Remaining scalars (if no STRUCT bound was found) are the
	// CLAMPED BETWEEN lo AND hi pair from the ANONYMIZATION surface.
	if !out.hasBounds && len(rest) >= 2 {
		if lo, err := rest[len(rest)-2].ToFloat64(); err == nil {
			if hi, err2 := rest[len(rest)-1].ToFloat64(); err2 == nil {
				out.lo, out.hi, out.hasBounds = lo, hi, true
			}
		}
	}
	return out
}

// dpStructBounds reads a STRUCT<lo, hi> Value pair (as produced by
// GoogleSQL's `contribution_bounds_per_group => (a, b)` named argument)
// and returns the (lo, hi) bounds.
func dpStructBounds(sv *value.StructValue) (float64, float64) {
	lo, hi := math.Inf(-1), math.Inf(1)
	if sv == nil {
		return lo, hi
	}
	if len(sv.Values) >= 1 && sv.Values[0] != nil {
		if f, err := sv.Values[0].ToFloat64(); err == nil {
			lo = f
		}
	}
	if len(sv.Values) >= 2 && sv.Values[1] != nil {
		if f, err := sv.Values[1].ToFloat64(); err == nil {
			hi = f
		}
	}
	return lo, hi
}

// dpSensitivity returns the L1 sensitivity for a SUM / AVG aggregate
// with contribution bounds (lo, hi): the largest absolute clamped
// contribution. An unbounded aggregate falls back to 1.
func dpSensitivity(a dpArgs) float64 {
	if !a.hasBounds {
		return 1
	}
	s := math.Max(math.Abs(a.lo), math.Abs(a.hi))
	if s == 0 || math.IsInf(s, 0) {
		return 1
	}
	return s
}

// dpScalarValues folds a Step tuple into a running list of clamped
// per-user contributions. The value may be a scalar or an ARRAY of
// per-user values; every numeric leaf is clamped and collected.
func dpCollect(a dpArgs, vals *[]float64) {
	if a.val == nil {
		return
	}
	for _, f := range flattenToFloats(a.val) {
		*vals = append(*vals, a.clamp(f))
	}
}

func BindDifferentialPrivacySum() func() *helper.Aggregator {
	return func() *helper.Aggregator {
		var vals []float64
		var pa dpArgs
		return helper.NewAggregator(
			func(args []value.Value, _ *helper.Option) error {
				pa = parseDPArgs(args, 0)
				dpCollect(pa, &vals)
				return nil
			},
			func() (value.Value, error) {
				var sum float64
				for _, v := range vals {
					sum += v
				}
				scale := dpSensitivity(pa) / math.Max(pa.eps, dpNoiseEpsilonFloor)
				return value.FloatValue(sum + laplaceSample(scale)), nil
			},
		)
	}
}

func BindDifferentialPrivacyCount() func() *helper.Aggregator {
	return func() *helper.Aggregator {
		var n int64
		var pa dpArgs
		return helper.NewAggregator(
			func(args []value.Value, _ *helper.Option) error {
				pa = parseDPArgs(args, 0)
				if pa.val != nil {
					n++
				}
				return nil
			},
			func() (value.Value, error) {
				scale := dpSensitivity(pa) / math.Max(pa.eps, dpNoiseEpsilonFloor)
				return value.IntValue(int64(math.Round(float64(n) + laplaceSample(scale)))), nil
			},
		)
	}
}

func BindDifferentialPrivacyCountStar() func() *helper.Aggregator {
	return func() *helper.Aggregator {
		var n int64
		var pa dpArgs
		return helper.NewAggregator(
			func(args []value.Value, _ *helper.Option) error {
				pa = parseDPArgs(args, 0)
				n++
				return nil
			},
			func() (value.Value, error) {
				// COUNT(*) sensitivity is 1 (adding/removing one user
				// changes the count by 1).
				scale := 1.0 / math.Max(pa.eps, dpNoiseEpsilonFloor)
				return value.IntValue(int64(math.Round(float64(n) + laplaceSample(scale)))), nil
			},
		)
	}
}

func BindDifferentialPrivacyAvg() func() *helper.Aggregator {
	return func() *helper.Aggregator {
		var vals []float64
		var pa dpArgs
		return helper.NewAggregator(
			func(args []value.Value, _ *helper.Option) error {
				pa = parseDPArgs(args, 0)
				dpCollect(pa, &vals)
				return nil
			},
			func() (value.Value, error) {
				if len(vals) == 0 {
					return nil, nil
				}
				var sum float64
				for _, v := range vals {
					sum += v
				}
				scale := dpSensitivity(pa) / math.Max(pa.eps, dpNoiseEpsilonFloor)
				mean := (sum + laplaceSample(scale)) / float64(len(vals))
				return value.FloatValue(mean), nil
			},
		)
	}
}

func BindDifferentialPrivacyVarPop() func() *helper.Aggregator {
	return func() *helper.Aggregator {
		var vals []float64
		var pa dpArgs
		return helper.NewAggregator(
			func(args []value.Value, _ *helper.Option) error {
				pa = parseDPArgs(args, 0)
				dpCollect(pa, &vals)
				return nil
			},
			func() (value.Value, error) {
				if len(vals) == 0 {
					return nil, nil
				}
				v := popVariance(vals)
				sens := dpSensitivity(pa)
				scale := (sens * sens) / math.Max(pa.eps, dpNoiseEpsilonFloor)
				return value.FloatValue(math.Max(v+laplaceSample(scale), 0)), nil
			},
		)
	}
}

func BindDifferentialPrivacyStddevPop() func() *helper.Aggregator {
	return func() *helper.Aggregator {
		var vals []float64
		var pa dpArgs
		return helper.NewAggregator(
			func(args []value.Value, _ *helper.Option) error {
				pa = parseDPArgs(args, 0)
				dpCollect(pa, &vals)
				return nil
			},
			func() (value.Value, error) {
				if len(vals) == 0 {
					return nil, nil
				}
				v := popVariance(vals)
				sens := dpSensitivity(pa)
				scale := (sens * sens) / math.Max(pa.eps, dpNoiseEpsilonFloor)
				return value.FloatValue(math.Sqrt(math.Max(v+laplaceSample(scale), 0))), nil
			},
		)
	}
}

func BindDifferentialPrivacyApproxCountDistinct() func() *helper.Aggregator {
	return func() *helper.Aggregator {
		seen := map[string]struct{}{}
		var pa dpArgs
		return helper.NewAggregator(
			func(args []value.Value, _ *helper.Option) error {
				pa = parseDPArgs(args, 0)
				if pa.val == nil {
					return nil
				}
				s, err := pa.val.ToString()
				if err != nil {
					return err
				}
				seen[s] = struct{}{}
				return nil
			},
			func() (value.Value, error) {
				scale := 1.0 / math.Max(pa.eps, dpNoiseEpsilonFloor)
				return value.IntValue(int64(math.Round(float64(len(seen)) + laplaceSample(scale)))), nil
			},
		)
	}
}

// BindDifferentialPrivacyPercentileCont approximates the
// differentially-private continuous percentile. The percentile fraction
// is the single leading positional arg; values flow through args[0]
// (the per-user ARRAY). Done sorts the collected clamped values and
// linearly interpolates the requested percentile, then adds Laplace
// noise scaled by the contribution-bound sensitivity / epsilon.
func BindDifferentialPrivacyPercentileCont() func() *helper.Aggregator {
	return func() *helper.Aggregator {
		var vals []float64
		var pa dpArgs
		percentile := 0.5
		return helper.NewAggregator(
			func(args []value.Value, _ *helper.Option) error {
				pa = parseDPArgs(args, 1)
				if len(pa.positional) >= 1 && pa.positional[0] != nil {
					if f, err := pa.positional[0].ToFloat64(); err == nil {
						percentile = f
					}
				}
				dpCollect(pa, &vals)
				return nil
			},
			func() (value.Value, error) {
				if len(vals) == 0 {
					return nil, nil
				}
				sorted := append([]float64(nil), vals...)
				sortFloats(sorted)
				v := interpolatePercentile(sorted, percentile)
				scale := dpSensitivity(pa) / math.Max(pa.eps, dpNoiseEpsilonFloor)
				return value.FloatValue(v + laplaceSample(scale)), nil
			},
		)
	}
}

// BindDifferentialPrivacyQuantiles emits an ARRAY of quantile cut
// points. The single leading positional arg is the bucket count n; the
// returned array has length n+1 (0/n, 1/n, ..., n/n). Each quantile
// point gets independent Laplace noise.
func BindDifferentialPrivacyQuantiles() func() *helper.Aggregator {
	return func() *helper.Aggregator {
		var vals []float64
		var pa dpArgs
		buckets := int64(4)
		return helper.NewAggregator(
			func(args []value.Value, _ *helper.Option) error {
				pa = parseDPArgs(args, 1)
				if len(pa.positional) >= 1 && pa.positional[0] != nil {
					if n, err := pa.positional[0].ToInt64(); err == nil && n > 0 {
						buckets = n
					}
				}
				dpCollect(pa, &vals)
				return nil
			},
			func() (value.Value, error) {
				if len(vals) == 0 {
					return &value.ArrayValue{}, nil
				}
				sorted := append([]float64(nil), vals...)
				sortFloats(sorted)
				scale := dpSensitivity(pa) / math.Max(pa.eps, dpNoiseEpsilonFloor)
				out := make([]value.Value, 0, buckets+1)
				for i := int64(0); i <= buckets; i++ {
					p := float64(i) / float64(buckets)
					v := interpolatePercentile(sorted, p)
					out = append(out, value.FloatValue(v+laplaceSample(scale)))
				}
				return &value.ArrayValue{Values: out}, nil
			},
		)
	}
}

// popVariance returns the population variance of vals.
func popVariance(vals []float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	var mean float64
	for _, v := range vals {
		mean += v
	}
	mean /= float64(len(vals))
	var ss float64
	for _, v := range vals {
		ss += (v - mean) * (v - mean)
	}
	return ss / float64(len(vals))
}

// interpolatePercentile returns the linearly-interpolated percentile p
// (0..1) of a pre-sorted slice.
func interpolatePercentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	if p < 0 {
		p = 0
	}
	if p > 1 {
		p = 1
	}
	idx := p * float64(len(sorted)-1)
	lo := int(math.Floor(idx))
	hi := int(math.Ceil(idx))
	v := sorted[lo]
	if hi != lo {
		v += (sorted[hi] - sorted[lo]) * (idx - float64(lo))
	}
	return v
}

func sortFloats(s []float64) {
	// Small inline insertion sort is enough for the per-group sizes our
	// DP examples exercise (a few hundred at most).
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}
