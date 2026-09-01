//go:build e2e

package e2e

import (
	"context"
	"testing"
	"time"

	"github.com/kettleofketchup/per-user-container-operator/internal/identity"
)

// TestIsolationStorageWriteReadAcrossIdentities is the plan's assertion 1:
// a marker written into identity A's workspace volume must be unreachable
// from identity B's own workspace pod.
//
// Both halves of the observation channel carry their own positive control,
// in this same test, before the absence means anything (task-13b-brief.md
// Step 1 item 1):
//
//   - Writer side: A reads its own marker back, in A's own workspace pod,
//     immediately after writing it. Without this, a write that silently
//     failed (a cold start that never completed, a non-zero exit) would
//     make B's later "marker not found" true for the wrong reason -- the
//     marker never existed anywhere, not because per-user storage kept it out
//     of B's reach.
//   - Reader side: B's own workspace reaches Ready and B reads back a marker
//     B wrote itself, under a distinct filename from A's. Only once that
//     succeeds does B's failure to find A's marker prove isolation rather
//     than a broken exec/session/volume path on B's side.
func TestIsolationStorageWriteReadAcrossIdentities(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	ns := globalEnv.Namespaces[0]
	clientPod := "puc-e2e-client"
	const (
		identityA = "iso-write-read-A"
		identityB = "iso-write-read-B"
		markerA   = "/workspace/marker-a-iso1"
		markerB   = "/workspace/marker-b-iso1"
		contentA  = "owned-by-A"
		contentB  = "owned-by-B"
	)

	// --- Cold-start A and locate its workspace pod. ---
	code, err := coldStart(globalEnv, ns, clientPod, smokeApp, identityA)
	if err != nil {
		t.Fatalf("cold start identity A: %v", err)
	}
	if code != "200" {
		t.Fatalf("cold start identity A: router returned %q, want 200", code)
	}
	userKeyA := identity.UserKey(ns, smokeApp, identityA)
	podA, err := findWorkspacePod(ctx, globalClient, ns, smokeApp, userKeyA)
	if err != nil {
		t.Fatalf("find A's workspace pod: %v", err)
	}

	// --- Writer-side positive control: A writes, then A reads its own
	// marker back in its own pod. ---
	if err := writeMarker(globalEnv.Kubeconfig, ns, podA.Name, markerA, contentA); err != nil {
		t.Fatalf("A: write its own marker: %v", err)
	}
	gotA, err := readMarker(globalEnv.Kubeconfig, ns, podA.Name, markerA)
	if err != nil {
		t.Fatalf("POSITIVE CONTROL (writer side) failed: A could not read back its own marker: %v", err)
	}
	if gotA != contentA {
		t.Fatalf("POSITIVE CONTROL (writer side) failed: A read back %q, want %q", gotA, contentA)
	}
	t.Logf("writer-side positive control observed passing: A read back its own marker (%q)", gotA)

	// --- Cold-start B and locate its workspace pod. ---
	code, err = coldStart(globalEnv, ns, clientPod, smokeApp, identityB)
	if err != nil {
		t.Fatalf("cold start identity B: %v", err)
	}
	if code != "200" {
		t.Fatalf("cold start identity B: router returned %q, want 200", code)
	}
	userKeyB := identity.UserKey(ns, smokeApp, identityB)
	if userKeyB == userKeyA {
		t.Fatalf("identity A and B derived the same userKey (%s) -- this test cannot tell isolation from a coincidence", userKeyA)
	}
	podB, err := findWorkspacePod(ctx, globalClient, ns, smokeApp, userKeyB)
	if err != nil {
		t.Fatalf("find B's workspace pod: %v", err)
	}
	if podB.Name == podA.Name {
		t.Fatalf("A and B resolved to the SAME workspace pod (%s) -- per-user pod isolation is entirely absent", podA.Name)
	}

	// --- Reader-side positive control: B writes and reads back its OWN
	// marker, under a distinct filename from A's. ---
	if err := writeMarker(globalEnv.Kubeconfig, ns, podB.Name, markerB, contentB); err != nil {
		t.Fatalf("B: write its own marker: %v", err)
	}
	gotB, err := readMarker(globalEnv.Kubeconfig, ns, podB.Name, markerB)
	if err != nil {
		t.Fatalf("POSITIVE CONTROL (reader side) failed: B could not read back its own marker: %v", err)
	}
	if gotB != contentB {
		t.Fatalf("POSITIVE CONTROL (reader side) failed: B read back %q, want %q", gotB, contentB)
	}
	t.Logf("reader-side positive control observed passing: B read back its own marker (%q)", gotB)

	// --- The absence assertion itself: B must NOT be able to read A's
	// marker from B's own workspace pod/volume. ---
	if leaked, err := readMarker(globalEnv.Kubeconfig, ns, podB.Name, markerA); err == nil {
		t.Fatalf("ISOLATION VIOLATION: B read A's marker (%q) from B's own workspace pod: per-user storage isolation is broken", leaked)
	}
	t.Logf("absence assertion observed passing: B cannot read A's marker from its own workspace")
}
