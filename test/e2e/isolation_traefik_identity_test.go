//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/kettleofketchup/per-user-container-operator/internal/identity"
)

// traefikNoPinIngressRoute is a harness addition this assertion needs and
// Task 13a's Step 0 did not create: a second route to the SAME router
// Service, carrying only the caller-auth middleware (never
// puc-e2e-identity-pin), on a distinct Host. It exists so this test can
// prove the observation channel and the victim's marker both work (this
// test's RED/control phase) without ever mutating the shared, already-in-use
// puc-e2e-app IngressRoute -- a separate object means a failure here can
// never leave the harness's pinned route broken for any other test.
// Created idempotently by this test itself (kubectlHost apply), not by
// kind-up.sh: see this dispatch's report for why it was added here instead
// of in the Step 0 harness.
const traefikNoPinIngressRouteYAML = `
apiVersion: traefik.io/v1alpha1
kind: IngressRoute
metadata:
  name: puc-e2e-app-nopin
  namespace: %s
spec:
  entryPoints:
    - web
  routes:
    - match: Host(%s)
      kind: Rule
      services:
        - name: e2e-app-router
          port: 8080
      middlewares:
        - name: puc-e2e-caller-auth
`

const (
	traefikPinnedHost = "e2e-app.puc-e2e.local"
	traefikNoPinHost  = "e2e-app-nopin.puc-e2e.local"
)

