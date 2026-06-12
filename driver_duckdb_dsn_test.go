package googlesqlite

import "testing"

func TestDuckDBDSNPreservesMemoryLimitParams(t *testing.T) {
	tests := []struct {
		name string
		dsn  string
		want string
	}{
		{
			name: "file max memory",
			dsn:  "file:/tmp/example-project.db?cache=shared&max_memory=3GB",
			want: "/tmp/example-project.db?max_memory=3GB",
		},
		{
			name: "file memory limit alias",
			dsn:  "file:/tmp/example-project.db?cache=shared&memory_limit=3072MB",
			want: "/tmp/example-project.db?memory_limit=3072MB",
		},
		{
			name: "memory max memory",
			dsn:  "file::memory:?cache=shared&max_memory=512MB",
			want: ":memory:?max_memory=512MB",
		},
		{
			name: "sqlite params stripped",
			dsn:  "file:/tmp/example-project.db?cache=shared&mode=memory",
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := duckdbDSN(tt.dsn); got != tt.want {
				t.Fatalf("duckdbDSN(%q) = %q; want %q", tt.dsn, got, tt.want)
			}
		})
	}
}
