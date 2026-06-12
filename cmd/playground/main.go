// Command playground is a small local HTTP server that runs the
// googlesqlite engine natively (pure-Go DuckDB backend, CGO_ENABLED=0)
// for the browser Playground UI under docs/playground.
//
// It exposes the same API surface as the js/wasm Playground engine
// (cmd/googlesqlite, the js && wasm build): exec, history, export and
// listTables — but over HTTP instead of globalThis.googlesqlite. The
// worker (docs/playground/src/api/worker.ts) selects this engine when
// the UI is started with VITE_ENGINE=server.
//
// JSON shapes match entryToJS in cmd/googlesqlite/main_js.go exactly,
// so the UI renders results from either engine identically.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/goccy/googlesqlite/internal/cli"
)

// resolveDSN maps the -db flag to a googlesqlite DSN plus a cleanup
// function. ":memory:" (or an empty path) is backed by a temporary
// file that cleanup removes — a literal in-memory DSN cannot work
// here, because the engine's catalog is read over a second short-lived
// connection (cli.Runner.catalogNames), and separate opens of an
// in-memory DuckDB database do not share state. A throwaway file keeps
// the ":memory:" contract (nothing persists) while the catalog, and so
// /tables and /schema, keep working.
func resolveDSN(db string) (dsn string, cleanup func(), err error) {
	if db == "" || db == ":memory:" {
		dir, err := os.MkdirTemp("", "googlesqlite-playground-")
		if err != nil {
			return "", nil, err
		}
		path := filepath.Join(dir, "mem.db")
		return fmt.Sprintf("file:%s?cache=shared", path),
			func() { _ = os.RemoveAll(dir) }, nil
	}
	return fmt.Sprintf("file:%s?cache=shared", db), func() {}, nil
}

// server is one Playground engine session behind HTTP. Every request
// runs under mu, mirroring the wasm entrypoint: a session is a single
// pinned connection, so statements must be serialised.
type server struct {
	mu      sync.Mutex
	session *cli.Session
	history *cli.History
	sink    *strings.Builder
}

func newServer(ctx context.Context, dsn string) (*server, error) {
	runner, err := cli.NewRunner(ctx, dsn)
	if err != nil {
		return nil, err
	}
	sink := &strings.Builder{}
	return &server{
		session: cli.NewSession(runner, sink),
		history: &cli.History{},
		sink:    sink,
	}, nil
}

func (s *server) close() error { return s.session.Runner().Close() }

