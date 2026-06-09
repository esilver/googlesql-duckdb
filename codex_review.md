internal/functions/aggregate/agg.go:62 - HIGH - `COUNT(expr)` re-aggregation counts locked rows including NULL values; BigQuery `COUNT(expr)` must skip NULLs. `COUNT(*)` is the only row-count case.

internal/functions/aggregate/agg.go:91 - HIGH - `SUM(INT64)` re-aggregation sums via `float64` then casts back to `int64`, losing precision for values above 2^53 and breaking type round-trips.

internal/formatter.go:2548 - HIGH - ST_CLUSTERDBSCAN reuses the analytic `OVER` clause as-is; with `ORDER BY` DuckDB aggregate windows default to a prefix frame, so clustering is computed over rows up to the current row, not the whole partition.

internal/graph_scan_node.go:770 - MED - MEASURE inner expressions are treated as a single quoted column name; `MEASURE(SUM(x + y))` lowers to `"alias"."x + y"` instead of evaluating the expression.

internal/graph_scan_node.go:819 - MED - AGG locking key ignores registered graph element key metadata and uses physical `rowid`; this diverges from element-key semantics and can mis-dedup if logical element identity is not physical-row identity.