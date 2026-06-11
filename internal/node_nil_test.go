package internal

// Regression test for issue #18 (recoverable nil-deref under
// cancellation churn): resolved-AST accessors return (nil, nil) when a
// child handle reads as 0, which happens without warning when the
// analyzer module is degraded (heap exhaustion, a trap mid-walk).
// newNode(nil) used to return a nil Formatter interface, so the very
// next FormatSQL call was a nil-interface method panic — the panic the
// churn campaign recovered at the driver seam. newNode must instead
// return a Formatter that fails loudly as an ordinary error.

import (
	"context"
	"strings"
	"testing"
)

func TestNewNodeNilReturnsLoudFormatter(t *testing.T) {
	f := newNode(nil)
	if f == nil {
		t.Fatal("newNode(nil) returned a nil Formatter; the next FormatSQL call would panic on a nil interface")
	}
	sql, err := f.FormatSQL(context.Background())
	if err == nil {
		t.Fatalf("FormatSQL on the nil-node Formatter must return an error, got %q", sql)
	}
	if !strings.Contains(err.Error(), "nil resolved node") {
		t.Fatalf("error should identify the nil resolved node, got: %v", err)
	}
}
