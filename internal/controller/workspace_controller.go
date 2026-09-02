package controller

import (
	"context"
	"fmt"
	"reflect"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/kettleofketchup/per-user-container-operator/api/v1alpha1"
	"github.com/kettleofketchup/per-user-container-operator/internal/identity"
	"github.com/kettleofketchup/per-user-container-operator/internal/metrics"
)

// workspaceFinalizer blocks a Workspace's actual removal for exactly one
// reconcile pass: long enough to delete this workspace's
// puc_workspace_user_info row (see internal/metrics.DeleteWorkspaceUserInfo)
// before the object — and the userKey/userDisplay labels needed to address
// that row — are gone for good. It never blocks deletion of the PVC: this
// reconciler has no code path that deletes a PersistentVolumeClaim, finalizer
// or not.
const workspaceFinalizer = "puc.kettleofketchup/workspace-cleanup"

// WorkspaceAdmitter decides whether a Pending Workspace may proceed to
// Starting and records the reason a Starting Workspace failed. Task 6
// declares this interface; Task 8's admission.go supplies the real
// implementation (the exported concrete type *Admitter* -- a distinct name
// from this interface, both living in this package -- FIFO admission on
// enqueuedAt, maxConcurrentStarts, migration gating, backoff arithmetic) and
// both admitter() and SetupWithManager default to it when the caller has not
// set one. See task-6-brief.md's admission seam section for why the split is
// here rather than left implicit.
type WorkspaceAdmitter interface {
	// TryAdmit reports whether ws may transition Pending -> Starting.
	TryAdmit(ctx context.Context, ws *v1alpha1.Workspace, app *v1alpha1.PerUserApp) (bool, error)
	// RecordStartFailure is called on both Starting -> Failed rows this
	// reconciler owns (start-deadline expiry and crashloop). It is the sole
	// emitter of puc_workspace_start_failures_total — a reconciler that also
	// emitted it would double every start failure silently, since the
	// counter is a rate input with no per-call delta assertion on both
	// emitters at once.
	RecordStartFailure(ctx context.Context, ws *v1alpha1.Workspace, app *v1alpha1.PerUserApp, reason string) error
}

// Default poll/requeue intervals, overridable per-reconciler for fast tests.
const (
	defaultPendingRetryInterval = 3 * time.Second
	defaultStartingPollInterval = 2 * time.Second
	defaultSteadyStateInterval  = 30 * time.Second

	// crashLoopBackOffReason is the container waiting.reason kubelet reports;
	// it is a Kubernetes-defined string, distinct from
	// v1alpha1.StartFailureCrash (the puc_workspace_start_failures_total
	// label this reason maps to).
	crashLoopBackOffReason = "CrashLoopBackOff"
)

// WorkspaceReconciler reconciles a Workspace: it is the sole writer of
// status.phase and of Deployment.spec.replicas for that workspace's child
// Deployment. See task-6-brief.md Step 3 for the full phase transition
// table this reconciler implements.
type WorkspaceReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// Admitter gates Pending -> Starting and records Starting -> Failed
	// reasons. Defaults to the real admission.go implementation, bound to
	// this reconciler's own Client, when nil (see admitter()).
	Admitter WorkspaceAdmitter

	// Name overrides the controller-runtime controller name (default
	// "workspace"). controller-runtime tracks controller names uniquely per
	// process, not per Manager, so a test harness running several
	// short-lived Managers in one binary must vary this to avoid a
	// "controller with name X already exists" panic on the second one.
	Name string

	// PodCIDR and NodeCIDR are threaded into RenderWorkspaceNetworkPolicies;
	// supplied by the controller entrypoint's --pod-cidr / --node-cidr flags
	// (Task 11), which the chart renders (Task 12).
	PodCIDR  string
	NodeCIDR string

	// Clock returns the current time; defaults to time.Now when nil. Tests
	// may seed status.startDeadline directly in the past instead of
	// injecting a Clock — both are supported.
	Clock func() time.Time

	// Requeue/poll intervals, defaulted when zero. Exposed so tests can
	// shorten them instead of waiting on production-scale timers.
	PendingRetryInterval time.Duration
	StartingPollInterval time.Duration
	SteadyStateInterval  time.Duration
}

