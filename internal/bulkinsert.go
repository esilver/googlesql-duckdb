// bulkinsert.go — the DuckDB-Appender bulk-load seam (duckdb-go-pure#1 lever 2,
// extended to complex column types by googlesqlite#6).
//
// Per-row parameterized INSERT through the dialect layer costs ~0.24 ms/row:
// every stmt.Exec re-enters the wazero-interpreted analyzer module to encode
// arguments, then the engine executes one statement per row. The engine's
// Appender collapses a batch to ONE statement (INSERT .. SELECT over a Go feed
// table function) at ~0.0005 ms/row. BulkInsert routes a plain single-table
// VALUES-shaped load around the analyzer/formatter entirely:
//
//	catalog spec (plain Go) -> per-kind native encode -> duckdb.NewAppender
//
// Column kinds split into three classes (bulkColumnClassOf):
//
// DIRECT — the googlesqlite STORAGE representation is produced in pure Go and
// appended straight into the target table:
//
//	INT32/INT64/UINT32/UINT64 -> native int64   (BIGINT column)
//	BOOL                      -> native bool    (BOOLEAN column)
//	FLOAT/DOUBLE              -> native float64 (FLOAT/DOUBLE column)
//	STRING                    -> the value-layer base64 envelope (TEXT column;
//	                             byte-identical to what the per-row path stores,
//	                             so WHERE/ORDER BY/UDF semantics match)
//	DATE                      -> native time.Time (DATE column)
//	DATETIME                  -> native time.Time (TIMESTAMP column)
//	TIMESTAMP                 -> native time.Time (TIMESTAMPTZ column)
//	NUMERIC/BIGNUMERIC        -> the value-layer envelope (TEXT column; the
//	                             per-row path binds the same envelope VARCHAR)
//	BYTES                     -> the value-layer envelope's bytes (BLOB column;
//	                             the per-row path binds the envelope VARCHAR and
//	                             DuckDB's VARCHAR->BLOB cast stores its bytes)
//	ARRAY/STRUCT              -> the value-layer envelope (TEXT column; same
//	                             encoder the per-row encode layer uses)
//
// CAST — the column's STORAGE type is engine-native and outside the appender's
// writable set, so the feed produces the canonical VARCHAR the per-row path
// binds and the flush applies the same engine cast the per-row INSERT applies
// implicitly (issue #6 "cast-in-flush"):
//
//	TIME -> "15:04:05.999999" text, CAST(c AS TIME)   (TIME column)
//	JSON -> raw JSON text,         CAST(c AS JSON)    (JSON column)
//
// Because duckdb.NewAppender rejects any table containing a TIME/JSON column,
// tables with CAST-class columns load through a private TEMP staging table
// (VARCHAR for the cast columns, storage types elsewhere) and one
// INSERT INTO target SELECT ..casts.. FROM stage statement. That keeps the
// bulk economics (O(1) statements per batch — create/append/insert/drop — and
// the casts are vectorized) while the stored cells stay byte-identical to the
// per-row path's. NOTE the documented dead end: batching the fallback as one
// multi-row INSERT..VALUES is 3x WORSE than per-row exec (analysis cost scales
// with statement size; measured in duckdb-go-pure#1) — do not "optimize" this
// seam that way.
//
// NULL-ONLY — everything else (GEOGRAPHY/INTERVAL/RANGE/PROTO/ENUM/...) stays
// on the fast path only while every batch value for the column is NULL (NULL
// writes into any appendable vector); the first non-NULL value raises
// ErrBulkInsertUnsupported so the caller falls back to the per-row INSERT
// path. Every Unsupported return is instrumented (BulkInsertUnsupportedCount /
// SetBulkInsertUnsupportedHook) so an emulator-style caller can report whether
// real workloads hit the fallback at scale.
package internal

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"

	duckdb "github.com/esilver/duckdb-go-pure"
	googlesql "github.com/goccy/go-googlesql"

	"github.com/goccy/googlesqlite/internal/value"
)

// ErrBulkInsertUnsupported reports that the target table or column set falls
// outside the bulk-insert fast path. Callers should treat it as "use the
// ordinary INSERT path", not as a failure of the data.
var ErrBulkInsertUnsupported = errors.New("googlesqlite: bulk insert: shape not supported by the fast path")

