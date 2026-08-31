//go:build e2e

package e2e

import (
	"context"
	"testing"
)

// TestRouterSmoke exposes TestMain's second precondition as an ordinary,
// individually selectable test, for the same reason TestCNIIsCalico does
// (see cni_test.go): TestMain already enforces this unconditionally before
// m.Run() is called.
func TestRouterSmoke(t *testing.T) {
	if err := checkRouterSmoke(context.Background(), globalClient, globalEnv); err != nil {
		t.Fatal(err)
	}
}
