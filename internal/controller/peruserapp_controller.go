package controller

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"

	"github.com/kettleofketchup/per-user-container-operator/api/v1alpha1"
)

// ValidateStorageClass refuses a StorageClass whose reclaimPolicy is not
// Retain: a Delete policy destroys the underlying volume the instant its
// PersistentVolumeClaim is deleted, which defeats RenderWorkspacePVC's
// entire deliberately-no-ownerReference/Prune=false/resource-policy:keep
// survival guarantee the moment a PVC is ever deleted for any reason. This
// lives here, not in ValidateApp (internal/controller/render.go), because it
// needs a live GET against a cluster-scoped StorageClass and that package is
// deliberately client-free so it stays table-testable without an API server.
func ValidateStorageClass(ctx context.Context, c client.Client, app *v1alpha1.PerUserApp) error {
	var sc storagev1.StorageClass
	if err := c.Get(ctx, client.ObjectKey{Name: app.Spec.Storage.StorageClassName}, &sc); err != nil {
		return fmt.Errorf("spec.storage.storageClassName %q: %w", app.Spec.Storage.StorageClassName, err)
	}
	if sc.ReclaimPolicy == nil || *sc.ReclaimPolicy != corev1.PersistentVolumeReclaimRetain {
		got := "unset"
		if sc.ReclaimPolicy != nil {
			got = string(*sc.ReclaimPolicy)
		}
		return fmt.Errorf("spec.storage.storageClassName %q has reclaimPolicy %s, must be Retain: a Delete policy destroys the underlying volume the moment its PersistentVolumeClaim is deleted, defeating the survive-cascade-delete guarantee every per-user PVC depends on", app.Spec.Storage.StorageClassName, got)
	}
	return nil
}

// PerUserAppReconciler reconciles a PerUserApp: the router Deployment,
// Service and NetworkPolicy, the <app>-workspace and <app>-router
// ServiceAccounts, the router's RoleBinding, and the ConfigValid /
// WorkspaceLimitReached conditions. It is the sole writer of both
// conditions and never removes or replaces a condition type it does not
// own (Task 16's MigrationComplete, written from another repository, must
// survive every reconcile of this controller untouched).
//
// It never creates, owns, or deletes a Workspace: those are created by the
// router (internal/router/server.go) and reconciled by WorkspaceReconciler
// (workspace_controller.go). This reconciler only lists them, by
// LabelApp+namespace, to compute WorkspaceLimitReached --
// ctrl.SetControllerReference(app, ws, ...) anywhere in this operator would
// make the first ArgoCD prune-and-recreate cascade-delete every Workspace,
// every workspace Deployment and every Service; the PVCs would survive
// (RenderWorkspacePVC carries no ownerReference at all) so the fleet would
// re-provision and look healthy while every user's volume is orphaned.
type PerUserAppReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// RouterImage is RELATED_IMAGE_ROUTER, resolved and fail-fast-checked by
	// the controller entrypoint before this reconciler is ever constructed.
	// spec.router.Image overrides it per-app when set (development only).
	RouterImage string

	// PodCIDR and NodeCIDR are threaded into RenderRouterNetworkPolicy;
	// supplied by the controller entrypoint's --pod-cidr / --node-cidr flags,
	// which Task 12's chart renders -- the same flags WorkspaceReconciler
	// takes.
	PodCIDR  string
	NodeCIDR string

	// Recorder emits ValidateApp's warnings as Events. Defaulted from mgr in
	// SetupWithManager when nil.
	Recorder record.EventRecorder

	// Name overrides the controller-runtime controller name (default
	// "peruserapp"); see WorkspaceReconciler.Name's doc comment for why a
	// test harness running several short-lived Managers needs this.
	Name string
}