// BulkInsertUnsupportedEvent describes one ErrBulkInsertUnsupported return:
// which table, which column gated (empty for whole-table-shape rejections),
// the column's GoogleSQL type name, and a human-readable reason. Emitted to
// the hook installed with SetBulkInsertUnsupportedHook.
type BulkInsertUnsupportedEvent struct {
	Table  string
	Column string
	Type   string
	Reason string
}

var (
	bulkInsertUnsupportedCount atomic.Uint64
	bulkInsertUnsupportedHook  atomic.Pointer[func(BulkInsertUnsupportedEvent)]
)

// BulkInsertUnsupportedCount returns the number of times BulkInsert returned
// ErrBulkInsertUnsupported since process start (monotonic, process-wide).
// Callers that fall back to the per-row INSERT path can sample it to report
// how often real workloads miss the fast path.
func BulkInsertUnsupportedCount() uint64 { return bulkInsertUnsupportedCount.Load() }

// SetBulkInsertUnsupportedHook installs fn as the process-wide observer
// called (synchronously) with one event per ErrBulkInsertUnsupported return.
// nil uninstalls. The hook must be cheap and must not call BulkInsert.
func SetBulkInsertUnsupportedHook(fn func(BulkInsertUnsupportedEvent)) {
	if fn == nil {
		bulkInsertUnsupportedHook.Store(nil)
		return
	}
	bulkInsertUnsupportedHook.Store(&fn)
}

// bulkUnsupported counts + publishes one fast-path rejection and returns the
// wrapped ErrBulkInsertUnsupported carrying the same context.
func bulkUnsupported(ev BulkInsertUnsupportedEvent) error {
	bulkInsertUnsupportedCount.Add(1)
	if fn := bulkInsertUnsupportedHook.Load(); fn != nil {
		(*fn)(ev)
	}
	msg := ev.Reason
	if ev.Column != "" {
		if ev.Type != "" {
			msg = fmt.Sprintf("column %q (type %s): %s", ev.Column, ev.Type, ev.Reason)
		} else {
			msg = fmt.Sprintf("column %q: %s", ev.Column, ev.Reason)
		}
	}
	return fmt.Errorf("%w: table %q: %s", ErrBulkInsertUnsupported, ev.Table, msg)
}

// TableSpecByName returns the catalog's TableSpec under its formatted name
// (NamePath joined with "_" — the same key addTableSpec uses), or nil.
func (c *Catalog) TableSpecByName(name string) *TableSpec {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.tableMap[name]
}

// bulkColumnClass partitions GoogleSQL column kinds by how the bulk fast path
// can serve them; see the package comment.
type bulkColumnClass int

const (
	bulkClassDirect   bulkColumnClass = iota // exact storage value produced in Go, appender-writable
	bulkClassCast                            // canonical VARCHAR feed + engine cast in the flush (TIME/JSON)
	bulkClassNullOnly                        // fast path only while every batch value is NULL
)

func bulkColumnClassOf(kind googlesql.TypeKind) bulkColumnClass {
	switch kind {
	case googlesql.TypeKindTypeInt32, googlesql.TypeKindTypeInt64,
		googlesql.TypeKindTypeUint32, googlesql.TypeKindTypeUint64,
		googlesql.TypeKindTypeBool,
		googlesql.TypeKindTypeFloat, googlesql.TypeKindTypeDouble,
		googlesql.TypeKindTypeString,
		googlesql.TypeKindTypeDate, googlesql.TypeKindTypeDatetime, googlesql.TypeKindTypeTimestamp,
		googlesql.TypeKindTypeNumeric, googlesql.TypeKindTypeBignumeric,
		googlesql.TypeKindTypeBytes,
		googlesql.TypeKindTypeArray, googlesql.TypeKindTypeStruct:
		return bulkClassDirect
	case googlesql.TypeKindTypeTime, googlesql.TypeKindTypeJson:
		return bulkClassCast
	}
	return bulkClassNullOnly
}

// bulkStageSeq numbers the per-call TEMP staging tables (collision-free names;
// the table is dropped at the end of the call either way).
var bulkStageSeq atomic.Uint64

