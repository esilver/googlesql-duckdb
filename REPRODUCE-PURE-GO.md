# Running the DuckDB backend with NO cgo (pure-Go DuckDB via wasm2go)

This branch can execute GoogleSQL/BigQuery queries with **`CGO_ENABLED=0`** by
swapping the native-`libduckdb` backend (`github.com/duckdb/duckdb-go/v2`) for a
**pure-Go DuckDB v1.5.3** transpiled by [`ncruces/wasm2go`](https://github.com/ncruces/wasm2go).
The BigQuery dialect (GoogleSQL analysis + the `googlesqlite_*` UDFs) is unchanged —
only the DuckDB *execution backend* changes. `CGO_ENABLED=1` still builds the
original native path, untouched.

## Setup — ONE clone, zero replaces

The engine is a normal fetchable Go module:
[`github.com/esilver/duckdb-go-pure`](https://github.com/esilver/duckdb-go-pure)
(root package `duckdb`, a `database/sql` driver registered as `"duckdb"`, with
the transpiled engine committed under `internal/duckdbcore`). This repo
`require`s it like any other dependency — **no `replace` directives, no sibling
checkouts, no generated files**.

```sh
git clone -b pure-go-duckdb-backend https://github.com/esilver/googlesqlite
cd googlesqlite

# Run the emulator with NO cgo (no build flags required):
CGO_ENABLED=0 go run ./cmd/t0boot
```

(Engine developers hacking on the transpiler output can still point the engine
at a local checkout the old way — add
`replace github.com/esilver/duckdb-go-pure => ../duckdb-go-pure` (or the PoC
repo's `converge` tree via its previous `duckdbconverge` wiring) — but nothing
in this repo requires it anymore.)

Expected: GoogleSQL queries run pure-Go, e.g.

```
SELECT UPPER('duck')        -> DUCK
SELECT SAFE_DIVIDE(10, 0)   -> NULL          (real googlesqlite_* UDF, BigQuery semantics)
SELECT STARTS_WITH('duckdb','duck') -> true
SELECT SAFE_CAST('42' AS INT64) -> 42
```

(No build flags required — since duckdb-go-pure v0.3.1 a plain `go build`
works: the engine is sharded into ~30 packages under
`internal/duckdbcore/{core,shardNN,all}` with its oversized functions split,
so the whole engine compiles **fully optimized with inlining**. Run tests
with `-vet=off` — `go vet` can't chew the generated packages. The first
build compiles the engine in well under a minute on a multicore box; cached
afterwards.)

## How the swap works

- **`go.mod` requires `github.com/esilver/duckdb-go-pure`** (the pure-Go engine
  + `database/sql` driver). The former `github.com/duckdb/duckdb-go/v2` import
  sites now import `internal/duckdbgo` — a drop-in stand-in for the
  `duckdb-go/v2` API subset the emulator consumes (`Driver`, `Decimal`,
  `Type`/`TypeInfo`, `RegisterScalarUDF`, `RegisterTableUDF`, …) backed by the
  pure-Go engine. Call sites are unchanged apart from the import path.
- The single cgo translation unit `duckdb_aggregate_udf.go` is build-tagged
  `//go:build cgo`; `duckdb_aggregate_udf_purego.go` (`//go:build !cgo`) is the
  pure-Go port of the same bridge: all 32 aggregate specs (KLL, HLL, ANY_VALUE
  HAVING, googlesqlite_agg, ST_*_AGG, st_clusterdbscan, tf_idf, and both
  differential-privacy families) register as arity bands of ANY-parameter
  overloads through the engine's `RegisterAggregateBandConn`, with the same
  collect / concat / replay state model. Verified by `cmd/t2boot` (9/9
  corpus-derived aggregate queries, including HLL INIT+MERGE over struct
  UNNEST and the noiseless `WITH ANONYMIZATION ANON_COUNT(*)` rewrite).
- `doLoadSpatialExtension` is a no-op (geography lowers to pure-Go
  `googlesqlite_st_*`/`s2_*` UDFs, so the native `INSTALL spatial` was a dead probe).

## How the UDFs run with no cgo

The cgo build hands DuckDB a C function pointer for each `googlesqlite_*` UDF. In
the wasm2go engine the indirect-call table is a Go `[]any` of closures, so the
backend appends the Go UDF closure to that table and passes its index as the
"function pointer" to `duckdb_create_scalar_function`; DuckDB then `call_indirect`s
back into Go — registered through the same `database/sql` + `RegisterScalarUDF`
surface the emulator already uses. (See `SWAP-BLUEPRINT.md` in the PoC repo.)

## Status

- ✅ Compiles `CGO_ENABLED=0`; T0 (data path, DDL/DML, cross-conn) + scalar UDFs
  (GoogleSQL string/math/`SAFE_*` builtins) run pure-Go.
- ✅ T2 aggregates: all 32 specs registered and exercised (`cmd/t2boot`, 9/9).
  Needs an engine built WITH the json + icu extensions (the UNNEST lowering
  rides `json_each`; timezone-aware temporals need icu) — the PoC repo's
  `rebuild_fs_all.sh` produces it.
- ✅ **Conformance: 986 PASS / 0 FAIL / 8 SKIP over 994 — EXCEEDS the cgo
  baseline (972/994).** The cgo build's 14 `bigquery/functions/{search,
  objectref}` failures turned out to be a wiring gap, not external-service
  scope: the pure-Go bodies (textfn.BindSearch, extras.BindObj*) always
  existed — they were never routed on the DuckDB path. A formatter retarget
  (trySearchObjectRefFunc) plus five entries in the DuckDB UDF allow-list
  (SEARCH declared BOOL) fixed all 14; the same patch would fix the cgo
  backend. The 8 skips are proto/graph features with no assertable cases.
  Run it pure-Go with:
  `CGO_ENABLED=0 go test -vet=off
  -run 'TestSpec/(googlesql|bigquery)' -test.parallel 4 -timeout 2400s .`
- Known latent issue (backend-independent): a DuckDB-level failure inside
  `catalog.Sync` can leave a transaction open on the pooled connection
  ("cannot start a transaction within a transaction" on later queries) — the
  sync error path has no rollback. Not currently reproducible after the json
  fix; revisit if conformance runs hit it.