// SetupWithManager wires the WorkspaceReconciler into mgr, defaulting
// Admitter to the real implementation (admission.go's Admitter, bound to
// this reconciler's own Client) when the caller has not set one.
func (r *WorkspaceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Admitter == nil {
		r.Admitter = NewAdmitter(r.Client)
	}
	name := r.Name
	if name == "" {
		name = "workspace"
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named(name).
		For(&v1alpha1.Workspace{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&networkingv1.NetworkPolicy{}).
		Complete(r)
}

func (r *WorkspaceReconciler) admitter() WorkspaceAdmitter {
	if r.Admitter == nil {
		return NewAdmitter(r.Client)
	}
	return r.Admitter
}

func (r *WorkspaceReconciler) now() time.Time {
	if r.Clock != nil {
		return r.Clock()
	}
	return time.Now()
}

func (r *WorkspaceReconciler) pendingRetryInterval() time.Duration {
	if r.PendingRetryInterval > 0 {
		return r.PendingRetryInterval
	}
	return defaultPendingRetryInterval
}

func (r *WorkspaceReconciler) startingPollInterval() time.Duration {
	if r.StartingPollInterval > 0 {
		return r.StartingPollInterval
	}
	return defaultStartingPollInterval
}

func (r *WorkspaceReconciler) steadyStateInterval() time.Duration {
	if r.SteadyStateInterval > 0 {
		return r.SteadyStateInterval
	}
	return defaultSteadyStateInterval
}

// +kubebuilder:rbac:groups=apps.kettleofketchup,resources=workspaces,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps.kettleofketchup,resources=workspaces/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps.kettleofketchup,resources=workspaces/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps.kettleofketchup,resources=peruserapps,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=persistentvolumes,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch

// Reconcile drives one Workspace through the phase state machine described
// in task-6-brief.md Step 3.
func (r *WorkspaceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var ws v1alpha1.Workspace
	if err := r.Get(ctx, req.NamespacedName, &ws); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if !ws.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &ws)
	}

	if !controllerutil.ContainsFinalizer(&ws, workspaceFinalizer) {
		controllerutil.AddFinalizer(&ws, workspaceFinalizer)
		if err := r.Update(ctx, &ws); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	if ws.Status.Phase == "" {
		base := ws.DeepCopy()
		ws.Status.Phase = v1alpha1.PhasePending
		if err := r.Status().Patch(ctx, &ws, client.MergeFrom(base)); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	var app v1alpha1.PerUserApp
	if err := r.Get(ctx, types.NamespacedName{Namespace: ws.Namespace, Name: ws.Spec.AppRef.Name}, &app); err != nil {
		metrics.RecordReconcileError(ws.Namespace, ws.Spec.AppRef.Name, "app_not_found")
		return ctrl.Result{}, err
	}

	metrics.SetWorkspaceUserInfo(ws.Namespace, app.Name, ws.Spec.UserKey, ws.Annotations[v1alpha1.AnnUserDisplay], true)
	if err := r.refreshGauges(ctx, ws.Namespace, app.Name); err != nil {
		return ctrl.Result{}, err
	}

	base := ws.DeepCopy()
	var (
		result ctrl.Result
		rerr   error
	)
	switch ws.Status.Phase {
	case v1alpha1.PhasePending:
		result, rerr = r.reconcilePending(ctx, &ws, &app)
	case v1alpha1.PhaseStarting:
		result, rerr = r.reconcileStarting(ctx, &ws, &app)
	case v1alpha1.PhaseReady:
		result, rerr = r.reconcileReady(ctx, &ws, &app)
	case v1alpha1.PhaseIdle:
		result, rerr = r.reconcileIdle(ctx, &ws, &app)
	case v1alpha1.PhaseFailed:
		result, rerr = r.reconcileFailed(ctx, &ws, &app)
	}

	if perr := r.patchStatusIfChanged(ctx, &ws, base); perr != nil {
		if rerr == nil {
			rerr = perr
		}
	}
	if rerr != nil {
		metrics.RecordReconcileError(ws.Namespace, app.Name, "reconcile")
	}
	return result, rerr
}

func (r *WorkspaceReconciler) patchStatusIfChanged(ctx context.Context, ws, base *v1alpha1.Workspace) error {
	if apiequality.Semantic.DeepEqual(base.Status, ws.Status) {
		return nil
	}
	return r.Status().Patch(ctx, ws, client.MergeFrom(base))
}

// reconcileDelete deletes this workspace's join-metric row (the ABSENT path
// — see internal/metrics.DeleteWorkspaceUserInfo) and lets deletion proceed.
// It never touches the PVC: that survival guarantee holds because no code
// path in this file issues a Delete against a PersistentVolumeClaim, not
// because deletion is intercepted here.
func (r *WorkspaceReconciler) reconcileDelete(ctx context.Context, ws *v1alpha1.Workspace) (ctrl.Result, error) {
	if controllerutil.ContainsFinalizer(ws, workspaceFinalizer) {
		metrics.DeleteWorkspaceUserInfo(ws.Namespace, ws.Spec.AppRef.Name, ws.Spec.UserKey, ws.Annotations[v1alpha1.AnnUserDisplay])
		controllerutil.RemoveFinalizer(ws, workspaceFinalizer)
		if err := r.Update(ctx, ws); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, nil
}

// refreshGauges recomputes puc_workspaces{phase} and
// puc_workspace_pvcs_total for ns/app from a fresh List, rather than
// incrementing/decrementing per transition: a gauge derived from a list is
// self-correcting after a missed event, a hand-maintained running total is
// not.
func (r *WorkspaceReconciler) refreshGauges(ctx context.Context, ns, appName string) error {
	var wsList v1alpha1.WorkspaceList
	if err := r.List(ctx, &wsList, client.InNamespace(ns), client.MatchingLabels{v1alpha1.LabelApp: appName}); err != nil {
		return err
	}
	counts := map[v1alpha1.Phase]int{}
	for _, w := range wsList.Items {
		counts[w.Status.Phase]++
	}
	for _, p := range []v1alpha1.Phase{v1alpha1.PhasePending, v1alpha1.PhaseStarting, v1alpha1.PhaseReady, v1alpha1.PhaseIdle, v1alpha1.PhaseFailed} {
		metrics.SetWorkspacesByPhase(ns, appName, string(p), float64(counts[p]))
	}

	var pvcList corev1.PersistentVolumeClaimList
	if err := r.List(ctx, &pvcList, client.InNamespace(ns), client.MatchingLabels{v1alpha1.LabelApp: appName}); err != nil {
		return err
	}
	metrics.SetWorkspacePVCsTotal(ns, appName, float64(len(pvcList.Items)))
	return nil
}

// reconcilePending implements the (none)->Pending->Starting rows: it asks
// the Admitter, and only on admission does it write startDeadline, create
// the PVC (create-only — see reconcilePVC) and ensure the Deployment,
// Service and NetworkPolicies exist.
func (r *WorkspaceReconciler) reconcilePending(ctx context.Context, ws *v1alpha1.Workspace, app *v1alpha1.PerUserApp) (ctrl.Result, error) {
	admitted, err := r.admitter().TryAdmit(ctx, ws, app)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !admitted {
		return ctrl.Result{RequeueAfter: r.pendingRetryInterval()}, nil
	}

	ws.Status.Phase = v1alpha1.PhaseStarting
	deadline := metav1.NewTime(r.now().Add(app.Spec.Lifecycle.StartupTimeout.Duration))
	ws.Status.StartDeadline = &deadline

	if refused, err := r.reconcilePVC(ctx, ws, app, true); err != nil {
		return ctrl.Result{}, err
	} else if refused {
		// A shrink or ambiguity was detected at the moment of admission:
		// stay Pending rather than starting a workspace with no usable
		// volume decision made.
		ws.Status.Phase = v1alpha1.PhasePending
		ws.Status.StartDeadline = nil
		return ctrl.Result{}, nil
	}

	dep := RenderWorkspaceDeployment(app, ws)
	if err := r.Create(ctx, dep); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return ctrl.Result{}, err
		}
		if err := r.ensureReplicas(ctx, ws, app, 1); err != nil {
			return ctrl.Result{}, err
		}
	}

	svc := RenderWorkspaceService(app, ws)
	if err := r.Create(ctx, svc); err != nil && !apierrors.IsAlreadyExists(err) {
		return ctrl.Result{}, err
	}

	if err := r.ensureWorkspaceNetworkPolicies(ctx, ws, app); err != nil {
		return ctrl.Result{}, err
	}

	metrics.RecordWorkspaceStart(ws.Namespace, app.Name, "admitted")
	return ctrl.Result{}, nil
}

// reconcileStarting implements the Starting row's four exits: Ready (on
// readyReplicas>=1), Failed/StartFailureCrash (pod CrashLoopBackOff),
// Failed/StartFailureTimeout (startDeadline elapsed), and Failed with no
// producer call (the orphan sweep's signal: no Deployment and a non-nil
// backoffUntil). It also copies the pod's waiting.reason verbatim, with one
// derived value: WaitingRWOPConflict, which has no other producer.
func (r *WorkspaceReconciler) reconcileStarting(ctx context.Context, ws *v1alpha1.Workspace, app *v1alpha1.PerUserApp) (ctrl.Result, error) {
	if refused, err := r.reconcilePVC(ctx, ws, app, true); err != nil {
		return ctrl.Result{}, err
	} else if refused {
		return ctrl.Result{}, nil
	}
	if err := r.ensureWorkspaceNetworkPolicies(ctx, ws, app); err != nil {
		return ctrl.Result{}, err
	}

	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(ws.Namespace), client.MatchingLabels(WorkspacePodLabels(app.Name, ws.Spec.UserKey))); err != nil {
		return ctrl.Result{}, err
	}
	var pod *corev1.Pod
	if len(pods.Items) > 0 {
		pod = &pods.Items[0]
	}

	name := identity.ChildName(app.Name, ws.Spec.UserKey)
	var dep appsv1.Deployment
	depErr := r.Get(ctx, types.NamespacedName{Namespace: ws.Namespace, Name: name}, &dep)
	switch {
	case depErr == nil:
		if dep.Status.ReadyReplicas >= 1 {
			r.transitionToReady(ws, app, pod)
			return ctrl.Result{}, nil
		}
	case apierrors.IsNotFound(depErr):
		if ws.Status.BackoffUntil != nil {
			// The orphan sweep (Task 8's OnLeaseAcquired) already emitted
			// StartFailureOrphaned and deleted the Deployment; this
			// reconciler only derives the phase.
			ws.Status.Phase = v1alpha1.PhaseFailed
			return ctrl.Result{}, nil
		}
	default:
		return ctrl.Result{}, depErr
	}

	if pod != nil {
		reason := firstWaitingReason(pod)
		switch {
		case reason == crashLoopBackOffReason:
			r.failStarting(ctx, ws, app, v1alpha1.StartFailureCrash)
			return ctrl.Result{}, nil
		case reason != "":
			ws.Status.WaitingReason = reason
		case isRWOPConflictPending(pod):
			// RWOPConflict has no producer of its own: a ReadWriteOncePod
			// attach conflict never surfaces as a container waiting.reason,
			// it just leaves the pod Pending with no container statuses.
			ws.Status.WaitingReason = v1alpha1.WaitingRWOPConflict
		}
	}

	if ws.Status.StartDeadline != nil && !r.now().Before(ws.Status.StartDeadline.Time) {
		r.failStarting(ctx, ws, app, v1alpha1.StartFailureTimeout)
		return ctrl.Result{}, nil
	}

	return ctrl.Result{RequeueAfter: r.startingPollInterval()}, nil
}

