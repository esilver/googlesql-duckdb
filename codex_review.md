internal/formatter.go:1854 - HIGH - `IFERROR` always decodes scalar try arms as value-layer `VARCHAR`; native expressions like `1 + 2` are `BIGINT`, so `IFERROR(1 + 2, 99)` now fails binding `googlesqlite_decode_scalar(BIGINT)`.

duckdb_aggregate_udf.go:399 - HIGH - `HLL_COUNT.MERGE` is registered with default `VARCHAR` envelope despite returning `INT64`; top-level selects decode later, but composed SQL sees a string/envelope instead of numeric.

duckdb_aggregate_udf.go:406 - MEDIUM - `having_any_value` also uses default `VARCHAR` aggregate result for type-preserving `ANY_VALUE`; numeric/typed results round-trip only at row scan, breaking composition inside SQL.

internal/info_schema.go:336 - MEDIUM - `INFORMATION_SCHEMA` rows are materialized during format/prepare, so prepared/reused statements can return stale catalog rows after DDL instead of execution-time state.

Targeted specs mostly passed; `TestIferrorNoError` failed with the first issue.