//go:build !cgo

package googlesqlite

// This file is the pure-Go (CGO_ENABLED=0) counterpart of duckdb_aggregate_udf.go
// (which is tagged //go:build cgo). The cgo file reaches past duckdb-go straight
// to the C aggregate-function API (duckdb_create_aggregate_function et al.) and
// claws the raw duckdb_connection out of duckdb-go's unexported fields via
// reflect+unsafe — none of which is available in the pure-Go (wasm2go) DuckDB
// backend.
//
// For the T0/T1 boot on the duckdbcompat engine, aggregate / window UDFs are
// intentionally NOT registered: registerDuckDBAggregates is a no-op returning nil
// so that driver.go's connector path (driver.go:208,297) wires up cleanly and the
// emulator boots. Queries that bind no aggregate UDF (SELECT 1, scalar
// string/math BigQuery functions, CREATE/INSERT/COUNT) run unaffected. The full
// pure-Go aggregate rewrite — porting duckdbAggSpecs() onto our engine's
// RegisterAggregateUDF — is the T2 work item (SWAP-BLUEPRINT §1.3, §3 T2).

import "database/sql"

// registerDuckDBAggregates is the pure-Go no-op stub matching the cgo file's
// signature exactly. It registers no aggregate functions and never errors.
func registerDuckDBAggregates(db *sql.DB) error {
	_ = db
	return nil
}
