package internal

import (
	"fmt"

	googlesql "github.com/goccy/go-googlesql"
)

// infoSchemaTableMeta is what infoSchemaTables maps a SimpleTable
// handle key to. The formatter looks the meta up by handle key
// during TableScanNode.FormatSQL to detect that a scan targets an
// INFORMATION_SCHEMA view; if matched, it calls
// InfoSchemaInlineRelationSQL(meta) to render meta.view limited to
// meta.dataset as an inline DuckDB relation and splices that in place
// of the scan.
type infoSchemaTableMeta struct {
	view    *infoSchemaView
	dataset string
}

// ensureInfoSchemaForDataset auto-registers every INFORMATION_SCHEMA
// view as a SimpleTable under <project>.<dataset>.INFORMATION_SCHEMA
// so the analyzer accepts the path at parse time. Each registration
// is recorded in c.infoSchemaTables so TableScanNode can recognise
// the scan and route it through the corresponding vtab module.
//
// Registration is idempotent per (project, dataset). Calling this
// from addTableSpec means a dataset's INFORMATION_SCHEMA appears
// the moment it has its first table — matching BigQuery's "the
// dataset must exist before INFORMATION_SCHEMA can be queried"
// rule.
func (c *Catalog) ensureInfoSchemaForDataset(project, dataset string) error {
	key := project + "\x00" + dataset
	if _, ok := c.infoSchemaRegistered[key]; ok {
		return nil
	}
	c.infoSchemaRegistered[key] = struct{}{}
	for _, view := range infoSchemaViews {
		if err := c.registerInfoSchemaView(project, dataset, view); err != nil {
			return err
		}
	}
	return nil
}

// registerInfoSchemaView builds a SimpleTable mirroring view.Columns,
// aliases it under every name path the analyzer might descend through
// (similar to addTableSpecRecursiveImpl), and stores its meta so the
// formatter can identify the scan later.
func (c *Catalog) registerInfoSchemaView(project, dataset string, view *infoSchemaView) error {
	storageName := fmt.Sprintf("%s.%s.INFORMATION_SCHEMA.%s", project, dataset, view.Name)
	columns := []*googlesql.SimpleColumn{}
	for _, col := range view.Columns {
		typ, err := infoSchemaSimpleTypeForColumn(col)
		if err != nil {
			return err
		}
		simpleCol, err := googlesql.NewSimpleColumn(storageName, col.Name, typ, false, false)
		if err != nil {
			return err
		}
		columns = append(columns, simpleCol)
	}
	tbl := newSimpleTableWithColumns(storageName, columns)
	if tbl == nil {
		return fmt.Errorf("failed to construct INFORMATION_SCHEMA table %q", storageName)
	}
	// The SimpleTable's Name() is its registry key. Each
	// (project, dataset, view) trio maps to a unique storageName,
	// so name-keyed lookups stay stable across analyzer descents.
	c.infoSchemaTables[storageName] = &infoSchemaTableMeta{view: view, dataset: dataset}
	// Alias the same handle through every catalog level the analyzer
	// can descend through, mirroring addTableSpecRecursiveImpl. Path
	// is [project, dataset, "INFORMATION_SCHEMA", view.Name].
	path := []string{project, dataset, "INFORMATION_SCHEMA", view.Name}
	return c.aliasInfoSchemaTable(c.catalog, path, tbl)
}

func (c *Catalog) aliasInfoSchemaTable(cat *googlesql.SimpleCatalog, path []string, tbl *googlesql.SimpleTable) error {
	if len(path) > 1 {
		head := path[0]
		sub := c.getOrCreateSubCatalog(cat, head)
		fullName := joinDot(path)
		if !c.existsTable(cat, fullName) {
			if err := cat.AddTable2(fullName, tbl); err != nil {
				return fmt.Errorf("failed to register INFORMATION_SCHEMA alias %q: %w", fullName, err)
			}
		}
		rest := path[1:]
		if err := c.aliasInfoSchemaTable(cat, rest, tbl); err != nil {
			return err
		}
		if sub != nil {
			if err := c.aliasInfoSchemaTable(sub, rest, tbl); err != nil {
				return err
			}
		}
		return nil
	}
	if len(path) == 0 {
		return nil
	}
	if c.existsTable(cat, path[0]) {
		return nil
	}
	return cat.AddTable2(path[0], tbl)
}

func joinDot(path []string) string {
	out := ""
	for i, p := range path {
		if i > 0 {
			out += "."
		}
		out += p
	}
	return out
}

// infoSchemaInlineRelationFor resolves the given scan target to its
// INFORMATION_SCHEMA view and, when matched, renders the view's live
// rows as a self-contained DuckDB relation. The meta lookup and the
// BuildRows call happen under a single mutex hold so the rendered
// snapshot is internally consistent with any concurrent DDL. ok is
// false when the scan is an ordinary table (not an info-schema view).
func (c *Catalog) infoSchemaInlineRelationFor(table googlesql.TableNode) (string, bool, error) {
	if table == nil {
		return "", false, nil
	}
	name, err := table.Name()
	if err != nil || name == "" {
		return "", false, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.infoSchemaTables == nil {
		return "", false, nil
	}
	meta, ok := c.infoSchemaTables[name]
	if !ok {
		return "", false, nil
	}
	relation, err := c.InfoSchemaInlineRelationSQL(meta)
	if err != nil {
		return "", false, err
	}
	return relation, true, nil
}

// infoSchemaSimpleTypeForColumn maps the view-declared column type
// string ("STRING" / "INT64") to a googlesql Type. Only the kinds
// info-schema rows actually use are handled.
func infoSchemaSimpleTypeForColumn(col *infoSchemaColumn) (googlesql.Googlesql_TypeNode, error) {
	factory := tf()
	switch col.Type {
	case "STRING":
		return factory.MakeSimpleType(googlesql.TypeKindTypeString)
	case "INT64":
		return factory.MakeSimpleType(googlesql.TypeKindTypeInt64)
	case "BOOL":
		return factory.MakeSimpleType(googlesql.TypeKindTypeBool)
	case "TIMESTAMP":
		return factory.MakeSimpleType(googlesql.TypeKindTypeTimestamp)
	}
	return factory.MakeSimpleType(googlesql.TypeKindTypeString)
}
