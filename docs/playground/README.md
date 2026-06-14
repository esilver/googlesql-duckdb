# googlesqlite Playground

A browser playground for GoogleSQL queries against the `googlesqlite` engine
(the GoogleSQL/BigQuery dialect on the pure-Go DuckDB backend). The same UI runs
against either of two interchangeable engine modes, selected at start with
`VITE_ENGINE`:

- **Browser (default, `VITE_ENGINE=wasm`)**: the `js/wasm` build, with the
  whole engine compiled to WebAssembly and run client-side. Nothing is sent
  anywhere.
- **Server (`VITE_ENGINE=server`)**: the UI talks HTTP to a small local server
  (`cmd/playground`) that runs the engine natively with `CGO_ENABLED=0`. The
  fast path for large data: no browser download, native-speed queries.

The modes differ only in delivery. The SQL engine is the same wasm2go-transpiled
native Go (DuckDB plus the GoogleSQL analyzer). Browser mode compiles it to
WebAssembly, server mode runs it as a native binary. Both keep the DuckDB
database **in-memory and reset on reload**, with query history in the **Origin
Private File System** (OPFS).

The UI is **Vite + React + TypeScript + MUI** with **CodeMirror 6** as the SQL
editor (query history, result export), and runs the engine in a **Web Worker**
to stay responsive. The engine seam is `src/api/worker.ts`. Because the server
reuses `internal/cli` (the package behind the wasm entrypoint and the REPL) and
mirrors `entryToJS` from `cmd/googlesqlite/main_js.go`, both engines return
identical JSON shapes and are interchangeable above the seam.

## Prerequisites

- Go (the version pinned in the repository `go.mod`)
- Node.js 26 and npm (matches `.github/workflows/release.yml`)

## Run locally (wasm mode)

```console
$ cd docs/playground
$ make deps      # install npm dependencies (first time only)
$ make dev       # build the wasm engine, then start the dev server
```

`make dev` opens <http://localhost:5173/googlesqlite/>. It compiles
`cmd/googlesqlite` (the `js && wasm` build) into `public/googlesqlite.wasm` and
copies the Go `wasm_exec.js` loader next to it, both gitignored artifacts.
The worker (`src/api/worker.ts`) loads the loader, instantiates the engine, and
exposes `exec`/history/export calls, which the UI thread reaches through
`src/api/googlesqlite.ts`.

### Wasm performance

The wasm module bundles the whole stack into one `googlesqlite.wasm`. Measured
on node26/V8, M-series, with `/usr/bin/time -l`:

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

Terminal 1, the engine server (repository root). The pure-Go engine builds with
a plain `go build`, no flags (see
[REPRODUCE-PURE-GO.md](../../REPRODUCE-PURE-GO.md)):

```sh
CGO_ENABLED=0 go run ./cmd/playground
```

| flag    | default          | meaning                                            |
|---------|------------------|----------------------------------------------------|
| `-db`   | `playground.db`  | database file path, `:memory:` for non-persistent  |
| `-addr` | `localhost:8765` | listen address                                     |

Terminal 2, the UI:

```sh
cd docs/playground
make deps                  # first time only
VITE_ENGINE=server make dev
```

Open <http://localhost:5173/googlesqlite/>. The UI loads instantly (no wasm
download) and every query runs on the native server. The worker skips the wasm
load and talks HTTP. `VITE_SERVER_URL` overrides the default
`http://localhost:8765`.

Manual browser check, a round-trip in the editor:

```sql
CREATE TABLE songs (id INT64, title STRING);
INSERT INTO songs VALUES (1, 'paranoid android');
SELECT UPPER(title) AS t, SAFE_DIVIDE(id, 0) AS d FROM songs;
```

Expect `PARANOID ANDROID` / `NULL`, the `songs` table in the schema panel, and
the statements in the history panel.

### HTTP API (server mode)

CORS is granted to localhost origins (any port), so the Vite dev (5173) and
preview (4173) servers both work.

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
elapsedMs, timestamp}`, where rows are display strings exactly as the wasm
engine reports them. The legacy `sqliteQuery` alias is also emitted for older
clients and history exports. `/exec` accepts multi-statement input, and a SQL
error is HTTP 200 with the per-statement `error` field set (only malformed
requests are 4xx, as `{"error": "..."}`).

```sh
curl -s localhost:8765/exec -d '{"sql":"CREATE TABLE t (n INT64); INSERT INTO t VALUES (1),(2); SELECT SUM(n) AS s FROM t;"}'
CGO_ENABLED=0 go test -vet=off ./cmd/playground   # server tests
```

## Build the static site

```console
$ make build     # builds the engine and bundles the site into dist/
$ make preview   # serve the production build locally
```

## Deployment

The `Release` workflow (`.github/workflows/release.yml`) deploys to GitHub Pages
on every `v*` tag: it builds the engine, bundles the site, and publishes `dist/`
through `actions/deploy-pages`.

The uncompressed `googlesqlite.wasm` exceeds GitHub's 100 MB per-file limit but
is never deployed. The worker fetches the gzip-compressed `googlesqlite.wasm.gz`
(tens of MB) and decompresses it in the browser, and `make build/page` drops the
uncompressed copy from `dist/`. The same workflow attaches `googlesqlite.wasm`
to the GitHub release as a downloadable asset.
