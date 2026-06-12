# Running the BigQuery-dialect CLI on pure-Go DuckDB (no cgo)

`cmd/googlesqlite` is an interactive GoogleSQL/BigQuery REPL (in the spirit of
`sqlite3` / `spanner-cli`). On this branch it runs on the **pure-Go DuckDB**
engine — `CGO_ENABLED=0`, no libduckdb, no C toolchain at runtime.

## Build once, run forever

You do **not** need to build it every time. The result is a single
self-contained static binary (~375 MB — it embeds the whole transpiled DuckDB);
copy it anywhere and run it. Rebuild only when you change the Go code or pull a
new engine.

Prerequisites: **none beyond Go** — the engine is the fetchable module
`github.com/esilver/duckdb-go-pure` (no sibling checkouts, no generated files;
see REPRODUCE-PURE-GO.md).

```sh
cd googlesqlite     # this repo, branch pure-go-duckdb-backend
CGO_ENABLED=0 go build -o gsx ./cmd/googlesqlite
```

No build flags required — since duckdb-go-pure v0.3.1 the engine builds with
a plain `go build` (it is sharded into ~30 packages with its oversized
functions split, so it compiles fully optimized with inlining in well under
a minute on a multicore box). The first build compiles the generated engine;
afterwards Go's build cache makes rebuilds take seconds unless the engine
itself changed.

## Use it

```sh
./gsx :memory:           # interactive REPL, in-memory database
./gsx /tmp/mydata.db     # file-backed — tables persist across runs
```

Always pass a database argument (`:memory:` or a path): the built-in default
points at an unwritable root path. Statements end with `;`:

```
> CREATE TABLE fruits (name STRING, sold INT64);
> INSERT INTO fruits VALUES ("pears", 30), ("apples", 20);
> SELECT name, ANY_VALUE(name HAVING MAX sold) OVER () AS top FROM fruits;
> SELECT SEARCH("Hello World", "world"), UNIX_SECONDS(CURRENT_TIMESTAMP());
```

Scripted / non-interactive use:

```sh
echo 'SELECT 1 + 1;' | ./gsx :memory:        # pipe stdin
./gsx --file setup.sql --file queries.sql :memory:
./gsx --continue-on-error --file all.sql :memory:
```

Useful flags: `--debug` (show the lowered DuckDB SQL per statement),
`--no-color`, `--history <file>` (default `~/.googlesqlite_history`),
`--dsn` (overrides the positional database argument).

## What works

The full dialect: 986 of the 994 conformance specs pass on this engine with
zero failures (the 8 skips are proto/graph features with no assertable cases) —
scalar functions, all 32 aggregate/window UDF families (KLL, HLL, differential
privacy, geography, TF-IDF), timezone-aware temporals (icu), UNNEST/structs,
SEARCH, OBJ.*, file persistence, and cross-process reopen.