// SetupWithManager wires the PerUserAppReconciler into mgr, defaulting
// Recorder to mgr's own event recorder when the caller has not set one, and
// requeuing a PerUserApp whenever a Workspace naming it via spec.appRef
// changes -- the only way WorkspaceLimitReached stays accurate as the fleet
// grows and shrinks, since this controller does not own Workspaces and so
// gets no owner-based watch on them for free.
func (r *PerUserAppReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorderFor("peruserapp-controller")
	}
	name := r.Name
	if name == "" {
		name = "peruserapp"
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named(name).
		For(&v1alpha1.PerUserApp{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&networkingv1.NetworkPolicy{}).
		Owns(&corev1.ServiceAccount{}).
		Owns(&rbacv1.RoleBinding{}).
		Watches(&v1alpha1.Workspace{}, handler.EnqueueRequestsFromMapFunc(mapWorkspaceToApp)).
		Complete(r)
}

// mapWorkspaceToApp requeues the PerUserApp a Workspace's spec.appRef names,
// in the same namespace: WorkspaceLimitReached is a property of the fleet,
// and this controller has no other watch that fires when the fleet's size
// changes.
func mapWorkspaceToApp(_ context.Context, obj client.Object) []ctrl.Request {
	ws, ok := obj.(*v1alpha1.Workspace)
	if !ok || ws.Spec.AppRef.Name == "" {
		return nil
	}
	return []ctrl.Request{{NamespacedName: types.NamespacedName{Namespace: ws.Namespace, Name: ws.Spec.AppRef.Name}}}
}

// +kubebuilder:rbac:groups=apps.kettleofketchup,resources=peruserapps,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps.kettleofketchup,resources=peruserapps/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps.kettleofketchup,resources=workspaces,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=rolebindings,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=storage.k8s.io,resources=storageclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile drives a PerUserApp: validate the spec (both the pure
// ValidateApp checks and the client-backed ValidateStorageClass), set
// ConfigValid accordingly, and -- only while ConfigValid -- ensure the
// router's Deployment/Service/NetworkPolicy and identity objects exist, then
// recompute WorkspaceLimitReached from a fresh List.
func (r *PerUserAppReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var app v1alpha1.PerUserApp
	if err := r.Get(ctx, req.NamespacedName, &app); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	base := app.DeepCopy()

	warnings, verr := ValidateApp(&app)
	if verr == nil {
		verr = ValidateStorageClass(ctx, r.Client, &app)
	}

	var routerImage string
	if verr == nil {
		routerImage = app.Spec.Router.Image
		if routerImage == "" {
			routerImage = r.RouterImage
		}
		if routerImage == "" {
			verr = fmt.Errorf("router image unresolved: RELATED_IMAGE_ROUTER must be set on the controller, or spec.router.image set for development")
		}
	}

	if verr != nil {
		setAppCondition(&app, v1alpha1.CondConfigValid, metav1.ConditionFalse, v1alpha1.ReasonConfigInvalid, verr.Error())
		if perr := r.patchAppStatusIfChanged(ctx, &app, base); perr != nil {
			return ctrl.Result{}, perr
		}
		return ctrl.Result{}, nil
	}
	setAppCondition(&app, v1alpha1.CondConfigValid, metav1.ConditionTrue, "Valid", "spec passed validation")

	for _, w := range warnings {
		r.Recorder.Event(&app, corev1.EventTypeWarning, "ConfigWarning", w)
	}

	if err := r.ensureWorkspaceIdentity(ctx, &app); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.ensureRouterIdentity(ctx, &app); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.ensureRouterWorkload(ctx, &app, routerImage); err != nil {
		return ctrl.Result{}, err
	}

	reached, err := r.workspaceLimitReached(ctx, &app)
	if err != nil {
		return ctrl.Result{}, err
	}
	if reached {
		setAppCondition(&app, v1alpha1.CondWorkspaceLimitReached, metav1.ConditionTrue, "LimitReached", "the fleet is at spec.limits.maxWorkspaces")
	} else {
		setAppCondition(&app, v1alpha1.CondWorkspaceLimitReached, metav1.ConditionFalse, "BelowLimit", "the fleet is below spec.limits.maxWorkspaces")
	}

	if perr := r.patchAppStatusIfChanged(ctx, &app, base); perr != nil {
		return ctrl.Result{}, perr
	}
	return ctrl.Result{}, nil
}

// ensureWorkspaceIdentity creates the <app>-workspace ServiceAccount if
// absent. Deliberately no RoleBinding: see RenderWorkspaceServiceAccount's
// doc comment.
func (r *PerUserAppReconciler) ensureWorkspaceIdentity(ctx context.Context, app *v1alpha1.PerUserApp) error {
	desired := RenderWorkspaceServiceAccount(app)
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: desired.Name, Namespace: desired.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, sa, func() error {
		sa.Labels = desired.Labels
		sa.OwnerReferences = desired.OwnerReferences
		return nil
	})
	return err
}

