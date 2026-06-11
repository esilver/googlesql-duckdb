package internal

import "testing"

func TestDuckDBNormalUDFSpecsIncludeFormatterHelpers(t *testing.T) {
	t.Parallel()

	got := map[string]DuckDBUDFResultKind{}
	for _, spec := range DuckDBNormalUDFSpecs() {
		got[spec.Name] = spec.ResultKind
	}

	for _, tc := range []struct {
		name string
		kind DuckDBUDFResultKind
	}{
		{name: "ignore_nulls", kind: DuckDBResultVarchar},
		{name: "distinct", kind: DuckDBResultVarchar},
		{name: "is_true", kind: DuckDBResultBoolean},
		{name: "is_false", kind: DuckDBResultBoolean},
		{name: "subscript", kind: DuckDBResultVarchar},
		{name: "extract_date", kind: DuckDBResultVarchar},
	} {
		kind, ok := got[tc.name]
		if !ok {
			t.Fatalf("DuckDBNormalUDFSpecs missing %q", tc.name)
		}
		if kind != tc.kind {
			t.Fatalf("DuckDBNormalUDFSpecs[%q] kind = %v; want %v", tc.name, kind, tc.kind)
		}
	}
}
