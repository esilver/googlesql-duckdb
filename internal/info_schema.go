package internal

import (
	"fmt"
	"strings"

	googlesql "github.com/goccy/go-googlesql"
	"github.com/goccy/googlesqlite/internal/value"
)

// InfoSchemaView describes one BigQuery `INFORMATION_SCHEMA.<name>`
// view. There is no virtual-table layer: each view is
// registered catalog-side as a SimpleTable, and at formatter time
// InfoSchemaInlineRelationSQL synthesises the live rows from the
// catalog and splices them into the DuckDB query as an inline
// relation. Because the rows are built from the catalog at query
// time, INFORMATION_SCHEMA queries always observe whatever the
// catalog currently contains, not a stale snapshot.
type infoSchemaView struct {
	// Name is the BigQuery view name, e.g. "TABLES" / "COLUMNS".
	Name string
	// Columns are the visible columns in declaration order. The
	// trailing hidden `__schema` column carries the dataset
	// constraint passed by the formatter.
	Columns []*infoSchemaColumn
	// BuildRows synthesises rows for the view limited to the given
	// dataset. An empty schema (no constraint) means all datasets.
	BuildRows func(c *Catalog, schema string) []map[string]value.Value
}

// InfoSchemaColumn declares one view column. The declared type maps
// to the DuckDB column type emitted for the inline relation (see
// infoSchemaDuckDBColumnType).
type infoSchemaColumn struct {
	Name string
	// Type is the declared type. We use STRING for most columns since
	// BigQuery's INFORMATION_SCHEMA columns are nearly all text-typed;
	// INT64 / TIMESTAMP are reflected through the value layer the same
	// way as anywhere else.
	Type string
}

// infoSchemaViews enumerates every supported view. New views append
// here; ensureInfoSchemaForDataset walks this slice to register each
// view as a SimpleTable.
var infoSchemaViews = []*infoSchemaView{
	{
		Name: "SCHEMATA",
		Columns: []*infoSchemaColumn{
			{Name: "catalog_name", Type: "STRING"},
			{Name: "schema_name", Type: "STRING"},
			{Name: "schema_owner", Type: "STRING"},
			{Name: "creation_time", Type: "STRING"},
			{Name: "last_modified_time", Type: "STRING"},
			{Name: "location", Type: "STRING"},
		},
		BuildRows: buildSchemataRows,
	},
	{
		Name: "TABLES",
		Columns: []*infoSchemaColumn{
			{Name: "table_catalog", Type: "STRING"},
			{Name: "table_schema", Type: "STRING"},
			{Name: "table_name", Type: "STRING"},
			{Name: "table_type", Type: "STRING"},
			{Name: "is_insertable_into", Type: "STRING"},
			{Name: "is_typed", Type: "STRING"},
			{Name: "creation_time", Type: "STRING"},
		},
		BuildRows: buildTablesRows,
	},
	{
		Name: "COLUMNS",
		Columns: []*infoSchemaColumn{
			{Name: "table_catalog", Type: "STRING"},
			{Name: "table_schema", Type: "STRING"},
			{Name: "table_name", Type: "STRING"},
			{Name: "column_name", Type: "STRING"},
			{Name: "ordinal_position", Type: "INT64"},
			{Name: "is_nullable", Type: "STRING"},
			{Name: "data_type", Type: "STRING"},
			{Name: "is_partitioning_column", Type: "STRING"},
			{Name: "is_hidden", Type: "STRING"},
			{Name: "is_system_defined", Type: "STRING"},
		},
		BuildRows: buildColumnsRows,
	},
	{
		Name: "TABLE_OPTIONS",
		Columns: []*infoSchemaColumn{
			{Name: "table_catalog", Type: "STRING"},
			{Name: "table_schema", Type: "STRING"},
			{Name: "table_name", Type: "STRING"},
			{Name: "option_name", Type: "STRING"},
			{Name: "option_type", Type: "STRING"},
			{Name: "option_value", Type: "STRING"},
		},
		BuildRows: buildTableOptionsRows,
	},
}

// catalogProjectAndSchemas walks every TableSpec / FunctionSpec /
// TVFSpec and gathers the (catalog, schema) pairs in deterministic
// order. The first NamePath segment is the catalog (BigQuery
// project), the second is the schema (BigQuery dataset).
func catalogProjectAndSchemas(c *Catalog) []struct{ catalog, schema string } {
	seen := map[string]struct{}{}
	out := make([]struct{ catalog, schema string }, 0)
	// Like matchesSchema: a 2-segment path is `dataset.table` (no
	// project prefix); a 3+ segment path is `project.dataset.table`
	// (project is the third-from-last segment, schema is the
	// second-from-last). Anything shorter than 2 segments is just a
	// bare name.
	add := func(path []string) {
		if len(path) < 2 {
			return
		}
		var cat, sch string
		switch len(path) {
		case 2:
			sch = path[0]
		default:
			cat = path[len(path)-3]
			sch = path[len(path)-2]
		}
		key := cat + "\x00" + sch
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		out = append(out, struct{ catalog, schema string }{cat, sch})
	}
	for _, t := range c.tables {
		add(t.NamePath)
	}
	for _, f := range c.functions {
		add(f.NamePath)
	}
	for _, t := range c.tvfs {
		add(t.NamePath)
	}
	return out
}

