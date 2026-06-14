package internal

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"

	googlesql "github.com/goccy/go-googlesql"
)

var (
	_ driver.Stmt = &CreateTableStmt{}
	_ driver.Stmt = &CreateFunctionStmt{}
	_ driver.Stmt = &CreateTableFunctionStmt{}
	_ driver.Stmt = &DMLStmt{}
	_ driver.Stmt = &QueryStmt{}
)

// encodeOrPassArgs wraps EncodeGoValues with a fall-through: when the
// resolved-AST walker failed to populate params (current bridge limitation),
// s.args is empty even though the caller passed real arguments. In that
// case we hand the raw values to DuckDB directly — its driver handles
// @name / ? native binding for primitive types.
//
// TODO: once ResolvedNode.ChildrenAccept is bridged and
// getParamsFromNode returns a full list, this fallback can go away and
// EncodeGoValues alone will type-check each argument against the
// googlesql-inferred type.
// CheckNamedValue is the canonical CheckNamedValue used by Conn,
// DMLStmt, and QueryStmt. It encodes the value into the googlesqlite
// layout so the underlying DuckDB driver only ever sees primitive
// driver.Value types. Numeric and bool primitives stay raw because
// EncodeValue passes them through unchanged.
func CheckNamedValue(value *driver.NamedValue) error {
	// Scalar fast path: nil/int64/float64/bool round-trip unchanged
	// through ValueFromGoValue → EncodeValue (IntValue/FloatValue/
	// BoolValue encode back to the same Go primitive), so skip the
	// reflection walk and re-boxing for them. Strings are NOT identity
	// (they encode into the base64 layout envelope) and must take the
	// full path.
	switch value.Value.(type) {
	case nil, int64, float64, bool:
		return nil
	}
	val, err := ValueFromGoValue(value.Value)
	if err != nil {
		return err
	}
	encoded, err := EncodeValue(val)
	if err != nil {
		return err
	}
	value.Value = encoded
	return nil
}

func encodeOrPassArgs(values []any, params []*googlesql.ResolvedParameter) ([]any, error) {
	if len(params) == 0 && len(values) > 0 {
		// Conn.CheckNamedValue has already encoded each value into
		// the canonical googlesqlite layout. Pass through unchanged
		// — re-encoding here would double-wrap the value.
		out := make([]any, len(values))
		copy(out, values)
		return out, nil
	}
	return EncodeGoValues(values, params)
}

// typeKindCache pins the analyzer-resolved Kind of a googlesql type node so
// prepared statements pay the analyzer round-trip once at prepare time
// instead of on every Exec/Query. hasKind is false when the type was nil
// or its Kind() call failed; consumers then fall back to the un-typed
// behavior the un-cached path would have produced.
type typeKindCache struct {
	kind    googlesql.TypeKind
	hasKind bool
}

// cacheTypeKinds resolves Kind once per type node. A nil node yields a
// hasKind=false slot.
func cacheTypeKinds(types []googlesql.Googlesql_TypeNode) []typeKindCache {
	if len(types) == 0 {
		return nil
	}
	out := make([]typeKindCache, len(types))
	for i, t := range types {
		if t == nil {
			continue
		}
		kind, err := t.Kind()
		if err != nil {
			continue
		}
		out[i] = typeKindCache{kind: kind, hasKind: true}
	}
	return out
}

// paramTypeCache pins a resolved parameter's type node and kind, both of
// which are analyzer round-trips (ResolvedParameter.Type() and Type.Kind())
// that EncodeGoValues would otherwise re-issue per exec.
type paramTypeCache struct {
	typ googlesql.Googlesql_TypeNode
	typeKindCache
}

// cacheParamTypes resolves each parameter's Type and Kind once, at
// statement-prepare time. A failed Type() lookup leaves typ nil, which
// reproduces EncodeGoValues' m1(param.Type()) == nil behavior.
func cacheParamTypes(params []*googlesql.ResolvedParameter) []paramTypeCache {
	if len(params) == 0 {
		return nil
	}
	out := make([]paramTypeCache, len(params))
	for i, p := range params {
		if p == nil {
			continue
		}
		t, err := p.Type()
		if err != nil || t == nil {
			continue
		}
		out[i].typ = t
		kind, err := t.Kind()
		if err != nil {
			continue
		}
		out[i].typeKindCache = typeKindCache{kind: kind, hasKind: true}
	}
	return out
}

// encodeOrPassArgsCached is encodeOrPassArgs for statements that cached
// their parameter types at prepare time. Behavior matches
// EncodeGoValues(values, params) exactly, minus the per-exec analyzer
// round-trips.
func encodeOrPassArgsCached(values []any, params []*googlesql.ResolvedParameter, cache []paramTypeCache) ([]any, error) {
	if len(params) == 0 && len(values) > 0 {
		out := make([]any, len(values))
		copy(out, values)
		return out, nil
	}
	if len(values) != len(params) {
		return nil, fmt.Errorf(
			"failed to match args values num (%d) and params num (%d)",
			len(values), len(params),
		)
	}
	ret := make([]any, 0, len(values))
	for idx, vv := range values {
		pc := cache[idx]
		encoded, err := encodeValueAgainstParamTypeWithKind(vv, pc.typ, pc.kind, pc.hasKind)
		if err != nil {
			return nil, err
		}
		ret = append(ret, encoded)
	}
	return ret, nil
}

