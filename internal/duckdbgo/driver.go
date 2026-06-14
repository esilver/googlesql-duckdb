package duckdbgo

import (
	"database/sql/driver"

	convergeduckdb "github.com/esilver/duckdb-go-pure"
)

// Driver is the compat mirror of duckdb-go's duckdb.Driver. It is the only direct
// data-path API the emulator calls (internalDuckDBDriver.Open forwards through
// `duckdb.Driver{}.Open(duckdbDSN(name))`). Open delegates straight to our pure-Go
// engine driver, so the DSN, connection-sharing, and database/sql plumbing are all
// the engine's (which already shares one in-memory database across every
// connection minted from a connector, matching the emulator's assumption).
//
// The empty struct carries no state and is constructed by value (duckdb.Driver{}),
// matching duckdb-go.
type Driver struct{}

// Compile-time checks that Driver satisfies driver.Driver and
// driver.DriverContext (the latter is what lets database/sql share ONE
// engine+database across every pooled connection of a sql.DB).
var (
	_ driver.Driver        = Driver{}
	_ driver.DriverContext = Driver{}
)

// Open implements driver.Driver by forwarding to the engine driver's Open. An
// empty dsn is handled by the engine (it maps "" to an in-memory database).
//
// NOTE: the engine's raw Open mints a brand-new transpiled DuckDB engine
// instance per call. Pooled use must go through OpenConnector instead, which
// shares one engine across the pool. database/sql prefers OpenConnector
// automatically because Driver implements driver.DriverContext.
func (Driver) Open(dsn string) (driver.Conn, error) {
	return (&convergeduckdb.Driver{}).Open(dsn)
}

// OpenConnector implements driver.DriverContext by forwarding to the engine
// driver's connector, which lazily creates ONE shared engine + database and
// hands every Connect call a fresh duckdb_connection against it. This is the
// scope database/sql pools on: without it, dsnConnector falls back to calling
// Open once per pooled connection, and each pooled conn would be a separate
// engine — only the first of which has the googlesqlite_* UDFs registered.
func (Driver) OpenConnector(dsn string) (driver.Connector, error) {
	return (&convergeduckdb.Driver{}).OpenConnector(dsn)
}