// BulkInsert appends rows into the table at namePath through the engine's
// Appender, bypassing per-row analyze/format/exec. columns names the targeted
// table columns (a subset, in the caller's row order); rows carry one value
// per targeted column. Omitted columns are stored as NULL — tables whose
// omitted columns carry a DEFAULT fall back (the slow path splices defaults,
// so silently writing NULL would diverge).
//
// All rows are validated and encoded BEFORE any engine work, so an encoding
// error costs nothing engine-side; the flush itself applies all rows through
// one INSERT..SELECT statement, which DuckDB applies atomically (all rows or
// none). If a transaction is open on conn, the insert joins it.
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
		return 0, bulkUnsupported(BulkInsertUnsupportedEvent{Table: tableName, Reason: "not a plain table"})
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

	// Type gate (the fast pre-check): classify every column once. CAST-class
	// storage types (TIME/JSON) force the staging flush for the whole table —
	// duckdb.NewAppender rejects any table containing such a column, targeted
	// or not. NULL-ONLY columns are confirmed value-by-value below.
	classes := make([]bulkColumnClass, len(spec.Columns))
	needStage := false
	for i, cs := range spec.Columns {
		kind := googlesql.TypeKind(cs.Type.Kind)
		classes[i] = bulkColumnClassOf(kind)
		if classes[i] == bulkClassCast {
			needStage = true
		}
		if !targeted[i] && cs.DefaultExpr != "" {
			// The per-row path substitutes the DEFAULT for omitted columns.
			return 0, bulkUnsupported(BulkInsertUnsupportedEvent{
				Table: tableName, Column: cs.Name, Type: typeKindToSQLName(kind),
				Reason: "omitted column has a DEFAULT",
			})
		}
	}

	// Encode everything up front: pure Go, no engine entry, clean abort. The
	// value-aware gate for NULL-ONLY columns runs here too: a complex-typed
	// column whose batch values are all NULL stays on the fast path (NULL
	// writes into any vector); the first non-NULL value falls back.
	encoded := make([][]any, len(rows))
	for ri, row := range rows {
		if len(row) != len(columns) {
			return 0, fmt.Errorf("googlesqlite: bulk insert: row %d has %d values, want %d", ri, len(row), len(columns))
		}
		vals := make([]any, len(spec.Columns)) // omitted columns stay nil (NULL)
		for ci, v := range row {
			p := target[ci]
			cs := spec.Columns[p]
			if classes[p] == bulkClassNullOnly {
				if !bulkIsNullValue(v) {
					return 0, bulkUnsupported(BulkInsertUnsupportedEvent{
						Table: tableName, Column: cs.Name,
						Type:   typeKindToSQLName(googlesql.TypeKind(cs.Type.Kind)),
						Reason: fmt.Sprintf("non-NULL value at row %d", ri),
					})
				}
				continue // vals[p] stays nil
			}
			cell, err := bulkEncodeCell(cs.Type, v)
			if err != nil {
				return 0, fmt.Errorf("googlesqlite: bulk insert: row %d, column %q: %w", ri, columns[ci], err)
			}
			vals[p] = cell
		}
		encoded[ri] = vals
	}

	if needStage {
		return bulkFlushStaged(ctx, conn, tableName, spec, classes, encoded)
	}
	return bulkFlushDirect(conn, tableName, encoded)
}

