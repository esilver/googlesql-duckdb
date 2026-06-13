package googlesqlite

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"net/url"
	"os"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"

	googlesql "github.com/goccy/go-googlesql"
	duckdb "github.com/goccy/googlesqlite/internal/duckdbgo"

	internal "github.com/goccy/googlesqlite/internal"
)

// dsnParts splits a googlesqlite DSN into its path and driver-option query
// string while preserving historical SQLite-style URI forms. The DuckDB DSN
// translator consumes the returned pieces and either maps those URI semantics
// onto DuckDB or strips SQLite-only parameters before opening the raw engine.
// For non-URI DSNs like `:memory:?dialect=bigquery`, we strip the first `?`
// so googlesqlite options do not become part of the raw-engine filename.
func dsnParts(dsn string) (path, query string) {
	if strings.HasPrefix(dsn, "file:") {
		return dsn, ""
	}
	if i := strings.IndexByte(dsn, '?'); i >= 0 {
		return dsn[:i], dsn[i+1:]
	}
	return dsn, ""
}

// WasmCompilationMode selects how the embedded GoogleSQL wasm module
// is executed.
//
// Deprecated: since go-googlesql moved from the wazero interpreter to a
// wasm2go-transpiled native-Go module (github.com/goccy/googlesqlwasm2go),
// there is no wasm runtime to configure. The type and its constants are
// retained for source compatibility only and have no effect.
type WasmCompilationMode string

const (
	// WasmCompilationModeInterpreter is retained for source
	// compatibility; the analyzer now always runs as ordinary Go code.
	//
	// Deprecated: no effect; see WasmCompilationMode.
	WasmCompilationModeInterpreter WasmCompilationMode = "interpreter"
	// WasmCompilationModeCompiler is retained for source compatibility;
	// the analyzer now always runs as ordinary Go code.
	//
	// Deprecated: no effect; see WasmCompilationMode.
	WasmCompilationModeCompiler WasmCompilationMode = "compiler"
)

// Environment variables that previously configured the embedded
// GoogleSQL wasm runtime. The analyzer is now a wasm2go-transpiled
// native-Go module, so both are ignored.
const (
	// EnvWasmCompilationMode previously selected the wazero execution
	// engine.
	//
	// Deprecated: ignored; the analyzer runs as ordinary Go code.
	EnvWasmCompilationMode = "GOOGLESQLITE_WASM_COMPILATION_MODE"
	// EnvWasmCacheDir previously pointed the wazero compilation cache
	// at a directory.
	//
	// Deprecated: ignored; there is no wasm compilation step.
	EnvWasmCacheDir = "GOOGLESQLITE_WASM_CACHE_DIR"
)

// ensureGoogleSQLInit initialises the embedded GoogleSQL analyzer module once
// per process. Subsequent calls are no-ops (Init uses sync.Once).
// It is invoked on the first sql.Open of a googlesqlite DB.
//
// It also primes the one-time spatial-extension load here, inside the same
// process-init barrier. Because this runs under sync.OnceValue, the first
// open blocks every other open until it returns, so the single INSTALL/LOAD
// of the spatial extension completes while the process is still effectively
// single-threaded - before any concurrent query reaches DuckDB's
// duckdb_execute_pending. That ordering is what keeps the extension loader
// from racing the steady-state query stream (see loadSpatialExtension).
var ensureGoogleSQLInit = sync.OnceValue(func() error {
	if err := googlesql.Init(); err != nil {
		return err
	}
	return loadSpatialExtension(nil)
})

var (
	_ driver.Driver = &Driver{}
	_ driver.Conn   = &Conn{}
	_ driver.Tx     = &Tx{}
)

var (
	nameToCatalogMap = map[string]*internal.Catalog{}
	nameToDBMap      = map[string]*sql.DB{}
	nameToValueMapMu sync.Mutex

	// DuckDB extension/UDF registration mutates per-database catalogs through
	// the pure-Go DuckDB bridge. Keep that registration path single-threaded;
	// callers may still open/query already-registered databases concurrently.
	duckDBRegistrationMu sync.Mutex
)

// internalDriverName is the database/sql driver name for the raw
// engine that backs every googlesqlite query. The DuckDB fork routes
// formatter-emitted SQL straight to the duckdb-go driver registered as
// "duckdb"; we open it through that registered driver via a thin
// connector wrapper that only rewrites the DSN.
const internalDriverName = "googlesqlite_duckdb"

