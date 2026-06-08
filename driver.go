package googlesqlite

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"os"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"

	duckdb "github.com/duckdb/duckdb-go/v2"
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
// is executed. It is the value of the EnvWasmCompilationMode
// environment variable.
type WasmCompilationMode string

const (
	// WasmCompilationModeInterpreter runs the wasm on the wazero
	// interpreter: portable and zero-setup, but slower at hot SQL
	// workloads. This is the default.
	WasmCompilationModeInterpreter WasmCompilationMode = "interpreter"
	// WasmCompilationModeCompiler AOT-compiles the wasm with the
	// wazero compiler for higher steady-state performance, at a
	// one-time compile cost on the first sql.Open. Pair it with
	// EnvWasmCacheDir to amortise that cost across processes.
	WasmCompilationModeCompiler WasmCompilationMode = "compiler"
)

// Environment variables that configure the embedded GoogleSQL wasm
// runtime. They are read once, on the first sql.Open of a googlesqlite
// database in the process, and let CI and other out-of-process callers
// tune the runtime without code changes.
const (
	// EnvWasmCompilationMode selects the wazero execution engine. Its
	// value is a WasmCompilationMode; absent or unrecognised, the
	// runtime defaults to WasmCompilationModeInterpreter.
	EnvWasmCompilationMode = "GOOGLESQLITE_WASM_COMPILATION_MODE"
	// EnvWasmCacheDir points the wazero on-disk compilation cache at a
	// directory, so subsequent processes load the precompiled bytes
	// instead of recompiling the wasm. Honoured only under
	// WasmCompilationModeCompiler.
	EnvWasmCacheDir = "GOOGLESQLITE_WASM_CACHE_DIR"
)

// wasmOptions is the resolved wasm runtime configuration.
type wasmOptions struct {
	mode     WasmCompilationMode
	cacheDir string
}

// resolveWasmOptions reads the EnvWasmCompilationMode / EnvWasmCacheDir
// environment variables; absent them it returns the interpreter-mode
// default.
func resolveWasmOptions() wasmOptions {
	if strings.EqualFold(os.Getenv(EnvWasmCompilationMode), string(WasmCompilationModeCompiler)) {
		return wasmOptions{
			mode:     WasmCompilationModeCompiler,
			cacheDir: os.Getenv(EnvWasmCacheDir),
		}
	}
	return wasmOptions{mode: WasmCompilationModeInterpreter}
}

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
	opts := resolveWasmOptions()
	googlesqlOpts := make([]googlesql.Option, 0, 2)
	switch opts.mode {
	case WasmCompilationModeCompiler:
		googlesqlOpts = append(googlesqlOpts, googlesql.WithCompilationMode(googlesql.CompilationModeCompiler))
		if opts.cacheDir != "" {
			googlesqlOpts = append(googlesqlOpts, googlesql.WithCompilationCache(opts.cacheDir))
		}
	default:
		googlesqlOpts = append(googlesqlOpts, googlesql.WithCompilationMode(googlesql.CompilationModeInterpreter))
	}
	if err := googlesql.Init(googlesqlOpts...); err != nil {
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

func (internalDuckDBDriver) Open(name string) (driver.Conn, error) {
	return duckdb.Driver{}.Open(duckdbDSN(name))
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
}

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

func (c *Conn) PrepareContext(ctx context.Context, query string) (s driver.Stmt, e error) {
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

func (c *Conn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
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

func (c *Conn) Begin() (driver.Tx, error) {
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
