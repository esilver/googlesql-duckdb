# googlesql-duckdb

[![CI](https://github.com/esilver/googlesql-duckdb/actions/workflows/ci.yml/badge.svg)](https://github.com/esilver/googlesql-duckdb/actions/workflows/ci.yml)

A drop-in DuckDB backend for [goccy/googlesqlite](https://github.com/goccy/googlesqlite):
the same GoogleSQL `database/sql` driver, with the SQLite backend replaced by a
pure-Go DuckDB backend. No cgo, no external services, and no C toolchain, giving
you a local execution engine for the dialect behind BigQuery and Cloud Spanner.

It keeps goccy's module path, `github.com/goccy/googlesqlite`, and the
`googlesqlite` driver name, so code written against goccy's engine switches
backends with `replace` lines in `go.mod` and no source changes (see
[Module path](#module-path)).

The stack has two pure-Go layers:

- Parsing and analysis come from the forked
  [`esilver/go-googlesql`](https://github.com/esilver/go-googlesql), consumed
  through the compatibility module path `github.com/goccy/go-googlesql`.
- Execution runs on
  [`esilver/duckdb-go-pure`](https://github.com/esilver/duckdb-go-pure), DuckDB
  transpiled from WebAssembly to Go via [`ncruces/wasm2go`](https://github.com/ncruces/wasm2go).

Both layers are pure Go, so the whole stack builds with `CGO_ENABLED=0` and no
build flags. See [REPRODUCE-PURE-GO.md](REPRODUCE-PURE-GO.md) for the no-cgo
build and [cmd/googlesqlite/README.md](cmd/googlesqlite/README.md) for the CLI.

## Quick start

```sh
make build            # build bin/googlesqlite (the CLI)
make test             # unit and spec tests
make lint             # golangci-lint via tools/go.mod
make spec/check       # regenerate and verify the conformance matrix
```

## Usage

The driver registers as `googlesqlite`. The DSN is the DuckDB database path
(`:memory:` or a file path).

```go
import (
	"database/sql"

	_ "github.com/goccy/googlesqlite"
)

db, _ := sql.Open("googlesqlite", ":memory:")
defer db.Close()
rows, _ := db.Query(`SELECT FORMAT('%t', DATE '2026-05-14') AS today`)
```

### Module path

The repository is `esilver/googlesql-duckdb`, but the Go module path stays
goccy's: `github.com/goccy/googlesqlite`. That is what makes this a drop-in
replacement. Code that already imports goccy's engine keeps its import paths,
its `sql.Open("googlesqlite", ...)` calls and the rest of its source unchanged;
`replace` directives in `go.mod` are the only edit.
[`esilver/bigquery-emulator`](https://github.com/esilver/bigquery-emulator) runs
on this engine that way.

Because the path is goccy's, a bare `go get github.com/goccy/googlesqlite`
resolves to the upstream SQLite-backed engine. To build against this pure-Go
DuckDB engine from your own module, add both redirects to your `go.mod`. The
second is required because a `replace` inside a dependency does not apply to
your build:

```
replace github.com/goccy/googlesqlite => github.com/esilver/googlesql-duckdb v0.2.9-pure-go
replace github.com/goccy/go-googlesql => github.com/esilver/go-googlesql v0.2.4-finalizer.1
```

The pins advance over time, so match them to this repo's `go.mod` when you build.

## Status

Every function and type is backed by a declarative spec under `docs/specs/` and
at least one upstream-sourced case under `testdata/specs/`, run against the
driver by the repo-root `TestSpec`. The per-dialect counts and the
implemented/partial/unsupported breakdown are generated from spec frontmatter
into [`docs/specs/INDEX.md`](docs/specs/INDEX.md).

Specs marked `partial` are documented gaps with a concrete infrastructure
dependency. Each spec's `notes:` frontmatter says what is missing.

## Playground

Run the engine in your browser, nothing to install:
**<https://esilver.github.io/googlesqlite/>**.

The whole engine, GoogleSQL parsing, analysis, and the pure-Go DuckDB backend,
compiles to WebAssembly and runs client-side, sending nothing anywhere. The
engine runs in a Web Worker so the UI stays responsive. See
[docs/playground/README.md](docs/playground/README.md).

## Caveats

- The browser DuckDB build is in-memory only, so tables and data reset on
  reload (the Playground UI keeps query history). Long-term persistence waits on
  the DuckDB filesystem seam.
- For the no-cgo build's known edge cases, see
  [REPRODUCE-PURE-GO.md](REPRODUCE-PURE-GO.md).

## Layout

- `docs/specs/`: canonical normalized spec markdown, one file per function/type.
- `testdata/specs/`: declarative YAML cases referenced by spec frontmatter.
- `docs/third_party/googlesql-docs/`: Apache-2.0 snapshot of upstream
  `google/googlesql` `docs/`, refreshed by `make spec/upstream-sync`.
- `cmd/specctl/`: operational CLI (normalize, upstream-sync, check, ...).
- `tools/go.mod`: pinned developer tools (golangci-lint).

## Sponsorship

The upstream `googlesqlite` engine and the `bigquery-emulator` it powers were
created and are maintained by [@goccy](https://github.com/goccy) (Masaaki
Goshima). This repository forks that work to retarget the engine from a SQLite
backend to a pure-Go DuckDB backend. The GoogleSQL engine it builds on is
goccy's.

If this project is useful to you, please consider sponsoring the upstream
author: <https://github.com/sponsors/goccy>.