type CreateTableStmt struct {
	stmt    *sql.Stmt
	conn    *Conn
	catalog *Catalog
	spec    *TableSpec
}

type CreateViewStmt struct {
	stmt    *sql.Stmt
	conn    *Conn
	catalog *Catalog
	spec    *TableSpec
}

func (s *CreateTableStmt) Close() error {
	return s.stmt.Close()
}

func (s *CreateTableStmt) NumInput() int {
	return 0
}

func (s *CreateTableStmt) Exec(args []driver.Value) (driver.Result, error) {
	anyArgs := make([]any, len(args))
	for i, a := range args {
		anyArgs[i] = a
	}
	if _, err := s.stmt.Exec(anyArgs...); err != nil {
		return nil, err
	}
	if err := s.catalog.AddNewTableSpec(context.Background(), s.conn, s.spec); err != nil {
		return nil, fmt.Errorf("failed to add new table spec: %w", err)
	}
	return nil, nil
}

func (s *CreateTableStmt) Query(args []driver.Value) (driver.Rows, error) {
	return nil, fmt.Errorf("CREATE TABLE statement does not return rows")
}

func newCreateTableStmt(stmt *sql.Stmt, conn *Conn, catalog *Catalog, spec *TableSpec) *CreateTableStmt {
	return &CreateTableStmt{
		stmt:    stmt,
		conn:    conn,
		catalog: catalog,
		spec:    spec,
	}
}

func newCreateViewStmt(stmt *sql.Stmt, conn *Conn, catalog *Catalog, spec *TableSpec) *CreateViewStmt {
	return &CreateViewStmt{
		stmt:    stmt,
		conn:    conn,
		catalog: catalog,
		spec:    spec,
	}
}

func (s *CreateViewStmt) Close() error {
	return s.stmt.Close()
}

func (s *CreateViewStmt) NumInput() int {
	return 0
}

func (s *CreateViewStmt) Exec(args []driver.Value) (driver.Result, error) {
	anyArgs := make([]any, len(args))
	for i, a := range args {
		anyArgs[i] = a
	}
	if _, err := s.stmt.Exec(anyArgs...); err != nil {
		return nil, err
	}
	if err := s.catalog.AddNewTableSpec(context.Background(), s.conn, s.spec); err != nil {
		return nil, fmt.Errorf("failed to add new table spec: %w", err)
	}
	return nil, nil
}

func (s *CreateViewStmt) Query(args []driver.Value) (driver.Rows, error) {
	return nil, fmt.Errorf("CREATE VIEW statement does not return rows")
}

type CreateFunctionStmt struct {
	conn    *Conn
	catalog *Catalog
	spec    *FunctionSpec
}

func (s *CreateFunctionStmt) Close() error {
	return nil
}

func (s *CreateFunctionStmt) NumInput() int {
	return 0
}

func (s *CreateFunctionStmt) Exec(args []driver.Value) (driver.Result, error) {
	if err := s.catalog.AddNewFunctionSpec(context.Background(), s.conn, s.spec); err != nil {
		return nil, fmt.Errorf("failed to add new function spec: %w", err)
	}
	return nil, nil
}

func (s *CreateFunctionStmt) Query(args []driver.Value) (driver.Rows, error) {
	return nil, fmt.Errorf("CREATE FUNCTION statement does not return rows")
}

func newCreateFunctionStmt(conn *Conn, catalog *Catalog, spec *FunctionSpec) *CreateFunctionStmt {
	return &CreateFunctionStmt{
		conn:    conn,
		catalog: catalog,
		spec:    spec,
	}
}

type CreateTableFunctionStmt struct {
	conn    *Conn
	catalog *Catalog
	spec    *TVFSpec
}

func (s *CreateTableFunctionStmt) Close() error {
	return nil
}

func (s *CreateTableFunctionStmt) NumInput() int {
	return 0
}

func (s *CreateTableFunctionStmt) Exec(args []driver.Value) (driver.Result, error) {
	if err := s.catalog.AddNewTVFSpec(context.Background(), s.conn, s.spec); err != nil {
		return nil, fmt.Errorf("failed to add new TVF spec: %w", err)
	}
	return nil, nil
}

func (s *CreateTableFunctionStmt) Query(args []driver.Value) (driver.Rows, error) {
	return nil, fmt.Errorf("CREATE TABLE FUNCTION statement does not return rows")
}

func newCreateTableFunctionStmt(conn *Conn, catalog *Catalog, spec *TVFSpec) *CreateTableFunctionStmt {
	return &CreateTableFunctionStmt{
		conn:    conn,
		catalog: catalog,
		spec:    spec,
	}
}