// TestIsolationTraefikIdentityPinning is the plan's assertion 3.
// task-13b-brief.md's closing section explains at length why spec 724's
// literal "require failure" is wrong against a CORRECTLY configured proxy:
// when the identity header is set via a header-replacing middleware (this
// harness's Traefik stand-in for forward-auth's authResponseHeaders), the
// proxy deletes the client-supplied copy and the router sees exactly one
// value -- the attacker's own -- and legitimately returns 200, proxied to
// the attacker's own workspace. So this test does NOT assert "must fail";
// it asserts the workspace REACHED is the attacker's own, proved by the
// victim's marker being absent AND the attacker's own marker being present
// in the same pinned session.
//
// It carries its own RED phase, in the same test, the same way assertions 2
// and 4 do: without it, the pinned middleware *always* replaces the header
// with "A", so "attacker marker present, victim marker absent" would be
// true even with identity.Extract stubbed out or no identity logic at all --
// the assertion would have no failure mode in the system under test. Running
// the identical probe first against a route with NO identity-pinning
// middleware, and requiring the victim's marker to be reachable there,
// proves the observation channel and both markers actually exist before the
// pinned result is trusted.
func TestIsolationTraefikIdentityPinning(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	ns := globalEnv.Namespaces[0]
	clientPod := "puc-e2e-client"

	// Attacker "A" and victim "V" are fixed, non-negotiable values: the
	// harness's puc-e2e-identity-pin middleware (kind-up.sh) hardcodes
	// X-User-Id: A, and kind-up.sh's own Step 0 setup provisions "V" off the
	// Traefik path specifically so this assertion has a victim identity that
	// never once passed through the pinning middleware.
	const identityA = "A"
	const identityV = "V"

	if code, err := coldStart(globalEnv, ns, clientPod, smokeApp, identityA); err != nil || code != "200" {
		t.Fatalf("cold start (ensure Ready) identity A: code=%q err=%v", code, err)
	}
	if code, err := coldStart(globalEnv, ns, clientPod, smokeApp, identityV); err != nil || code != "200" {
		t.Fatalf("cold start (ensure Ready) identity V: code=%q err=%v", code, err)
	}
	userKeyA := identity.UserKey(ns, smokeApp, identityA)
	userKeyV := identity.UserKey(ns, smokeApp, identityV)
	podA, err := findWorkspacePod(ctx, globalClient, ns, smokeApp, userKeyA)
	if err != nil {
		t.Fatalf("find A's workspace pod: %v", err)
	}
	podV, err := findWorkspacePod(ctx, globalClient, ns, smokeApp, userKeyV)
	if err != nil {
		t.Fatalf("find V's workspace pod: %v", err)
	}

	// Markers are written into the PVC mount at /workspace, which
	// fixture-workspace-nginx.conf serves as nginx's document root (see that
	// file's doc comment: the image's own /usr/share/nginx/html is
	// root-owned and this fixture's pinned uid 101 cannot write there): this
	// assertion is about which workspace a given identity's request routes
	// to, not about storage persistence, and an HTTP-fetchable file at a
	// fixed path is the direct observation channel the router itself
	// proxies through.
	const markerV = "assertion3-marker-v"
	const markerA = "assertion3-marker-a"
	if err := writeMarker(globalEnv.Kubeconfig, ns, podV.Name, "/workspace/"+markerV, "owned-by-V"); err != nil {
		t.Fatalf("write V's marker: %v", err)
	}
	if err := writeMarker(globalEnv.Kubeconfig, ns, podA.Name, "/workspace/"+markerA, "owned-by-A"); err != nil {
		t.Fatalf("write A's marker: %v", err)
	}

	traefikIP, err := getServiceClusterIP(ctx, globalClient, "traefik", "app.kubernetes.io/name=traefik")
	if err != nil {
		t.Fatalf("resolve Traefik ClusterIP: %v", err)
	}

	// Create (idempotently) the un-pinned route this test's control phase
	// needs -- see traefikNoPinIngressRouteYAML's doc comment.
	yaml := fmt.Sprintf(traefikNoPinIngressRouteYAML, ns, "`"+traefikNoPinHost+"`")
	if out, err := kubectlApplyStdin(globalEnv.Kubeconfig, yaml); err != nil {
		t.Fatalf("create no-pin IngressRoute: %v (output: %s)", err, out)
	}

	fetch := func(host, marker string) probeResult {
		url := fmt.Sprintf("http://%s:80/%s", traefikIP, marker)
		return probeHTTP(globalEnv.Kubeconfig, ns, clientPod, url, map[string]string{
			"Host":         host,
			identityHeader: identityV,
		}, probeBlockTimeoutSecs)
	}

	// --- RED / control phase: through the UN-pinned route, X-User-Id: V
	// passes through unmodified and must reach V's own workspace -- proving
	// the observation channel and both markers actually exist. ---
	ctrl := fetch(traefikNoPinHost, markerV)
	if !ctrl.Reached {
		t.Fatalf("CONTROL FAILED: could not even reach the un-pinned route: %s", ctrl.Raw)
	}
	if ctrl.Code != "200" {
		t.Fatalf("CONTROL FAILED: victim V's own marker is not reachable through the un-pinned route (code %s) -- the observation channel itself is broken, so the pinned result below would prove nothing", ctrl.Code)
	}
	t.Logf("control observed passing: un-pinned route with X-User-Id: V reached V's own marker (code %s)", ctrl.Code)

	// --- The assertion itself: through the PINNED route, a client-supplied
	// X-User-Id: V is replaced with "A" by the middleware. The request must
	// reach ATTACKER A's own workspace: V's marker absent, A's marker
	// present, in this same pinned session. ---
	victimProbe := fetch(traefikPinnedHost, markerV)
	if victimProbe.Reached && victimProbe.Code == "200" {
		t.Fatalf("ISOLATION VIOLATION: pinned route (identity forged to V) reached the VICTIM's marker (code %s) -- the router proxied the attacker to another user's workspace", victimProbe.Code)
	}
	attackerProbe := fetch(traefikPinnedHost, markerA)
	if !attackerProbe.Reached || attackerProbe.Code != "200" {
		t.Fatalf("pinned route did not reach the ATTACKER's own marker (reached=%v code=%s) -- expected a 200 proxied to A's own workspace", attackerProbe.Reached, attackerProbe.Code)
	}
	t.Logf("assertion observed passing: pinned route (forged X-User-Id: V) reached attacker A's own workspace only (victim marker code=%s, attacker marker code=%s)", victimProbe.Code, attackerProbe.Code)
}
