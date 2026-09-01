package controller

import (
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	storagev1 "k8s.io/api/storage/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/kettleofketchup/per-user-container-operator/api/v1alpha1"
	"github.com/kettleofketchup/per-user-container-operator/internal/testfixtures"
)

// mustAppScheme builds the scheme every PerUserAppReconciler test needs.
// Panics on error rather than taking *testing.T: this is a fixed, compiled-in
// set of scheme registrations, not user input, so a failure here is a test
// wiring bug, not a test case.
func mustAppScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		corev1.AddToScheme, appsv1.AddToScheme, networkingv1.AddToScheme,
		rbacv1.AddToScheme, storagev1.AddToScheme, v1alpha1.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			panic(err)
		}
	}
	return scheme
}

func newAppFakeClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(mustAppScheme()).
		WithStatusSubresource(&v1alpha1.PerUserApp{}, &v1alpha1.Workspace{}).
		WithObjects(objs...).
		Build()
}

func retainStorageClass(name string) *storagev1.StorageClass {
	retain := corev1.PersistentVolumeReclaimRetain
	return &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: name}, ReclaimPolicy: &retain}
}

func deleteStorageClass(name string) *storagev1.StorageClass {
	del := corev1.PersistentVolumeReclaimDelete
	return &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: name}, ReclaimPolicy: &del}
}

func TestValidateStorageClassRequiresRetain(t *testing.T) {
	app := testfixtures.ValidApp()

	t.Run("retain passes", func(t *testing.T) {
		c := newAppFakeClient(t, retainStorageClass(app.Spec.Storage.StorageClassName))
		if err := ValidateStorageClass(context.Background(), c, app); err != nil {
			t.Fatalf("Retain storage class rejected: %v", err)
		}
	})
	t.Run("delete rejected", func(t *testing.T) {
		c := newAppFakeClient(t, deleteStorageClass(app.Spec.Storage.StorageClassName))
		if err := ValidateStorageClass(context.Background(), c, app); err == nil {
			t.Fatal("Delete reclaimPolicy accepted; a deleted PVC destroys the underlying volume, defeating the survive-cascade-delete guarantee")
		}
	})
	t.Run("missing storage class rejected", func(t *testing.T) {
		c := newAppFakeClient(t)
		if err := ValidateStorageClass(context.Background(), c, app); err == nil {
			t.Fatal("a StorageClass that does not exist must not be treated as valid")
		}
	})
}

// appReconciler builds a PerUserAppReconciler wired to c with a fake event
// recorder and fast-testable CIDR/image config.
func appReconciler(c client.Client, recorder *record.FakeRecorder) *PerUserAppReconciler {
	return &PerUserAppReconciler{
		Client:      c,
		Scheme:      mustAppScheme(),
		RouterImage: testRouterImage,
		PodCIDR:     "10.42.0.0/16",
		NodeCIDR:    "10.0.0.0/24",
		Recorder:    recorder,
	}
}

func reconcileApp(t *testing.T, rec *PerUserAppReconciler, app *v1alpha1.PerUserApp) {
	t.Helper()
	if _, err := rec.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: app.Namespace, Name: app.Name}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}

func getApp(t *testing.T, c client.Client, app *v1alpha1.PerUserApp) *v1alpha1.PerUserApp {
	t.Helper()
	var got v1alpha1.PerUserApp
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: app.Namespace, Name: app.Name}, &got); err != nil {
		t.Fatalf("get app: %v", err)
	}
	return &got
}

// TestReconcileRejectsDefaultAllowRouterIngress locks the carry-forward
// property that a ConfigInvalid routerIngress must set ConfigValid:False and
// create NONE of the router's identity or workload objects -- a reconciler
// that validates but still creates everything on error would leave a router
// Deployment running against a spec the operator itself has declared unsafe.
func TestReconcileRejectsDefaultAllowRouterIngress(t *testing.T) {
	app := testfixtures.ValidApp()
	app.Namespace = "ns-invalid"
	app.Spec.Network.RouterIngress = v1alpha1.RouterIngressSpec{}
	c := newAppFakeClient(t, app, retainStorageClass(app.Spec.Storage.StorageClassName))
	rec := appReconciler(c, record.NewFakeRecorder(10))
	reconcileApp(t, rec, app)

	got := getApp(t, c, app)
	cond := apimeta.FindStatusCondition(got.Status.Conditions, v1alpha1.CondConfigValid)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != v1alpha1.ReasonConfigInvalid {
		t.Fatalf("ConfigValid condition = %+v, want False/ConfigInvalid", cond)
	}

	var dep appsv1.Deployment
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: app.Namespace, Name: app.Name + "-router"}, &dep); err == nil {
		t.Fatal("router Deployment created despite ConfigInvalid")
	}
}

