# googlesqlite Playground

A browser-based playground that runs GoogleSQL queries entirely
client-side, powered by `googlesqlite` compiled to WebAssembly.

- **Vite + React + TypeScript + MUI** for the UI
- **CodeMirror 6** as the SQL editor
- The `googlesqlite` engine runs in a **Web Worker** so the UI stays
  responsive
- The pure-Go DuckDB engine runs an in-memory database (fresh on each
  reload); query history persists in the **Origin Private File System**
  (OPFS)

## Prerequisites

- Go (the version pinned in the repository `go.mod`)
- Node.js 26 and npm for the wasm build path

## Run locally

```console
$ cd docs/playground
$ make deps      # install npm dependencies (first time only)
$ make dev       # build the wasm engine, then start the dev server
```

`make dev` opens the page at <http://localhost:5173/googlesqlite/>.

## Build the static site

```console
$ make build     # builds the engine and bundles the site into dist/
$ make preview   # serve the production build locally
```

## How it works

`make build/wasm` compiles `cmd/googlesqlite` (the `js && wasm` build)
into `public/googlesqlite.wasm` and copies the Go `wasm_exec.js` loader
next to it. Both files are generated artifacts and are gitignored.

The worker (`src/api/worker.ts`) loads `wasm_exec.js`, instantiates the
engine, and exposes `exec` / history / export calls. The UI thread
talks to it through `src/api/googlesqlite.ts`.

## Deployment

The site is deployed to GitHub Pages by the `Release` workflow
(`.github/workflows/release.yml`) whenever a `v*` tag is pushed. The
workflow builds the engine, bundles the site, uploads `dist/` as the
Pages artifact, and publishes it through `actions/deploy-pages`.

The engine binary is large — it bundles the whole googlesqlite stack
into one js/wasm module: the pure-Go DuckDB engine (transpiled to Go)
plus the GoogleSQL analyzer, which is itself an embedded wasm module
run through wazero.
The uncompressed binary exceeds GitHub's 100 MB per-file limit, but it
is never deployed: the worker fetches the gzip-compressed
`googlesqlite.wasm.gz` (tens of MB) and decompresses it in the browser,
and `make build/page` drops the uncompressed copy from `dist/`. The
same release workflow also attaches `googlesqlite.wasm` to the GitHub
release as a downloadable asset.

## Wasm mode (fully in-browser, no server)

`make dev` (the default path) runs everything in the tab: the pure-Go DuckDB
engine, the BigQuery dialect, and the GoogleSQL analyzer compiled into one
`googlesqlite.wasm`. The database is **in-memory and resets on reload**
(query history persists via OPFS). Server mode (`VITE_ENGINE=server make dev`)
remains the fast path for big data.

Measured (node26/V8, M-series):

| metric | value |
|---|---|
| module size | 708 MB raw / **123 MB gzipped** |
| instantiate | ~0.4 s |
| one-time engine+analyzer init | ~7 s |
| first analysis | ~216 ms |
| repeat query (analysis cache) | **~22 ms** |
| CREATE / INSERT / aggregate | 0.1-0.4 s |
| memory | ~4.8 GB RSS |

Build it with `make build/wasm` (or let `make dev` do it). Requires node 26
(`/opt/homebrew/opt/node/bin` on this box; node@22 is broken).