func init() {
	sql.Register("googlesqlite", &Driver{})
	sql.Register(internalDriverName, &internalDuckDBDriver{})
}

// duckdbDSN translates a googlesqlite-style DSN into one DuckDB accepts.
// SQLite URI forms (file::memory:?cache=shared, :memory:?<opts>) and the
// engine's per-open `_googlesqlite_id` freshness suffix are SQLite-only;
// DuckDB rejects unknown options, so any in-memory variant collapses to a
// DuckDB in-memory DSN. A plain on-disk path is passed through after stripping
// the leading "file:" scheme and SQLite-only query params. DuckDB-native
// allowlisted params are preserved.
func duckdbDSN(name string) string {
	path, query := dsnParts(name)
	if query == "" {
		if i := strings.IndexByte(path, '?'); i >= 0 {
			query = path[i+1:]
		}
	}
	duckParams := duckDBQueryParams(query)
	// Any in-memory variant -> DuckDB in-memory. The supported
	// file:/NAME?vfs=memdb form names a process-shared in-memory
	// database keyed by its path. Mapping it to a root-level on-disk
	// file fails on DuckDB (errno 30), so it collapses to a DuckDB
	// in-memory DSN. The DSN-keyed *sql.DB cache in newDBAndCatalog
	// already shares one engine per full DSN string, which reproduces
	// the shared-by-name semantics on a DuckDB in-memory database.
	if path == ":memory:" ||
		strings.HasPrefix(name, "file::memory:") ||
		strings.Contains(name, "mode=memory") ||
		strings.Contains(name, "vfs=memdb") {
		if duckParams != "" {
			return ":memory:?" + duckParams
		}
		return ""
	}
	// On-disk: strip the file: scheme DuckDB does not parse, and drop
	// SQLite-only query params (cache=shared etc).
	path = strings.TrimPrefix(path, "file:")
	if i := strings.IndexByte(path, '?'); i >= 0 {
		path = path[:i]
	}
	if path != "" {
		if st, err := os.Stat(path); err == nil && st.Mode().IsRegular() && st.Size() == 0 {
			_ = os.Remove(path)
		}
	}
	if duckParams != "" {
		return path + "?" + duckParams
	}
	return path
}

func duckDBQueryParams(query string) string {
	values, err := url.ParseQuery(query)
	if err != nil {
		return ""
	}
	out := url.Values{}
	for _, key := range []string{"max_memory", "memory_limit"} {
		for _, value := range values[key] {
			if value != "" {
				out.Add(key, value)
			}
		}
	}
	return out.Encode()
}

// internalDuckDBDriver runs formatter-emitted SQL on duckdb-go. Each
// connection registers the googlesqlite_* scalar and aggregate UDFs
// against the DuckDB handle via registerDuckDBUDFs in duckdb_udf.go,
// which calls internal.EnsureFuncMaps so the formatter lowers exactly
// the builtins that are registered.
type internalDuckDBDriver struct{}

// internalDuckDBDriver must implement driver.DriverContext: without
// OpenConnector, database/sql wraps the driver in its dsnConnector
// fallback and calls Open once per POOLED CONNECTION — and the engine
// driver's raw Open creates a brand-new wasm engine (~200MB, no
// googlesqlite_* UDFs) per call. Any second pooled connection (forced
// by a held transaction or concurrent load) then ran on a fresh,
// UDF-less engine ("Scalar Function with name googlesqlite_in_array
// does not exist"). Delegating to the engine driver's connector shares
// ONE engine + database across the whole pool.
var _ driver.DriverContext = internalDuckDBDriver{}

func (internalDuckDBDriver) Open(name string) (driver.Conn, error) {
	return duckdb.Driver{}.Open(duckdbDSN(name))
}

// OpenConnector implements driver.DriverContext by handing back the
// engine driver's connector (one shared engine + database; a fresh
// duckdb_connection per Connect).
func (internalDuckDBDriver) OpenConnector(name string) (driver.Connector, error) {
	return duckdb.Driver{}.OpenConnector(duckdbDSN(name))
}