// transitionToReady implements the Starting->Ready row. ObserveWorkspaceStartSeconds
// measures Starting->Ready (derived from startDeadline minus the configured
// startupTimeout), never enqueuedAt->Ready: the latter folds admission
// queueing into the number Task 15 uses to size maxConcurrentStarts.
func (r *WorkspaceReconciler) transitionToReady(ws *v1alpha1.Workspace, app *v1alpha1.PerUserApp, pod *corev1.Pod) {
	if ws.Status.StartDeadline != nil {
		startedAt := ws.Status.StartDeadline.Add(-app.Spec.Lifecycle.StartupTimeout.Duration)
		metrics.ObserveWorkspaceStartSeconds(ws.Namespace, app.Name, r.now().Sub(startedAt).Seconds())
	}

	name := identity.ChildName(app.Name, ws.Spec.UserKey)
	ws.Status.Phase = v1alpha1.PhaseReady
	ws.Status.ServiceRef = name
	ws.Status.PVCRef = name
	if pod != nil {
		ws.Status.PodRef = pod.Name
	}
	ws.Status.StartDeadline = nil
	ws.Status.ConsecutiveFailures = 0
	ws.Status.WaitingReason = ""
}

// failStarting implements both Starting->Failed rows this reconciler owns.
// It always sets the phase; RecordStartFailure errors are recorded as a
// reconcile error but never block the phase transition, and never trigger a
// second emission of puc_workspace_start_failures_total from this side.
func (r *WorkspaceReconciler) failStarting(ctx context.Context, ws *v1alpha1.Workspace, app *v1alpha1.PerUserApp, reason string) {
	ws.Status.Phase = v1alpha1.PhaseFailed
	if err := r.admitter().RecordStartFailure(ctx, ws, app, reason); err != nil {
		metrics.RecordReconcileError(ws.Namespace, app.Name, "record_start_failure")
	}
}

