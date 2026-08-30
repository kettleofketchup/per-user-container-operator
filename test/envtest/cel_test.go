//go:build envtest

// cel_test.go exercises the CEL rules Task 4 declared that a grep canary
// cannot enforce: the API server, not this operator's Go code, is what
// rejects these requests.
package envtest

import (
	"context"
	"strings"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kettleofketchup/per-user-container-operator/api/v1alpha1"
	"github.com/kettleofketchup/per-user-container-operator/internal/testfixtures"
)

func validAppFor(ns string) *v1alpha1.PerUserApp {
	app := testfixtures.ValidApp()
	app.Namespace = ns
	return app
}

func TestNameLengthCELIsEnforcedByTheAPIServer(t *testing.T) {
	ns := newNamespace(t)

	ok := validAppFor(ns)
	ok.Name = strings.Repeat("a", 27)
	if err := k8sClient.Create(context.Background(), ok); err != nil {
		t.Fatalf("a 27-char name must be accepted: %v", err)
	}

	tooLong := validAppFor(ns)
	tooLong.Name = strings.Repeat("a", 28)
	err := k8sClient.Create(context.Background(), tooLong)
	if err == nil {
		t.Fatal("a 28-char name must be rejected")
	}
	if !strings.Contains(err.Error(), "pod name budget") {
		t.Fatalf("rejection message must name the pod-name-budget arithmetic, got: %v", err)
	}
}

func TestStorageShrinkCELRejectsAnUpdate(t *testing.T) {
	ns := newNamespace(t)
	app := validAppFor(ns)
	app.Spec.Storage.Size = resource.MustParse("10Gi")
	mustCreate(t, app)

	// 10Gi -> 9Gi must be REJECTED. This is the lexicographic trap: "9Gi" >=
	// "10Gi" as a string compare, so a bare self.size >= oldSelf.size passes
	// this shrink.
	var shrink v1alpha1.PerUserApp
	mustGetObj(t, client.ObjectKeyFromObject(app), &shrink)
	shrinkBase := shrink.DeepCopy()
	shrink.Spec.Storage.Size = resource.MustParse("9Gi")
	if err := k8sClient.Patch(context.Background(), &shrink, client.MergeFrom(shrinkBase)); err == nil {
		t.Fatal("10Gi -> 9Gi must be rejected by the CEL transition rule")
	}

	// 10Gi -> 20Gi must be ACCEPTED: growth is the one mutation the
	// controller performs, and an over-strict or erroring rule would fire
	// StorageSpecDrift on every workspace at once.
	var grow v1alpha1.PerUserApp
	mustGetObj(t, client.ObjectKeyFromObject(app), &grow)
	growBase := grow.DeepCopy()
	grow.Spec.Storage.Size = resource.MustParse("20Gi")
	if err := k8sClient.Patch(context.Background(), &grow, client.MergeFrom(growBase)); err != nil {
		t.Fatalf("10Gi -> 20Gi must be accepted by the CEL transition rule: %v", err)
	}
}

func TestSelectorEgressPeerIsRejectedByTheAPIServer(t *testing.T) {
	ns := newNamespace(t)

	selectorPeer := validAppFor(ns)
	selectorPeer.Name = "cel-selector-peer"
	selectorPeer.Spec.Network.WorkspaceEgress = []networkingv1.NetworkPolicyEgressRule{{
		To: []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "x"}}}},
	}}
	if err := k8sClient.Create(context.Background(), selectorPeer); err == nil {
		t.Fatal("a podSelector egress peer must be rejected")
	}

	allowAnywhere := validAppFor(ns)
	allowAnywhere.Name = "cel-allow-anywhere"
	port := intstr.FromInt32(443)
	allowAnywhere.Spec.Network.WorkspaceEgress = []networkingv1.NetworkPolicyEgressRule{{
		Ports: []networkingv1.NetworkPolicyPort{{Port: &port}},
	}}
	if err := k8sClient.Create(context.Background(), allowAnywhere); err == nil {
		t.Fatal("a ports-only egress rule with 'to' omitted (allow-to-anywhere) must be rejected")
	}
}

func TestReapIntervalAboveIdleTimeoutIsRejected(t *testing.T) {
	ns := newNamespace(t)

	equal := validAppFor(ns)
	equal.Name = "cel-reap-equal"
	equal.Spec.Lifecycle.IdleTimeout = testfixtures.Dur("60s")
	equal.Spec.Lifecycle.ReapInterval = testfixtures.Dur("60s")
	if err := k8sClient.Create(context.Background(), equal); err == nil {
		t.Fatal("reapInterval == idleTimeout must be rejected")
	}

	greater := validAppFor(ns)
	greater.Name = "cel-reap-greater"
	greater.Spec.Lifecycle.IdleTimeout = testfixtures.Dur("60s")
	greater.Spec.Lifecycle.ReapInterval = testfixtures.Dur("90s")
	if err := k8sClient.Create(context.Background(), greater); err == nil {
		t.Fatal("reapInterval > idleTimeout must be rejected")
	}
}

func TestWorkspaceUserKeyIsImmutable(t *testing.T) {
	ns := newNamespace(t)
	app, ws := newFixtures(ns)
	mustCreate(t, app)
	mustCreate(t, ws)

	var current v1alpha1.Workspace
	mustGetObj(t, client.ObjectKeyFromObject(ws), &current)
	base := current.DeepCopy()
	current.Spec.UserKey = "u-0000000000000000"
	if err := k8sClient.Patch(context.Background(), &current, client.MergeFrom(base)); err == nil {
		t.Fatal("spec.userKey must be immutable after create")
	}
}
