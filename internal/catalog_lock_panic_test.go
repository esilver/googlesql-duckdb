package internal

// Regression test for issue #18 (lock sections without defer-unlock
// leak the lock when a panic crosses them): NewCatalog's
// wasmAnalyzeMu-guarded proto FFI block used a bare Lock/Unlock pair,
// so an analyzer abort inside registerWellKnownProtos orphaned
// wasmAnalyzeMu — every later Analyze in the process then blocked
// forever even when the caller recovered the panic.

import (
	"testing"
	"time"
)

func TestNewCatalogProtoBlockPanicDoesNotLeakWasmAnalyzeMu(t *testing.T) {
	newCatalogProtoBlockHookForTest = func() {
		panic("synthetic analyzer abort inside the proto FFI block")
	}
	defer func() { newCatalogProtoBlockHookForTest = nil }()

	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("the synthetic panic did not propagate out of NewCatalog; the hook did not run")
			}
		}()
		_ = NewCatalog(nil)
	}()

	// The lock must be re-acquirable after the recovered panic. Poll
	// with TryLock under a deadline so a regression fails fast instead
	// of deadlocking the suite.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if wasmAnalyzeMu.TryLock() {
			wasmAnalyzeMu.Unlock()
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("issue #18 regression: wasmAnalyzeMu is still held after a panic crossed NewCatalog's proto block; the deferred unlock is gone")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
