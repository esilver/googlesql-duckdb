# Running the DuckDB backend with NO cgo (pure-Go DuckDB via wasm2go)

This fork can execute GoogleSQL/BigQuery queries with **`CGO_ENABLED=0`** by
swapping the native-`libduckdb` backend (`github.com/duckdb/duckdb-go/v2`) for a
**pure-Go DuckDB v1.5.3** transpiled by [`ncruces/wasm2go`](https://github.com/ncruces/wasm2go).
The BigQuery dialect (GoogleSQL analysis + the `googlesqlite_*` UDFs) is the same as
upstream, only the DuckDB *execution backend* changes. The pure-Go backend is the
maintained path that CI exercises on this branch. The earlier native cgo backend
remains in the history for reference.

## Setup - ONE clone, zero replaces for the engine

The engine is a normal fetchable Go module:
[`github.com/esilver/duckdb-go-pure`](https://github.com/esilver/duckdb-go-pure)
(root package `duckdb`, a `database/sql` driver registered as `"duckdb"`, with
the transpiled engine committed under `internal/duckdbcore`). This repo
`require`s it like any other dependency: zero replaces for the DuckDB engine,
no sibling checkouts, no generated files. The GoogleSQL analyzer is consumed
via a documented replace to the `go-googlesql` fork until the fixes land
upstream (see the `replace` directive in `go.mod`).

```sh
git clone https://github.com/esilver/googlesqlite
cd googlesqlite

# Run the emulator with NO cgo (no build flags required):
CGO_ENABLED=0 go run ./cmd/googlesqlite :memory:
```

(Engine developers working on the transpiler output can point the build at a
local checkout by adding
`replace github.com/esilver/duckdb-go-pure => ../duckdb-go-pure` to `go.mod`.
This repo's default build needs no replace directive.)

Expected: GoogleSQL queries run pure-Go, e.g.

```
SELECT UPPER('duck')        -> DUCK
SELECT SAFE_DIVIDE(10, 0)   -> NULL          (real googlesqlite_* UDF, BigQuery semantics)
SELECT STARTS_WITH('duckdb','duck') -> true
SELECT SAFE_CAST('42' AS INT64) -> 42
```

(No build flags required. Since duckdb-go-pure v0.3.1 a plain `go build`
works: the engine is sharded into ~30 packages under
`internal/duckdbcore/{core,shardNN,all}` with its oversized functions split,
so the whole engine compiles **fully optimized with inlining**. Run tests
with `-vet=off`, because `go vet` cannot analyze the generated packages. The
first build compiles the engine in well under a minute on a typical multi-core
development machine, and is cached afterwards.)

## How the swap works

- **`go.mod` requires `github.com/esilver/duckdb-go-pure`** (the pure-Go engine
  + `database/sql` driver). The former `github.com/duckdb/duckdb-go/v2` import
  sites now import `internal/duckdbgo` — a drop-in stand-in for the
  `duckdb-go/v2` API subset the emulator consumes (`Driver`, `Decimal`,
  `Type`/`TypeInfo`, `RegisterScalarUDF`, `RegisterTableUDF`, …) backed by the
  pure-Go engine. Call sites are unchanged apart from the import path.
- The historical cgo translation unit `duckdb_aggregate_udf.go` was removed
  (history-only in git). `duckdb_aggregate_udf_purego.go` is the
  supported pure-Go bridge: all 32 aggregate specs (KLL, HLL, ANY_VALUE
  HAVING, googlesqlite_agg, ST_*_AGG, st_clusterdbscan, tf_idf, and both
  differential-privacy families) register as arity bands of ANY-parameter
  overloads through the engine's `RegisterAggregateBandConn`, with the same
  collect / concat / replay state model. Verified by the aggregate spec
  suite (`CGO_ENABLED=0 go run ./cmd/googlesqlite :memory:` reproduces
  individual cases), including HLL INIT+MERGE over struct UNNEST and the
  noiseless `WITH ANONYMIZATION ANON_COUNT(*)` rewrite.
- `doLoadSpatialExtension` is a no-op: geography is handled entirely by the pure-Go
  `googlesqlite_st_*`/`s2_*` UDFs, so the backend serves spatial queries without
  DuckDB's native `INSTALL spatial`.

## How the UDFs run with no cgo

The cgo build hands DuckDB a C function pointer for each `googlesqlite_*` UDF. In
the wasm2go engine the indirect-call table is a Go `[]any` of closures, so the
backend appends the Go UDF closure to that table and passes its index as the
"function pointer" to `duckdb_create_scalar_function`. DuckDB then `call_indirect`s
back into Go, registered through the same `database/sql` + `RegisterScalarUDF`
surface the emulator already uses.

## Status

- ✅ Compiles with `CGO_ENABLED=0`. The core data path (DDL/DML, cross-connection
  queries) and scalar UDFs (GoogleSQL string/math/`SAFE_*` builtins) run pure-Go.
- ✅ Aggregates: all 32 aggregate specs are registered and exercised by the spec
  test suite. The engine ships built with the json + icu extensions (the UNNEST
  lowering rides `json_each`, and timezone-aware temporals need icu).
- ✅ **Conformance:** the named spec gate runs the full normalized spec
  corpus. The current per-dialect counts and the
  implemented/partial/unsupported breakdown live in the generated
  [docs/specs/INDEX.md](docs/specs/INDEX.md). The
  `bigquery/functions/{search, objectref}` family runs on pure-Go bodies
  (textfn.BindSearch, extras.BindObj*) routed through the formatter retarget
  (trySearchObjectRefFunc) plus the DuckDB UDF allow-list entries
  (SEARCH declared BOOL). Run the gate pure-Go with:
  `CGO_ENABLED=0 go test -vet=off
  -run 'TestSpec/(googlesql|bigquery)' -test.parallel 4 -timeout 2400s .`
- Known edge case: if a DuckDB-level error occurs inside `catalog.Sync`, the sync
  error path currently skips a rollback, which can leave a transaction open on the
  pooled connection and surface as "cannot start a transaction within a transaction"
  on later queries. It has been stable since the JSON-extension fix, and adding a
  rollback on that path would close it fully.
