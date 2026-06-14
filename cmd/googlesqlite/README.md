# googlesqlite CLI

`googlesqlite` is an interactive console for GoogleSQL queries, in the spirit of
the `sqlite3` and `spanner-cli` REPLs. It runs on the pure-Go DuckDB engine:
`CGO_ENABLED=0`, no libduckdb, no C toolchain at runtime. The same program
builds for `js/wasm` and powers the browser Playground.

## Install

Build from this checkout (`go install ...@latest` may fetch a different
backend):

```console
$ make build        # native binary -> bin/googlesqlite
$ make build-wasm   # bin/googlesqlite.wasm + bin/wasm_exec.js
```

The result is a single self-contained static binary (~375 MB, it embeds the
whole transpiled DuckDB), so build once and copy it anywhere. Rebuild only when
you change the Go code or pull a new engine. No build flags or prerequisites
beyond Go are required. See [REPRODUCE-PURE-GO.md](../../REPRODUCE-PURE-GO.md)
for the full pure-Go build guide.

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

With no `database` argument the CLI uses a shared in-memory database. A
positional argument opens (or creates) that database file. Statements end with
`;`.

```sh
googlesqlite :memory:           # interactive REPL, in-memory
googlesqlite /tmp/mydata.db     # file-backed, persists across runs
```

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

All input paths run the same statement pipeline, and whenever a terminal is
available the REPL opens afterward in the resulting state:

- **Interactive**: statements accumulate across lines until a `;` (or trailing `\G`).
- **Startup file**: `--file script.sql` runs the file, then opens the REPL (repeatable).
- **Piped stdin**: `cat script.sql | googlesqlite` runs the SQL, then opens the REPL on `/dev/tty`.
- **`.read`**: source a file mid-session.

## Dot-commands

- `.help`: show help
- `.quit`, `.exit`: leave the CLI
- `.debug [on|off]`: show the translated DuckDB SQL for each statement
- `.tables`: list tables and views
- `.functions`: list functions
- `.read <path>`: run the statements in a SQL file

## Display

Results print as an aligned table, with column widths measured by display width
so full-width (CJK) text stays aligned. Append `\G` to print a query vertically,
one stanza per row.

The full dialect runs on this engine. Per-dialect conformance counts and the
implemented/partial/unsupported breakdown live in
[docs/specs/INDEX.md](../../docs/specs/INDEX.md).

## WebAssembly Playground API

The `js/wasm` build installs `globalThis.googlesqlite`:

| function | description |
|----------|-------------|
| `exec(sql)` / `execScript(text)` | run statements, returns `{ output, results }` |
| `getHistory()` | return the query/result history |
| `loadHistory(json)` | restore a history exported as JSON |
| `clearHistory()` | drop all history |
| `exportHistory(format)` | export history as `json`, `sql`, `markdown` or `csv` |
| `listTables()` | return table names visible to the session |
| `setDebug(bool)` | toggle the translated-SQL display |

The browser DuckDB build is in-memory only, so tables reset on reload. The host
page may persist query history via `exportHistory`/`loadHistory`. Load the
module with the Go `wasm_exec.js` shipped alongside `googlesqlite.wasm`, and
call code after `onGoogleSQLiteReady` fires.