func newDBAndCatalog(name string) (*sql.DB, *internal.Catalog, error) {
	if err := ensureGoogleSQLInit(); err != nil {
		return nil, nil, fmt.Errorf("googlesqlite: googlesql init: %w", err)
	}
	nameToValueMapMu.Lock()
	defer nameToValueMapMu.Unlock()
	db, exists := nameToDBMap[name]
	if exists {
		return db, nameToCatalogMap[name], nil
	}
	db, err := sql.Open(internalDriverName, name)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to open database by %s: %w", name, err)
	}
	// duckdb-go shares one underlying in-memory database across every
	// connection minted from the same connector, so DDL on one pooled
	// connection is visible to queries on another. We deliberately do NOT
	// pin MaxOpenConns(1): the engine issues nested statements during
	// query execution, and a single-connection pool deadlocks them.
	if err := registerDuckDBExtensions(db); err != nil {
		return nil, nil, err
	}
	catalog := internal.NewCatalog(db)
	nameToDBMap[name] = db
	nameToCatalogMap[name] = catalog
	return db, catalog, nil
}

func registerDuckDBExtensions(db *sql.DB) error {
	duckDBRegistrationMu.Lock()
	defer duckDBRegistrationMu.Unlock()

	if err := registerDuckDBUDFs(db); err != nil {
		return err
	}
	if err := registerDuckDBJSONUDFs(db); err != nil {
		return err
	}
	if err := registerDuckDBGeography(db); err != nil {
		return err
	}
	if err := registerDuckDBAggregates(db); err != nil {
		return err
	}
	if err := registerKLLExtractScalars(db); err != nil {
		return err
	}
	return nil
}

type Driver struct {
	ConnectHook func(*Conn) error
}

func (d *Driver) Open(name string) (driver.Conn, error) {
	c, err := d.OpenConnector(name)
	if err != nil {
		return nil, err
	}
	return c.Connect(context.Background())
}

// memoryConnectorSeq mints a unique suffix per :memory: connector so each
// sql.Open(":memory:") gets its own underlying *sql.DB and catalog. Without
// this, the DSN-keyed cache would hand every :memory: open the same *sql.DB,
// and SQLite's connection pool would persist DDL across sql.Opens — observable
// as "table already exists" on a second sql.Open(":memory:") run.
var memoryConnectorSeq atomic.Uint64

// connector is the googlesqlite driver.Connector. Holding the inner *sql.DB
// and catalog on the connector means every conn from the same *sql.DB shares
// state, while different *sql.DB instances get independent catalogs. The
// previous global DSN-keyed cache could not distinguish those two cases.
type connector struct {
	driver     *Driver
	name       string
	inner      *sql.DB
	catalog    *internal.Catalog
	innerOwned bool
	innerName  string
}

func (c *connector) Connect(_ context.Context) (dc driver.Conn, e error) {
	defer recoverPanicAsError(&e)
	conn, err := newConn(c.inner, c.catalog)
	if err != nil {
		return nil, err
	}
	if c.driver.ConnectHook != nil {
		if err := c.driver.ConnectHook(conn); err != nil {
			return nil, err
		}
	}
	return conn, nil
}

func (c *connector) Driver() driver.Driver { return c.driver }

func (c *connector) Close() error {
	if !c.innerOwned {
		return nil
	}
	nameToValueMapMu.Lock()
	delete(nameToCatalogMap, c.innerName)
	nameToValueMapMu.Unlock()
	return c.inner.Close()
}

func (d *Driver) OpenConnector(name string) (driver.Connector, error) {
	if err := ensureGoogleSQLInit(); err != nil {
		return nil, fmt.Errorf("googlesqlite: googlesql init: %w", err)
	}
	if isPrivateInMemoryDSN(name) {
		innerName := freshMemoryDSN(name)
		inner, err := sql.Open(internalDriverName, innerName)
		if err != nil {
			return nil, fmt.Errorf("failed to open database by %s: %w", name, err)
		}
		if err := registerDuckDBExtensions(inner); err != nil {
			return nil, err
		}
		catalog := internal.NewCatalog(inner)
		nameToValueMapMu.Lock()
		nameToCatalogMap[innerName] = catalog
		nameToValueMapMu.Unlock()
		return &connector{driver: d, name: name, inner: inner, catalog: catalog, innerOwned: true, innerName: innerName}, nil
	}
	db, catalog, err := newDBAndCatalog(name)
	if err != nil {
		return nil, err
	}
	return &connector{driver: d, name: name, inner: db, catalog: catalog}, nil
}