// reconcileFailed implements the Failed->Pending row: a user's retry is the
// fast path, gated purely on backoffUntil (Task 8's arithmetic; a recording
// test stub that never sets it leaves a workspace it fails stuck Failed,
// which is what the recordingAdmitter-based envtest coverage relies on).
func (r *WorkspaceReconciler) reconcileFailed(_ context.Context, ws *v1alpha1.Workspace, _ *v1alpha1.PerUserApp) (ctrl.Result, error) {
	if ws.Status.BackoffUntil != nil && r.now().After(ws.Status.BackoffUntil.Time) {
		ws.Status.Phase = v1alpha1.PhasePending
		ws.Status.BackoffUntil = nil
	}
	return ctrl.Result{}, nil
}

// reconcileReady implements the Ready->Idle and Idle/Ready->Pending (wake)
// rows, and keeps storage in sync with the current PerUserApp spec. It is
// also the sole writer of Deployment.spec.replicas on the scale-down path:
// the reaper (Task 9) only ever writes status.scaledDown.
//
// It also re-ensures the workspace's own NetworkPolicies on every pass, not
// just at admission: SetupWithManager's Owns(&networkingv1.NetworkPolicy{})
// means an externally deleted policy (operator error, an ArgoCD prune,
// anything) DOES enqueue a reconcile of this Workspace, but before this fix
// a Ready workspace's reconcile landed here and did nothing about it -- the
// reconciler stayed awake and reported nothing wrong while a live pod ran
// with its isolation policies permanently gone. See
// ensureWorkspaceNetworkPolicies's own doc comment for which other phases
// get the same treatment and why.
func (r *WorkspaceReconciler) reconcileReady(ctx context.Context, ws *v1alpha1.Workspace, app *v1alpha1.PerUserApp) (ctrl.Result, error) {
	if refused, err := r.reconcilePVC(ctx, ws, app, true); err != nil {
		return ctrl.Result{}, err
	} else if refused {
		return ctrl.Result{}, nil
	}
	if err := r.ensureWorkspaceNetworkPolicies(ctx, ws, app); err != nil {
		return ctrl.Result{}, err
	}

	if ws.Status.WakeRequestedAt != nil {
		ws.Status.WakeRequestedAt = nil
		ws.Status.ScaledDown = false
		ws.Status.Phase = v1alpha1.PhasePending
		return ctrl.Result{}, nil
	}

	if ws.Status.ScaledDown {
		if err := r.ensureReplicas(ctx, ws, app, 0); err != nil {
			return ctrl.Result{}, err
		}
		var pods corev1.PodList
		if err := r.List(ctx, &pods, client.InNamespace(ws.Namespace), client.MatchingLabels(WorkspacePodLabels(app.Name, ws.Spec.UserKey))); err != nil {
			return ctrl.Result{}, err
		}
		if len(pods.Items) == 0 {
			ws.Status.Phase = v1alpha1.PhaseIdle
			return ctrl.Result{}, nil
		}
		// Pod termination is asynchronous (kubelet/ReplicaSet controller);
		// nothing else re-triggers this reconciler once scaledDown is
		// already set, so poll until the pod is confirmed gone.
		return ctrl.Result{RequeueAfter: r.steadyStateInterval()}, nil
	}

	return ctrl.Result{}, nil
}