// TestReconcileRejectsDeleteReclaimStorageClass exercises the client-backed
// half of ConfigValid that ValidateApp alone cannot make.
func TestReconcileRejectsDeleteReclaimStorageClass(t *testing.T) {
	app := testfixtures.ValidApp()
	app.Namespace = "ns-delete-sc"
	c := newAppFakeClient(t, app, deleteStorageClass(app.Spec.Storage.StorageClassName))
	rec := appReconciler(c, record.NewFakeRecorder(10))
	reconcileApp(t, rec, app)

	got := getApp(t, c, app)
	cond := apimeta.FindStatusCondition(got.Status.Conditions, v1alpha1.CondConfigValid)
	if cond == nil || cond.Status != metav1.ConditionFalse {
		t.Fatalf("ConfigValid = %+v, want False for a Delete-reclaim storage class", cond)
	}
}

// TestReconcileValidAppCreatesRouterObjectsAndIdentity is the primary-path
// assertion: a valid PerUserApp gets a router Deployment/Service/
// NetworkPolicy, a <app>-router ServiceAccount bound to v1alpha1.RouterRoleName,
// and a <app>-workspace ServiceAccount -- everything the router needs to
// reach the API server as itself instead of running as default.
func TestReconcileValidAppCreatesRouterObjectsAndIdentity(t *testing.T) {
	app := testfixtures.ValidApp()
	app.Namespace = "ns-valid"
	c := newAppFakeClient(t, app, retainStorageClass(app.Spec.Storage.StorageClassName))
	rec := appReconciler(c, record.NewFakeRecorder(10))
	reconcileApp(t, rec, app)

	routerKey := types.NamespacedName{Namespace: app.Namespace, Name: app.Name + "-router"}

	var dep appsv1.Deployment
	if err := c.Get(context.Background(), routerKey, &dep); err != nil {
		t.Fatalf("router Deployment not created: %v", err)
	}
	var svc corev1.Service
	if err := c.Get(context.Background(), routerKey, &svc); err != nil {
		t.Fatalf("router Service not created: %v", err)
	}
	var pol networkingv1.NetworkPolicy
	if err := c.Get(context.Background(), routerKey, &pol); err != nil {
		t.Fatalf("router NetworkPolicy not created: %v", err)
	}
	var routerSA corev1.ServiceAccount
	if err := c.Get(context.Background(), routerKey, &routerSA); err != nil {
		t.Fatalf("router ServiceAccount not created: %v", err)
	}
	var wsSA corev1.ServiceAccount
	wsKey := types.NamespacedName{Namespace: app.Namespace, Name: app.Name + "-workspace"}
	if err := c.Get(context.Background(), wsKey, &wsSA); err != nil {
		t.Fatalf("workspace ServiceAccount not created: %v", err)
	}
	var rb rbacv1.RoleBinding
	if err := c.Get(context.Background(), routerKey, &rb); err != nil {
		t.Fatalf("router RoleBinding not created: %v", err)
	}
	if rb.RoleRef.Name != v1alpha1.RouterRoleName || rb.RoleRef.Kind != "Role" {
		t.Fatalf("router RoleBinding roleRef = %+v, want Role/%s", rb.RoleRef, v1alpha1.RouterRoleName)
	}
	if len(rb.Subjects) != 1 || rb.Subjects[0].Name != routerSA.Name {
		t.Fatalf("router RoleBinding subjects = %+v, want [%s]", rb.Subjects, routerSA.Name)
	}

	got := getApp(t, c, app)
	if cond := apimeta.FindStatusCondition(got.Status.Conditions, v1alpha1.CondConfigValid); cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("ConfigValid = %+v, want True", cond)
	}
}

