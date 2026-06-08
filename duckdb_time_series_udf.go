package googlesqlite

import (
	"database/sql"
	"fmt"

	duckdb "github.com/duckdb/duckdb-go/v2"
)

func registerChangeHistoryTableUDFs(conn *sql.Conn) error {
	varchar, err := duckdb.NewTypeInfo(duckdb.TYPE_VARCHAR)
	if err != nil {
		return fmt.Errorf("googlesqlite: change-history varchar type: %w", err)
	}
	timestamp, err := duckdb.NewTypeInfo(duckdb.TYPE_TIMESTAMP)
	if err != nil {
		return fmt.Errorf("googlesqlite: change-history timestamp type: %w", err)
	}
	for _, spec := range []struct {
		name string
		cols []duckdb.ColumnInfo
	}{
		{
			name: "appends",
			cols: []duckdb.ColumnInfo{
				{Name: "_CHANGE_TYPE", T: varchar},
				{Name: "_CHANGE_TIMESTAMP", T: timestamp},
			},
		},
		{
			name: "changes",
			cols: []duckdb.ColumnInfo{
				{Name: "_CHANGE_TYPE", T: varchar},
				{Name: "_CHANGE_TIMESTAMP", T: timestamp},
				{Name: "_CHANGE_SEQUENCE_NUMBER", T: varchar},
			},
		},
	} {
		cols := spec.cols
		udf := duckdb.RowTableFunction{
			BindArguments: func(map[string]any, ...any) (duckdb.RowTableSource, error) {
				return &emptyChangeHistoryTable{cols: cols}, nil
			},
		}
		if err := duckdb.RegisterTableUDF(conn, spec.name, udf); err != nil {
			return fmt.Errorf("googlesqlite: register table udf %s: %w", spec.name, err)
		}
	}
	return nil
}

type emptyChangeHistoryTable struct {
	cols []duckdb.ColumnInfo
}

func (t *emptyChangeHistoryTable) ColumnInfos() []duckdb.ColumnInfo {
	return t.cols
}

func (*emptyChangeHistoryTable) Cardinality() *duckdb.CardinalityInfo {
	return &duckdb.CardinalityInfo{Cardinality: 0, Exact: true}
}

func (*emptyChangeHistoryTable) Init() {}

func (*emptyChangeHistoryTable) FillRow(duckdb.Row) (bool, error) {
	return false, nil
}
