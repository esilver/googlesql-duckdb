# GoogleSQLite

[![CI](https://github.com/esilver/googlesqlite/actions/workflows/ci.yml/badge.svg)](https://github.com/esilver/googlesqlite/actions/workflows/ci.yml)

A pure-Go GoogleSQL engine, exposed as a `database/sql` driver. No cgo, no
external services, no C toolchain. GoogleSQL is the dialect behind BigQuery and
Cloud Spanner, so this is a local execution engine for those workloads.

It runs the GoogleSQL dialect on a pure-Go DuckDB backend:

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