// isPrivateInMemoryDSN reports whether the DSN names a private in-memory
// database that should be re-created on every sql.Open. Bare ":memory:" is
// the obvious case; SQLite URIs of the form `file:NAME?mode=memory&...`
// without `cache=shared` are also private per-connection in-memory DBs.
// The flip side — `file::memory:?cache=shared` (used by some consumers)
// — is *intentionally* shared across opens, so we keep that in the DSN
// cache.
func isPrivateInMemoryDSN(name string) bool {
	path, _ := dsnParts(name)
	if path == ":memory:" {
		return true
	}
	if !strings.HasPrefix(name, "file:") {
		return false
	}
	i := strings.IndexByte(name, '?')
	if i < 0 {
		return false
	}
	hasModeMemory := false
	hasSharedCache := false
	for _, kv := range strings.Split(name[i+1:], "&") {
		switch kv {
		case "mode=memory":
			hasModeMemory = true
		case "cache=shared":
			hasSharedCache = true
		}
	}
	return hasModeMemory && !hasSharedCache
}

// freshMemoryDSN appends a unique query parameter so the inner sql.Open call
// produces a distinct cache key per outer sql.Open. SQLite ignores unknown
// URI parameters, so adding `_googlesqlite_id` is safe for both bare
// ":memory:" DSNs and SQLite URI-form DSNs.
func freshMemoryDSN(name string) string {
	id := memoryConnectorSeq.Add(1)
	suffix := fmt.Sprintf("_googlesqlite_id=%d", id)
	if strings.IndexByte(name, '?') >= 0 {
		return name + "&" + suffix
	}
	return name + "?" + suffix
}

// RawEngineDB returns the raw-engine *sql.DB the googlesqlite driver is
// sharing for the given DSN, if one is currently open. Callers that need
// raw-engine reads (the CLI's .tables/.functions catalog queries) MUST use
// this handle rather than sql.Open a second engine on the same DSN:
// duckdb-go-pure >= v0.3.3 enforces DuckDB's single-instance file lock, so
// a second engine on the same on-disk database fails with "Conflicting
// lock is held". The returned handle is owned by the driver — callers must
// not Close it. Private in-memory connectors (bare :memory:) are connector-
// owned and not in the DSN cache; they report false.
func RawEngineDB(name string) (*sql.DB, bool) {
	nameToValueMapMu.Lock()
	defer nameToValueMapMu.Unlock()
	db, ok := nameToDBMap[name]
	return db, ok
}

var _ driver.DriverContext = (*Driver)(nil)

type Conn struct {
	conn           *sql.Conn
	txActive       bool
	txReset        string
	analyzer       *internal.Analyzer
	catalog        *internal.Catalog
	systemVars     map[string]string
	scriptVars     map[string]string
	materializeCTE bool

	// dead is set when an operation on this Conn observes that the
	// inner *sql.Conn has been closed (sql.ErrConnDone). database/sql
	// pools Conn values across requests; without an explicit signal it
	// happily hands a Conn whose inner handle has been closed by a
	// cancelled tx's discardConn rollback to the next caller, which
	// then surfaces a confusing "sql: connection is already closed"
	// error on every subsequent request (see IsValid and the
	// translateConnDone defers below). On this fork the exposure is
	// larger than upstream's: every pooled Conn shares one DuckDB
	// engine, so a wedged Conn is a wedged slot against the shared
	// engine. The dead flag is read by IsValid and written from any
	// goroutine that drove the failing operation; concurrent writes are
	// harmless because they all store the same value (true).
	//
	// Ported from upstream goccy/googlesqlite bf326675 (issue #12).
	dead atomic.Bool
}

// IsValid implements database/sql/driver.Validator. database/sql calls
// it on every pool fetch before handing the Conn to the next caller —
// returning false makes the pool discard us and pull a fresh one
// instead of letting us surface a confusing "sql: connection is
// already closed" error inside the next request.
func (c *Conn) IsValid() bool {
	return !c.dead.Load() && !c.txActive
}

// ResetSession implements driver.SessionResetter as a second line of
// defense for the same poisoned states IsValid covers: database/sql
// calls it on every cached-conn fetch and silently swaps in a fresh
// conn when it reports driver.ErrBadConn, so the caller never observes
// the dead conn even if it slipped past put-back validation.
func (c *Conn) ResetSession(_ context.Context) error {
	if c.dead.Load() || c.txActive {
		return driver.ErrBadConn
	}
	return nil
}