// bulkFlushDirect appends the encoded rows straight into the target table —
// every column type is in the appender's writable set and the encoded cells
// already carry the exact storage representation.
func bulkFlushDirect(conn *Conn, tableName string, encoded [][]any) (int64, error) {
	ap, err := duckdb.NewAppender(conn.conn, "", tableName)
	if err != nil {
		if strings.Contains(err.Error(), "unsupported type") {
			return 0, bulkUnsupported(BulkInsertUnsupportedEvent{Table: tableName, Reason: err.Error()})
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
	return int64(len(encoded)), nil
}

// bulkFlushStaged is the cast-in-flush path for tables containing TIME/JSON
// storage columns (which duckdb.NewAppender cannot target): append the
// encoded rows into a private TEMP staging table whose cast-class columns are
// VARCHAR, then apply the batch with ONE
//
//	INSERT INTO target SELECT c0, CAST(c1 AS JSON), ... FROM stage
//
// statement — the same vectorized engine cast the per-row INSERT path invokes
// implicitly when binding its canonical VARCHAR parameter, so the stored cells
// are byte-identical. O(1) statements per batch; the INSERT..SELECT applies
// atomically and the target is untouched on any earlier error.
func bulkFlushStaged(ctx context.Context, conn *Conn, tableName string, spec *TableSpec, classes []bulkColumnClass, encoded [][]any) (int64, error) {
	if len(encoded) == 0 {
		return 0, nil
	}
	stage := fmt.Sprintf("__gsx_bulk_stage_%d", bulkStageSeq.Add(1))

	colDefs := make([]string, len(spec.Columns))
	selExprs := make([]string, len(spec.Columns))
	for i, cs := range spec.Columns {
		kind := googlesql.TypeKind(cs.Type.Kind)
		colType := storageTypeForKind(kind)
		ref := fmt.Sprintf(`"c%d"`, i)
		selExprs[i] = ref
		if classes[i] == bulkClassCast {
			colType = "VARCHAR"
			selExprs[i] = fmt.Sprintf("CAST(%s AS %s)", ref, storageTypeForKind(kind))
		}
		colDefs[i] = fmt.Sprintf(`"c%d" %s`, i, colType)
	}
	if _, err := conn.conn.ExecContext(ctx, fmt.Sprintf(`CREATE TEMP TABLE "%s" (%s)`, stage, strings.Join(colDefs, ","))); err != nil {
		return 0, fmt.Errorf("googlesqlite: bulk insert: create staging table: %w", err)
	}
	dropStage := func() {
		_, _ = conn.conn.ExecContext(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS "%s"`, stage))
	}

	ap, err := duckdb.NewAppender(conn.conn, "", stage)
	if err != nil {
		dropStage()
		return 0, fmt.Errorf("googlesqlite: bulk insert: staging appender: %w", err)
	}
	for ri, vals := range encoded {
		if err := ap.AppendRow(vals...); err != nil {
			dropStage()
			return 0, fmt.Errorf("googlesqlite: bulk insert: row %d: %w", ri, err)
		}
	}
	if err := ap.Close(); err != nil {
		dropStage()
		return 0, fmt.Errorf("googlesqlite: bulk insert: staging flush: %w", err)
	}

	res, err := conn.conn.ExecContext(ctx, fmt.Sprintf(`INSERT INTO "%s" SELECT %s FROM "%s"`,
		tableName, strings.Join(selExprs, ","), stage))
	if err != nil {
		dropStage()
		return 0, fmt.Errorf("googlesqlite: bulk insert: flush: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil && n != int64(len(encoded)) {
		dropStage()
		return 0, fmt.Errorf("googlesqlite: bulk insert: flush inserted %d of %d rows", n, len(encoded))
	}
	dropStage()
	return int64(len(encoded)), nil
}

// bulkIsNullValue reports whether v is SQL NULL under the value layer's
// reading (nil, typed nils and friends) — the value-aware gate for NULL-ONLY
// columns. Unconvertible values count as non-NULL (the caller rejects them).
func bulkIsNullValue(v any) bool {
	if v == nil {
		return true
	}
	val, err := value.ValueFromGoValue(v)
	return err == nil && val == nil
}

// bulkEncodeCell converts one caller value into the exact representation the
// per-row INSERT path stores for a column of this type (or, for CAST-class
// columns, the canonical VARCHAR the per-row path binds). The coercions
// mirror the per-row parameter pipeline — ValueFromGoValue -> castValueWithKind
// -> encodeParamValue — in pure Go (no analyzer-module entry).
func bulkEncodeCell(t *Type, v any) (any, error) {
	if v == nil {
		return nil, nil
	}
	kind := googlesql.TypeKind(t.Kind)
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
	case googlesql.TypeKindTypeNumeric, googlesql.TypeKindTypeBignumeric,
		googlesql.TypeKindTypeBytes, googlesql.TypeKindTypeTime, googlesql.TypeKindTypeJson,
		googlesql.TypeKindTypeArray, googlesql.TypeKindTypeStruct:
		return bulkEncodeComplexCell(t, kind, v, val)
	}
	return nil, fmt.Errorf("%w: type %s", ErrBulkInsertUnsupported, typeKindToSQLName(kind))
}

// bulkEncodeComplexCell encodes the issue-#6 complex kinds, mirroring the
// per-row parameter pipeline exactly:
//
//  1. a string input that decodes as a value-layer envelope is decoded first
//     (encodeValueAgainstParamTypeWithKind's preamble), so a caller handing
//     back a previously-scanned envelope round-trips like the per-row path;
//  2. the value is coerced with the same per-kind readings castValueWithKind
//     applies (ToRat / ToBytes / ToTime / ToJSON / ToArray / struct reshape);
//  3. the coerced value is rendered with the SAME encodeParamValue the
//     per-row path binds: envelopes for NUMERIC/ARRAY/STRUCT/BYTES, canonical
//     text for TIME, raw JSON text for JSON.
//
// The only divergence from encodeParamValue's output is BYTES: the per-row
// path binds the envelope as a VARCHAR parameter and DuckDB's VARCHAR->BLOB
// insert cast stores the string's bytes; the appender writes the BLOB column
// directly, so the envelope is converted to its []byte form here (the stored
// cell is byte-identical — measured in the storage-parity tests).
func bulkEncodeComplexCell(t *Type, kind googlesql.TypeKind, raw any, val value.Value) (any, error) {
	var casted value.Value
	if s, ok := raw.(string); ok {
		if decoded, err := DecodeValue(s); err == nil && decoded != nil {
			if c, err := bulkCastValue(t, decoded); err == nil && c != nil {
				casted = c
			}
		}
	}
	if casted == nil {
		c, err := bulkCastValue(t, val)
		if err != nil {
			return nil, err
		}
		if c == nil {
			return nil, nil
		}
		casted = c
	}
	enc, err := encodeParamValue(casted)
	if err != nil {
		return nil, err
	}
	if kind == googlesql.TypeKindTypeBytes {
		if s, ok := enc.(string); ok {
			return []byte(s), nil
		}
	}
	return enc, nil
}

// bulkCastValue is the pure-Go mirror of castValueWithKind over the catalog's
// serialized *Type (no analyzer-module entry): identical per-kind readings,
// identical struct reshaping, and the same pass-through default for the kinds
// CastValue passes through (GEOGRAPHY/RANGE/...). Kinds that castValueWithKind
// resolves through the analyzer module (PROTO text parsing) pass through
// unchanged — their top-level columns never reach this function (NULL-ONLY
// class), only struct fields could, where the wire-bytes pass-through matches.
func bulkCastValue(t *Type, v value.Value) (value.Value, error) {
	if v == nil || t == nil {
		return v, nil
	}
	switch googlesql.TypeKind(t.Kind) {
	case googlesql.TypeKindTypeInt32, googlesql.TypeKindTypeInt64,
		googlesql.TypeKindTypeUint32, googlesql.TypeKindTypeUint64:
		i64, err := v.ToInt64()
		if err != nil {
			return nil, err
		}
		return value.IntValue(i64), nil
	case googlesql.TypeKindTypeBool:
		b, err := v.ToBool()
		if err != nil {
			return nil, err
		}
		return value.BoolValue(b), nil
	case googlesql.TypeKindTypeFloat, googlesql.TypeKindTypeDouble:
		f64, err := v.ToFloat64()
		if err != nil {
			return nil, err
		}
		return value.FloatValue(f64), nil
	case googlesql.TypeKindTypeString, googlesql.TypeKindTypeEnum:
		s, err := v.ToString()
		if err != nil {
			return nil, err
		}
		return value.StringValue(s), nil
	case googlesql.TypeKindTypeBytes:
		b, err := v.ToBytes()
		if err != nil {
			return nil, err
		}
		return value.BytesValue(b), nil
	case googlesql.TypeKindTypeDate:
		tt, err := v.ToTime()
		if err != nil {
			return nil, err
		}
		return value.DateValue(tt), nil
	case googlesql.TypeKindTypeDatetime:
		tt, err := v.ToTime()
		if err != nil {
			return nil, err
		}
		return value.DatetimeValue(tt), nil
	case googlesql.TypeKindTypeTime:
		tt, err := v.ToTime()
		if err != nil {
			return nil, err
		}
		return value.TimeValue(tt), nil
	case googlesql.TypeKindTypeTimestamp:
		tt, err := v.ToTime()
		if err != nil {
			return nil, err
		}
		return value.TimestampValue(tt), nil
	case googlesql.TypeKindTypeInterval:
		s, err := v.ToString()
		if err != nil {
			return nil, err
		}
		return value.ParseInterval(s)
	case googlesql.TypeKindTypeNumeric:
		r, err := v.ToRat()
		if err != nil {
			return nil, err
		}
		return &value.NumericValue{Rat: r}, nil
	case googlesql.TypeKindTypeBignumeric:
		r, err := v.ToRat()
		if err != nil {
			return nil, err
		}
		return &value.NumericValue{Rat: r, IsBigNumeric: true}, nil
	case googlesql.TypeKindTypeJson:
		j, err := v.ToJSON()
		if err != nil {
			return nil, err
		}
		return value.JsonValue(j), nil
	case googlesql.TypeKindTypeArray:
		array, err := v.ToArray()
		if err != nil {
			return nil, err
		}
		// Mirror castValueWithKind: elements pass through untyped (the
		// envelope encoder serializes each element by its runtime kind).
		ret := &value.ArrayValue{}
		ret.Values = append(ret.Values, array.Values...)
		return ret, nil
	case googlesql.TypeKindTypeStruct:
		return bulkCastStruct(t, v)
	}
	return v, nil
}

// bulkCastStruct mirrors castValueWithKind's STRUCT branch over the catalog's
// serialized field list: reshape the source struct to the declared fields
// (declaration order, absent fields nil, recursive per-field cast), zipping
// positionally when names don't line up or duplicate.
func bulkCastStruct(t *Type, v value.Value) (value.Value, error) {
	if array, ok := v.(*value.ArrayValue); ok {
		ret := &value.StructValue{M: map[string]value.Value{}}
		for _, av := range array.Values {
			st, err := av.ToStruct()
			if err != nil {
				return nil, err
			}
			ret.Keys = append(ret.Keys, st.Keys...)
			ret.Values = append(ret.Values, st.Values...)
			for i, k := range st.Keys {
				ret.M[k] = st.Values[i]
			}
		}
		return ret, nil
	}
	s, err := v.ToStruct()
	if err != nil {
		return nil, err
	}
	fields := t.FieldTypes
	if len(fields) == 0 {
		return s, nil
	}
	dupTargetNames := false
	seen := make(map[string]struct{}, len(fields))
	matching := false
	for _, f := range fields {
		if f == nil || f.Name == "" {
			continue
		}
		if _, ok := seen[f.Name]; ok {
			dupTargetNames = true
		}
		seen[f.Name] = struct{}{}
		if s != nil && len(s.M) > 0 {
			if _, ok := s.M[f.Name]; ok {
				matching = true
			}
		}
	}
	usePositional := !matching || dupTargetNames || hasDuplicateKeys(s.Keys)
	reshaped := &value.StructValue{M: map[string]value.Value{}}
	for i, f := range fields {
		key := ""
		var ftype *Type
		if f != nil {
			key = f.Name
			ftype = f.Type
		}
		var existing value.Value
		switch {
		case usePositional:
			if i < len(s.Values) {
				existing = s.Values[i]
			}
		case key == "":
			// Mixed name + anonymous targets: fall back to the positional
			// value for unnamed target fields (mirrors CastValue).
			if i < len(s.Values) {
				existing = s.Values[i]
			}
		default:
			if vv, found := s.M[key]; found {
				existing = vv
			}
		}
		var fieldValue value.Value
		if existing != nil {
			if ftype != nil {
				casted, err := bulkCastValue(ftype, existing)
				if err != nil {
					return nil, err
				}
				fieldValue = casted
			} else {
				fieldValue = existing
			}
		}
		reshaped.Keys = append(reshaped.Keys, key)
		reshaped.Values = append(reshaped.Values, fieldValue)
		reshaped.M[key] = fieldValue
	}
	return reshaped, nil
}
