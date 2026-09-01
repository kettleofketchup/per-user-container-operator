//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/kettleofketchup/per-user-container-operator/internal/identity"
)

// TestIsolationMetricsScrapedWithNetworkPolicies is the plan's assertion 4.
//
// The positive half is not "the target appears in /api/v1/targets": target
// DISCOVERY is done by the Prometheus operator from the ServiceMonitor and
// EndpointSlices and happens whether or not the scrape connection actually
// succeeds -- a router NetworkPolicy missing the pod-CIDR-on-metrics-port
// ingress rule leaves the target listed with health "down" while a naive
// assertion (just checking the target list) goes green. So this asserts
// `up{...} == 1` for BOTH the router and controller series, queried from
// Prometheus itself, plus a non-zero sample from a series each binary only
// emits from real traffic (never from the scrape handshake alone) -- proof
// the connection did not just succeed, it carried real samples.
//
// The RED phase, in the same test, per this dispatch's Step 1 preamble:
// with the controller quiesced, patch the router NetworkPolicy's pod-CIDR
// metrics ingress rule out (a field of a reconciled object, so only
// expressible as a patch -- deleting the whole policy would also break the
// router's main ingress rule 0), require up == 0 once Prometheus has had a
// chance to re-scrape, then restore the controller and wait for the rule to
// be reconciled back (confirmed elsewhere in this dispatch: unlike a
// workspace's own NetworkPolicies, the router NetworkPolicy IS unconditionally
// re-ensured on every PerUserApp reconcile).
func TestIsolationMetricsScrapedWithNetworkPolicies(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	ns := globalEnv.Namespaces[0]
	clientPod := "puc-e2e-client"
	identityM := fmt.Sprintf("iso-metrics-%d", time.Now().UnixNano())

	promIP, err := getServiceClusterIP(ctx, globalClient, "monitoring", "app=kube-prometheus-stack-prometheus")
	if err != nil {
		t.Fatalf("resolve Prometheus ClusterIP: %v", err)
	}

	// --- Positive: cold start a workspace and confirm DNS, the declared
	// workspaceEgress target, and readiness all work with the
	// NetworkPolicies applied. ---
	if code, err := coldStart(globalEnv, ns, clientPod, smokeApp, identityM); err != nil || code != "200" {
		t.Fatalf("cold start identity M: code=%q err=%v", code, err)
	}
	userKeyM := identity.UserKey(ns, smokeApp, identityM)
	ws, err := waitWorkspaceReady(ctx, ns, identity.ChildName(smokeApp, userKeyM))
	if err != nil {
		t.Fatalf("readiness: %v", err)
	}
	if ws.Status.Phase != "Ready" {
		t.Fatalf("readiness: workspace phase is %q, want Ready", ws.Status.Phase)
	}
	podM, err := findWorkspacePod(ctx, globalClient, ns, smokeApp, userKeyM)
	if err != nil {
		t.Fatalf("find M's workspace pod: %v", err)
	}
	if out, err := runInPod(globalEnv.Kubeconfig, ns, podM.Name, "nslookup", "kubernetes.default.svc.cluster.local"); err != nil {
		t.Fatalf("DNS egress: nslookup failed with NetworkPolicies applied: %v (output: %s)", err, out)
	}
	t.Log("positive (workspace side) observed passing: DNS resolves with NetworkPolicies applied")

	// The fixture's declared workspaceEgress peer selects the Prometheus
	// POD directly (namespace + pod labels), not its Service's ClusterIP --
	// see NetworkSpec.WorkspaceEgress's doc comment
	// (api/v1alpha1/peruserapp_types.go) for why an ipBlock naming a
	// ClusterIP would silently drop (NetworkPolicy egress evaluates against
	// the POST-DNAT destination) while a selector peer resolving to the
	// pod's real IP is evaluated correctly. Probed here at the pod's real
	// IP:port, mirroring exactly what the policy actually admits.
	promPodIP, err := getPodIP(ctx, globalClient, "monitoring", "app.kubernetes.io/name=prometheus")
	if err != nil {
		t.Fatalf("resolve Prometheus pod IP: %v", err)
	}
	egressProbe := probeHTTP(globalEnv.Kubeconfig, ns, podM.Name, fmt.Sprintf("http://%s:9090/", promPodIP), nil, probeBlockTimeoutSecs)
	if !egressProbe.Reached {
		t.Fatalf("declared workspaceEgress target (Prometheus pod %s:9090) unreachable from the workspace pod with NetworkPolicies applied", promPodIP)
	}
	t.Logf("positive (workspace side) observed passing: declared workspaceEgress target reachable (code %s)", egressProbe.Code)

	// --- Positive: both metrics endpoints are ACTUALLY scraped (up == 1),
	// not merely discovered, plus a non-zero sample from each binary. ---
	routerUpQuery := fmt.Sprintf(`up{service="%s-router",namespace="%s"}`, smokeApp, ns)
	controllerUpQuery := fmt.Sprintf(`up{service="%s-metrics",namespace="%s"}`, operatorDeployment, operatorNamespace)

	requireUp := func(query string, want float64, timeout time.Duration) {
		t.Helper()
		waitCtx, waitCancel := context.WithTimeout(ctx, timeout)
		defer waitCancel()
		var last []promQueryResult
		err := pollUntil(waitCtx, 5*time.Second, func() (bool, error) {
			results, err := promQuery(globalEnv.Kubeconfig, ns, clientPod, promIP, query)
			if err != nil {
				return false, err
			}
			last = results
			for _, r := range results {
				if r.Value != want {
					return false, nil
				}
			}
			return len(results) > 0, nil
		})
		if err != nil {
			t.Fatalf("prometheus query %q did not settle on %v within %s: %v (last result: %+v)", query, want, timeout, err, last)
		}
	}

	requireUp(routerUpQuery, 1, 30*time.Second)
	requireUp(controllerUpQuery, 1, 30*time.Second)

	// Polled, not a single query: a counter's exposed value only updates on
	// Prometheus's NEXT scrape (up to the 30s scrape interval away), so a
	// router pod that was recently restarted (e.g. by an earlier run of this
	// same test's own RED phase, in a full-package run) can have already
	// reset its in-memory counters to 0 with up==1 already confirmed from a
	// scrape that predates this test's own cold-start request. A single,
	// non-retried query observed exactly this race directly on this
	// cluster.
	requireNonZeroSample := func(query string) {
		t.Helper()
		waitCtx, waitCancel := context.WithTimeout(ctx, 40*time.Second)
		defer waitCancel()
		var last []promQueryResult
		err := pollUntil(waitCtx, 5*time.Second, func() (bool, error) {
			results, err := promQuery(globalEnv.Kubeconfig, ns, clientPod, promIP, query)
			if err != nil {
				return false, err
			}
			last = results
			for _, r := range results {
				if r.Value > 0 {
					return true, nil
				}
			}
			return false, nil
		})
		if err != nil {
			t.Fatalf("prometheus query %q never returned a series with a non-zero sample within 40s: %v (last result: %+v)", query, err, last)
		}
	}
	requireNonZeroSample("puc_controller_leader")
	requireNonZeroSample(fmt.Sprintf(`puc_router_requests_total{namespace="%s"}`, ns))
	t.Log("positive observed passing: up==1 for both router and controller targets, non-zero sample from each binary")

	// --- RED phase: quiesce the controller, patch the pod-CIDR metrics
	// ingress rule out of the router NetworkPolicy, require up == 0 once
	// Prometheus has re-scraped, then restore and require up == 1 again. ---
	restore := quiesceController(t, ctx)

	routerPolicyName := smokeApp + "-router"
	patch := []byte(`[{"op":"remove","path":"/spec/ingress/1"}]`)
	if _, err := globalClient.NetworkingV1().NetworkPolicies(ns).Patch(ctx, routerPolicyName, types.JSONPatchType, patch, metav1.PatchOptions{}); err != nil {
		t.Fatalf("patch out the router NetworkPolicy's metrics ingress rule: %v", err)
	}

	// A NetworkPolicy change does not tear down a connection conntrack
	// already has ESTABLISHED: Prometheus's scrape client keeps its
	// connection to the router's metrics port alive across scrape
	// intervals, so up would keep reading 1 over that same old connection
	// even with the admitting rule now gone (observed directly: the first
	// version of this test waited 90s on exactly this and never saw 0).
	// Deleting the router pod forces a new pod IP and therefore a genuinely
	// NEW connection attempt, which the missing ingress rule now blocks.
	routerPodSelector := fmt.Sprintf("%s=%s,%s=%s", "puc.kettleofketchup/app", smokeApp, "puc.kettleofketchup/component", "router")
	if err := globalClient.CoreV1().Pods(ns).DeleteCollection(ctx, metav1.DeleteOptions{}, metav1.ListOptions{LabelSelector: routerPodSelector}); err != nil {
		t.Fatalf("delete router pod to force a new scrape connection: %v", err)
	}
	if err := waitDeploymentReady(ctx, globalClient, ns, smokeApp+"-router"); err != nil {
		t.Fatalf("wait for router Deployment to recover after forced pod restart: %v", err)
	}

	requireUp(routerUpQuery, 0, 90*time.Second)
	t.Log("RED phase observed: up==0 for the router target once its metrics ingress rule was patched out")

	restore()
	// 60s, not 30s: quiesceController's restore only waits for the
	// Deployment's ReadyReplicas, and this controller does leader election
	// on top of that (observed directly: ~20-25s from pod Ready to the
	// first post-election reconcile) before it reconciles anything at all.
	waitCtx, waitCancel := context.WithTimeout(ctx, 60*time.Second)
	defer waitCancel()
	err = pollUntil(waitCtx, 2*time.Second, func() (bool, error) {
		np, err := globalClient.NetworkingV1().NetworkPolicies(ns).Get(ctx, routerPolicyName, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		return len(np.Spec.Ingress) == 2, nil
	})
	if err != nil {
		t.Fatalf("wait for the router NetworkPolicy's metrics ingress rule to be reconciled back: %v", err)
	}

	requireUp(routerUpQuery, 1, 90*time.Second)
	t.Log("post-restore observed: up==1 for the router target again once its metrics ingress rule was reconciled back")
}
