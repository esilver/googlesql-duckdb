# googlesqlite Playground

A browser-based playground for running GoogleSQL queries against the
`googlesqlite` engine (the GoogleSQL/BigQuery dialect on the pure-Go
DuckDB backend). The same UI runs against either of two interchangeable
engine modes, selected at UI start time with `VITE_ENGINE`:

- **Wasm (default)**, where the whole engine is compiled to WebAssembly
  and runs entirely client-side in the browser tab. Nothing is sent
  anywhere.
- **Server**, where the UI talks HTTP to a small local native server
  (`cmd/playground`) that runs the engine natively with
  `CGO_ENABLED=0`. This is the fast path for big data, since there is no
  wasm download and queries run at native speed.

The UI is the same in both modes:

- **Vite + React + TypeScript + MUI** for the UI
- **CodeMirror 6** as the SQL editor, with query history and result export
- The engine runs in a **Web Worker** so the UI stays responsive

In both modes the DuckDB database is **in-memory and resets on reload**.
Query history persists in the **Origin Private File System** (OPFS).

The engine seam lives in `src/api/worker.ts`. Because the server reuses
`internal/cli` (the same package behind the wasm entrypoint and the
REPL) and mirrors `entryToJS` from `cmd/googlesqlite/main_js.go`, the two
engines return identical JSON shapes and are interchangeable above the
seam.

## Prerequisites

- Go (the version pinned in the repository `go.mod`)
- Node.js 26 and npm (matches `.github/workflows/release.yml`)

## Run locally (wasm mode)

```console
$ cd docs/playground
$ make deps      # install npm dependencies (first time only)
$ make dev       # build the wasm engine, then start the dev server
```

`make dev` opens the page at <http://localhost:5173/googlesqlite/>. It
compiles `cmd/googlesqlite` (the `js && wasm` build) into
`public/googlesqlite.wasm` and copies the Go `wasm_exec.js` loader next
to it. Both files are generated artifacts and are gitignored.

`VITE_ENGINE=wasm` (or unset) selects this in-browser engine. The worker
(`src/api/worker.ts`) loads `wasm_exec.js`, instantiates the engine, and
exposes `exec` / history / export calls. The UI thread talks to it
through `src/api/googlesqlite.ts`.

### Wasm performance

The wasm module bundles the whole stack (pure-Go DuckDB engine plus the
GoogleSQL analyzer, both transpiled to native Go via wasm2go) into one
`googlesqlite.wasm`. Measured (node26/V8, M-series):

| metric | value |
|---|---|
| module size | 708 MB raw / **123 MB gzipped** |
| instantiate | ~0.4 s |
| one-time engine+analyzer init | ~7 s |
| first analysis | ~216 ms |
| repeat query (analysis cache) | **~22 ms** |
| CREATE / INSERT / aggregate | 0.1-0.4 s |
| memory | ~4.8 GB RSS |

## Run locally (server mode)

The native server is the "local native server" alternative to the
in-browser wasm engine: same API surface, same JSON shapes, picked at UI
start time with `VITE_ENGINE=server`.

Terminal 1, the engine server (repository root):

```sh
CGO_ENABLED=0 go run ./cmd/playground
```

Flags:

| flag    | default          | meaning                                            |
|---------|------------------|----------------------------------------------------|
| `-db`   | `playground.db`  | database file path, `:memory:` for non-persistent  |
| `-addr` | `localhost:8765` | listen address                                     |

(No build flags required, the pure-Go engine builds with a plain `go
build`. See [REPRODUCE-PURE-GO.md](../../REPRODUCE-PURE-GO.md). The first
build compiles the engine once, and is cached afterwards.)

Terminal 2, the UI:

```sh
cd docs/playground
make deps                  # first time only
VITE_ENGINE=server make dev
```

Then open <http://localhost:5173/googlesqlite/>. The UI loads instantly
(no wasm download), and every query is executed by the native server.
The worker skips the wasm load and talks HTTP to the server.
`VITE_SERVER_URL` overrides the default `http://localhost:8765`.

Manual browser check, running a round-trip in the editor:

```sql
CREATE TABLE songs (id INT64, title STRING);
INSERT INTO songs VALUES (1, 'paranoid android');
SELECT UPPER(title) AS t, SAFE_DIVIDE(id, 0) AS d FROM songs;
```

expect `PARANOID ANDROID` / `NULL`, the `songs` table in the schema
panel, and the statements in the history panel.

### HTTP API (server mode)

CORS is granted to localhost origins (any port), so the Vite dev server
(5173) and preview server (4173) both work.

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
`{statement, duckdbQuery, isQuery, columns, rows, rowsAffected, error,
elapsedMs, timestamp}`, where rows are display strings, exactly as the
wasm engine reports them. The legacy `sqliteQuery` alias is also emitted
for older clients and history exports. `/exec` accepts multi-statement
input, and a SQL error is HTTP 200 with the per-statement `error` field
set (only malformed requests are 4xx, as `{"error": "..."}`).

Smoke test with curl:

```sh
curl -s localhost:8765/exec -d '{"sql":"CREATE TABLE t (n INT64); INSERT INTO t VALUES (1),(2); SELECT SUM(n) AS s FROM t;"}'
```

Run the server tests with:

```sh
CGO_ENABLED=0 go test -vet=off ./cmd/playground
```

## Build the static site

```console
$ make build     # builds the engine and bundles the site into dist/
$ make preview   # serve the production build locally
```

## Deployment

The site is deployed to GitHub Pages by the `Release` workflow
(`.github/workflows/release.yml`) whenever a `v*` tag is pushed. The
workflow builds the engine, bundles the site, uploads `dist/` as the
Pages artifact, and publishes it through `actions/deploy-pages`.

The engine binary is large, bundling the whole googlesqlite stack
into one js/wasm module: the pure-Go DuckDB engine (transpiled to Go)
plus the GoogleSQL analyzer, transpiled to native Go via wasm2go.
The uncompressed binary exceeds GitHub's 100 MB per-file limit, but it
is never deployed. The worker fetches the gzip-compressed
`googlesqlite.wasm.gz` (tens of MB) and decompresses it in the browser,
and `make build/page` drops the uncompressed copy from `dist/`. The
same release workflow also attaches `googlesqlite.wasm` to the GitHub
release as a downloadable asset.
