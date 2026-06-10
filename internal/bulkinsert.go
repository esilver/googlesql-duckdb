// bulkinsert.go — the DuckDB-Appender bulk-load seam (duckdb-go-pure#1 lever 2).
//
// Per-row parameterized INSERT through the dialect layer costs ~1.5 ms/row:
// every stmt.Exec re-enters the wazero-interpreted analyzer module to encode
// arguments, then the engine executes one statement per row. The engine's
// Appender collapses a batch to ONE statement (INSERT .. SELECT over a Go feed
// table function) at ~0.003 ms/row. BulkInsert routes a plain single-table
// VALUES-shaped load around the analyzer/formatter entirely:
//
//	catalog spec (plain Go) -> per-kind native encode -> duckdb.NewAppender
//
// The fast path is restricted to column sets whose googlesqlite STORAGE
// representation we can produce without the analyzer:
//
//	INT32/INT64/UINT32/UINT64 -> native int64   (INT column)
//	BOOL                      -> native bool    (BOOLEAN column)
//	FLOAT/DOUBLE              -> native float64 (FLOAT/DOUBLE column)
//	STRING                    -> the value-layer base64 envelope (TEXT column;
//	                             this is byte-identical to what the slow path
//	                             stores, so WHERE/ORDER BY/UDF semantics match)
//	DATE                      -> native time.Time (DATE column)
//	DATETIME                  -> native time.Time (TIMESTAMP column)
//	TIMESTAMP                 -> native time.Time (TIMESTAMPTZ column)
//
// Everything else (ARRAY/STRUCT/NUMERIC/JSON/BYTES/TIME/GEOGRAPHY/INTERVAL/
// RANGE/PROTO/...) raises ErrBulkInsertUnsupported so the caller can fall back
// to the ordinary per-row INSERT path. Note TIMESTAMP/DATE/DATETIME natively
// improve on the per-row path, which cannot bind a time.Time parameter at all
// (the value-layer envelope reaches the TIMESTAMPTZ column and fails DuckDB's
// VARCHAR cast).
package internal

import (
	"context"
	"errors"
	"fmt"
	"strings"

	duckdb "github.com/esilver/duckdb-go-pure"
	googlesql "github.com/goccy/go-googlesql"

	"github.com/goccy/googlesqlite/internal/value"
)

// ErrBulkInsertUnsupported reports that the target table or column set falls
// outside the bulk-insert fast path. Callers should treat it as "use the
// ordinary INSERT path", not as a failure of the data.
var ErrBulkInsertUnsupported = errors.New("googlesqlite: bulk insert: shape not supported by the fast path")

// TableSpecByName returns the catalog's TableSpec under its formatted name
// (NamePath joined with "_" — the same key addTableSpec uses), or nil.
func (c *Catalog) TableSpecByName(name string) *TableSpec {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.tableMap[name]
}

// BulkInsert appends rows into the table at namePath through the engine's
// Appender, bypassing per-row analyze/format/exec. columns names the targeted
// table columns (a subset, in the caller's row order); rows carry one value
// per targeted column. Omitted columns are stored as NULL — tables whose
// omitted columns carry a DEFAULT fall back (the slow path splices defaults,
// so silently writing NULL would diverge).
//
// All rows are validated and encoded BEFORE any engine work, so an encoding
// error costs nothing engine-side; the flush itself is one INSERT..SELECT
// statement, which DuckDB applies atomically (all rows or none). If a
// transaction is open on conn, the insert joins it.
func BulkInsert(ctx context.Context, conn *Conn, catalog *Catalog, namePath []string, columns []string, rows [][]any) (int64, error) {
	if err := catalog.Sync(ctx, conn); err != nil {
		return 0, fmt.Errorf("googlesqlite: bulk insert: catalog sync: %w", err)
	}
	tableName := formatPath(namePath)
	spec := catalog.TableSpecByName(tableName)
	if spec == nil {
		return 0, fmt.Errorf("googlesqlite: bulk insert: table %q not found", strings.Join(namePath, "."))
	}
	if spec.IsView || spec.Query != "" {
		return 0, fmt.Errorf("%w: %q is not a plain table", ErrBulkInsertUnsupported, tableName)
	}

	// Map targeted columns onto table positions (table order == spec.Columns
	// order == the CREATE TABLE order the appender introspects).
	pos := make(map[string]int, len(spec.Columns))
	for i, cs := range spec.Columns {
		pos[cs.Name] = i
	}
	target := make([]int, len(columns))
	targeted := make([]bool, len(spec.Columns))
	for i, name := range columns {
		p, ok := pos[name]
		if !ok {
			return 0, fmt.Errorf("googlesqlite: bulk insert: column %q not in table %q", name, tableName)
		}
		if targeted[p] {
			return 0, fmt.Errorf("googlesqlite: bulk insert: column %q targeted twice", name)
		}
		target[i] = p
		targeted[p] = true
	}
	for i, cs := range spec.Columns {
		kind := googlesql.TypeKind(cs.Type.Kind)
		if targeted[i] {
			if !bulkAppendableKind(kind) {
				return 0, fmt.Errorf("%w: column %q has type %s", ErrBulkInsertUnsupported, cs.Name, typeKindToSQLName(kind))
			}
			continue
		}
		if cs.DefaultExpr != "" {
			// The per-row path substitutes the DEFAULT for omitted columns.
			return 0, fmt.Errorf("%w: omitted column %q has a DEFAULT", ErrBulkInsertUnsupported, cs.Name)
		}
		if !bulkStorableNullKind(kind) {
			// The appender must emit a NULL of the column's storage type;
			// TIME/JSON storage types are outside its writable set.
			return 0, fmt.Errorf("%w: omitted column %q has storage type %s", ErrBulkInsertUnsupported, cs.Name, typeKindToSQLName(kind))
		}
	}

	// Encode everything up front: pure Go, no engine entry, clean abort.
	encoded := make([][]any, len(rows))
	for ri, row := range rows {
		if len(row) != len(columns) {
			return 0, fmt.Errorf("googlesqlite: bulk insert: row %d has %d values, want %d", ri, len(row), len(columns))
		}
		vals := make([]any, len(spec.Columns)) // omitted columns stay nil (NULL)
		for ci, v := range row {
			cell, err := bulkEncodeCell(googlesql.TypeKind(spec.Columns[target[ci]].Type.Kind), v)
			if err != nil {
				return 0, fmt.Errorf("googlesqlite: bulk insert: row %d, column %q: %w", ri, columns[ci], err)
			}
			vals[target[ci]] = cell
		}
		encoded[ri] = vals
	}

	ap, err := duckdb.NewAppender(conn.conn, "", tableName)
	if err != nil {
		if strings.Contains(err.Error(), "unsupported type") {
			return 0, fmt.Errorf("%w: %v", ErrBulkInsertUnsupported, err)
		}
		return 0, fmt.Errorf("googlesqlite: bulk insert: %w", err)
	}
	for ri, vals := range encoded {
		if err := ap.AppendRow(vals...); err != nil {
			// Abandon without Close: Close would flush the rows buffered so
			// far, turning an error into a partial insert. The appender's
			// feed function stays registered but inert.
			return 0, fmt.Errorf("googlesqlite: bulk insert: row %d: %w", ri, err)
		}
	}
	if err := ap.Close(); err != nil {
		return 0, fmt.Errorf("googlesqlite: bulk insert: flush: %w", err)
	}
	return int64(len(rows)), nil
}

