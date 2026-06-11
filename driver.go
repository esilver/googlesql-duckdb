package googlesqlite

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"

	duckdb "github.com/goccy/googlesqlite/internal/duckdbgo"
	googlesql "github.com/goccy/go-googlesql"

	internal "github.com/goccy/googlesqlite/internal"
)

// dsnParts splits a googlesqlite DSN into the filename portion the SQLite
// engine sees and the query string we parse for our own options. SQLite URIs
// (those that start with "file:") carry their own query parameters such as
// `mode=memory` and `cache=shared` that SQLite itself interprets — those must
// pass through to the engine untouched, otherwise `file:foo?mode=memory`
// silently becomes an on-disk file named `file:foo` and accumulates state
// across test runs. For non-URI DSNs like `:memory:?dialect=bigquery`, we
// strip the first `?` since SQLite would otherwise treat the whole DSN as a
// filename.
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

// ensureGoogleSQLInit initialises the embedded GoogleSQL WASM runtime
// once per process. Subsequent calls are no-ops (Init uses sync.Once).
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
// DuckDB rejects unknown options, so any in-memory variant collapses to
// DuckDB's bare in-memory database (""). A plain on-disk path is passed
// through after stripping the leading "file:" scheme and any query string.
func duckdbDSN(name string) string {
	path, _ := dsnParts(name)
	// Any in-memory variant -> DuckDB in-memory.
	if path == ":memory:" ||
		strings.HasPrefix(name, "file::memory:") ||
		strings.Contains(name, "mode=memory") {
		return ""
	}
	// On-disk: strip the file: scheme DuckDB does not parse, and drop the
	// SQLite query string (cache=shared etc).
	path = strings.TrimPrefix(path, "file:")
	if i := strings.IndexByte(path, '?'); i >= 0 {
		path = path[:i]
	}
	return path
}

// internalDuckDBDriver runs formatter-emitted SQL on duckdb-go. The
// SQLite-specific connection setup (DQS config, TRUSTED_SCHEMA, the ~348
// googlesqlite_* UDF registrations and INFORMATION_SCHEMA vtab modules)
// is intentionally omitted for the Milestone-0 spine: the tracer-bullet
// queries (SELECT 1, CREATE/INSERT/COUNT over scalar columns) use only
// native DuckDB SQL and bind no googlesqlite_* function. Retargeting the
// UDF layer to DuckDB scalar/aggregate UDFs is the Milestone-1 work item.
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
	if err := registerDuckDBUDFs(db); err != nil {
		return nil, nil, err
	}
	if err := registerDuckDBJSONUDFs(db); err != nil {
		return nil, nil, err
	}
	if err := registerDuckDBGeography(db); err != nil {
		return nil, nil, err
	}
	if err := registerDuckDBAggregates(db); err != nil {
		return nil, nil, err
	}
	if err := registerKLLExtractScalars(db); err != nil {
		return nil, nil, err
	}
	catalog := internal.NewCatalog(db)
	nameToDBMap[name] = db
	nameToCatalogMap[name] = catalog
	return db, catalog, nil
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
		if err := registerDuckDBUDFs(inner); err != nil {
			return nil, err
		}
		if err := registerDuckDBJSONUDFs(inner); err != nil {
			return nil, err
		}
		if err := registerDuckDBGeography(inner); err != nil {
			return nil, err
		}
		if err := registerDuckDBAggregates(inner); err != nil {
			return nil, err
		}
		if err := registerKLLExtractScalars(inner); err != nil {
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

var _ driver.DriverContext = (*Driver)(nil)

type Conn struct {
	conn           *sql.Conn
	tx             *sql.Tx
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
// already closed" error inside the next request. The flag is set by
// translateConnDone whenever an earlier operation observed the inner
// handle had been closed (typically by a request whose context was
// cancelled mid-statement; database/sql's tx watcher then ran
// Rollback(discardConn=true) on the inner connection).
func (c *Conn) IsValid() bool {
	return !c.dead.Load()
}

// translateConnDone is the single point that converts the inner
// "connection is closed" sentinel into the driver-level "drop me"
// sentinel.
//
// The inner *sql.Conn we hold can become done while still appearing
// healthy to database/sql's outer pool: a cancelled request triggers
// the inner-tx watcher's Rollback(discardConn=true), which closes the
// inner conn, but the outer pool doesn't know because the failure
// hasn't surfaced yet. The first operation on the poisoned Conn then
// fails with sql.ErrConnDone — which to a caller looks like a server
// error, not a transient retryable condition. Mapping it to
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
var _ driver.Validator = (*Conn)(nil)

func newConn(db *sql.DB, catalog *internal.Catalog) (*Conn, error) {
	conn, err := db.Conn(context.Background())
	if err != nil {
		return nil, fmt.Errorf("failed to get sqlite3 connection: %w", err)
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
	// always sees the same shape. Primitives that the underlying
	// SQLite driver can store natively (int64, float64, bool) are
	// returned unchanged by EncodeValue.
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
	stack := debug.Stack()
	if existing := *e; existing != nil {
		*e = fmt.Errorf("googlesqlite: panic %v (prior error: %w)\n%s", r, existing, stack)
	} else {
		*e = fmt.Errorf("googlesqlite: panic %v\n%s", r, stack)
	}
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
// (c.tx) is open — rolling that back is the user's decision.
func (c *Conn) rollbackDanglingTx() {
	if c.tx != nil {
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
	defer recoverPanicAsError(&e)
	conn := internal.NewConnWithOptions(c.conn, c.tx, c.systemVars, c.scriptVars, c.materializeCTE)
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
	defer recoverPanicAsError(&e)
	conn := internal.NewConnWithOptions(c.conn, c.tx, c.systemVars, c.scriptVars, c.materializeCTE)
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
	defer recoverPanicAsError(&e)
	conn := internal.NewConnWithOptions(c.conn, c.tx, c.systemVars, c.scriptVars, c.materializeCTE)
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
	tx, err := c.conn.BeginTx(ctx, &sql.TxOptions{
		Isolation: sql.IsolationLevel(opts.Isolation),
		ReadOnly:  opts.ReadOnly,
	})
	if err != nil {
		return nil, err
	}
	c.tx = tx
	return &Tx{
		tx:   tx,
		conn: c,
	}, nil
}

func (c *Conn) Begin() (t driver.Tx, e error) {
	defer func() { e = c.translateConnDone(e) }()
	tx, err := c.conn.BeginTx(context.Background(), nil)
	if err != nil {
		return nil, err
	}
	c.tx = tx
	return &Tx{
		tx:   tx,
		conn: c,
	}, nil
}

type Tx struct {
	tx   *sql.Tx
	conn *Conn
}

func (tx *Tx) Commit() error {
	defer func() {
		tx.conn.tx = nil
	}()
	return tx.tx.Commit()
}

func (tx *Tx) Rollback() error {
	defer func() {
		tx.conn.tx = nil
	}()
	return tx.tx.Rollback()
}

// AnalysisCacheCounters reports the process-wide GoogleSQL
// analysis-cache hit / miss counts (see internal/analysis_cache.go).
// Repeated identical query texts (same parameter shape, no
// intervening DDL) are served from the cache, skipping the wasm
// analyzer entirely. Intended for observability and tests.
func AnalysisCacheCounters() (hits, misses uint64) {
	return internal.AnalysisCacheCounters()
}