// translateConnDone is the single point that converts the inner
// "connection is closed" sentinel into the driver-level "drop me"
// sentinel.
//
// The inner *sql.Conn we hold can become done while still appearing
// healthy to database/sql's outer pool. The first operation on the
// poisoned Conn then fails with sql.ErrConnDone — which to a caller
// looks like a server error, not a transient retryable condition. Mapping it to
// driver.ErrBadConn tells database/sql to drop the Conn from the pool
// and retry on a fresh one (when the call has not yet sent data),
// hiding the transient from the caller; setting dead=true means the
// next IsValid call returns false so the Conn never even gets that
// far again.
//
// Wrapped errors are inspected because ExecContext / QueryContext may
// fold cleanup-time errors into an internal.ErrorGroup; the group
// implements Unwrap() []error so errors.Is reaches the sentinel.
func (c *Conn) translateConnDone(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, sql.ErrConnDone) {
		c.dead.Store(true)
		return driver.ErrBadConn
	}
	return err
}

// Static contract check: database/sql's pool only calls IsValid via a
// type assertion against driver.Validator. Failing this assertion at
// compile time keeps a future refactor that drops the method visible
// instead of silently regressing the fix.
var (
	_ driver.Validator       = (*Conn)(nil)
	_ driver.SessionResetter = (*Conn)(nil)
)

func newConn(db *sql.DB, catalog *internal.Catalog) (*Conn, error) {
	conn, err := db.Conn(context.Background())
	if err != nil {
		return nil, fmt.Errorf("failed to get DuckDB connection: %w", err)
	}
	analyzer, err := internal.NewAnalyzer(catalog)
	if err != nil {
		return nil, fmt.Errorf("failed to create analyzer: %w", err)
	}
	return &Conn{
		conn:           conn,
		analyzer:       analyzer,
		catalog:        catalog,
		systemVars:     map[string]string{},
		scriptVars:     map[string]string{},
		materializeCTE: true,
	}, nil
}

// SetMaxNamePath specifies the maximum value of name path.
// If the name path in the query is the maximum value, the name path set as prefix is not used.
// Effective only when a value greater than zero is specified ( default zero ).
func (c *Conn) SetMaxNamePath(num int) {
	c.analyzer.SetMaxNamePath(num)
}

// SetNamePath set path to name path to be set as prefix.
// If max name path is specified, an error is returned if the number is exceeded.
func (c *Conn) SetNamePath(path []string) error {
	return c.analyzer.SetNamePath(path)
}

func (s *Conn) CheckNamedValue(value *driver.NamedValue) error {
	// Encode every value into the googlesqlite value layout so the
	// rest of the pipeline (formatter, reshapeInsertArgs, scanner)
	// always sees the same shape. Primitives that the raw engine can
	// store natively (int64, float64, bool) are returned unchanged by
	// EncodeValue.
	return internal.CheckNamedValue(value)
}

func (c *Conn) Prepare(query string) (driver.Stmt, error) {
	stmt, err := c.PrepareContext(context.Background(), query)
	return stmt, err
}

// recoverPanicAsError converts an arbitrary panic value into an error
// recorded on `e`. It is meant to be invoked from a deferred call in
// every driver-public entry point so that a bug anywhere inside
// analyzer/formatter/runtime never escapes as a panic — a `nil`
// `*sql.Conn`/`*sql.Stmt` being returned by a panicking driver call
// is what triggers `database/sql`'s deferred close to deadlock on the
// per-conn closemu lock.
//
// The first line of the stack trace is preserved on the returned
// error so the failure remains debuggable; downstream callers
// (database/sql) get an ordinary error and can clean up normally.
func recoverPanicAsError(e *error) {
	r := recover()
	if r == nil {
		return
	}
	*e = panicAsError(r, *e)
}

// panicAsError formats a recovered panic value (plus the in-flight
// error, if any) into the ordinary error the boundary returns. The
// panic value is rendered with %v on purpose: the result must NEVER
// satisfy errors.Is(err, driver.ErrBadConn) — a Raw callback or driver
// entry point that surfaces ErrBadConn makes database/sql call
// Conn.close synchronously, which deadlocks on closemu against an open
// transaction's read lock (issue #20).
func panicAsError(r any, prior error) error {
	stack := debug.Stack()
	if prior != nil {
		return fmt.Errorf("googlesqlite: panic %v (prior error: %w)\n%s", r, prior, stack)
	}
	return fmt.Errorf("googlesqlite: panic %v\n%s", r, stack)
}

