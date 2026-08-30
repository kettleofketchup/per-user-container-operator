package v1alpha1

import (
	"os"
	"strings"
	"testing"
)

func TestCELMarkersPresent(t *testing.T) {
	b, err := os.ReadFile("peruserapp_types.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	for _, want := range []string{
		"self.metadata.name.size() <= 27",
		"pod name", // the arithmetic, so the failure is diagnosable
		"quantity(self.size).compareTo(quantity(oldSelf.size)) >= 0", // storage shrink on update
		"self.workspaceEgress.all(r, r.to.size() > 0 && r.to.all(p, has(p.ipBlock)))",
		"duration(self.reapInterval) < duration(self.idleTimeout)",
	} {
		if !strings.Contains(src, want) {
			t.Fatalf("missing CEL rule %q", want)
		}
	}
	b, err = os.ReadFile("workspace_types.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "self == oldSelf") {
		t.Fatal("Workspace.spec.userKey must be create-only/immutable; a stray edit re-points a workspace at another user's PVC name")
	}
}
