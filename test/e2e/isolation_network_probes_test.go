//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/kettleofketchup/per-user-container-operator/internal/identity"
)

// probeBlockTimeoutSecs bounds each blocked probe's wait. Calico's default
// deny action is a silent DROP (no TCP RST), so a correctly-blocked
// connection times out rather than being refused instantly; 8s is well
// above the ~6s this was observed to need on this harness's kind cluster and
// still small enough that four sequential blocked probes, in both the
// negative and (after being unblocked) restored-negative phase, do not blow
// the suite's time budget.
const probeBlockTimeoutSecs = 8

// appBRouterProbePod is the debug pod kind-up.sh creates in NS1 carrying
// RouterPodLabels("e2e-app-b") -- the peer assertion 2's fourth (cross-app)
// probe must run from, per task-13b-brief.md item 2: the real e2e-app-b
// router pod runs the operator's CGO_ENABLED=0 image with no /bin/sh and no
// curl, so exec'ing into it would make "binary missing" indistinguishable
// from "NetworkPolicy refused it".
const appBRouterProbePod = "puc-e2e-app-b-router-probe"

// TestIsolationNetworkProbes is the plan's assertion 2: four independent
// connectivity probes that must all fail with isolation intact, each of
// which shares ONE positive control (task-13b-brief.md item 2): with the
// controller quiesced, delete both the affected workspaces' NetworkPolicies
// AND the fixture app's router NetworkPolicy, require all four probes to
// SUCCEED, then restore the controller, wait for every deleted policy to be
// reconciled back, and require all four probes to fail again.
//
// Both workspace policy sets (A's and B's) are deleted together: probe 1
// needs A's egress AND B's ingress both absent to succeed, and deleting only
// one leaves the other still refusing the connection -- a positive control
// specified as "require each to succeed" would go red for the wrong reason,
// and the brief calls out exactly this as the shortcut that produces four
// vacuously-passing absence assertions instead.
func TestIsolationNetworkProbes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	ns := globalEnv.Namespaces[0]
	clientPod := "puc-e2e-client"
	nonce := time.Now().UnixNano()
	identityA := fmt.Sprintf("iso-probe2-A-%d", nonce)
	identityB := fmt.Sprintf("iso-probe2-B-%d", nonce)

	// --- Cold-start A and B. ---
	if code, err := coldStart(globalEnv, ns, clientPod, smokeApp, identityA); err != nil || code != "200" {
		t.Fatalf("cold start identity A: code=%q err=%v", code, err)
	}
	if code, err := coldStart(globalEnv, ns, clientPod, smokeApp, identityB); err != nil || code != "200" {
		t.Fatalf("cold start identity B: code=%q err=%v", code, err)
	}
	userKeyA := identity.UserKey(ns, smokeApp, identityA)
	userKeyB := identity.UserKey(ns, smokeApp, identityB)
	podA, err := findWorkspacePod(ctx, globalClient, ns, smokeApp, userKeyA)
	if err != nil {
		t.Fatalf("find A's workspace pod: %v", err)
	}

	// --- Resolve the non-fixture target: kube-prometheus-stack's
	// Alertmanager ClusterIP (Step 0 item 10 names it explicitly; it is
	// installed and resolvable, and deliberately absent from the fixture's
	// spec.network.workspaceEgress). ---
	alertmanagerIP, err := getServiceClusterIP(ctx, globalClient, "monitoring", "app=kube-prometheus-stack-alertmanager")
	if err != nil {
		t.Fatalf("resolve Alertmanager ClusterIP: %v", err)
	}

	svcAURL := fmt.Sprintf("http://%s.%s.svc.cluster.local:8080/", identity.ChildName(smokeApp, userKeyA), ns)
	svcBURL := fmt.Sprintf("http://%s.%s.svc.cluster.local:8080/", identity.ChildName(smokeApp, userKeyB), ns)
	routerURLA := routerURL(smokeApp, ns)
	alertmanagerURL := fmt.Sprintf("http://%s:9093/", alertmanagerIP)

	runProbes := func() (p1, p2, p3, p4 probeResult) {
		p1 = probeHTTP(globalEnv.Kubeconfig, ns, podA.Name, svcBURL, nil, probeBlockTimeoutSecs)
		p2 = probeHTTP(globalEnv.Kubeconfig, ns, podA.Name, routerURLA, map[string]string{identityHeader: "forged-identity"}, probeBlockTimeoutSecs)
		p3 = probeHTTP(globalEnv.Kubeconfig, ns, podA.Name, alertmanagerURL, nil, probeBlockTimeoutSecs)
		p4 = probeHTTP(globalEnv.Kubeconfig, ns, appBRouterProbePod, svcAURL, nil, probeBlockTimeoutSecs)
		return
	}

	// --- Baseline (isolation intact): all four must fail. ---
	p1, p2, p3, p4 := runProbes()
	if p1.Reached {
		t.Fatalf("BASELINE FAILED before any control ran: probe 1 (workspace A -> workspace B) reached (code %s); isolation is not intact even before the positive control", p1.Code)
	}
	if p2.Reached {
		t.Fatalf("BASELINE FAILED before any control ran: probe 2 (workspace A -> router) reached (code %s)", p2.Code)
	}
	if p3.Reached {
		t.Fatalf("BASELINE FAILED before any control ran: probe 3 (workspace A -> Alertmanager) reached (code %s)", p3.Code)
	}
	if p4.Reached {
		t.Fatalf("BASELINE FAILED before any control ran: probe 4 (app-b router pod -> workspace A) reached (code %s)", p4.Code)
	}
	t.Log("baseline observed: all four probes blocked with isolation intact")

	// --- Positive control: quiesce the controller, delete both users'
	// workspace NetworkPolicies and the fixture app's router NetworkPolicy,
	// require all four probes to SUCCEED. ---
	restore := quiesceController(t, ctx)

	nameA := identity.ChildName(smokeApp, userKeyA)
	nameB := identity.ChildName(smokeApp, userKeyB)
	deletedPolicies := []string{
		nameA + "-ingress", nameA + "-egress",
		nameB + "-ingress", nameB + "-egress",
		smokeApp + "-router",
	}
	for _, name := range deletedPolicies {
		if err := deleteNetworkPolicy(ctx, globalClient, ns, name); err != nil {
			t.Fatalf("delete NetworkPolicy %s/%s: %v", ns, name, err)
		}
	}

	p1, p2, p3, p4 = runProbes()
	if !p1.Reached {
		t.Fatalf("POSITIVE CONTROL FAILED: probe 1 (workspace A -> workspace B) still blocked with both users' NetworkPolicies deleted")
	}
	if !p2.Reached {
		t.Fatalf("POSITIVE CONTROL FAILED: probe 2 (workspace A -> router) still blocked with the workspace and router NetworkPolicies deleted")
	}
	if !p3.Reached {
		t.Fatalf("POSITIVE CONTROL FAILED: probe 3 (workspace A -> Alertmanager) still blocked with A's workspace NetworkPolicies deleted")
	}
	if !p4.Reached {
		t.Fatalf("POSITIVE CONTROL FAILED: probe 4 (app-b router pod -> workspace A) still blocked with A's workspace NetworkPolicies deleted")
	}
	t.Logf("positive control observed passing (isolation absent -> all four probes reached): p1=%s p2=%s p3=%s p4=%s", p1.Code, p2.Code, p3.Code, p4.Code)

	// --- Restore: scale the controller back up, wait for every deleted
	// policy to be reconciled back, then require all four probes to fail
	// again. This is a genuine check, not a formality: PerUserAppReconciler
	// unconditionally re-ensures the router NetworkPolicy on every reconcile
	// (ensureRouterWorkload, internal/controller/peruserapp_controller.go),
	// but RenderWorkspaceNetworkPolicies is only ever applied from
	// reconcilePending (internal/controller/workspace_controller.go) -- a
	// Ready workspace's own ingress/egress NetworkPolicies have no
	// reconciliation path that re-creates them if deleted. A short,
	// per-policy timeout below (rather than this test's full context) turns
	// that gap into a clear, attributable failure instead of the whole test
	// hanging until the outer deadline.
	restore()
	waitCtx, waitCancel := context.WithTimeout(ctx, 30*time.Second)
	defer waitCancel()
	for _, name := range deletedPolicies {
		if err := waitNetworkPolicyExists(waitCtx, globalClient, ns, name); err != nil {
			t.Fatalf("wait for NetworkPolicy %s/%s to be reconciled back after restore: %v -- if this is one of %v (workspace-scoped), see this test's comment: WorkspaceReconciler has no reconcile path that re-creates a Ready workspace's own NetworkPolicies once deleted, only the router NetworkPolicy self-heals", ns, name, err, []string{nameA + "-ingress", nameA + "-egress", nameB + "-ingress", nameB + "-egress"})
		}
	}

	p1, p2, p3, p4 = runProbes()
	if p1.Reached {
		t.Fatalf("ISOLATION VIOLATION after restore: probe 1 (workspace A -> workspace B) reached (code %s)", p1.Code)
	}
	if p2.Reached {
		t.Fatalf("ISOLATION VIOLATION after restore: probe 2 (workspace A -> router) reached (code %s)", p2.Code)
	}
	if p3.Reached {
		t.Fatalf("ISOLATION VIOLATION after restore: probe 3 (workspace A -> Alertmanager) reached (code %s)", p3.Code)
	}
	if p4.Reached {
		t.Fatalf("ISOLATION VIOLATION after restore: probe 4 (app-b router pod -> workspace A) reached (code %s)", p4.Code)
	}
	t.Log("post-restore observed: all four probes blocked again with isolation reconciled back")
}
