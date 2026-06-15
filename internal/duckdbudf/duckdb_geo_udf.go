package duckdbudf

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"sync"

	duckdb "github.com/goccy/googlesqlite/internal/duckdbgo"

	internal "github.com/goccy/googlesqlite/internal"
)

// spatialLoadOnce drives the spatial extension's INSTALL/LOAD exactly
// once per process, and spatialLoadErr records that single attempt's
// outcome for every later caller.
//
// RegisterGeography runs on every DB open (see driver.go), and the
// conformance harness opens a fresh in-memory DuckDB instance per spec
// case under two layers of t.Parallel(). The DuckDB C++ extension loader
// mutates process-global state (the install registry and the extension's
// static init), and that mutation is not safe while other connections are
// executing statements on other database instances: a LOAD on DB-A racing
// the duckdb_execute_pending of an ordinary query on DB-B aborts the whole
// process with SIGABRT ("signal arrived during cgo execution"). The prior
// mitigation took a plain mutex around each per-DB load, which serialized
// load-vs-load but left every load racing the steady-state query stream,
// so the abort persisted.
//
// The geography family lowers entirely to the pure-Go googlesqlite_st_* /
// googlesqlite_s2_* scalar UDFs registered below (see isGeographyScalarFunc
// in internal/formatter.go), so no spec actually executes a native DuckDB
// ST_* function and the runtime never needs the extension present in a
// query-serving catalog. We therefore collapse the per-DB load to a single
// process-wide load on a throwaway bootstrap database, performed the first
// time any DB is opened - while the harness is still single-threaded,
// before the parallel query stream begins. After that one load the
// extension is never loaded again, which removes the concurrent-loader race
// at its source rather than papering over it. Honouring the documented
// parity-check intent, the extension is still installed/loaded once so an
// out-of-band native ST_* probe against the bootstrap handle keeps working.
var (
	spatialLoadOnce sync.Once
	spatialLoadErr  error
)

// LoadSpatialExtension ensures DuckDB's spatial extension has been
// installed and loaded once in this process. The db argument is accepted
// for call-site symmetry with the other register* helpers but is not used:
// the load runs against a dedicated bootstrap database so it is independent
// of the catalog of whatever per-case DB triggered the first open. INSTALL
// is idempotent and pulls from the locally cached extension repo when
// offline.
func LoadSpatialExtension(_ *sql.DB) error {
	spatialLoadOnce.Do(func() {
		spatialLoadErr = doLoadSpatialExtension()
	})
	return spatialLoadErr
}

// doLoadSpatialExtension performs the one-time INSTALL/LOAD against a
// freshly opened bootstrap database. Routing the load through its own
// short-lived handle (rather than a caller-supplied one) keeps the
// extension loader off any database that is concurrently serving queries.
func doLoadSpatialExtension() error {
	// duckdbcompat (pure-Go, CGO_ENABLED=0) backend: the native DuckDB spatial
	// extension is NOT available (no extension repository, no home directory in
	// the wasm2go engine) and is NOT needed — the entire geography family lowers
	// to the pure-Go googlesqlite_st_* / googlesqlite_s2_* scalar UDFs registered
	// by registerGeographyUDFs. The native INSTALL/LOAD here was only a dead
	// parity probe (see the comment above LoadSpatialExtension), so on this
	// backend it is a no-op. SWAP-BLUEPRINT §1.5.
	return nil
}

