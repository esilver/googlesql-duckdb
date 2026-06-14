# googlesqlite CLI

`googlesqlite` is an interactive console for running GoogleSQL queries
against the googlesqlite engine, in the spirit of the `sqlite3` and
`spanner-cli` REPLs. In this fork it runs on the pure-Go DuckDB engine:
`CGO_ENABLED=0`, no libduckdb, no C toolchain at runtime.

The same program also builds for `js/wasm` and powers the browser
Playground.

## Install

Build from this checkout (`go install ...@latest` may fetch a different
upstream backend):

```console
$ make build        # native binary -> bin/googlesqlite
$ make build-wasm   # bin/googlesqlite.wasm + bin/wasm_exec.js
```

You do not need to rebuild it every time. The result is a single
self-contained static binary (~375 MB, it embeds the whole transpiled
DuckDB), so build once and run forever: copy it anywhere and run it.
Rebuild only when you change the Go code or pull a new engine.

No build flags are required since duckdb-go-pure v0.3.1. The engine is
sharded into ~30 packages and compiles fully optimized in under a minute
on a typical multi-core development machine. Tests run with `-vet=off`
because `go vet` cannot analyze the generated packages.

Prerequisites: none beyond Go. The engine is the fetchable module
`github.com/esilver/duckdb-go-pure`, with no sibling checkouts. See
[REPRODUCE-PURE-GO.md](../../REPRODUCE-PURE-GO.md) for the full pure-Go
build guide.

## Usage

```
googlesqlite [flags] [database]

Flags:
  --dsn string            data source name (overrides the positional database argument)
  --file value            run statements from a SQL file before the REPL starts (repeatable)
  --debug                 show the translated DuckDB SQL for each statement
  --no-color              disable coloured output
  --history string        REPL history file (default ~/.googlesqlite_history)
  --continue-on-error     keep running a script after a statement fails
```

With no `database` argument the CLI uses a shared in-memory database.
A positional argument opens (or creates) that database file.

```sh
googlesqlite :memory:           # interactive REPL, in-memory database
googlesqlite /tmp/mydata.db     # file-backed, tables persist across runs
```

Statements end with `;`:

```
> CREATE TABLE fruits (name STRING, sold INT64);
> INSERT INTO fruits VALUES ("pears", 30), ("apples", 20);
> SELECT name, ANY_VALUE(name HAVING MAX sold) OVER () AS top FROM fruits;
> SELECT SEARCH("Hello World", "world"), UNIX_SECONDS(CURRENT_TIMESTAMP());
```

### Scripted use

```sh
echo 'SELECT 1 + 1;' | googlesqlite :memory:        # pipe stdin
googlesqlite --file setup.sql --file queries.sql :memory:
googlesqlite --continue-on-error --file all.sql :memory:
```

### Input modes

All four input paths run the same statement pipeline; whenever a
terminal is available the REPL opens afterwards in the resulting state.

- **Interactive** — type statements at the prompt. Input is accumulated
  across lines until a `;` (or a trailing `\G`).
- **Startup file** — `--file script.sql` runs the file, then the REPL
  opens. `--file` may be repeated.
- **Piped stdin** — `cat script.sql | googlesqlite` runs the piped SQL,
  then the REPL opens on the controlling terminal (`/dev/tty`).
- **`.read`** — source a file mid-session.

## Dot-commands

- `.help` — show help
- `.quit`, `.exit` — leave the CLI
- `.debug [on|off]` — show the translated DuckDB SQL for each statement
- `.tables` — list tables and views
- `.functions` — list functions
- `.read <path>` — run the statements in a SQL file

## Display modes

Results print as an aligned table. Column widths are measured by
display width, so full-width (CJK) text stays aligned. Append `\G` to a
query to print it vertically, one stanza per row.

## What works

The full dialect runs on this engine. The spec conformance gate covers
the full normalized spec corpus; the current per-dialect counts and the
implemented/partial/unsupported breakdown live in the generated
[docs/specs/INDEX.md](../../docs/specs/INDEX.md).

## WebAssembly Playground API

The `js/wasm` build installs `globalThis.googlesqlite` with:

| function | description |
|----------|-------------|
| `exec(sql)` / `execScript(text)` | run statements; returns `{ output, results }` |
| `getHistory()` | return the query/result history |
| `loadHistory(json)` | restore a history exported as JSON |
| `clearHistory()` | drop all history |
| `exportHistory(format)` | export history as `json`, `sql`, `markdown` or `csv` |
| `listTables()` | return table names visible to the session |
| `setDebug(bool)` | toggle the translated-SQL display |

The browser DuckDB build is currently in-memory only, so tables reset
on reload. The host page may persist and restore query history via
`exportHistory` and `loadHistory`. Load the module with the Go
`wasm_exec.js` shipped alongside `googlesqlite.wasm`; call code after
`onGoogleSQLiteReady` fires.
