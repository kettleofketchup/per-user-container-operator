//go:build envtest

package envtest

import (
	"context"
	"fmt"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dto "github.com/prometheus/client_model/go"

	"github.com/kettleofketchup/per-user-container-operator/api/v1alpha1"
	"github.com/kettleofketchup/per-user-container-operator/internal/metrics"
)

// waitFor polls cond every 50ms until it returns true or timeout elapses,
// failing the test on timeout. Every producer this suite cannot observe
// (Step 0's table) is forged directly by the caller before waitFor is used
// to observe what the reconciler derives from it.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("condition not met within %s", timeout)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func mustCreate(t *testing.T, obj client.Object) {
	t.Helper()
	if err := k8sClient.Create(context.Background(), obj); err != nil {
		t.Fatalf("create %T %s: %v", obj, obj.GetName(), err)
	}
}

func mustGet(t *testing.T, ws *v1alpha1.Workspace, out *v1alpha1.Workspace) {
	t.Helper()
	if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(ws), out); err != nil {
		t.Fatalf("get workspace %s/%s: %v", ws.Namespace, ws.Name, err)
	}
}

func mustGetObj(t *testing.T, key client.ObjectKey, out client.Object) {
	t.Helper()
	if err := k8sClient.Get(context.Background(), key, out); err != nil {
		t.Fatalf("get %T %s: %v", out, key, err)
	}
}

func waitForPhaseNS(t *testing.T, ns, name string, phase v1alpha1.Phase) {
	t.Helper()
	waitFor(t, 10*time.Second, func() bool {
		var got v1alpha1.Workspace
		if err := k8sClient.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, &got); err != nil {
			return false
		}
		return got.Status.Phase == phase
	})
}

func waitForPhase(t *testing.T, ws *v1alpha1.Workspace, phase v1alpha1.Phase) {
	t.Helper()
	waitForPhaseNS(t, ws.Namespace, ws.Name, phase)
}

// patchWorkspaceStatus re-Gets ws, applies mutate to its status, and patches
// it back. This is how this suite forges every input Step 0's table says has
// no producer in envtest (the router's enqueuedAt/wakeRequestedAt writes,
// the reaper's scaledDown write, a directly-seeded startDeadline or
// largestObservedSize).
func patchWorkspaceStatus(t *testing.T, ws *v1alpha1.Workspace, mutate func(*v1alpha1.WorkspaceStatus)) {
	t.Helper()
	var current v1alpha1.Workspace
	mustGet(t, ws, &current)
	base := current.DeepCopy()
	mutate(&current.Status)
	if err := k8sClient.Status().Patch(context.Background(), &current, client.MergeFrom(base)); err != nil {
		t.Fatalf("patch workspace status %s/%s: %v", ws.Namespace, ws.Name, err)
	}
}

// forgeReadyReplicas stands in for the absent Deployment/ReplicaSet
// controller: it patches the Deployment's status subresource directly,
// which is what drives this suite's Starting -> Ready transitions.
func forgeReadyReplicas(t *testing.T, ns, name string, n int32) {
	t.Helper()
	waitFor(t, 10*time.Second, func() bool {
		var dep appsv1.Deployment
		if err := k8sClient.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, &dep); err != nil {
			return false
		}
		base := dep.DeepCopy()
		dep.Status.Replicas = n
		dep.Status.ReadyReplicas = n
		if err := k8sClient.Status().Patch(context.Background(), &dep, client.MergeFrom(base)); err != nil {
			return false
		}
		return true
	})
}

// impersonatedClient returns a client authenticating as the given
// ServiceAccount identity, exercised against the real envtest RBAC
// authorizer (envtest enables RBAC by default).
func impersonatedClient(t *testing.T, ns, saName string) client.Client {
	t.Helper()
	impCfg := rest.CopyConfig(cfg)
	impCfg.Impersonate = rest.ImpersonationConfig{
		UserName: fmt.Sprintf("system:serviceaccount:%s:%s", ns, saName),
		Groups:   []string{"system:serviceaccounts", fmt.Sprintf("system:serviceaccounts:%s", ns), "system:authenticated"},
	}
	c, err := client.New(impCfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("impersonated client: %v", err)
	}
	return c
}

// gatherMetric searches the shared metrics registry for a sample of family
// name whose labels are a superset of want, returning its value. Namespaces
// are unique per test (newNamespace), so distinct tests never share a label
// set and this never needs a registry reset.
func gatherMetric(t *testing.T, name string, want map[string]string) (float64, bool) {
	t.Helper()
	mfs, err := metrics.Gatherer().Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			if !matchLabels(m, want) {
				continue
			}
			switch {
			case m.Counter != nil:
				return m.Counter.GetValue(), true
			case m.Gauge != nil:
				return m.Gauge.GetValue(), true
			case m.Histogram != nil:
				return float64(m.Histogram.GetSampleCount()), true
			}
		}
	}
	return 0, false
}

func matchLabels(m *dto.Metric, want map[string]string) bool {
	got := map[string]string{}
	for _, lp := range m.GetLabel() {
		got[lp.GetName()] = lp.GetValue()
	}
	for k, v := range want {
		if got[k] != v {
			return false
		}
	}
	return true
}

func hasCondition(conds []metav1.Condition, condType string, status metav1.ConditionStatus) bool {
	for _, c := range conds {
		if c.Type == condType && c.Status == status {
			return true
		}
	}
	return false
}