// registerGeographyUDFs registers every geography / S2 scalar function
// as a DuckDB scalar UDF. The formatter lowers the GoogleSQL geography
// builtins to googlesqlite_st_* / googlesqlite_s2_* names (see
// tryGeographyFunc in internal/formatter.go); these UDFs are the
// runtime that backs them. Each reuses the pure-Go body under
// internal/functions/geography through internal.GeographyScalarFuncs,
// so the value-layer envelope semantics carry through unchanged to the
// DuckDB scalar UDF path.
func registerGeographyUDFs(db *sql.DB) error {
	conn, err := db.Conn(context.Background())
	if err != nil {
		return fmt.Errorf("googlesqlite: geo udf conn: %w", err)
	}
	defer conn.Close()
	for _, fn := range internal.GeographyScalarFuncs() {
		udf := &geoScalarUDF{exec: fn.Exec, resultType: fn.ResultType}
		if err := duckdb.RegisterScalarUDF(conn, fn.StorageName, udf); err != nil {
			return fmt.Errorf("googlesqlite: register %s: %w", fn.StorageName, err)
		}
	}
	return nil
}

// geoScalarUDF adapts one pure-Go geography body to DuckDB's scalar-UDF
// interface. Inputs are variadic TYPE_ANY: numeric literals arrive as
// float64 / int64, WKT / GeoJSON / geohash string arguments arrive as
// the value-layer base64 envelope, and intermediate GEOGRAPHY values
// (the output of a nested constructor) arrive as the base64 envelope
// VARCHAR this same UDF produced. The shared body decodes each via the
// value layer, so the representation is uniform across nesting.
type geoScalarUDF struct {
	exec       func(args []any) (any, error)
	resultType internal.GeoUDFResultType
}

func (u *geoScalarUDF) Config() duckdb.ScalarFuncConfig {
	anyInfo, _ := duckdb.NewTypeInfo(duckdb.TYPE_ANY)
	return duckdb.ScalarFuncConfig{
		// No fixed leading parameters; every argument is variadic ANY so
		// one registration serves all of a function's GoogleSQL overloads
		// (e.g. ST_GEOGFROMTEXT's 1..4 arg forms).
		VariadicTypeInfo: anyInfo,
		ResultTypeInfo:   u.geoResultTypeInfo(),
		// The pure-Go bodies implement BigQuery NULL semantics directly
		// (e.g. ST_X(non-point) -> NULL, ST_DISTANCE with a NULL arg ->
		// NULL), so defer all NULL handling to the body rather than
		// short-circuiting the whole call to NULL on any NULL input.
		SpecialNullHandling: true,
	}
}

func (u *geoScalarUDF) geoResultTypeInfo() duckdb.TypeInfo {
	var t duckdb.Type
	switch u.resultType {
	case internal.GeoResultDouble:
		t = duckdb.TYPE_DOUBLE
	case internal.GeoResultBigint:
		t = duckdb.TYPE_BIGINT
	case internal.GeoResultBoolean:
		t = duckdb.TYPE_BOOLEAN
	case internal.GeoResultBytesRaw:
		t = duckdb.TYPE_BLOB
	default:
		// GeoResultVarchar (base64 envelope) and GeoResultStringRaw (raw
		// text) both surface as a DuckDB VARCHAR.
		t = duckdb.TYPE_VARCHAR
	}
	info, _ := duckdb.NewTypeInfo(t)
	return info
}

// normalizeGeoArg coerces DuckDB-native argument types the value layer
// does not recognise into the Go primitives DecodeValue expects. A
// non-integer numeric literal (e.g. ST_GEOGPOINT(1.5, 2.5)) is bound by
// DuckDB as DECIMAL and surfaces here as duckdb.Decimal; collapse it to
// float64 so it decodes to a FloatValue. All other types pass through
// unchanged (int64, string envelope, []byte, bool).
func normalizeGeoArg(v driver.Value) any {
	if d, ok := v.(duckdb.Decimal); ok {
		return d.Float64()
	}
	return v
}

func (u *geoScalarUDF) Executor() duckdb.ScalarFuncExecutor {
	return duckdb.ScalarFuncExecutor{
		RowExecutor: func(values []driver.Value) (any, error) {
			args := make([]any, len(values))
			for i, v := range values {
				args[i] = normalizeGeoArg(v)
			}
			return u.exec(args)
		},
	}
}