// recoverPanicPoison is the Conn-bound recoverPanicAsError: the same
// panic→error conversion, plus it marks the Conn dead so the pool
// discards it (via IsValid / ResetSession) once the caller is done
// with it.
//
// This is the issue #20 containment seam. A panic that escapes the
// driver boundary while the caller holds an open transaction on the
// same *sql.Conn is fatal upstream: stdlib Conn.Raw's deferred release
// treats the panic as driver.ErrBadConn and synchronously calls
// Conn.close, which needs the closemu WRITE lock — held as a read lock
// by the open transaction until it ends, which it never can because
// its rollback defer sits in a caller frame above the parked panic.
// Permanent mid-unwind self-deadlock; every caller-held mutex is
// orphaned (observed live as a wedged bigquery-emulator after one
// analyzer bad_alloc, #19/#20). Recovering here returns a plain
// non-ErrBadConn error instead: Raw skips the synchronous close, the
// transaction unwinds through the caller's rollback defer, and the
// poisoned Conn — whose analyzer/wasm state may be unusable after the
// abort — is pruned at pool put-back/fetch instead.
func (c *Conn) recoverPanicPoison(e *error) {
	r := recover()
	if r == nil {
		return
	}
	c.dead.Store(true)
	*e = panicAsError(r, *e)
}

// rollbackDanglingTx clears a transaction the DuckDB engine may have
// left open on the pooled connection after a failed statement. The
// pure-Go engine begins an implicit transaction at PREPARE time and a
// Binder-stage prepare failure returns without aborting it; every
// later statement on the same connection then fails with "cannot
// start a transaction within a transaction" (the first victim is the
// catalog sync). Issuing a best-effort ROLLBACK clears the dangling
// transaction; if none is open the ROLLBACK itself fails harmlessly
// and is discarded. Never called while an explicit user transaction
// (txActive) is open — rolling that back is the user's decision.
func (c *Conn) rollbackDanglingTx() {
	if c.txActive {
		return
	}
	// context.Background() deliberately: the statement's ctx may be the
	// reason the statement failed (cancellation), and the rollback must
	// still run to un-poison the pooled connection.
	_, _ = c.conn.ExecContext(context.Background(), "ROLLBACK")
}

func (c *Conn) PrepareContext(ctx context.Context, query string) (s driver.Stmt, e error) {
	defer func() { e = c.translateConnDone(e) }()
	defer func() {
		if e != nil {
			c.rollbackDanglingTx()
		}
	}()
	defer c.recoverPanicPoison(&e)
	conn := internal.NewConnWithOptions(c.conn, nil, c.systemVars, c.scriptVars, c.materializeCTE)
	actionFuncs, err := c.analyzer.Analyze(ctx, conn, query, nil)
	if err != nil {
		return nil, err
	}
	var stmt driver.Stmt
	for _, actionFunc := range actionFuncs {
		action, err := actionFunc()
		if err != nil {
			return nil, err
		}
		got, err := action.Prepare(ctx, conn)
		if err != nil {
			return nil, err
		}
		stmt = got
	}
	return stmt, nil
}

