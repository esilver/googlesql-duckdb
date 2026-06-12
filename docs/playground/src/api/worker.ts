// worker.ts runs the googlesqlite engine off the UI thread. Two
// engine modes exist, selected by the VITE_ENGINE build variable:
//
//   - "wasm" (default): the googlesqlite WebAssembly engine runs
//     inside this worker. The engine performs blocking work, so
//     running it in a worker keeps the page responsive.
//   - "server": a thin HTTP client to a local native engine
//     (`go run ./cmd/playground`, default http://localhost:8765,
//     override with VITE_SERVER_URL). The server exposes the same
//     API surface and JSON shapes as the wasm engine, so everything
//     below the engine seam is shared.
//
// Persistence: only the query/result history persists, in an Origin
// Private File System (OPFS) file owned by this worker. The
// DuckDB-backed engine is :memory:-only in the browser — it has no
// OPFS-backed filesystem the way the previous SQLite "opfs" VFS did —
// so the database itself is rebuilt from scratch on every load.

import { installWasmFS } from './wasmfs'
import type {
  ExecResponse,
  LoadPhase,
  QueryResult,
  WorkerRequest,
  WorkerResponse,
} from './types'

// GoogleSQLiteAPI is the surface the js/wasm build installs on the
// worker's global scope (see cmd/googlesqlite/main_js.go). Every call
// returns a Promise.
interface GoogleSQLiteAPI {
  exec(sql: string): Promise<ExecResponse>
  getHistory(): Promise<QueryResult[]>
  loadHistory(json: string): Promise<{ ok: true }>
  clearHistory(): Promise<{ ok: true }>
  exportHistory(format: string): Promise<string>
  listTables(): Promise<string[]>
}

declare global {
  var googlesqlite: GoogleSQLiteAPI | undefined
  var onGoogleSQLiteReady: (() => void) | undefined
}

// ENGINE_MODE selects the engine backing this worker; SERVER_URL is
// the native server's base URL in "server" mode.
const ENGINE_MODE: string =
  (import.meta.env.VITE_ENGINE as string | undefined) ?? 'wasm'
const SERVER_URL: string = (
  (import.meta.env.VITE_SERVER_URL as string | undefined) ??
  'http://localhost:8765'
).replace(/\/$/, '')

function post(msg: WorkerResponse): void {
  self.postMessage(msg)
}

function api(): GoogleSQLiteAPI {
  if (!globalThis.googlesqlite) {
    throw new Error('the googlesqlite wasm engine is not initialised')
  }
  return globalThis.googlesqlite
}

// installProcessShim provides the minimal `process` object the Go
// js/wasm runtime expects. It mirrors the wasm_exec.js fallback but
// returns "/" from cwd() instead of throwing.
function installProcessShim(): void {
  const enosys = () => {
    const err = new Error('not implemented') as Error & { code?: string }
    err.code = 'ENOSYS'
    return err
  }
  ;(globalThis as { process?: unknown }).process = {
    getuid: () => -1,
    getgid: () => -1,
    geteuid: () => -1,
    getegid: () => -1,
    getgroups: () => { throw enosys() },
    pid: -1,
    ppid: -1,
    umask: () => { throw enosys() },
    cwd: () => '/',
    chdir: () => { throw enosys() },
  }
}

// --- OPFS persistence -----------------------------------------------

// OPFS_HISTORY_FILE holds the query/result history JSON. (The old
// SQLite build also kept the database file here; the DuckDB engine is
// :memory:-only, so only the history file remains.)
const OPFS_HISTORY_FILE = 'history.json'

// historyHandle is the OPFS access handle for the history file, or
// null when OPFS is unavailable.
let historyHandle: FileSystemSyncAccessHandle | null = null