type DMLStmt struct {
	stmt           *sql.Stmt
	args           []*googlesql.ResolvedParameter
	argTypes       []paramTypeCache
	colTypes       []googlesql.Googlesql_TypeNode
	colKinds       []typeKindCache
	formattedQuery string
}

func newDMLStmt(stmt *sql.Stmt, args []*googlesql.ResolvedParameter, colTypes []googlesql.Googlesql_TypeNode, formattedQuery string) *DMLStmt {
	return &DMLStmt{
		stmt:           stmt,
		args:           args,
		argTypes:       cacheParamTypes(args),
		colTypes:       colTypes,
		colKinds:       cacheTypeKinds(colTypes),
		formattedQuery: formattedQuery,
	}
}

func (s *DMLStmt) CheckNamedValue(value *driver.NamedValue) error {
	return CheckNamedValue(value)
}

func (s *DMLStmt) Close() error {
	return s.stmt.Close()
}

func (s *DMLStmt) NumInput() int {
	// -1 tells database/sql "we don't know how many placeholders" —
	// required because our resolved-tree walker doesn't yet surface
	// the full parameter list, so a conservative len() would refuse
	// user-supplied args.
	return -1
}

func (s *DMLStmt) Exec(args []driver.Value) (driver.Result, error) {
	values := make([]any, 0, len(args))
	for _, arg := range args {
		values = append(values, arg)
	}
	// Reshape struct-shaped args against the destination column types
	// before encoding them. The wire format that BigQuery's client uses
	// for STRUCT values (an ordered list of single-key objects) is
	// indistinguishable from an ARRAY of single-key STRUCTs without
	// the schema hint, so the encode side reaches its conclusion only
	// once we know which positional column is a STRUCT.
	for i, v := range values {
		if i >= len(s.colTypes) || s.colTypes[i] == nil {
			continue
		}
		// colKinds is built from colTypes, so the index is in range and
		// hasKind mirrors the Kind() error reshapeArgToType would have
		// swallowed (kind 0 / unknown is not a reshape candidate).
		ck := s.colKinds[i]
		if !ck.hasKind {
			continue
		}
		values[i] = reshapeArgToTypeWithKind(v, s.colTypes[i], ck.kind)
	}
	newArgs, err := encodeOrPassArgsCached(values, s.args, s.argTypes)
	if err != nil {
		return nil, err
	}
	result, err := s.stmt.Exec(newArgs...)
	if err != nil {
		return nil, fmt.Errorf(
			"failed to execute query %s: args %v: %w",
			s.formattedQuery,
			newArgs,
			err,
		)
	}
	return &execResult{result: result}, nil
}

func (s *DMLStmt) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	return nil, fmt.Errorf("unimplemented ExecContext for DMLStmt")
}

func (s *DMLStmt) Query(args []driver.Value) (driver.Rows, error) {
	return nil, fmt.Errorf("unsupported query for DMLStmt")
}

func (s *DMLStmt) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	return nil, fmt.Errorf("unsupported query for DMLStmt")
}

type QueryStmt struct {
	stmt           *sql.Stmt
	args           []*googlesql.ResolvedParameter
	argTypes       []paramTypeCache
	formattedQuery string
	outputColumns  []*ColumnSpec
}

func newQueryStmt(stmt *sql.Stmt, args []*googlesql.ResolvedParameter, formattedQuery string, outputColumns []*ColumnSpec) *QueryStmt {
	return &QueryStmt{
		stmt:           stmt,
		args:           args,
		argTypes:       cacheParamTypes(args),
		formattedQuery: formattedQuery,
		outputColumns:  outputColumns,
	}
}

func (s *QueryStmt) CheckNamedValue(value *driver.NamedValue) error {
	return CheckNamedValue(value)
}

func (s *QueryStmt) Close() error {
	return s.stmt.Close()
}

func (s *QueryStmt) NumInput() int {
	return -1
}

func (s *QueryStmt) OutputColumns() []*ColumnSpec {
	return s.outputColumns
}

func (s *QueryStmt) Exec(args []driver.Value) (driver.Result, error) {
	return nil, fmt.Errorf("unsupported exec for QueryStmt")
}

func (s *QueryStmt) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	return nil, fmt.Errorf("unsupported exec for QueryStmt")
}

func (s *QueryStmt) Query(args []driver.Value) (driver.Rows, error) {
	values := make([]any, 0, len(args))
	for _, arg := range args {
		values = append(values, arg)
	}
	newArgs, err := encodeOrPassArgsCached(values, s.args, s.argTypes)
	if err != nil {
		return nil, err
	}
	rows, err := s.stmt.Query(newArgs...)
	if err != nil {
		return nil, fmt.Errorf(
			"failed to query %s: args: %v: %w",
			s.formattedQuery,
			newArgs,
			err,
		)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf(
			"failed to query %s: args: %v: %w",
			s.formattedQuery,
			newArgs,
			err,
		)
	}
	return &Rows{rows: rows, columns: s.outputColumns}, nil
}

func (s *QueryStmt) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	return nil, fmt.Errorf("unimplemented QueryContext for QueryStmt")
}