func (c *Conn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (r driver.Result, e error) {
	defer func() { e = c.translateConnDone(e) }()
	defer func() {
		if e != nil {
			c.rollbackDanglingTx()
		}
	}()
	defer c.recoverPanicPoison(&e)
	conn := internal.NewConnWithOptions(c.conn, nil, c.systemVars, c.scriptVars, c.materializeCTE)
	actionFuncs, err := c.analyzer.Analyze(ctx, conn, query, args)
	if err != nil {
		return nil, err
	}
	var actions []internal.StmtAction
	defer func() {
		eg := new(internal.ErrorGroup)
		eg.Add(e)
		for _, action := range actions {
			eg.Add(action.Cleanup(ctx, conn))
		}
		if eg.HasError() {
			e = eg
		}
	}()

	var result driver.Result
	for _, actionFunc := range actionFuncs {
		action, err := actionFunc()
		if err != nil {
			return nil, err
		}
		actions = append(actions, action)
		r, err := action.ExecContext(ctx, conn)
		if err != nil {
			return nil, err
		}
		result = r
	}
	return result, nil
}

func (c *Conn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (r driver.Rows, e error) {
	defer func() { e = c.translateConnDone(e) }()
	defer func() {
		if e != nil {
			c.rollbackDanglingTx()
		}
	}()
	defer c.recoverPanicPoison(&e)
	conn := internal.NewConnWithOptions(c.conn, nil, c.systemVars, c.scriptVars, c.materializeCTE)
	actionFuncs, err := c.analyzer.Analyze(ctx, conn, query, args)
	if err != nil {
		return nil, err
	}
	var (
		actions []internal.StmtAction
		rows    *internal.Rows
	)
	defer func() {
		if rows != nil {
			// If we call cleanup action at the end of QueryContext function,
			// there is a possibility that the deleted table will be referenced when scanning from Rows,
			// so cleanup action should be executed in the Close() process of Rows.
			// For that, let Rows have a reference to actions ( and connection ).
			rows.SetActions(actions)
		}
	}()
	for _, actionFunc := range actionFuncs {
		action, err := actionFunc()
		if err != nil {
			return nil, err
		}
		actions = append(actions, action)
		queryRows, err := action.QueryContext(ctx, conn)
		if err != nil {
			return nil, err
		}
		rows = queryRows
	}
	return rows, nil
}

func (c *Conn) Close() error {
	return c.conn.Close()
}

func (c *Conn) BeginTx(ctx context.Context, opts driver.TxOptions) (t driver.Tx, e error) {
	defer func() { e = c.translateConnDone(e) }()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	txReset, err := c.beginExplicitTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	c.txActive = true
	c.txReset = txReset
	return &Tx{
		conn: c,
	}, nil
}

func (c *Conn) Begin() (t driver.Tx, e error) {
	defer func() { e = c.translateConnDone(e) }()
	txReset, err := c.beginExplicitTx(context.Background(), driver.TxOptions{})
	if err != nil {
		return nil, err
	}
	c.txActive = true
	c.txReset = txReset
	return &Tx{
		conn: c,
	}, nil
}

// beginExplicitTx uses the request context only while running BEGIN.
// It deliberately avoids creating an inner *sql.Tx, whose cancel
// watcher can close the inner *sql.Conn asynchronously after statement
// cancellation and leave the outer driver Conn poisoned.
func (c *Conn) beginExplicitTx(ctx context.Context, opts driver.TxOptions) (string, error) {
	if opts.ReadOnly {
		return "", errors.New("googlesqlite: read-only transactions are not supported by the DuckDB backend")
	}
	if opts.Isolation != driver.IsolationLevel(sql.LevelDefault) {
		return "", fmt.Errorf("googlesqlite: unsupported isolation level %d on the DuckDB backend", opts.Isolation)
	}
	if _, err := c.conn.ExecContext(ctx, "BEGIN TRANSACTION"); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", ctxErr
		}
		return "", err
	}
	return "", nil
}

type Tx struct {
	conn *Conn
}

func (tx *Tx) Commit() error {
	defer func() {
		tx.conn.txActive = false
		tx.conn.txReset = ""
	}()
	err := tx.conn.execTxEnd("COMMIT")
	if err != nil {
		_ = tx.conn.execTxEnd("ROLLBACK")
	}
	return err
}

func (tx *Tx) Rollback() error {
	defer func() {
		tx.conn.txActive = false
		tx.conn.txReset = ""
	}()
	return tx.conn.execTxEnd("ROLLBACK")
}

func (c *Conn) execTxEnd(command string) error {
	if !c.txActive {
		return sql.ErrTxDone
	}
	query := command
	if c.txReset != "" {
		query += "; " + c.txReset
	}
	_, err := c.conn.ExecContext(context.Background(), query)
	return err
}

// AnalysisCacheCounters reports the process-wide GoogleSQL
// analysis-cache hit / miss counts (see internal/analysis_cache.go).
// Repeated identical query texts (same parameter shape, no
// intervening DDL) are served from the cache, skipping the wasm
// analyzer entirely. Intended for observability and tests.
func AnalysisCacheCounters() (hits, misses uint64) {
	return internal.AnalysisCacheCounters()
}