// entryJSON converts a history entry into the wire form the worker
// expects. It mirrors entryToJS in cmd/googlesqlite/main_js.go: every
// field is always present (columns and rows as empty arrays, never
// null/absent), because the UI types them as required.
func entryJSON(e cli.HistoryEntry) map[string]any {
	columns := e.Columns
	if columns == nil {
		columns = []string{}
	}
	rows := e.Rows
	if rows == nil {
		rows = [][]string{}
	}
	translatedQuery := e.TranslatedQuery()
	return map[string]any{
		"statement":    e.Statement,
		"duckdbQuery":  translatedQuery,
		"sqliteQuery":  translatedQuery,
		"isQuery":      e.IsQuery,
		"columns":      columns,
		"rows":         rows,
		"rowsAffected": e.RowsAffected,
		"error":        e.Error,
		"elapsedMs":    e.ElapsedMs,
		"timestamp":    e.Timestamp.Format(time.RFC3339Nano),
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": msg})
}

// handleExec runs one or more GoogleSQL statements. Request body:
// {"sql": "..."}; response: {"output": "...", "results": [entry...]}.
// Per-statement SQL errors are reported inside each entry's "error"
// field with HTTP 200 — only a malformed request is a 4xx.
func (s *server) handleExec(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST required")
		return
	}
	var req struct {
		SQL string `json:"sql"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.sink.Reset()
	results := s.session.RunInput(r.Context(), req.SQL, false, nil)
	entries := make([]any, len(results))
	for i, res := range results {
		s.history.Add(res)
		entries[i] = entryJSON(cli.NewHistoryEntry(res))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"output":  s.sink.String(),
		"results": entries,
	})
}

func (s *server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "engine": "googlesqlite-pure-go"})
}

func (s *server) handleTables(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	names, err := s.session.TableNames(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if names == nil {
		names = []string{}
	}
	writeJSON(w, http.StatusOK, names)
}

// handleSchema reports every table/view with its column names, for
// editor autocomplete. Columns come from a zero-row probe query, so
// the engine's own name resolution (quoting, case) applies.
func (s *server) handleSchema(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	names, err := s.session.TableNames(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	type tableSchema struct {
		Name    string   `json:"name"`
		Columns []string `json:"columns"`
	}
	tables := make([]tableSchema, 0, len(names))
	for _, name := range names {
		ts := tableSchema{Name: name, Columns: []string{}}
		res := s.session.Runner().Exec(r.Context(), fmt.Sprintf("SELECT * FROM `%s` LIMIT 0", name))
		if res.Err == nil && res.Columns != nil {
			ts.Columns = res.Columns
		}
		tables = append(tables, ts)
	}
	writeJSON(w, http.StatusOK, map[string]any{"tables": tables})
}

func (s *server) handleHistory(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries := make([]any, len(s.history.Entries))
	for i, e := range s.history.Entries {
		entries[i] = entryJSON(e)
	}
	writeJSON(w, http.StatusOK, entries)
}

func (s *server) handleHistoryClear(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST required")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.history.Clear()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleHistoryExport renders the history as json, sql, markdown or
// csv (?format=..., default json) and returns it as plain text.
func (s *server) handleHistoryExport(w http.ResponseWriter, r *http.Request) {
	format := r.URL.Query().Get("format")
	if format == "" {
		format = "json"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out, err := s.history.Export(format)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, out)
}

// handleHistoryLoad replaces the history with a JSON document
// previously produced by /history/export?format=json.
func (s *server) handleHistoryLoad(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST required")
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var h cli.History
	if err := json.Unmarshal(body, &h); err != nil {
		writeError(w, http.StatusBadRequest, "invalid history JSON: "+err.Error())
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.history = &h
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// corsLocalhost wraps a handler with a CORS policy that admits any
// localhost / 127.0.0.1 / [::1] origin (the Vite dev server runs on
// localhost:5173) and answers preflight requests.
func corsLocalhost(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get("Origin"); origin != "" && isLocalOrigin(origin) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// isLocalOrigin reports whether origin is a localhost origin of any
// port and scheme (http://localhost:5173, http://127.0.0.1:4173, ...).
func isLocalOrigin(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	host := u.Hostname()
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

// handler builds the server's full route table behind the CORS layer.
func (s *server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/exec", s.handleExec)
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/tables", s.handleTables)
	mux.HandleFunc("/schema", s.handleSchema)
	mux.HandleFunc("/history", s.handleHistory)
	mux.HandleFunc("/history/clear", s.handleHistoryClear)
	mux.HandleFunc("/history/export", s.handleHistoryExport)
	mux.HandleFunc("/history/load", s.handleHistoryLoad)
	return corsLocalhost(mux)
}

func main() {
	os.Exit(realMain())
}

func realMain() int {
	var (
		dbPath string
		addr   string
	)
	flag.StringVar(&dbPath, "db", "playground.db", `database file path (":memory:" for a non-persistent database)`)
	flag.StringVar(&addr, "addr", "localhost:8765", "listen address")
	flag.Parse()

	dsn, cleanup, err := resolveDSN(dbPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "playground:", err)
		return 1
	}
	defer cleanup()
	srv, err := newServer(context.Background(), dsn)
	if err != nil {
		fmt.Fprintln(os.Stderr, "playground:", err)
		return 1
	}
	defer srv.close()

	log.Printf("googlesqlite playground server (pure-Go DuckDB backend)")
	log.Printf("database: %s", dsn)
	log.Printf("listening on http://%s — POST /exec, GET /healthz, GET /schema", addr)
	if err := http.ListenAndServe(addr, srv.handler()); err != nil {
		fmt.Fprintln(os.Stderr, "playground:", err)
		return 1
	}
	return 0
}
