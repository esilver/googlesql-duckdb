// bulkinsert.go — public bulk-load API over the engine Appender.
//
// This is the seam a consumer's insertAll-style repository calls instead of
// "Prepare INSERT VALUES (?,...) + Exec per row": one call per batch, one
// engine statement per batch, no per-row analyzer-module entry. See
// internal/bulkinsert.go for the fast path's exact storage contract and
// duckdb-go-pure#1 for the measurements that motivated it.
//
// bigquery-emulator wiring sketch (internal/contentdata/repository.go,
// Repository.AddTableData): the emulator already reaches the driver conn with
// t.conn.Conn.Raw for SetNamePath, so:
//
//	err := tx.Conn().Raw(func(dc interface{}) error {
//	    gsqlConn, ok := dc.(*googlesqlite.Conn)
//	    if !ok { return fmt.Errorf("not a googlesqlite conn: %T", dc) }
//	    _, err := gsqlConn.BulkInsert(ctx,
//	        []string{projectID, datasetID, table.ID}, // full name path
//	        colNames,  // table.Columns names (complex types included, #6)
//	        rowValues, // same per-column values it binds today
//	    )
//	    return err
//	})
//	if errors.Is(err, googlesqlite.ErrBulkInsertUnsupported) {
//	    ... existing per-row Prepare/Exec loop ...
//	}
//
// The insert joins the transaction already open on that connection.
package googlesqlite

import (
	"context"
	"database/sql"
	"fmt"

	internal "github.com/goccy/googlesqlite/internal"
)

// ErrBulkInsertUnsupported is returned (wrapped) when the target table or
// column set is outside the bulk fast path. Treat it as "fall back to the
// ordinary INSERT path"; test with errors.Is.
var ErrBulkInsertUnsupported = internal.ErrBulkInsertUnsupported

// BulkInsertUnsupportedEvent describes one ErrBulkInsertUnsupported return:
// the table, the gating column (empty for whole-table-shape rejections), the
// column's GoogleSQL type name, and a human-readable reason.
type BulkInsertUnsupportedEvent = internal.BulkInsertUnsupportedEvent

// BulkInsertUnsupportedCount returns how many times BulkInsert has returned
// ErrBulkInsertUnsupported since process start (monotonic, process-wide).
// Emulator-style callers that fall back to the per-row INSERT path can sample
// it to report whether real workloads miss the fast path at scale.
func BulkInsertUnsupportedCount() uint64 { return internal.BulkInsertUnsupportedCount() }

// SetBulkInsertUnsupportedHook installs fn as the process-wide observer
// called synchronously with one event per ErrBulkInsertUnsupported return
// (e.g. to emit a debug log line with the gating column and type). nil
// uninstalls. The hook must be cheap and must not call BulkInsert.
func SetBulkInsertUnsupportedHook(fn func(BulkInsertUnsupportedEvent)) {
	internal.SetBulkInsertUnsupportedHook(fn)
}

// BulkInsert loads rows into the table named by the full tablePath (e.g.
// {project, dataset, table}) through the engine's bulk Appender, bypassing
// per-row SQL execution. columns lists the targeted table columns in the
// callers' row order; each row carries one Go value per targeted column (nil
// means NULL). Omitted table columns are stored as NULL.
//
// Values take the value layer's usual coercions (any Go integer kind into
// INT64, time.Time or a parseable string into TIMESTAMP/DATE/DATETIME, ...)
// and are stored in exactly the representation the per-row INSERT path
// produces, so they round-trip through googlesqlite queries identically.
//
// The batch is applied as one atomic statement: on error no rows are
// inserted. If a transaction is open on the connection, the batch joins it.
func (c *Conn) BulkInsert(ctx context.Context, tablePath []string, columns []string, rows [][]any) (int64, error) {
	conn := internal.NewConnWithOptions(c.conn, c.tx, c.systemVars, c.scriptVars, c.materializeCTE)
	return internal.BulkInsert(ctx, conn, c.catalog, tablePath, columns, rows)
}

// BulkInsert is the *sql.Conn convenience wrapper around Conn.BulkInsert for
// callers holding a database/sql connection on the "googlesqlite" driver.
func BulkInsert(ctx context.Context, conn *sql.Conn, tablePath []string, columns []string, rows [][]any) (int64, error) {
	var n int64
	err := conn.Raw(func(dc any) error {
		gc, ok := dc.(*Conn)
		if !ok {
			return fmt.Errorf("googlesqlite: BulkInsert: driver connection is %T, not *googlesqlite.Conn", dc)
		}
		var err error
		n, err = gc.BulkInsert(ctx, tablePath, columns, rows)
		return err
	})
	return n, err
}