// openOPFSFile opens (creating if needed) a file in the Origin Private
// File System and returns a synchronous access handle to it.
//
// A file has at most one sync access handle at a time. Right after a
// reload the previous page's handle is not always released yet — and
// WebKit can take a noticeable moment — so the call is retried for up
// to ten seconds before giving up. On a fresh load it succeeds at
// once, so the retry budget costs nothing in the common case.
async function openOPFSFile(
  name: string,
): Promise<FileSystemSyncAccessHandle> {
  const root = await navigator.storage.getDirectory()
  const file = await root.getFileHandle(name, { create: true })
  let lastErr: unknown
  for (let attempt = 0; attempt < 100; attempt++) {
    try {
      return await file.createSyncAccessHandle()
    } catch (e) {
      lastErr = e
      await new Promise((resolve) => setTimeout(resolve, 100))
    }
  }
  throw lastErr
}

// installOPFS acquires the OPFS access handle for the history file.
// The database is :memory:-only and needs no handle.
//
// OPFS needs a secure context (HTTPS, or localhost). When it is absent
// — for example plain HTTP over a LAN IP — this logs and returns; the
// history then simply does not persist across reloads.
async function installOPFS(): Promise<void> {
  if (typeof navigator === 'undefined' || !navigator.storage?.getDirectory) {
    console.warn(
      'googlesqlite: OPFS unavailable (needs a secure context); ' +
        'the history will not persist across reloads',
    )
    return
  }
  try {
    historyHandle = await openOPFSFile(OPFS_HISTORY_FILE)
  } catch (e) {
    console.error('googlesqlite: opening OPFS failed:', e)
  }
}

// readOPFSText reads a sync access handle's whole content as text.
function readOPFSText(handle: FileSystemSyncAccessHandle): string {
  const size = handle.getSize()
  if (size === 0) {
    return ''
  }
  const buf = new Uint8Array(size)
  handle.read(buf, { at: 0 })
  return new TextDecoder().decode(buf)
}

// writeOPFSText replaces a sync access handle's content with text.
function writeOPFSText(
  handle: FileSystemSyncAccessHandle,
  text: string,
): void {
  const bytes = new TextEncoder().encode(text)
  handle.truncate(bytes.byteLength)
  handle.write(bytes, { at: 0 })
  handle.flush()
}

// restore loads the persisted query history into the engine. The
// database itself has no restore step: it is :memory:-only and starts
// empty on every load.
async function restore(): Promise<void> {
  if (!historyHandle) {
    return
  }
  try {
    const json = readOPFSText(historyHandle)
    if (json) {
      await api().loadHistory(json)
    }
  } catch (e) {
    console.error('googlesqlite: restoring the history failed:', e)
  }
}

// persist writes the query history back to its OPFS file. The
// database itself is not persisted (:memory:-only engine).
async function persist(): Promise<void> {
  if (!historyHandle) {
    return
  }
  try {
    writeOPFSText(historyHandle, await api().exportHistory('json'))
  } catch (e) {
    console.error('googlesqlite: persisting the history failed:', e)
  }
}

// --- server engine mode ----------------------------------------------

// serverFetch performs one request against the native engine server
// and throws a useful Error on any failure (connection refused, or a
// non-2xx with the server's {error} JSON body).
async function serverFetch(path: string, init?: RequestInit): Promise<Response> {
  let resp: Response
  try {
    resp = await fetch(`${SERVER_URL}${path}`, init)
  } catch {
    throw new Error(
      `cannot reach the engine server at ${SERVER_URL} — ` +
        'start it with: go run ./cmd/playground',
    )
  }
  if (!resp.ok) {
    let message = `engine server error: HTTP ${resp.status}`
    try {
      const body = (await resp.json()) as { error?: string }
      if (body.error) {
        message = body.error
      }
    } catch {
      // keep the HTTP status message
    }
    throw new Error(message)
  }
  return resp
}

async function serverJSON<T>(path: string, init?: RequestInit): Promise<T> {
  return (await serverFetch(path, init)).json() as Promise<T>
}

