# Running the DuckDB backend with NO cgo (pure-Go DuckDB via wasm2go)

This branch can execute GoogleSQL/BigQuery queries with **`CGO_ENABLED=0`** by
swapping the native-`libduckdb` backend (`github.com/duckdb/duckdb-go/v2`) for a
**pure-Go DuckDB v1.5.3** transpiled by [`ncruces/wasm2go`](https://github.com/ncruces/wasm2go).
The BigQuery dialect (ZetaSQL analysis + the `googlesqlite_*` UDFs) is unchanged —
only the DuckDB *execution backend* changes. `CGO_ENABLED=1` still builds the
original native path, untouched.

## Setup — two sibling checkouts

```sh
git clone https://github.com/esilver/googlesqlite -b pure-go-duckdb-backend
git clone https://github.com/esilver/duckdb-wasm2go-poc        # SIBLING directory

# 1. Build the pure-Go DuckDB engine once (generates converge/genpkg; needs
#    emscripten + wasm2go; see that repo's README / rebuild_all.sh):
cd duckdb-wasm2go-poc && ./rebuild_all.sh

# 2. Run the emulator with NO cgo:
cd ../googlesqlite
CGO_ENABLED=0 GOWORK=off GOTOOLCHAIN=go1.25.6 GOFLAGS=-mod=mod \
  go run -gcflags='duckdbconverge/genpkg=-N -l -c=16' ./cmd/t0boot
```

Expected: GoogleSQL queries run pure-Go, e.g.

```
SELECT UPPER('duck')        -> DUCK
SELECT SAFE_DIVIDE(10, 0)   -> NULL          (real googlesqlite_* UDF, BigQuery semantics)
SELECT STARTS_WITH('duckdb','duck') -> true
SELECT SAFE_CAST('42' AS INT64) -> 42
```

(`-gcflags='duckdbconverge/genpkg=-N -l ...'` is required: full Go optimization of
the ~141 MB transpiled engine package OOMs; `-N -l` keeps it compilable. Run tests
with `-vet=off` — `go vet` can't scope the giant generated file.)

## How the swap works

- **`go.mod` replaces** point `github.com/duckdb/duckdb-go/v2` at
  `../duckdb-wasm2go-poc/converge/duckdbcompat` — a drop-in stand-in for the
  `duckdb-go/v2` API (`Driver`, `Decimal`, `Type`/`TypeInfo`, `RegisterScalarUDF`,
  `RegisterTableUDF`, …) backed by the pure-Go engine — and `duckdbconverge` at
  `../duckdb-wasm2go-poc/converge`. **No call-site changes** in this repo.
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
  `CGO_ENABLED=0 GOWORK=off GOFLAGS=-mod=mod go test -vet=off
  -gcflags='duckdbconverge/genpkg=-N -l -c=16' -c -o gsx.test . &&
  ./gsx.test -test.run 'TestSpec/(googlesql|bigquery)' -test.parallel 4`.
- Known latent issue (backend-independent): a DuckDB-level failure inside
  `catalog.Sync` can leave a transaction open on the pooled connection
  ("cannot start a transaction within a transaction" on later queries) — the
  sync error path has no rollback. Not currently reproducible after the json
  fix; revisit if conformance runs hit it.