// reconcileIdle implements the Idle->Pending (wake) row and keeps storage in
// sync while idle. It also re-ensures NetworkPolicies (see
// ensureWorkspaceNetworkPolicies): an Idle workspace has no running pod
// (scaledDown, 0 replicas) so a missing policy has no live traffic to
// expose today, but the wake row above transitions straight to Pending and
// this keeps the invariant simple and uniform -- every non-Failed phase
// that reconcilePVC also runs in self-heals its NetworkPolicies the same
// way, rather than leaving Idle as a second, quieter version of the same
// gap for someone to rediscover later.
func (r *WorkspaceReconciler) reconcileIdle(ctx context.Context, ws *v1alpha1.Workspace, app *v1alpha1.PerUserApp) (ctrl.Result, error) {
	if refused, err := r.reconcilePVC(ctx, ws, app, true); err != nil {
		return ctrl.Result{}, err
	} else if refused {
		return ctrl.Result{}, nil
	}
	if err := r.ensureWorkspaceNetworkPolicies(ctx, ws, app); err != nil {
		return ctrl.Result{}, err
	}

	if ws.Status.WakeRequestedAt != nil {
		ws.Status.WakeRequestedAt = nil
		ws.Status.ScaledDown = false
		ws.Status.Phase = v1alpha1.PhasePending
	}
	return ctrl.Result{}, nil
}