function postJSON(body: unknown): RequestInit {
  return {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  }
}

// serverAPI implements the wasm engine's API surface over HTTP. The
// server returns the same JSON shapes (see cmd/playground/main.go),
// so callers cannot tell the engines apart.
function serverAPI(): GoogleSQLiteAPI {
  return {
    exec: (sql) => serverJSON<ExecResponse>('/exec', postJSON({ sql })),
    getHistory: () => serverJSON<QueryResult[]>('/history'),
    loadHistory: async (json) => {
      await serverFetch('/history/load', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: json,
      })
      return { ok: true }
    },
    clearHistory: async () => {
      await serverFetch('/history/clear', { method: 'POST' })
      return { ok: true }
    },
    exportHistory: async (format) =>
      (await serverFetch(`/history/export?format=${format}`)).text(),
    listTables: () => serverJSON<string[]>('/tables'),
  }
}

// initServer wires the worker to the native engine server: it checks
// the server is up, then installs the HTTP-backed API. Persistence is
// the server's database file, so OPFS is not touched in this mode.
async function initServer(): Promise<void> {
  progress('start')
  await serverFetch('/healthz')
  globalThis.googlesqlite = serverAPI()
}

// --- engine loading -------------------------------------------------

function progress(phase: LoadPhase, loaded = 0, total = 0): void {
  post({ type: 'progress', phase, loaded, total })
}

// ENGINE_CACHE is the Cache Storage bucket holding the engine binary.
const ENGINE_CACHE = 'googlesqlite-engine'

// fetchBuildHash reads the engine content hash from build-id.json. The
// hash versions the cached binary so a rebuilt engine is picked up.
async function fetchBuildHash(base: string): Promise<string> {
  try {
    const resp = await fetch(`${base}build-id.json`, { cache: 'no-cache' })
    if (resp.ok) {
      const id = (await resp.json()) as { wasm?: string }
      if (typeof id.wasm === 'string') {
        return id.wasm
      }
    }
  } catch {
    // Fall through to an unversioned URL.
  }
  return ''
}

// readWithProgress reads a response body fully into memory, reporting
// download progress as bytes arrive.
async function readWithProgress(resp: Response): Promise<Uint8Array> {
  const total = Number(resp.headers.get('Content-Length')) || 0
  const reader = resp.body!.getReader()
  const chunks: Uint8Array[] = []
  let loaded = 0
  let lastPct = -1
  for (;;) {
    const { done, value } = await reader.read()
    if (done || !value) {
      break
    }
    chunks.push(value)
    loaded += value.byteLength
    const pct = total > 0 ? Math.floor((loaded / total) * 100) : -1
    if (pct !== lastPct) {
      lastPct = pct
      progress('download', loaded, total)
    }
  }
  const out = new Uint8Array(loaded)
  let offset = 0
  for (const chunk of chunks) {
    out.set(chunk, offset)
    offset += chunk.byteLength
  }
  return out
}

