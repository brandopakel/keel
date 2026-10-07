package core

import "testing"

// TestLayoutControlKeepsPackageFunctions keeps CloseAOF and ResetStores
// linked, as develop's benchmarks kept them by using them as values, so that
// the product code lands where it did on develop. A layout control build
// only; never merged. The branch is never taken.
func TestLayoutControlKeepsPackageFunctions(t *testing.T) {
	if t == nil {
		t.Cleanup(ResetStores)
		t.Cleanup(func() { _ = CloseAOF() })
		defer CloseAOF()
	}
}