// TestWorkspaceServiceAccountGetsNoRoleBinding is the other half of the
// identity split: the workspace SA the reconciler creates must never appear
// as a subject on any RoleBinding this controller creates. The operator's
// own controller SA can create pods and read PVCs across served namespaces;
// binding that to a workspace SA would be `kubectl create pod` with someone
// else's claimName inside a user's own workspace.
func TestWorkspaceServiceAccountGetsNoRoleBinding(t *testing.T) {
	app := testfixtures.ValidApp()
	app.Namespace = "ns-no-binding"
	c := newAppFakeClient(t, app, retainStorageClass(app.Spec.Storage.StorageClassName))
	rec := appReconciler(c, record.NewFakeRecorder(10))
	reconcileApp(t, rec, app)

	var rbList rbacv1.RoleBindingList
	if err := c.List(context.Background(), &rbList, client.InNamespace(app.Namespace)); err != nil {
		t.Fatalf("list rolebindings: %v", err)
	}
	wsSAName := app.Name + "-workspace"
	for _, rb := range rbList.Items {
		for _, s := range rb.Subjects {
			if s.Name == wsSAName {
				t.Fatalf("RoleBinding %s names the workspace ServiceAccount %s; it must have none", rb.Name, wsSAName)
			}
		}
	}
}

// TestReconcileNamingAnOperatorServiceAccountIsRejected proves ValidateApp's
// existing OperatorServiceAccountName rejection is actually wired into this
// controller's ConfigValid gate, not just unit-tested in isolation: the
// controller SA can create pods and read PVCs across served namespaces, and
// that grant inside a user's workspace is `kubectl create pod` with someone
// else's claimName.
func TestReconcileNamingAnOperatorServiceAccountIsRejected(t *testing.T) {
	app := testfixtures.ValidApp()
	app.Namespace = "ns-operator-sa"
	app.Spec.Workspace.ServiceAccountName = v1alpha1.OperatorServiceAccountName
	c := newAppFakeClient(t, app, retainStorageClass(app.Spec.Storage.StorageClassName))
	rec := appReconciler(c, record.NewFakeRecorder(10))
	reconcileApp(t, rec, app)

	got := getApp(t, c, app)
	cond := apimeta.FindStatusCondition(got.Status.Conditions, v1alpha1.CondConfigValid)
	if cond == nil || cond.Status != metav1.ConditionFalse {
		t.Fatalf("ConfigValid = %+v, want False when spec.workspace.serviceAccountName is the operator's own SA", cond)
	}
}

// TestReconcilePreservesMigrationCompleteCondition is the sole-writer-merge
// property: this controller owns ConfigValid and WorkspaceLimitReached only,
// and must never remove or replace a condition type it does not own. A
// reconciler that rebuilds status.conditions wholesale would silently delete
// Task 16's MigrationComplete:False the moment this controller next
// reconciles -- and Task 8's gate would then wrongly admit a workspace whose
// legacy volume migration never finished.
func TestReconcilePreservesMigrationCompleteCondition(t *testing.T) {
	app := testfixtures.ValidApp()
	app.Namespace = "ns-migration"
	apimeta.SetStatusCondition(&app.Status.Conditions, metav1.Condition{
		Type: v1alpha1.CondMigrationComplete, Status: metav1.ConditionFalse,
		Reason: "Migrating", Message: "legacy volume migration in progress",
	})
	c := newAppFakeClient(t, app, retainStorageClass(app.Spec.Storage.StorageClassName))
	rec := appReconciler(c, record.NewFakeRecorder(10))
	reconcileApp(t, rec, app)

	got := getApp(t, c, app)
	mc := apimeta.FindStatusCondition(got.Status.Conditions, v1alpha1.CondMigrationComplete)
	if mc == nil || mc.Status != metav1.ConditionFalse || mc.Reason != "Migrating" {
		t.Fatalf("MigrationComplete condition clobbered: %+v", mc)
	}
	if cv := apimeta.FindStatusCondition(got.Status.Conditions, v1alpha1.CondConfigValid); cv == nil {
		t.Fatal("ConfigValid condition missing; the writer must merge by type via meta.SetStatusCondition, not rebuild the slice")
	}
}

// TestWorkspaceLimitReachedCondition and its inverse lock the fleet-count
// gate: set True exactly at limits.maxWorkspaces, never before.
func TestWorkspaceLimitReachedCondition(t *testing.T) {
	app := testfixtures.ValidApp()
	app.Namespace = "ns-limit"
	app.Spec.Limits.MaxWorkspaces = 1
	ws := testfixtures.ValidWorkspace()
	ws.Namespace = app.Namespace
	c := newAppFakeClient(t, app, ws, retainStorageClass(app.Spec.Storage.StorageClassName))
	rec := appReconciler(c, record.NewFakeRecorder(10))
	reconcileApp(t, rec, app)

	got := getApp(t, c, app)
	cond := apimeta.FindStatusCondition(got.Status.Conditions, v1alpha1.CondWorkspaceLimitReached)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("WorkspaceLimitReached = %+v, want True at fleet==maxWorkspaces (1)", cond)
	}
}

