//go:build e2e

package e2e

import (
	"context"
	"testing"
)

// TestCNIIsCalico exposes TestMain's first precondition as an ordinary,
// individually selectable test: `go test -tags e2e -run TestCNIIsCalico
// ./test/e2e/...`. TestMain (main_test.go) already enforces this same check
// unconditionally, before m.Run() is called at all, so this wrapper exists
// for direct observability of a RED result, not because the suite's
// enforcement depends on it being selected.
func TestCNIIsCalico(t *testing.T) {
	if err := checkCNIIsCalico(context.Background(), globalClient); err != nil {
		t.Fatal(err)
	}
}