// ensureRouterIdentity creates the <app>-router ServiceAccount and the
// RoleBinding naming it against v1alpha1.RouterRoleName. The Role itself is
// rendered by Task 12's chart; a RoleBinding may name a Role that does not
// exist yet with no error at creation time (RBAC is enforced at
// authorization time, not admission).
func (r *PerUserAppReconciler) ensureRouterIdentity(ctx context.Context, app *v1alpha1.PerUserApp) error {
	desiredSA := RenderRouterServiceAccount(app)
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: desiredSA.Name, Namespace: desiredSA.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, sa, func() error {
		sa.Labels = desiredSA.Labels
		sa.OwnerReferences = desiredSA.OwnerReferences
		return nil
	}); err != nil {
		return err
	}

	desiredRB := RenderRouterRoleBinding(app)
	rb := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: desiredRB.Name, Namespace: desiredRB.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, rb, func() error {
		rb.Labels = desiredRB.Labels
		rb.OwnerReferences = desiredRB.OwnerReferences
		rb.Subjects = desiredRB.Subjects
		rb.RoleRef = desiredRB.RoleRef
		return nil
	})
	return err
}

// ensureRouterWorkload creates or updates the router Deployment, Service and
// NetworkPolicy from the current spec.
func (r *PerUserAppReconciler) ensureRouterWorkload(ctx context.Context, app *v1alpha1.PerUserApp, routerImage string) error {
	desiredDep, err := RenderRouterDeployment(app, routerImage)
	if err != nil {
		return err
	}
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: desiredDep.Name, Namespace: desiredDep.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, dep, func() error {
		dep.Labels = desiredDep.Labels
		dep.OwnerReferences = desiredDep.OwnerReferences
		dep.Spec = desiredDep.Spec
		return nil
	}); err != nil {
		return err
	}

	desiredSvc := RenderRouterService(app)
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: desiredSvc.Name, Namespace: desiredSvc.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		svc.Labels = desiredSvc.Labels
		svc.OwnerReferences = desiredSvc.OwnerReferences
		// ClusterIP is immutable once assigned; only Selector/Ports may change.
		svc.Spec.Type = desiredSvc.Spec.Type
		svc.Spec.Selector = desiredSvc.Spec.Selector
		svc.Spec.Ports = desiredSvc.Spec.Ports
		return nil
	}); err != nil {
		return err
	}

	desiredPol := RenderRouterNetworkPolicy(app, r.PodCIDR, r.NodeCIDR)
	pol := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: desiredPol.Name, Namespace: desiredPol.Namespace}}
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, pol, func() error {
		pol.Labels = desiredPol.Labels
		pol.OwnerReferences = desiredPol.OwnerReferences
		pol.Spec = desiredPol.Spec
		return nil
	})
	return err
}

// workspaceLimitReached reports whether app's fleet (every Workspace
// carrying LabelApp==app.Name in app.Namespace) is at or above
// spec.limits.maxWorkspaces.
func (r *PerUserAppReconciler) workspaceLimitReached(ctx context.Context, app *v1alpha1.PerUserApp) (bool, error) {
	var list v1alpha1.WorkspaceList
	if err := r.List(ctx, &list, client.InNamespace(app.Namespace), client.MatchingLabels{v1alpha1.LabelApp: app.Name}); err != nil {
		return false, err
	}
	return int32(len(list.Items)) >= app.Spec.Limits.MaxWorkspaces, nil
}

// patchAppStatusIfChanged patches app's status against base if and only if
// it changed; base was snapshotted before any mutation in Reconcile.
func (r *PerUserAppReconciler) patchAppStatusIfChanged(ctx context.Context, app, base *v1alpha1.PerUserApp) error {
	if equalConditions(app.Status.Conditions, base.Status.Conditions) {
		return nil
	}
	return r.Status().Patch(ctx, app, client.MergeFrom(base))
}

func equalConditions(a, b []metav1.Condition) bool {
	if len(a) != len(b) {
		return false
	}
	for _, ca := range a {
		cb := apimeta.FindStatusCondition(b, ca.Type)
		if cb == nil || cb.Status != ca.Status || cb.Reason != ca.Reason || cb.Message != ca.Message {
			return false
		}
	}
	return true
}

// setAppCondition sets condType on app's status.conditions via
// meta.SetStatusCondition -- merge by type, never a slice rebuild -- so a
// condition type this controller does not own (Task 16's MigrationComplete)
// is never touched.
func setAppCondition(app *v1alpha1.PerUserApp, condType string, status metav1.ConditionStatus, reason, message string) {
	apimeta.SetStatusCondition(&app.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: app.Generation,
	})
}