func TestWorkspaceLimitNotReachedCondition(t *testing.T) {
	app := testfixtures.ValidApp()
	app.Namespace = "ns-no-limit"
	app.Spec.Limits.MaxWorkspaces = 200
	c := newAppFakeClient(t, app, retainStorageClass(app.Spec.Storage.StorageClassName))
	rec := appReconciler(c, record.NewFakeRecorder(10))
	reconcileApp(t, rec, app)

	got := getApp(t, c, app)
	cond := apimeta.FindStatusCondition(got.Status.Conditions, v1alpha1.CondWorkspaceLimitReached)
	if cond == nil || cond.Status != metav1.ConditionFalse {
		t.Fatalf("WorkspaceLimitReached = %+v, want False with zero workspaces against a 200 limit", cond)
	}
}

// TestValidateAppWarningsSurfaceAsEvents: ValidateApp's warnings (e.g. empty
// workspaceEgress) are not condition-worthy but must not be silently
// dropped -- an operator watching `kubectl get events` is the intended
// audience.
func TestValidateAppWarningsSurfaceAsEvents(t *testing.T) {
	app := testfixtures.ValidApp()
	app.Namespace = "ns-warn"
	app.Spec.Network.WorkspaceEgress = nil
	c := newAppFakeClient(t, app, retainStorageClass(app.Spec.Storage.StorageClassName))
	recorder := record.NewFakeRecorder(10)
	rec := appReconciler(c, recorder)
	reconcileApp(t, rec, app)

	select {
	case ev := <-recorder.Events:
		if !strings.Contains(ev, "Warning") {
			t.Fatalf("event %q is not a Warning event", ev)
		}
	default:
		t.Fatal("expected a warning Event for empty spec.network.workspaceEgress")
	}
}

// TestReconcileSurfacesUnresolvedRouterImageAsConfigInvalid is defense in
// depth: cmd/main.go fails fast on an unset RELATED_IMAGE_ROUTER before this
// reconciler is ever constructed, but a reconciler that ends up with no
// resolvable image (neither the env default nor spec.router.Image) must
// still refuse to create a Deployment with an empty image rather than
// creating one that will never become ready.
func TestReconcileSurfacesUnresolvedRouterImageAsConfigInvalid(t *testing.T) {
	app := testfixtures.ValidApp()
	app.Namespace = "ns-no-image"
	c := newAppFakeClient(t, app, retainStorageClass(app.Spec.Storage.StorageClassName))
	rec := appReconciler(c, record.NewFakeRecorder(10))
	rec.RouterImage = ""
	reconcileApp(t, rec, app)

	got := getApp(t, c, app)
	cond := apimeta.FindStatusCondition(got.Status.Conditions, v1alpha1.CondConfigValid)
	if cond == nil || cond.Status != metav1.ConditionFalse {
		t.Fatalf("ConfigValid = %+v, want False with no resolvable router image", cond)
	}
	var dep appsv1.Deployment
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: app.Namespace, Name: app.Name + "-router"}, &dep); err == nil {
		t.Fatal("router Deployment created with no resolvable image")
	}
}

// TestReconcileWorkspacesCarryNoOwnerReferenceToApp documents (rather than
// exercises production code) the data-safety property that makes this
// controller list Workspaces by LabelApp+namespace instead of via an owned
// Workspace: ctrl.SetControllerReference(app, ws, ...) anywhere in this
// operator would have the first ArgoCD prune-and-recreate cascade-delete
// every Workspace, every workspace Deployment and every Service.
func TestReconcileWorkspacesCarryNoOwnerReferenceToApp(t *testing.T) {
	app := testfixtures.ValidApp()
	app.Namespace = "ns-no-owner"
	ws := testfixtures.ValidWorkspace()
	ws.Namespace = app.Namespace
	c := newAppFakeClient(t, app, ws, retainStorageClass(app.Spec.Storage.StorageClassName))
	rec := appReconciler(c, record.NewFakeRecorder(10))
	reconcileApp(t, rec, app)

	var got v1alpha1.Workspace
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ws.Namespace, Name: ws.Name}, &got); err != nil {
		t.Fatalf("get workspace: %v", err)
	}
	if len(got.OwnerReferences) != 0 {
		t.Fatalf("Workspace carries an ownerReference: %+v; a cascade delete of the PerUserApp would delete every user's workspace and data", got.OwnerReferences)
	}
}