// ensureWorkspaceNetworkPolicies creates or updates ws's own ingress/egress
// NetworkPolicies to match RenderWorkspaceNetworkPolicies's current output,
// idempotently (controllerutil.CreateOrUpdate only issues an Update when the
// object actually differs, so a pass that finds everything already correct
// churns nothing).
//
// Called from every phase reconcilePVC also runs in -- the Pending row only
// after admission (reconcilePending), Starting, Ready, and Idle -- NOT
// Failed, mirroring reconcilePVC's own phase coverage exactly: a Failed
// workspace's only way forward is backoffUntil expiring back to Pending
// (reconcileFailed), which re-admits and re-ensures everything from there,
// the same precedent that already excuses Failed from reconcilePVC.
func (r *WorkspaceReconciler) ensureWorkspaceNetworkPolicies(ctx context.Context, ws *v1alpha1.Workspace, app *v1alpha1.PerUserApp) error {
	for _, desired := range RenderWorkspaceNetworkPolicies(app, ws, r.PodCIDR, r.NodeCIDR) {
		np := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: desired.Name, Namespace: desired.Namespace}}
		if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, np, func() error {
			np.Labels = desired.Labels
			np.OwnerReferences = desired.OwnerReferences
			np.Spec = desired.Spec
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

// ensureReplicas is the only place in this reconciler that writes
// Deployment.spec.replicas. It patches only that field: RenderWorkspaceDeployment
// is still the source of truth for everything else, but a blind
// re-Create/Update would fight other fields with no benefit here.
func (r *WorkspaceReconciler) ensureReplicas(ctx context.Context, ws *v1alpha1.Workspace, app *v1alpha1.PerUserApp, want int32) error {
	name := identity.ChildName(app.Name, ws.Spec.UserKey)
	var dep appsv1.Deployment
	if err := r.Get(ctx, types.NamespacedName{Namespace: ws.Namespace, Name: name}, &dep); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if dep.Spec.Replicas != nil && *dep.Spec.Replicas == want {
		return nil
	}
	patch := client.MergeFrom(dep.DeepCopy())
	dep.Spec.Replicas = &want
	return r.Patch(ctx, &dep, patch)
}

// reconcilePVC implements the storage invariants from task-6-brief.md:
// PVCs are create-only (create if absent, patch ONLY to raise
// resources.requests.storage), a shrink is refused by comparing against the
// largest observed size (so delete-and-recreate cannot bypass it the way it
// bypasses the CEL transition rule), and two claims sharing the app+userKey
// labels go AmbiguousVolume rather than picking one. It returns
// refused=true when the caller must not proceed to create/patch the
// Deployment this pass (a StorageShrinkRejected or AmbiguousVolume
// condition was set); the caller returns ctrl.Result{}, nil in that case —
// refusing rather than looping on an error.
func (r *WorkspaceReconciler) reconcilePVC(ctx context.Context, ws *v1alpha1.Workspace, app *v1alpha1.PerUserApp, allowCreate bool) (bool, error) {
	desiredSize := app.Spec.Storage.Size
	if largest := ws.Status.LargestObservedSize; largest != nil && desiredSize.Cmp(*largest) < 0 {
		setCondition(ws, v1alpha1.CondStorageShrinkRejected, metav1.ConditionTrue, "ShrinkRejected",
			fmt.Sprintf("spec.storage.size=%s is below the largest size this workspace's volume has ever held (%s); refusing rather than stranding the bound volume", desiredSize.String(), largest.String()))
		return true, nil
	}
	clearCondition(ws, v1alpha1.CondStorageShrinkRejected)

	var pvcList corev1.PersistentVolumeClaimList
	if err := r.List(ctx, &pvcList, client.InNamespace(ws.Namespace), client.MatchingLabels{
		v1alpha1.LabelApp:     app.Name,
		v1alpha1.LabelUserKey: ws.Spec.UserKey,
	}); err != nil {
		return true, err
	}

	switch len(pvcList.Items) {
	case 0:
		clearCondition(ws, v1alpha1.CondAmbiguousVolume)
		if !allowCreate {
			return false, nil
		}
		desired := RenderWorkspacePVC(app, ws)
		if err := r.Create(ctx, desired); err != nil && !apierrors.IsAlreadyExists(err) {
			return true, err
		}
		ws.Status.PVCRef = desired.Name
		r.raiseLargestObserved(ws, desired.Spec.Resources.Requests[corev1.ResourceStorage])
		clearCondition(ws, v1alpha1.CondStorageSpecDrift)
		return false, nil
	case 1:
		clearCondition(ws, v1alpha1.CondAmbiguousVolume)
		return r.reconcileExistingPVC(ctx, ws, app, &pvcList.Items[0])
	default:
		setCondition(ws, v1alpha1.CondAmbiguousVolume, metav1.ConditionTrue, "MultipleClaims",
			fmt.Sprintf("%d PersistentVolumeClaims match app=%s user-key=%s; refusing to choose one for this workspace", len(pvcList.Items), app.Name, ws.Spec.UserKey))
		return true, nil
	}
}

func (r *WorkspaceReconciler) reconcileExistingPVC(ctx context.Context, ws *v1alpha1.Workspace, app *v1alpha1.PerUserApp, existing *corev1.PersistentVolumeClaim) (bool, error) {
	ws.Status.PVCRef = existing.Name
	desired := RenderWorkspacePVC(app, ws)

	classMismatch := strPtrVal(existing.Spec.StorageClassName) != strPtrVal(desired.Spec.StorageClassName)
	accessMismatch := !reflect.DeepEqual(existing.Spec.AccessModes, desired.Spec.AccessModes)

	existingSize := existing.Spec.Resources.Requests[corev1.ResourceStorage]
	desiredSize := desired.Spec.Resources.Requests[corev1.ResourceStorage]
	cmp := desiredSize.Cmp(existingSize)

	switch {
	case cmp > 0:
		// The API server rejects a spec.resources.requests change on a
		// claim that has never bound ("spec is immutable after creation
		// except resources.requests ... for bound claims") -- so until the
		// PV binder (absent in this suite's envtest) reports Bound, this
		// pass leaves the request as-is and retries on a later reconcile.
		if existing.Status.Phase != corev1.ClaimBound {
			break
		}
		patch := client.MergeFrom(existing.DeepCopy())
		if existing.Spec.Resources.Requests == nil {
			existing.Spec.Resources.Requests = corev1.ResourceList{}
		}
		existing.Spec.Resources.Requests[corev1.ResourceStorage] = desiredSize
		if err := r.Patch(ctx, existing, patch); err != nil {
			return true, err
		}
		r.raiseLargestObserved(ws, desiredSize)
		clearCondition(ws, v1alpha1.CondStorageSpecDrift)
	case cmp == 0:
		if classMismatch || accessMismatch {
			setCondition(ws, v1alpha1.CondStorageSpecDrift, metav1.ConditionTrue, "SpecMismatch",
				"existing PersistentVolumeClaim's immutable fields no longer match this PerUserApp's storage spec")
		} else {
			clearCondition(ws, v1alpha1.CondStorageSpecDrift)
		}
	default:
		// desired < existing's current request, but not below
		// largestObservedSize (checked above). CEL prevents an in-place
		// spec.storage.size shrink on the PerUserApp, so this is an
		// otherwise-unreachable mismatch; flag it rather than patch an
		// immutable field and error forever.
		setCondition(ws, v1alpha1.CondStorageSpecDrift, metav1.ConditionTrue, "SpecMismatch",
			"existing PersistentVolumeClaim requests more storage than this PerUserApp currently declares")
	}

	if existing.Status.Phase == corev1.ClaimBound {
		capacity, ok := existing.Status.Capacity[corev1.ResourceStorage]
		if !ok {
			capacity = existing.Spec.Resources.Requests[corev1.ResourceStorage]
		}
		r.raiseLargestObserved(ws, capacity)
	}
	return false, nil
}

// raiseLargestObserved only ever raises ws.Status.LargestObservedSize, never
// lowers it: it is the baseline the controller-side shrink refusal compares
// against, and prune-and-recreate must not be able to reset it downward.
func (r *WorkspaceReconciler) raiseLargestObserved(ws *v1alpha1.Workspace, candidate resource.Quantity) {
	if ws.Status.LargestObservedSize == nil || candidate.Cmp(*ws.Status.LargestObservedSize) > 0 {
		c := candidate.DeepCopy()
		ws.Status.LargestObservedSize = &c
	}
}

func strPtrVal(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func firstWaitingReason(pod *corev1.Pod) string {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.State.Waiting != nil {
			return cs.State.Waiting.Reason
		}
	}
	return ""
}

// isRWOPConflictPending reports the shape a ReadWriteOncePod attach
// conflict actually takes: the pod sits Pending with no container statuses
// at all (kubelet never got far enough to report one) and at least one pod
// condition is not yet True (the scheduling/attach block). This is the only
// producer of WaitingRWOPConflict — see task-6-brief.md's note on why a
// purely copy-from-the-pod implementation can never write it.
func isRWOPConflictPending(pod *corev1.Pod) bool {
	if pod.Status.Phase != corev1.PodPending || len(pod.Status.ContainerStatuses) != 0 {
		return false
	}
	for _, c := range pod.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func setCondition(ws *v1alpha1.Workspace, condType string, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&ws.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: ws.Generation,
	})
}

func clearCondition(ws *v1alpha1.Workspace, condType string) {
	meta.RemoveStatusCondition(&ws.Status.Conditions, condType)
}