// bulkAppendableKind reports whether a targeted column of this GoogleSQL kind
// is on the bulk fast path (we can produce its exact storage representation).
func bulkAppendableKind(kind googlesql.TypeKind) bool {
	switch kind {
	case googlesql.TypeKindTypeInt32, googlesql.TypeKindTypeInt64,
		googlesql.TypeKindTypeUint32, googlesql.TypeKindTypeUint64,
		googlesql.TypeKindTypeBool,
		googlesql.TypeKindTypeFloat, googlesql.TypeKindTypeDouble,
		googlesql.TypeKindTypeString,
		googlesql.TypeKindTypeDate, googlesql.TypeKindTypeDatetime, googlesql.TypeKindTypeTimestamp:
		return true
	}
	return false
}

// bulkStorableNullKind reports whether an OMITTED column's storage type can
// take an appender NULL: every kind whose DuckDB column type is in the
// appender's writable set (TEXT-stored envelope kinds included). TIME and
// JSON storage types are not.
func bulkStorableNullKind(kind googlesql.TypeKind) bool {
	if bulkAppendableKind(kind) {
		return true
	}
	switch kind {
	case googlesql.TypeKindTypeTime, googlesql.TypeKindTypeJson:
		return false
	case googlesql.TypeKindTypeBytes: // BLOB column, appendable type
		return true
	}
	// ARRAY/STRUCT/PROTO/GEOGRAPHY/NUMERIC/BIGNUMERIC/INTERVAL/RANGE/EXTENDED
	// all store as TEXT (see ColumnSpec.SQLiteSchema), which the appender can
	// write a NULL into. ENUM stores as INT.
	return true
}

// bulkEncodeCell converts one caller value into the exact representation the
// slow INSERT path stores for a column of this kind. The coercions reuse the
// value layer (pure Go — no analyzer-module entry): any Go integer kind into
// INT64, strings parse into temporal kinds, etc.
func bulkEncodeCell(kind googlesql.TypeKind, v any) (any, error) {
	if v == nil {
		return nil, nil
	}
	val, err := value.ValueFromGoValue(v)
	if err != nil {
		return nil, err
	}
	if val == nil { // typed nil and friends
		return nil, nil
	}
	switch kind {
	case googlesql.TypeKindTypeInt32, googlesql.TypeKindTypeInt64,
		googlesql.TypeKindTypeUint32, googlesql.TypeKindTypeUint64:
		return val.ToInt64()
	case googlesql.TypeKindTypeBool:
		return val.ToBool()
	case googlesql.TypeKindTypeFloat, googlesql.TypeKindTypeDouble:
		return val.ToFloat64()
	case googlesql.TypeKindTypeString:
		s, err := val.ToString()
		if err != nil {
			return nil, err
		}
		// The storage form of a STRING cell is the value-layer envelope
		// (EncodeValue of a StringValue), NOT the raw string — comparisons,
		// ORDER BY and the googlesqlite_* UDFs all assume it.
		return value.EncodeValue(value.StringValue(s))
	case googlesql.TypeKindTypeDate, googlesql.TypeKindTypeDatetime:
		t, err := val.ToTime()
		if err != nil {
			return nil, err
		}
		return t, nil
	case googlesql.TypeKindTypeTimestamp:
		t, err := val.ToTime()
		if err != nil {
			return nil, err
		}
		// GoogleSQL TIMESTAMP is an absolute instant; normalize to UTC so the
		// TIMESTAMPTZ cell matches the literal path's `... UTC ... +00` form.
		return t.UTC(), nil
	}
	return nil, fmt.Errorf("%w: type %s", ErrBulkInsertUnsupported, typeKindToSQLName(kind))
}
