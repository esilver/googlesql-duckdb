# googlesqlite Playground server (native engine)

A small local HTTP server that runs the googlesqlite engine **natively**
— GoogleSQL/BigQuery dialect on the pure-Go DuckDB backend, built with
`CGO_ENABLED=0` — as the engine behind the browser Playground UI
(`docs/playground`). It is the "local native server" alternative to the
in-browser wasm engine: same API surface, same JSON shapes, picked at
UI start time with `VITE_ENGINE=server`.

![Playground screenshot placeholder](screenshot.png)
<!-- TODO: replace with a real screenshot of the UI in server mode -->

## Quickstart

Terminal 1 — the engine server (repository root):

```sh
CGO_ENABLED=0 go run ./cmd/playground
```

Flags:

| flag    | default          | meaning                                            |
|---------|------------------|----------------------------------------------------|
| `-db`   | `playground.db`  | database file path; `:memory:` for non-persistent  |
| `-addr` | `localhost:8765` | listen address                                     |

(No build flags required — the pure-Go engine builds with a plain `go
build`; see `REPRODUCE-PURE-GO.md`. The first build compiles the engine
once; it is cached afterwards.)

Terminal 2 — the UI:

```sh
cd docs/playground
make deps                  # first time only
VITE_ENGINE=server make dev
```

Then open <http://localhost:5173/googlesqlite/>. The UI loads instantly
(no wasm download), and every query is executed by the native server.

Manual browser check: run a round-trip in the editor —

```sql
CREATE TABLE songs (id INT64, title STRING);
INSERT INTO songs VALUES (1, 'paranoid android');
SELECT UPPER(title) AS t, SAFE_DIVIDE(id, 0) AS d FROM songs;
```

expect `PARANOID ANDROID` / `NULL`, the `songs` table in the schema
panel, and the statements in the history panel.

## Engine modes

The seam is in `docs/playground/src/api/worker.ts`:

- `VITE_ENGINE=wasm` (or unset) — the original in-browser wasm engine,
  untouched. `make dev` builds it as before.
- `VITE_ENGINE=server` — the worker skips the wasm load and talks HTTP
  to this server. `VITE_SERVER_URL` overrides the default
  `http://localhost:8765`.

Because the server reuses `internal/cli` (the same package behind the
wasm entrypoint and the REPL) and mirrors `entryToJS` from
`cmd/googlesqlite/main_js.go`, the two engines are interchangeable
above the seam.

## HTTP API

CORS is granted to localhost origins (any port), so the Vite dev
server (5173) and preview server (4173) both work.

| route                              | method | body / params       | returns                                  |
|------------------------------------|--------|---------------------|------------------------------------------|
| `/exec`                            | POST   | `{"sql": "..."}`    | `{"output": "...", "results": [entry…]}` |
| `/healthz`                         | GET    |                     | `{"ok": true, "engine": "..."}`          |
| `/tables`                          | GET    |                     | `["t1", …]`                              |
| `/schema`                          | GET    |                     | `{"tables": [{"name", "columns"}…]}`     |
| `/history`                         | GET    |                     | `[entry…]`                               |
| `/history/clear`                   | POST   |                     | `{"ok": true}`                           |
| `/history/export`                  | GET    | `?format=json\|sql\|markdown\|csv` | the rendered text          |
| `/history/load`                    | POST   | exported JSON       | `{"ok": true}`                           |

An `entry` is one statement's outcome:
`{statement, sqliteQuery, isQuery, columns, rows, rowsAffected, error,
elapsedMs, timestamp}` — rows are display strings, exactly as the wasm
engine reports them. `/exec` accepts multi-statement input; a SQL error
is HTTP 200 with the per-statement `error` field set (only malformed
requests are 4xx, as `{"error": "..."}`).

Smoke test with curl:

```sh
curl -s localhost:8765/exec -d '{"sql":"CREATE TABLE t (n INT64); INSERT INTO t VALUES (1),(2); SELECT SUM(n) AS s FROM t;"}'
```

## Tests

```sh
CGO_ENABLED=0 go test -vet=off ./cmd/playground
```