// matchesSchema reports whether spec.NamePath sits under the given
// schema filter. An empty filter accepts every dataset.
func matchesSchema(namePath []string, filter string) (catalog, schema string, ok bool) {
	// BigQuery's identifier layout is `project.dataset.table` (3
	// segments) or `dataset.table` (2 segments). The schema reported
	// by INFORMATION_SCHEMA is the dataset name — that's the
	// SECOND-from-last segment, never always NamePath[1].
	if len(namePath) < 2 {
		return "", "", false
	}
	var cat, sch string
	switch len(namePath) {
	case 2:
		sch = namePath[0]
	default:
		cat = namePath[len(namePath)-3]
		sch = namePath[len(namePath)-2]
	}
	if filter != "" && !strings.EqualFold(filter, sch) {
		return "", "", false
	}
	return cat, sch, true
}

func buildSchemataRows(c *Catalog, schema string) []map[string]value.Value {
	pairs := catalogProjectAndSchemas(c)
	out := make([]map[string]value.Value, 0, len(pairs))
	for _, p := range pairs {
		if schema != "" && !strings.EqualFold(schema, p.schema) {
			continue
		}
		out = append(out, map[string]value.Value{
			"catalog_name":       value.StringValue(p.catalog),
			"schema_name":        value.StringValue(p.schema),
			"schema_owner":       nil,
			"creation_time":      nil,
			"last_modified_time": nil,
			"location":           nil,
		})
	}
	return out
}

func buildTablesRows(c *Catalog, schema string) []map[string]value.Value {
	out := make([]map[string]value.Value, 0, len(c.tables))
	for _, t := range c.tables {
		cat, sch, ok := matchesSchema(t.NamePath, schema)
		if !ok {
			continue
		}
		tableType := "BASE TABLE"
		if t.IsView {
			tableType = "VIEW"
		}
		insertable := "YES"
		if t.IsView {
			insertable = "NO"
		}
		row := map[string]value.Value{
			"table_catalog":      value.StringValue(cat),
			"table_schema":       value.StringValue(sch),
			"table_name":         value.StringValue(t.NamePath[len(t.NamePath)-1]),
			"table_type":         value.StringValue(tableType),
			"is_insertable_into": value.StringValue(insertable),
			"is_typed":           value.StringValue("NO"),
			"creation_time":      nil,
		}
		if !t.CreatedAt.IsZero() {
			row["creation_time"] = value.StringValue(t.CreatedAt.UTC().Format("2006-01-02 15:04:05.999999 MST"))
		}
		out = append(out, row)
	}
	return out
}

func buildColumnsRows(c *Catalog, schema string) []map[string]value.Value {
	out := make([]map[string]value.Value, 0)
	for _, t := range c.tables {
		cat, sch, ok := matchesSchema(t.NamePath, schema)
		if !ok {
			continue
		}
		tableName := t.NamePath[len(t.NamePath)-1]
		for i, col := range t.Columns {
			isNullable := "YES"
			if col.IsNotNull {
				isNullable = "NO"
			}
			out = append(out, map[string]value.Value{
				"table_catalog":          value.StringValue(cat),
				"table_schema":           value.StringValue(sch),
				"table_name":             value.StringValue(tableName),
				"column_name":            value.StringValue(col.Name),
				"ordinal_position":       value.IntValue(int64(i + 1)),
				"is_nullable":            value.StringValue(isNullable),
				"data_type":              value.StringValue(infoSchemaDataType(col.Type)),
				"is_partitioning_column": value.StringValue("NO"),
				"is_hidden":              value.StringValue("NO"),
				"is_system_defined":      value.StringValue("NO"),
			})
		}
	}
	return out
}

func buildTableOptionsRows(c *Catalog, schema string) []map[string]value.Value {
	out := make([]map[string]value.Value, 0)
	for _, t := range c.tables {
		cat, sch, ok := matchesSchema(t.NamePath, schema)
		if !ok {
			continue
		}
		tableName := t.NamePath[len(t.NamePath)-1]
		for _, opt := range tableOptionsRows(t) {
			out = append(out, map[string]value.Value{
				"table_catalog": value.StringValue(cat),
				"table_schema":  value.StringValue(sch),
				"table_name":    value.StringValue(tableName),
				"option_name":   value.StringValue(opt.Name),
				"option_type":   value.StringValue(opt.Type),
				"option_value":  value.StringValue(opt.Value),
			})
		}
	}
	return out
}

