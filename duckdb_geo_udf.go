package googlesqlite

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"sync"

	duckdb "github.com/duckdb/duckdb-go/v2"

	internal "github.com/goccy/googlesqlite/internal"
)

// spatialLoadMu serializes the spatial-extension INSTALL/LOAD across all
// DuckDB databases opened in the process. registerDuckDBGeography runs on
// every DB open (see driver.go), so the conformance harness drives
// hundreds of concurrent first-loads of the extension; the DuckDB C++
// extension loader is not safe under that concurrency and aborts the
// process. Holding this mutex around the INSTALL/LOAD pair makes each load
// observe a quiescent loader while keeping the extension present in every
// database (geography behaviour is unchanged).
var spatialLoadMu sync.Mutex

// loadSpatialExtension installs and loads DuckDB's spatial extension on
// the connection. The geography family lowers to a mix of pure-Go
// googlesqlite_st_* UDFs (registered below) and, for the parity-check
// path, native DuckDB ST_* spatial functions; the latter require the
// extension to be present in the catalog. INSTALL is idempotent and
// pulls from the locally cached extension repo when offline.
func loadSpatialExtension(db *sql.DB) error {
	conn, err := db.Conn(context.Background())
	if err != nil {
		return fmt.Errorf("googlesqlite: spatial conn: %w", err)
	}
	defer conn.Close()
	spatialLoadMu.Lock()
	defer spatialLoadMu.Unlock()
	for _, stmt := range []string{"INSTALL spatial", "LOAD spatial"} {
		if _, err := conn.ExecContext(context.Background(), stmt); err != nil {
			return fmt.Errorf("googlesqlite: %q: %w", stmt, err)
		}
	}
	return nil
}

// registerGeographyUDFs registers every geography / S2 scalar function
// as a DuckDB scalar UDF. The formatter lowers the GoogleSQL geography
// builtins to googlesqlite_st_* / googlesqlite_s2_* names (see
// tryGeographyFunc in internal/formatter.go); these UDFs are the
// runtime that backs them. Each reuses the pure-Go body under
// internal/functions/geography through internal.GeographyScalarFuncs,
// so the value-layer envelope semantics are identical to the SQLite
// path that originally satisfied geography conformance.
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
