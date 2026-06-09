package geography

import (
	"fmt"

	"github.com/goccy/googlesqlite/internal/functions/helper"
	"github.com/goccy/googlesqlite/internal/value"
)

// BindStClusterDBSCANAgg adapts ST_CLUSTERDBSCAN to a plain
// helper.Aggregator that returns the whole partition's cluster-id
// ARRAY indexed by row position, so the DuckDB aggregate bridge can
// register it and DuckDB can run it as a window function under OVER ().
//
// DuckDB has no native per-row analytic DBSCAN, and the SQLite backend
// emulated it with a correlated-subquery + rowid-marker hack that relied
// on the engine preserving scan order (DuckDB's parallel scan does not).
// Instead the formatter lowers
//
//	ST_CLUSTERDBSCAN(geo, eps, min_pts) OVER (w)
//
// to (schematically)
//
//	array_at_ordinal(st_clusterdbscan(geo, eps, min_pts, row_number() OVER (w)) OVER (w),
//	                 row_number() OVER (w))
//
// The fourth argument is the row's 1-based row_number within the
// window. This aggregate, run as the window function, places each
// partition geography at slot (row_number-1) of its buffer - so the
// buffer is in row_number order regardless of the (parallel,
// unordered) order DuckDB feeds rows to the aggregate. At finalize it
// runs DBSCAN once over the slotted geographies (reusing the
// dbscanClusters body) and returns an ARRAY<INT64> whose i-th element
// is the 0-based cluster id of the row with row_number i+1 (noise /
// empty / NULL -> NULL element). The outer array_at_ordinal then picks
// the current row's element by its own row_number. Because the same
// row's row_number is used to both place and read its geography,
// alignment is guaranteed by construction and never depends on the
// aggregate's feed order matching the outer row_number()'s order
// (which DuckDB does not guarantee across two separate window calls).
// Slotting in row_number order also makes the cluster-id numbering
// deterministic and row-ordered, matching the upstream result.
func BindStClusterDBSCANAgg() func() *helper.Aggregator {
	return func() *helper.Aggregator {
		type slot struct {
			rn  int64
			geo *value.GeographyValue
		}
		var (
			slots      []slot
			maxRN      int64
			eps        float64
			minPts     int
			haveParams bool
		)
		return helper.NewAggregator(
			func(args []value.Value, _ *helper.Option) error {
				if len(args) == 0 {
					return nil
				}
				if !haveParams {
					if len(args) > 1 && args[1] != nil {
						v, err := args[1].ToFloat64()
						if err != nil {
							return err
						}
						eps = v
					}
					if len(args) > 2 && args[2] != nil {
						n, err := args[2].ToInt64()
						if err != nil {
							return err
						}
						if n < 0 {
							return fmt.Errorf("ST_CLUSTERDBSCAN: minimum_geographies must be non-negative, got %d", n)
						}
						mp, err := helper.SafeInt(n)
						if err != nil {
							return err
						}
						minPts = mp
					}
					haveParams = true
				}
				// args[3] is the row's 1-based row_number within the
				// window, threaded in by the formatter's lowering so the
				// buffer can be ordered independently of feed order.
				var rn int64
				if len(args) > 3 && args[3] != nil {
					r, err := args[3].ToInt64()
					if err != nil {
						return err
					}
					rn = r
				}
				if rn > maxRN {
					maxRN = rn
				}
				slots = append(slots, slot{rn: rn, geo: geographyArg(args[0])})
				return nil
			},
			func() (value.Value, error) {
				// Place each geography at row_number order so DBSCAN
				// visits rows in the deterministic, row-ordered sequence
				// the upstream cluster numbering expects.
				geos := make([]*value.GeographyValue, maxRN)
				for _, s := range slots {
					if s.rn >= 1 && s.rn <= maxRN {
						geos[s.rn-1] = s.geo
					}
				}
				clusters := dbscanClusters(geos, eps, minPts)
				out := &value.ArrayValue{Values: make([]value.Value, len(clusters))}
				for i, c := range clusters {
					if c < 0 {
						// noise / empty / NULL -> NULL element so the
						// outer array_at_ordinal yields SQL NULL.
						out.Values[i] = nil
						continue
					}
					out.Values[i] = value.IntValue(int64(c))
				}
				return out, nil
			},
		)
	}
}
