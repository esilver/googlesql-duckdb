# Reproduce on another Mac - cgo DuckDB-backed BigQuery engine

This fork retargets [goccy/googlesqlite](https://github.com/goccy/googlesqlite)'s
GoogleSQL engine from SQLite to **DuckDB** via cgo
(`github.com/duckdb/duckdb-go/v2`, DuckDB **v1.5.3**). On this commit it passes
**972 / 994 = 97.79%** of the GoogleSQL + BigQuery conformance suite (Spanner
excluded) - i.e. **100% of the in-scope, assertable cases**. The only remaining
fails are external-service stubs (`bigquery/functions/search`, `objectref`) and
spec-status-gated documentation-only cases.

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
- **Architecture/history:** see design notes in the conformance
  history; the engine lowers ZetaSQL-analyzed AST to DuckDB SQL + registers BigQuery
  builtins as DuckDB scalar / aggregate (via the C `duckdb_create_aggregate_function`
  binding) / table UDFs.
