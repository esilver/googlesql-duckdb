package internal

import (
	"fmt"
	"os"
	"testing"

	googlesql "github.com/goccy/go-googlesql"
)

// TestMain initialises the go-googlesql analyzer runtime exactly once
// for every test in package internal. The TypeFactory / Analyzer /
// Catalog surfaces all talk through that module; calls before Init
// panic with a nil-pointer dereference.
//
// The analyzer is a wasm2go-transpiled native-Go module (per-chunk
// amd64.s / arm64.s plus a pure-Go fallback selected by build
// constraint), so there is no wazero compilation step to configure:
// the old GOOGLESQLITE_WASM_COMPILATION_MODE / _WASM_CACHE_DIR
// environment variables are ignored.
func TestMain(m *testing.M) {
	if err := googlesql.Init(); err != nil {
		fmt.Fprintf(os.Stderr, "googlesql init failed: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}
