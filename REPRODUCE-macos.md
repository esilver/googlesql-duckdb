# Reproduce on another Mac - cgo DuckDB-backed BigQuery engine

## What this repo is

A **local BigQuery emulator backed by DuckDB** - run BigQuery GoogleSQL (dbt models,
ad-hoc queries, CI) on your machine without hitting real BigQuery, with far higher
SQL fidelity than a SQLite-based emulator gives.

It is a fork of **[goccy/googlesqlite](https://github.com/goccy/googlesqlite)**, the
GoogleSQL engine behind
[goccy/bigquery-emulator](https://github.com/goccy/bigquery-emulator). Upstream, that
engine parses + analyzes BigQuery's GoogleSQL with ZetaSQL and then executes against
a **SQLite** backend - but SQLite's semantics diverge from BigQuery in many places
(types, functions, NULL handling), which caps how much real BigQuery SQL it can run
faithfully.

**What we changed:** retarget the backend from SQLite to **DuckDB**, whose SQL
semantics are much closer to BigQuery (and which is fast - vectorized, native). The
ZetaSQL parse/analyze stays; the engine's *formatter* lowers the analyzed query to
**DuckDB** SQL instead of SQLite, and BigQuery's builtin functions are registered as
**DuckDB UDFs**.

## How it works

```
BigQuery GoogleSQL
  -> ZetaSQL parse + analyze            (go-googlesql; unchanged from upstream)
  -> resolved AST
  -> formatter lowers the AST to DuckDB SQL          (this fork's core work)
  -> BigQuery builtins registered as DuckDB UDFs     (scalar / aggregate / table, cgo)
  -> DuckDB executes        (github.com/duckdb/duckdb-go/v2, libduckdb v1.5.3)
```

Aggregates use the C `duckdb_create_aggregate_function` binding. Correctness is
scored by the conformance suite under `testdata/specs/`, which encodes BigQuery's own
documented behavior as cases.

## Status & scope

- **Coverage: 972 / 994 = 97.79%** of the GoogleSQL + BigQuery conformance suite
  (Spanner excluded - not a BigQuery target). That is **100% of the in-scope,
  assertable cases**; it climbed 8.2% -> 65% -> 79% -> 86% -> 91% -> 97.79% over
  several rounds.
- **Out of scope (all 14 remaining fails):** `bigquery/functions/search` (vector/text
  search indexes) and `objectref` (GCS object references) - these need external Google
  services and cannot run in an in-process engine. Plus 8 spec-status-gated
  documentation-only cases the suite itself does not require.
- **This is the cgo build** (links native libduckdb - fast). This branch also
  ships a *pure-Go, no-cgo* DuckDB backend (`github.com/esilver/duckdb-go-pure`,
  a wasm2go transpile) that now **exceeds** this document's number — 986/994
  with zero failures — at a runtime cost (slower than native cgo). See
  REPRODUCE-PURE-GO.md for that path; the steps below reproduce the cgo
  baseline.

These steps take a clean macOS machine to a working build + that conformance number.

## Prerequisites

- macOS (Apple Silicon or Intel - the DuckDB library below is universal)
- **Go 1.25+** - `brew install go`
- Xcode command-line tools (the C toolchain cgo needs) - `xcode-select --install`
- `git`, `curl`, `unzip`
- Network for the first build (Go modules fetch); ~3 GB free disk

## 1. Clone

```sh
git clone -b duckdb-backend https://github.com/esilver/googlesqlite.git
cd googlesqlite
```

## 2. Stage the DuckDB v1.5.3 library

The build dynamic-links an external `libduckdb` (the `duckdb_use_lib` build tag).
The version **must be v1.5.3** to match the binding (`duckdb-go/v2 v2.10503.1`).

```sh
curl -fL -o /tmp/libduckdb.zip \
  https://github.com/duckdb/duckdb/releases/download/v1.5.3/libduckdb-osx-universal.zip
mkdir -p "$HOME/libduckdb-stage"
unzip -o /tmp/libduckdb.zip -d "$HOME/libduckdb-stage"
# -> $HOME/libduckdb-stage/{libduckdb.dylib, duckdb.h, duckdb.hpp}
```

## 3. Build the conformance test binary

First (cold) build is ~1-2 min (cgo compile + DuckDB link). Keep the flags below
**exactly** - changing `CGO_LDFLAGS` between builds forces a full recompile.

```sh
export STAGE="$HOME/libduckdb-stage"
CGO_ENABLED=1 \
  CGO_LDFLAGS="-L$STAGE -Wl,-rpath,$STAGE" \
  DYLD_LIBRARY_PATH="$STAGE" \
  go test -tags=duckdb_use_lib -c -o ./gsx.test .
```

`-Wl,-rpath` bakes the library path into the binary so it finds `libduckdb.dylib`
at runtime; `DYLD_LIBRARY_PATH` is the fallback. Leave `GOPROXY` at its default for
the first build (modules must fetch); it can be `off` afterward.

## 4. Run the conformance suite

```sh
DYLD_LIBRARY_PATH="$HOME/libduckdb-stage" \
  ./gsx.test -test.run 'TestSpec/(googlesql|bigquery)' -test.parallel 4 -test.timeout 1200s
```

- **Pin `-test.parallel 4`.** DuckDB's cgo under heavy `t.Parallel()` can hit the OS
  thread cap (`runtime/cgo: pthread_create failed: Resource temporarily unavailable`)
  when `ulimit -u` is low; `-test.parallel 4` avoids it. Raise `ulimit -u` if you
  want more parallelism.
- Expect **~972 PASS / 14 FAIL / 8 SKIP** over 994 ex-Spanner leaves. All 14 fails
  are `bigquery/functions/{search,objectref}` (external services, out of scope).

To measure the exact count, run with `-test.v` and count the deepest (leaf)
`--- PASS/FAIL/SKIP` lines for `TestSpec/googlesql` + `TestSpec/bigquery`.

## 5. (Optional) embed it as a database/sql driver

The package registers a `database/sql` driver named `googlesqlite` in its `init()`.
The fork keeps the upstream module path `github.com/goccy/googlesqlite`, so a
separate consumer must point at this checkout with a replace directive:

```
// in the consumer's go.mod
require github.com/goccy/googlesqlite v0.0.0
replace github.com/goccy/googlesqlite => /path/to/this/googlesqlite
```

```go
import _ "github.com/goccy/googlesqlite"
db, _ := sql.Open("googlesqlite", "")
```

Build the consumer with the same `-tags=duckdb_use_lib` + `CGO_LDFLAGS` /
`DYLD_LIBRARY_PATH` pointing at your stage dir. (The conformance test binary in
step 3-4 is the simplest proof the engine works end to end.)

## Notes / gotchas

- **Version lock:** `libduckdb` must be **v1.5.3** (the binding is pinned to it). A
  mismatched dylib will fail to link or crash at runtime.
- **`duckdb_use_lib` tag:** dynamic-links the staged dylib (fast relinks, what this
  fork is built and verified with). Without the tag, `duckdb-go/v2` links its
  bundled static lib (no dylib download, heavier first build) - viable but not the
  verified path here.
- **Conformance is commit-pinned:** the authoritative number for this commit is
  **972/994**. Re-measure after any change.
- **Architecture/history:** the engine lowers ZetaSQL-analyzed AST to DuckDB SQL + registers BigQuery
  builtins as DuckDB scalar / aggregate (via the C `duckdb_create_aggregate_function`
  binding) / table UDFs.
