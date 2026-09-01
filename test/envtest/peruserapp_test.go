//go:build envtest

package envtest

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kettleofketchup/per-user-container-operator/api/v1alpha1"
	"github.com/kettleofketchup/per-user-container-operator/internal/controller"
)

const testRouterImage = "example.registry/router:v1"

// appReconcilerNameSeq gives each test's PerUserAppReconciler a unique
// controller-runtime controller name: that registry is process-global, not
// per-Manager, and this suite runs many short-lived Managers in one binary.
var appReconcilerNameSeq int64

func newAppReconciler(mgr ctrl.Manager) *controller.PerUserAppReconciler {
	return &controller.PerUserAppReconciler{
		Client:      mgr.GetClient(),
		Scheme:      scheme,
		RouterImage: testRouterImage,
		PodCIDR:     "10.42.0.0/16",
		NodeCIDR:    "10.0.0.0/24",
		Name:        fmt.Sprintf("peruserapp-test-%d", atomic.AddInt64(&appReconcilerNameSeq, 1)),
	}
}

func startAppReconciler(t *testing.T) *controller.PerUserAppReconciler {
	t.Helper()
	mgr, err := newTestManager()
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	rec := newAppReconciler(mgr)
	if err := rec.SetupWithManager(mgr); err != nil {
		t.Fatalf("setup app reconciler: %v", err)
	}
	t.Cleanup(runManager(t, mgr))
	return rec
}

// TestPerUserAppReconcileCreatesRouterWorkloadAndIdentity is the primary
// path against a real API server: RBAC kinds (ServiceAccount, RoleBinding),
// NetworkPolicy and a Deployment/Service all really admit what this
// reconciler renders, and ConfigValid settles True.
func TestPerUserAppReconcileCreatesRouterWorkloadAndIdentity(t *testing.T) {
	ns := newNamespace(t)
	app, _ := newFixtures(ns)
	startAppReconciler(t)
	mustCreate(t, app)

	routerKey := types.NamespacedName{Namespace: ns, Name: app.Name + "-router"}
	waitFor(t, 10*time.Second, func() bool {
		var dep appsv1.Deployment
		return k8sClient.Get(context.Background(), routerKey, &dep) == nil
	})

	var svc corev1.Service
	if err := k8sClient.Get(context.Background(), routerKey, &svc); err != nil {
		t.Fatalf("router Service not created: %v", err)
	}
	var pol networkingv1.NetworkPolicy
	if err := k8sClient.Get(context.Background(), routerKey, &pol); err != nil {
		t.Fatalf("router NetworkPolicy not created: %v", err)
	}
	var routerSA corev1.ServiceAccount
	if err := k8sClient.Get(context.Background(), routerKey, &routerSA); err != nil {
		t.Fatalf("router ServiceAccount not created: %v", err)
	}
	var rb rbacv1.RoleBinding
	if err := k8sClient.Get(context.Background(), routerKey, &rb); err != nil {
		t.Fatalf("router RoleBinding not created: %v", err)
	}
	if rb.RoleRef.Name != v1alpha1.RouterRoleName || rb.RoleRef.Kind != "Role" {
		t.Fatalf("router RoleBinding roleRef = %+v, want Role/%s", rb.RoleRef, v1alpha1.RouterRoleName)
	}
	var wsSA corev1.ServiceAccount
	wsKey := types.NamespacedName{Namespace: ns, Name: app.Name + "-workspace"}
	if err := k8sClient.Get(context.Background(), wsKey, &wsSA); err != nil {
		t.Fatalf("workspace ServiceAccount not created: %v", err)
	}

	var rbList rbacv1.RoleBindingList
	if err := k8sClient.List(context.Background(), &rbList, client.InNamespace(ns)); err != nil {
		t.Fatalf("list rolebindings: %v", err)
	}
	for _, b := range rbList.Items {
		for _, s := range b.Subjects {
			if s.Name == wsSA.Name {
				t.Fatalf("RoleBinding %s names the workspace ServiceAccount %s; it must have none", b.Name, wsSA.Name)
			}
		}
	}

	waitFor(t, 10*time.Second, func() bool {
		var got v1alpha1.PerUserApp
		if err := k8sClient.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: app.Name}, &got); err != nil {
			return false
		}
		return hasCondition(got.Status.Conditions, v1alpha1.CondConfigValid, metav1.ConditionTrue)
	})
}

// TestPerUserAppReconcileConfigInvalidBlocksRouterCreation is the
// carry-forward safety property against a real API server: a default-allow
// routerIngress must never result in a running router.
func TestPerUserAppReconcileConfigInvalidBlocksRouterCreation(t *testing.T) {
	ns := newNamespace(t)
	app, _ := newFixtures(ns)
	app.Spec.Network.RouterIngress = v1alpha1.RouterIngressSpec{}
	startAppReconciler(t)
	mustCreate(t, app)

	waitFor(t, 10*time.Second, func() bool {
		var got v1alpha1.PerUserApp
		if err := k8sClient.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: app.Name}, &got); err != nil {
			return false
		}
		return hasCondition(got.Status.Conditions, v1alpha1.CondConfigValid, metav1.ConditionFalse)
	})

	var dep appsv1.Deployment
	routerKey := types.NamespacedName{Namespace: ns, Name: app.Name + "-router"}
	if err := k8sClient.Get(context.Background(), routerKey, &dep); err == nil {
		t.Fatal("router Deployment created despite ConfigInvalid")
	}
}

// TestPerUserAppReconcileWorkspaceLimitReached exercises the fleet-count
// gate against a real List, requeued by this reconciler's Workspace watch.
func TestPerUserAppReconcileWorkspaceLimitReached(t *testing.T) {
	ns := newNamespace(t)
	app, ws := newFixtures(ns)
	app.Spec.Limits.MaxWorkspaces = 1
	startAppReconciler(t)
	mustCreate(t, app)
	mustCreate(t, ws)

	waitFor(t, 10*time.Second, func() bool {
		var got v1alpha1.PerUserApp
		if err := k8sClient.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: app.Name}, &got); err != nil {
			return false
		}
		return hasCondition(got.Status.Conditions, v1alpha1.CondWorkspaceLimitReached, metav1.ConditionTrue)
	})
}
