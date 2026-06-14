# Running with no cgo (pure-Go DuckDB via wasm2go)

This fork executes GoogleSQL/BigQuery queries with **`CGO_ENABLED=0`** by
running DuckDB v1.5.3 as pure Go, transpiled from WebAssembly by
[`ncruces/wasm2go`](https://github.com/ncruces/wasm2go) instead of linking
native `libduckdb`. Only the execution backend changes. The dialect (GoogleSQL
analysis plus the `googlesqlite_*` UDFs) is the same as upstream. This is the
maintained path CI exercises. The earlier native cgo backend survives in git
history for reference.

## Setup

The engine is a normal fetchable Go module,
[`github.com/esilver/duckdb-go-pure`](https://github.com/esilver/duckdb-go-pure):
root package `duckdb`, a `database/sql` driver registered as `"duckdb"`, with
the transpiled engine committed under `internal/duckdbcore`. This repo
`require`s it like any other dependency, with no replaces, sibling checkouts, or
generated files. (The GoogleSQL analyzer is consumed via a documented `replace`
to the `go-googlesql` fork in `go.mod`, pending upstream.)

```sh
git clone https://github.com/esilver/googlesqlite
cd googlesqlite
CGO_ENABLED=0 go run ./cmd/googlesqlite :memory:
```

Expected output:

```
SELECT UPPER('duck')                -> DUCK
SELECT SAFE_DIVIDE(10, 0)           -> NULL   (real googlesqlite_* UDF, BigQuery semantics)
SELECT STARTS_WITH('duckdb','duck') -> true
SELECT SAFE_CAST('42' AS INT64)     -> 42
```

No build flags are required. Since duckdb-go-pure v0.3.1 a plain `go build`
works: the engine is sharded into ~30 packages under
`internal/duckdbcore/{core,shardNN,all}` with its oversized functions split, so
it compiles **fully optimized with inlining** in well under a minute on a
typical multi-core machine, cached thereafter. Run tests with `-vet=off`,
because `go vet` cannot analyze the generated packages.

> [!NOTE]
> Engine developers can point the build at a local checkout with
> `replace github.com/esilver/duckdb-go-pure => ../duckdb-go-pure` in `go.mod`.
> The default build needs no replace.

## How the swap works

- **`go.mod` requires `github.com/esilver/duckdb-go-pure`** (the pure-Go engine
  plus its `database/sql` driver). Former `github.com/duckdb/duckdb-go/v2`
  import sites now import `internal/duckdbgo`, a drop-in stand-in for the
  `duckdb-go/v2` API subset the emulator consumes (`Driver`, `Decimal`,
  `Type`/`TypeInfo`, `RegisterScalarUDF`, `RegisterTableUDF`, …) backed by the
  pure-Go engine. Call sites are unchanged apart from the import path.
- The historical cgo `duckdb_aggregate_udf.go` is removed (history-only).
  `duckdb_aggregate_udf_purego.go` is the supported bridge: all 32 aggregate
  specs (KLL, HLL, ANY_VALUE HAVING, googlesqlite_agg, ST_*_AGG,
  st_clusterdbscan, tf_idf, and both differential-privacy families) register as
  arity bands of ANY-parameter overloads through the engine's
  `RegisterAggregateBandConn`, with the same collect/concat/replay state model.
  The aggregate spec suite verifies it, including HLL INIT+MERGE over struct
  UNNEST and the noiseless `WITH ANONYMIZATION ANON_COUNT(*)` rewrite.
- `doLoadSpatialExtension` is a no-op: geography runs entirely on the pure-Go
  `googlesqlite_st_*`/`s2_*` UDFs, so spatial queries work without DuckDB's
  native `INSTALL spatial`.

## How the UDFs run with no cgo

The cgo build hands DuckDB a C function pointer per `googlesqlite_*` UDF. In the
wasm2go engine the indirect-call table is a Go `[]any` of closures, so the
backend appends the Go UDF closure to that table and passes its index as the
"function pointer" to `duckdb_create_scalar_function`. DuckDB then
`call_indirect`s back into Go, through the same `database/sql` +
`RegisterScalarUDF` surface the emulator already uses.

## Status

- ✅ Compiles with `CGO_ENABLED=0`. The core data path (DDL/DML,
  cross-connection queries) and scalar UDFs (string/math/`SAFE_*` builtins) run
  pure Go.
- ✅ Aggregates: all 32 specs register and pass the spec suite. The engine ships
  with the json and icu extensions (UNNEST lowering rides `json_each`,
  timezone-aware temporals need icu).
- ✅ Conformance: the named spec gate runs the full normalized corpus. Counts
  and the implemented/partial/unsupported breakdown live in
  [docs/specs/INDEX.md](docs/specs/INDEX.md). The
  `bigquery/functions/{search,objectref}` family runs on pure-Go bodies
  (`textfn.BindSearch`, `extras.BindObj*`) routed through the formatter retarget
  (`trySearchObjectRefFunc`) plus the DuckDB UDF allow-list (SEARCH declared
  BOOL). Run the gate pure-Go with:

  ```sh
  CGO_ENABLED=0 go test -vet=off \
    -run 'TestSpec/(googlesql|bigquery)' -test.parallel 4 -timeout 2400s .
  ```

> [!CAUTION]
> Known edge case: a DuckDB-level error inside `catalog.Sync` currently skips a
> rollback on the error path, which can leave a transaction open on the pooled
> connection and surface as "cannot start a transaction within a transaction" on
> later queries. Stable since the JSON-extension fix. Adding a rollback on that
> path would close it fully.