// tableOptionRow is the trio that appears in INFORMATION_SCHEMA.
// TABLE_OPTIONS for one row. TableSpec carries the metadata; we
// surface whatever it set.
type tableOptionRow struct {
	Name, Type, Value string
}

func tableOptionsRows(t *TableSpec) []tableOptionRow {
	if t == nil || len(t.Options) == 0 {
		return nil
	}
	out := make([]tableOptionRow, 0, len(t.Options))
	for _, opt := range t.Options {
		if opt == nil || opt.Name == "" {
			continue
		}
		out = append(out, tableOptionRow{
			Name:  opt.Name,
			Type:  opt.Type,
			Value: opt.Value,
		})
	}
	return out
}

// infoSchemaDuckDBColumnType maps a view-declared column type string
// to the DuckDB column type used when the rows are materialised as an
// inline VALUES relation. STRING columns ride through as VARCHAR
// because their values are value-layer base64 envelopes (see
// literalFromValue); INT64 lands as BIGINT so ordinal_position keeps
// integer semantics. Anything else collapses to VARCHAR.
func infoSchemaDuckDBColumnType(declared string) string {
	switch declared {
	case "INT64":
		return "BIGINT"
	case "BOOL":
		return "BOOLEAN"
	default:
		return "VARCHAR"
	}
}

// InfoSchemaInlineRelationSQL renders the INFORMATION_SCHEMA view named
// by meta as a self-contained DuckDB relation expression that exposes
// every declared column by name. DuckDB has no
// virtual-table layer, so the formatter splices this inline relation
// in place of the (non-existent) eponymous vtab module: the catalog's
// BuildRows synthesises the live rows, each value is rendered through
// the same value-layer literal encoder the rest of the formatter uses
// (so the row scanner decodes them identically to ordinary table
// values), and an empty result degrades to a typed zero-row SELECT
// rather than an illegal empty VALUES list.
//
// The caller must hold the catalog mutex: BuildRows reads c.tables /
// c.functions / c.tvfs, which DDL mutates under the same lock.
func (c *Catalog) InfoSchemaInlineRelationSQL(meta *infoSchemaTableMeta) (string, error) {
	view := meta.view
	colNames := make([]string, 0, len(view.Columns))
	for _, col := range view.Columns {
		colNames = append(colNames, fmt.Sprintf("%q", col.Name))
	}

	rows := view.BuildRows(c, meta.dataset)
	if len(rows) == 0 {
		// An empty VALUES clause is not valid SQL. Emit a single typed
		// NULL row gated by WHERE FALSE so the relation carries the
		// right column names and types but yields zero rows.
		typedNulls := make([]string, 0, len(view.Columns))
		for _, col := range view.Columns {
			typedNulls = append(
				typedNulls,
				fmt.Sprintf("CAST(NULL AS %s) AS %q", infoSchemaDuckDBColumnType(col.Type), col.Name),
			)
		}
		return fmt.Sprintf("(SELECT %s WHERE FALSE)", strings.Join(typedNulls, ", ")), nil
	}

	valueRows := make([]string, 0, len(rows))
	for _, row := range rows {
		cells := make([]string, 0, len(view.Columns))
		for _, col := range view.Columns {
			lit, err := literalFromValue(row[col.Name])
			if err != nil {
				return "", fmt.Errorf("googlesqlite info-schema %s.%s: render value: %w", view.Name, col.Name, err)
			}
			if lit == "null" {
				// Pin the column type so a column that is NULL in every
				// row still binds to the declared type instead of
				// DuckDB's NULL/INTEGER default.
				lit = fmt.Sprintf("CAST(NULL AS %s)", infoSchemaDuckDBColumnType(col.Type))
			}
			cells = append(cells, lit)
		}
		valueRows = append(valueRows, fmt.Sprintf("(%s)", strings.Join(cells, ", ")))
	}
	return fmt.Sprintf(
		"(SELECT * FROM (VALUES %s) AS _info_schema(%s))",
		strings.Join(valueRows, ", "),
		strings.Join(colNames, ", "),
	), nil
}

func infoSchemaDataType(typ *Type) string {
	if typ == nil {
		return ""
	}
	switch googlesql.TypeKind(typ.Kind) {
	case googlesql.TypeKindTypeArray:
		if typ.ElementType == nil {
			return "ARRAY"
		}
		return "ARRAY<" + infoSchemaDataType(typ.ElementType) + ">"
	case googlesql.TypeKindTypeStruct:
		fields := make([]string, 0, len(typ.FieldTypes))
		for _, f := range typ.FieldTypes {
			fields = append(fields, f.Name+" "+infoSchemaDataType(f.Type))
		}
		return "STRUCT<" + strings.Join(fields, ", ") + ">"
	}
	return typeKindToSQLName(googlesql.TypeKind(typ.Kind))
}