// loadEngine returns the instantiated engine module. The
// gzip-compressed binary is kept in the Cache Storage API keyed by the
// build hash, so a reload or a later visit does not re-download it
// (the HTTP cache is unreliable for a binary this large). The binary
// is decompressed in the worker, so the load path matches a deploy
// that ships the compressed artifact.
async function loadEngine(
  base: string,
  importObject: WebAssembly.Imports,
): Promise<WebAssembly.WebAssemblyInstantiatedSource> {
  const hash = await fetchBuildHash(base)
  const gzURL = `${base}googlesqlite.wasm.gz${hash ? `?v=${hash}` : ''}`
  const absURL = new URL(gzURL, self.location.href).href

  // Cache Storage is only exposed in a secure context (HTTPS, or
  // localhost). Over plain HTTP — for example a LAN IP during local
  // testing — `caches` is absent; the engine then loads without being
  // cached rather than failing.
  const cache =
    typeof caches !== 'undefined' ? await caches.open(ENGINE_CACHE) : null

  let gz = cache ? await cache.match(gzURL) : undefined
  if (!gz) {
    progress('download', 0, 0)
    const fetched = await fetch(gzURL)
    if (!fetched.ok || !fetched.body) {
      throw new Error(`failed to fetch the engine binary: HTTP ${fetched.status}`)
    }
    const bytes = await readWithProgress(fetched)
    gz = new Response(bytes, {
      headers: { 'Content-Type': 'application/octet-stream' },
    })
    if (cache) {
      await cache.put(gzURL, gz.clone())
      // Drop engine binaries cached for previous builds.
      for (const req of await cache.keys()) {
        if (req.url !== absURL) {
          await cache.delete(req)
        }
      }
    }
  }

  progress('start')
  const wasmStream = gz.body!.pipeThrough(new DecompressionStream('gzip'))
  const wasmResponse = new Response(wasmStream, {
    headers: { 'Content-Type': 'application/wasm' },
  })
  return WebAssembly.instantiateStreaming(wasmResponse, importObject)
}

// initWASM installs the in-memory filesystem, loads wasm_exec.js and
// the engine binary, acquires the OPFS history handle, starts the Go
// program and resolves once the engine has installed its API.
async function initWASM(): Promise<void> {
  const base = import.meta.env.BASE_URL

  progress('timezone')
  // The filesystem and process shims must exist before wasm_exec.js
  // runs, otherwise it installs its own (non-working) fallbacks.
  await installWasmFS(base)
  installProcessShim()

  // wasm_exec.js is the Go-provided loader; it has no exports and
  // installs globalThis.Go as a side effect.
  await import(/* @vite-ignore */ `${base}wasm_exec.js`)

  const go = new Go()
  const source = await loadEngine(base, go.importObject)

  progress('start')
  const ready = new Promise<void>((resolve) => {
    globalThis.onGoogleSQLiteReady = resolve
  })
  // Acquire the history file handle before the first request can
  // persist; the engine itself takes nothing from OPFS.
  await installOPFS()
  // go.run resolves only when the Go program exits. The program parks
  // itself with select{}, so it is intentionally not awaited.
  void go.run(source.instance)
  await ready
}

// startup is awaited by every request handler so messages that arrive
// before the engine is ready queue behind initialisation.
//
// In server mode the engine (and its history) lives in the native
// server process, so the OPFS restore step does not apply.
const startup: Promise<void> = (async () => {
  if (ENGINE_MODE === 'server') {
    await initServer()
    return
  }
  await initWASM()
  await restore()
})()

async function handle(req: WorkerRequest): Promise<void> {
  await startup
  switch (req.kind) {
    case 'exec': {
      const res = await api().exec(req.sql)
      await persist()
      post({ type: 'result', id: req.id, ok: true, data: res })
      return
    }
    case 'getHistory': {
      post({
        type: 'result',
        id: req.id,
        ok: true,
        data: await api().getHistory(),
      })
      return
    }
    case 'clearHistory': {
      await api().clearHistory()
      await persist()
      post({ type: 'result', id: req.id, ok: true, data: null })
      return
    }
    case 'exportHistory': {
      post({
        type: 'result',
        id: req.id,
        ok: true,
        data: await api().exportHistory(req.format),
      })
      return
    }
    case 'listTables': {
      post({
        type: 'result',
        id: req.id,
        ok: true,
        data: await api().listTables(),
      })
      return
    }
  }
}

self.addEventListener('message', (event: MessageEvent<WorkerRequest>) => {
  const req = event.data
  handle(req).catch((e: unknown) => {
    post({
      type: 'result',
      id: req.id,
      ok: false,
      error: e instanceof Error ? e.message : String(e),
    })
  })
})

startup
  .then(() => post({ type: 'ready' }))
  .catch((e: unknown) => {
    post({
      type: 'init-error',
      error: e instanceof Error ? e.message : String(e),
    })
  })
