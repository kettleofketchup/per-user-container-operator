//go:build envtest

package envtest

import (
	"context"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kettleofketchup/per-user-container-operator/api/v1alpha1"
	"github.com/kettleofketchup/per-user-container-operator/internal/controller"
	"github.com/kettleofketchup/per-user-container-operator/internal/identity"
)

// TestConcurrentCreatesProduceOneWorkspace exercises the create-only
// invariant at the API-server level: two goroutines racing to create the
// same deterministically-named Workspace produce exactly one object, with
// the loser's Create returning AlreadyExists rather than an error.
func TestConcurrentCreatesProduceOneWorkspace(t *testing.T) {
	ns := newNamespace(t)
	app, ws := newFixtures(ns)
	mustCreate(t, app)

	var wg sync.WaitGroup
	results := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = k8sClient.Create(context.Background(), ws.DeepCopy())
		}(i)
	}
	wg.Wait()

	successes := 0
	for _, err := range results {
		switch {
		case err == nil:
			successes++
		case apierrors.IsAlreadyExists(err):
		default:
			t.Fatalf("unexpected create error: %v", err)
		}
	}
	if successes != 1 {
		t.Fatalf("want exactly 1 successful create, got %d", successes)
	}

	var list v1alpha1.WorkspaceList
	if err := k8sClient.List(context.Background(), &list, client.InNamespace(ns)); err != nil {
		t.Fatalf("list workspaces: %v", err)
	}
	if len(list.Items) != 1 {
		t.Fatalf("want exactly 1 Workspace, got %d", len(list.Items))
	}
}

// deleteRecordingClient wraps a client.Client and records every object
// passed to Delete, so TestReconcilerNeverDeletesThePVC can assert on what
// the reconciler actually did rather than just that Reconcile returned nil.
type deleteRecordingClient struct {
	client.Client
	mu      sync.Mutex
	deleted []client.Object
}

func (c *deleteRecordingClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	c.mu.Lock()
	c.deleted = append(c.deleted, obj.DeepCopyObject().(client.Object))
	c.mu.Unlock()
	return c.Client.Delete(ctx, obj, opts...)
}

func TestReconcilerNeverDeletesThePVC(t *testing.T) {
	ns := newNamespace(t)
	app, ws := newFixtures(ns)
	mustCreate(t, app)

	mgr, err := newTestManager()
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	rc := &deleteRecordingClient{Client: mgr.GetClient()}
	rec := newTestReconciler(mgr, nil)
	rec.Client = rc
	if err := rec.SetupWithManager(mgr); err != nil {
		t.Fatalf("setup reconciler: %v", err)
	}
	t.Cleanup(runManager(t, mgr))

	mustCreate(t, ws)
	waitForPhase(t, ws, v1alpha1.PhaseStarting)

	pvcName := identity.ChildName(app.Name, ws.Spec.UserKey)
	var pvc corev1.PersistentVolumeClaim
	waitFor(t, 10*time.Second, func() bool {
		return k8sClient.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: pvcName}, &pvc) == nil
	})
	if len(pvc.OwnerReferences) != 0 {
		t.Fatalf("PVC must carry no ownerReferences, got %v", pvc.OwnerReferences)
	}

	var current v1alpha1.Workspace
	mustGet(t, ws, &current)
	if err := k8sClient.Delete(context.Background(), &current); err != nil {
		t.Fatalf("delete workspace: %v", err)
	}
	waitFor(t, 10*time.Second, func() bool {
		var got v1alpha1.Workspace
		return apierrors.IsNotFound(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(ws), &got))
	})

	rc.mu.Lock()
	deleted := append([]client.Object(nil), rc.deleted...)
	rc.mu.Unlock()
	for _, obj := range deleted {
		if _, ok := obj.(*corev1.PersistentVolumeClaim); ok {
			t.Fatalf("reconciler issued a Delete against a PersistentVolumeClaim: %+v", obj)
		}
	}

	if err := k8sClient.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: pvcName}, &pvc); err != nil {
		t.Fatalf("PVC should still exist after the Workspace was deleted: %v", err)
	}
}

func TestPVCShrinkIsRejectedByTheController(t *testing.T) {
	ns := newNamespace(t)
	app, ws := newFixtures(ns)
	startReconciler(t, nil)
	mustCreate(t, app)
	mustCreate(t, ws)
	waitForPhase(t, ws, v1alpha1.PhaseStarting)

	pvcName := identity.ChildName(app.Name, ws.Spec.UserKey)
	waitFor(t, 10*time.Second, func() bool {
		var pvc corev1.PersistentVolumeClaim
		return k8sClient.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: pvcName}, &pvc) == nil
	})

	// Seed the baseline directly: envtest has no PV binder, so "observed at
	// bind time" is unobservable here.
	ten := resource.MustParse("10Gi")
	patchWorkspaceStatus(t, ws, func(s *v1alpha1.WorkspaceStatus) { s.LargestObservedSize = &ten })

	// Reach 5Gi via delete-and-recreate, which bypasses the CEL transition
	// rule entirely -- that bypass is the reason the controller-side check
	// exists.
	if err := k8sClient.Delete(context.Background(), app); err != nil {
		t.Fatalf("delete app: %v", err)
	}
	waitFor(t, 10*time.Second, func() bool {
		var got v1alpha1.PerUserApp
		return apierrors.IsNotFound(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(app), &got))
	})

	app2 := app.DeepCopy()
	app2.ResourceVersion = ""
	app2.UID = ""
	app2.Spec.Storage.Size = resource.MustParse("5Gi")
	mustCreate(t, app2)

	waitFor(t, 10*time.Second, func() bool {
		var got v1alpha1.Workspace
		if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(ws), &got); err != nil {
			return false
		}
		return hasCondition(got.Status.Conditions, v1alpha1.CondStorageShrinkRejected, metav1.ConditionTrue)
	})

	var final v1alpha1.Workspace
	mustGet(t, ws, &final)
	if len(final.OwnerReferences) != 0 {
		t.Fatalf("Workspace must hold no ownerReference, got %v", final.OwnerReferences)
	}
	if final.Status.LargestObservedSize == nil || final.Status.LargestObservedSize.Cmp(resource.MustParse("10Gi")) != 0 {
		t.Fatalf("largestObservedSize must remain 10Gi, got %v", final.Status.LargestObservedSize)
	}

	// The refusal must not be a requeueing error: let a few poll intervals
	// pass and assert no reconcile error was ever recorded for this app.
	time.Sleep(750 * time.Millisecond)
	if v, found := gatherMetric(t, "puc_reconcile_errors_total", map[string]string{"namespace": ns, "app": app.Name, "kind": "reconcile"}); found && v != 0 {
		t.Fatalf("shrink refusal must not error-loop, got puc_reconcile_errors_total=%v", v)
	}
}

func TestStorageGrowthPatchesAndMismatchSetsDrift(t *testing.T) {
	ns := newNamespace(t)
	app, ws := newFixtures(ns)
	startReconciler(t, nil)
	mustCreate(t, app)
	mustCreate(t, ws)
	waitForPhase(t, ws, v1alpha1.PhaseStarting)

	pvcName := identity.ChildName(app.Name, ws.Spec.UserKey)
	waitFor(t, 10*time.Second, func() bool {
		var pvc corev1.PersistentVolumeClaim
		return k8sClient.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: pvcName}, &pvc) == nil
	})

	// Forge the PV binder's output directly: the real API server refuses a
	// resources.requests patch on a claim that has never bound ("spec is
	// immutable ... except resources.requests ... for bound claims"), and
	// envtest has no PV binder to produce a genuinely Bound claim.
	var pvcToBind corev1.PersistentVolumeClaim
	mustGetObj(t, types.NamespacedName{Namespace: ns, Name: pvcName}, &pvcToBind)
	basePVC := pvcToBind.DeepCopy()
	pvcToBind.Status.Phase = corev1.ClaimBound
	pvcToBind.Status.Capacity = corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")}
	if err := k8sClient.Status().Patch(context.Background(), &pvcToBind, client.MergeFrom(basePVC)); err != nil {
		t.Fatalf("forge pvc bound status: %v", err)
	}

	var currentApp v1alpha1.PerUserApp
	mustGetObj(t, client.ObjectKeyFromObject(app), &currentApp)
	baseApp := currentApp.DeepCopy()
	currentApp.Spec.Storage.Size = resource.MustParse("20Gi")
	if err := k8sClient.Patch(context.Background(), &currentApp, client.MergeFrom(baseApp)); err != nil {
		t.Fatalf("patch app storage size: %v", err)
	}

	waitFor(t, 10*time.Second, func() bool {
		var pvc corev1.PersistentVolumeClaim
		if err := k8sClient.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: pvcName}, &pvc); err != nil {
			return false
		}
		got := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
		return got.Cmp(resource.MustParse("20Gi")) == 0
	})
	waitFor(t, 10*time.Second, func() bool {
		var got v1alpha1.Workspace
		if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(ws), &got); err != nil {
			return false
		}
		return got.Status.LargestObservedSize != nil && got.Status.LargestObservedSize.Cmp(resource.MustParse("20Gi")) == 0
	})

	// A mismatch that is NOT a raise: storageClassName is immutable and
	// must only be flagged, never blindly patched (the API server would
	// reject that patch outright).
	var currentApp2 v1alpha1.PerUserApp
	mustGetObj(t, client.ObjectKeyFromObject(app), &currentApp2)
	baseApp2 := currentApp2.DeepCopy()
	currentApp2.Spec.Storage.StorageClassName = "a-different-class"
	if err := k8sClient.Patch(context.Background(), &currentApp2, client.MergeFrom(baseApp2)); err != nil {
		t.Fatalf("patch app storage class: %v", err)
	}

	waitFor(t, 10*time.Second, func() bool {
		var got v1alpha1.Workspace
		if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(ws), &got); err != nil {
			return false
		}
		return hasCondition(got.Status.Conditions, v1alpha1.CondStorageSpecDrift, metav1.ConditionTrue)
	})

	var pvc corev1.PersistentVolumeClaim
	mustGetObj(t, types.NamespacedName{Namespace: ns, Name: pvcName}, &pvc)
	if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName != "ceph-block-static" {
		t.Fatalf("storageClassName is immutable and must never be patched, got %v", pvc.Spec.StorageClassName)
	}
}

func TestTwoClaimsWithTheSameUserKeyLabelGoAmbiguous(t *testing.T) {
	ns := newNamespace(t)
	app, ws := newFixtures(ns)
	startReconciler(t, nil)
	mustCreate(t, app)
	mustCreate(t, ws)
	waitForPhase(t, ws, v1alpha1.PhaseStarting)

	pvcName := identity.ChildName(app.Name, ws.Spec.UserKey)
	waitFor(t, 10*time.Second, func() bool {
		var pvc corev1.PersistentVolumeClaim
		return k8sClient.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: pvcName}, &pvc) == nil
	})

	extra := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "extra-claim",
			Namespace: ns,
			Labels: map[string]string{
				v1alpha1.LabelApp:     app.Name,
				v1alpha1.LabelUserKey: ws.Spec.UserKey,
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOncePod},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")},
			},
		},
	}
	mustCreate(t, extra)

	waitFor(t, 10*time.Second, func() bool {
		var got v1alpha1.Workspace
		if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(ws), &got); err != nil {
			return false
		}
		return hasCondition(got.Status.Conditions, v1alpha1.CondAmbiguousVolume, metav1.ConditionTrue)
	})
}

func TestWaitingReasonIsCopiedFromThePod(t *testing.T) {
	ns := newNamespace(t)
	app, ws := newFixtures(ns)
	startReconciler(t, nil)
	mustCreate(t, app)
	mustCreate(t, ws)
	waitForPhase(t, ws, v1alpha1.PhaseStarting)

	pod := forgeWorkspacePod(t, ns, app.Name, ws.Spec.UserKey)
	basePod := pod.DeepCopy()
	pod.Status = corev1.PodStatus{
		Phase: corev1.PodPending,
		ContainerStatuses: []corev1.ContainerStatus{{
			Name:  "workspace",
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: v1alpha1.WaitingImagePullBackOff}},
		}},
	}
	if err := k8sClient.Status().Patch(context.Background(), pod, client.MergeFrom(basePod)); err != nil {
		t.Fatalf("patch pod status: %v", err)
	}

	waitFor(t, 10*time.Second, func() bool {
		var got v1alpha1.Workspace
		if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(ws), &got); err != nil {
			return false
		}
		return got.Status.WaitingReason == v1alpha1.WaitingImagePullBackOff
	})
}

func forgeWorkspacePod(t *testing.T, ns, appName, userKey string) *corev1.Pod {
	t.Helper()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      identity.ChildName(appName, userKey) + "-forged",
			Namespace: ns,
			Labels:    controller.WorkspacePodLabels(appName, userKey),
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "workspace", Image: "example/app:1"}}},
	}
	mustCreate(t, pod)
	return pod
}

// recordingAdmitter is a test double for controller.Admitter that always
// admits and records every reason RecordStartFailure is called with, so the
// two Starting->Failed tests can assert on the admission seam directly
// rather than on the (not-yet-implemented) real Admitter's side effects.
type recordingAdmitter struct {
	mu      sync.Mutex
	reasons []string
}

func (a *recordingAdmitter) TryAdmit(context.Context, *v1alpha1.Workspace, *v1alpha1.PerUserApp) (bool, error) {
	return true, nil
}

func (a *recordingAdmitter) RecordStartFailure(_ context.Context, _ *v1alpha1.Workspace, _ *v1alpha1.PerUserApp, reason string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.reasons = append(a.reasons, reason)
	return nil
}

func (a *recordingAdmitter) Reasons() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, len(a.reasons))
	copy(out, a.reasons)
	return out
}

func TestCreateIdleResumeDeleteCycle(t *testing.T) {
	ns := newNamespace(t)
	app, ws := newFixtures(ns)
	startReconciler(t, nil)
	mustCreate(t, app)

	// Router forges the create + enqueuedAt patch.
	mustCreate(t, ws)
	enq := metav1.NewTime(time.Now())
	patchWorkspaceStatus(t, ws, func(s *v1alpha1.WorkspaceStatus) { s.EnqueuedAt = &enq })

	waitForPhase(t, ws, v1alpha1.PhaseStarting)

	depName := identity.ChildName(app.Name, ws.Spec.UserKey)
	pvcName := depName
	waitFor(t, 10*time.Second, func() bool {
		var pvc corev1.PersistentVolumeClaim
		return k8sClient.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: pvcName}, &pvc) == nil
	})

	forgeReadyReplicas(t, ns, depName, 1)
	waitForPhase(t, ws, v1alpha1.PhaseReady)

	// Reaper forges scaledDown and deletes the (never-created, in this
	// suite) pod -- both inputs the reconciler derives Idle from. The
	// absent ReplicaSet controller would also have dropped readyReplicas
	// back to 0 by now; forge that too, or the stale "1" makes the later
	// resume jump straight from Starting to Ready before the test can ever
	// observe Starting.
	patchWorkspaceStatus(t, ws, func(s *v1alpha1.WorkspaceStatus) { s.ScaledDown = true })
	forgeReadyReplicas(t, ns, depName, 0)
	waitForPhase(t, ws, v1alpha1.PhaseIdle)

	var scaledDep appsv1.Deployment
	mustGetObj(t, types.NamespacedName{Namespace: ns, Name: depName}, &scaledDep)
	if scaledDep.Spec.Replicas == nil || *scaledDep.Spec.Replicas != 0 {
		t.Fatalf("want Deployment scaled to 0 while Idle, got %v", scaledDep.Spec.Replicas)
	}

	// Router forges a wake.
	wake := metav1.NewTime(time.Now())
	patchWorkspaceStatus(t, ws, func(s *v1alpha1.WorkspaceStatus) { s.WakeRequestedAt = &wake })
	waitForPhase(t, ws, v1alpha1.PhaseStarting)

	forgeReadyReplicas(t, ns, depName, 1)
	waitForPhase(t, ws, v1alpha1.PhaseReady)

	var pvcAfter corev1.PersistentVolumeClaim
	if err := k8sClient.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: pvcName}, &pvcAfter); err != nil {
		t.Fatalf("PVC must be reused across the resume under the same name: %v", err)
	}

	var current v1alpha1.Workspace
	mustGet(t, ws, &current)
	if err := k8sClient.Delete(context.Background(), &current); err != nil {
		t.Fatalf("delete workspace: %v", err)
	}
	waitFor(t, 10*time.Second, func() bool {
		var got v1alpha1.Workspace
		return apierrors.IsNotFound(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(ws), &got))
	})
}

func TestStartDeadlineExpiryFailsWithReasonStartupTimeout(t *testing.T) {
	ns := newNamespace(t)
	app, ws := newFixtures(ns)
	admitter := &recordingAdmitter{}
	startReconciler(t, func(r *controller.WorkspaceReconciler) { r.Admitter = admitter })
	mustCreate(t, app)
	mustCreate(t, ws)
	waitForPhase(t, ws, v1alpha1.PhaseStarting)

	past := metav1.NewTime(time.Now().Add(-time.Minute))
	patchWorkspaceStatus(t, ws, func(s *v1alpha1.WorkspaceStatus) { s.StartDeadline = &past })

	waitForPhase(t, ws, v1alpha1.PhaseFailed)

	reasons := admitter.Reasons()
	if len(reasons) == 0 || reasons[len(reasons)-1] != v1alpha1.StartFailureTimeout {
		t.Fatalf("want RecordStartFailure's last call to be %q, got %v", v1alpha1.StartFailureTimeout, reasons)
	}
}

func TestCrashLoopingPodFailsWithReasonCrashloop(t *testing.T) {
	ns := newNamespace(t)
	app, ws := newFixtures(ns)
	admitter := &recordingAdmitter{}
	startReconciler(t, func(r *controller.WorkspaceReconciler) { r.Admitter = admitter })
	mustCreate(t, app)
	mustCreate(t, ws)
	waitForPhase(t, ws, v1alpha1.PhaseStarting)

	pod := forgeWorkspacePod(t, ns, app.Name, ws.Spec.UserKey)
	basePod := pod.DeepCopy()
	pod.Status = corev1.PodStatus{
		Phase: corev1.PodRunning,
		ContainerStatuses: []corev1.ContainerStatus{{
			Name:  "workspace",
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
		}},
	}
	if err := k8sClient.Status().Patch(context.Background(), pod, client.MergeFrom(basePod)); err != nil {
		t.Fatalf("patch pod status: %v", err)
	}

	waitForPhase(t, ws, v1alpha1.PhaseFailed)

	reasons := admitter.Reasons()
	if len(reasons) == 0 || reasons[len(reasons)-1] != v1alpha1.StartFailureCrash {
		t.Fatalf("want RecordStartFailure's last call to be %q (not startup_timeout), got %v", v1alpha1.StartFailureCrash, reasons)
	}
}

func TestControllerRestartConvergesWithoutDuplicates(t *testing.T) {
	ns := newNamespace(t)
	app, ws1 := newFixtures(ns)
	mustCreate(t, app)

	s := startStoppableReconciler(t, nil)
	mustCreate(t, ws1)
	waitForPhaseNS(t, ns, ws1.Name, v1alpha1.PhaseStarting)

	userKey2 := identity.UserKey(ns, app.Name, "bob")
	ws2 := ws1.DeepCopy()
	ws2.ResourceVersion = ""
	ws2.UID = ""
	ws2.Name = identity.ChildName(app.Name, userKey2)
	ws2.Spec.UserKey = userKey2
	ws2.Labels[v1alpha1.LabelUserKey] = userKey2
	mustCreate(t, ws2)
	waitForPhaseNS(t, ns, ws2.Name, v1alpha1.PhaseStarting)

	s.Stop()

	s2 := startStoppableReconciler(t, nil)
	t.Cleanup(s2.Stop)

	waitFor(t, 10*time.Second, func() bool {
		var l v1alpha1.WorkspaceList
		if err := k8sClient.List(context.Background(), &l, client.InNamespace(ns)); err != nil {
			return false
		}
		return len(l.Items) == 2
	})

	for _, userKey := range []string{ws1.Spec.UserKey, userKey2} {
		labels := controller.WorkspacePodLabels(app.Name, userKey)

		var deps appsv1.DeploymentList
		if err := k8sClient.List(context.Background(), &deps, client.InNamespace(ns), client.MatchingLabels(labels)); err != nil {
			t.Fatalf("list deployments: %v", err)
		}
		if len(deps.Items) != 1 {
			t.Fatalf("userKey %s: want exactly 1 Deployment, got %d", userKey, len(deps.Items))
		}

		var svcs corev1.ServiceList
		if err := k8sClient.List(context.Background(), &svcs, client.InNamespace(ns), client.MatchingLabels(labels)); err != nil {
			t.Fatalf("list services: %v", err)
		}
		if len(svcs.Items) != 1 {
			t.Fatalf("userKey %s: want exactly 1 Service, got %d", userKey, len(svcs.Items))
		}

		var pvcs corev1.PersistentVolumeClaimList
		if err := k8sClient.List(context.Background(), &pvcs, client.InNamespace(ns), client.MatchingLabels{
			v1alpha1.LabelApp: app.Name, v1alpha1.LabelUserKey: userKey,
		}); err != nil {
			t.Fatalf("list pvcs: %v", err)
		}
		if len(pvcs.Items) != 1 {
			t.Fatalf("userKey %s: want exactly 1 PersistentVolumeClaim, got %d", userKey, len(pvcs.Items))
		}
	}
}

func TestRouterRoleGrantsExactlyWhatTheRouterDoes(t *testing.T) {
	ns := newNamespace(t)
	app, ws := newFixtures(ns)
	mustCreate(t, app)
	mustCreate(t, ws)

	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: app.Name + "-router", Namespace: ns}}
	mustCreate(t, sa)

	// TEMPORARY hand-written fixture. Task 12 Step 1 replaces this Role with
	// the chart-rendered one; until then this test can only assert the
	// fixture's own rules, not the rules that ship. See
	// task-6-brief.md's note on why that is deliberate.
	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.RouterRoleName, Namespace: ns},
		Rules: []rbacv1.PolicyRule{
			{APIGroups: []string{v1alpha1.GroupVersion.Group}, Resources: []string{"workspaces"}, Verbs: []string{"get", "list", "watch", "create"}},
			{APIGroups: []string{v1alpha1.GroupVersion.Group}, Resources: []string{"workspaces/status"}, Verbs: []string{"get", "patch"}},
			{APIGroups: []string{""}, Resources: []string{"services"}, Verbs: []string{"get", "list"}},
			{APIGroups: []string{"discovery.k8s.io"}, Resources: []string{"endpointslices"}, Verbs: []string{"list"}},
		},
	}
	mustCreate(t, role)
	binding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: app.Name + "-router", Namespace: ns},
		Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: sa.Name, Namespace: ns}},
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: v1alpha1.RouterRoleName},
	}
	mustCreate(t, binding)

	routerClient := impersonatedClient(t, ns, sa.Name)
	unboundClient := impersonatedClient(t, ns, "no-such-router")

	newUserKey := identity.UserKey(ns, app.Name, "carol")
	newWorkspace := ws.DeepCopy()
	newWorkspace.ResourceVersion = ""
	newWorkspace.UID = ""
	newWorkspace.Name = identity.ChildName(app.Name, newUserKey)
	newWorkspace.Spec.UserKey = newUserKey
	newWorkspace.Labels[v1alpha1.LabelUserKey] = newUserKey

	if err := routerClient.Create(context.Background(), newWorkspace.DeepCopy()); err != nil {
		t.Fatalf("router role must allow creating a Workspace: %v", err)
	}

	var current v1alpha1.Workspace
	mustGet(t, ws, &current)
	baseCurrent := current.DeepCopy()
	activity := metav1.NewTime(time.Now())
	current.Status.LastActivity = &activity
	if err := routerClient.Status().Patch(context.Background(), &current, client.MergeFrom(baseCurrent)); err != nil {
		t.Fatalf("router role must allow patching workspaces/status: %v", err)
	}

	var svcList corev1.ServiceList
	if err := routerClient.List(context.Background(), &svcList, client.InNamespace(ns)); err != nil {
		t.Fatalf("router role must allow listing services: %v", err)
	}

	var epsList discoveryv1.EndpointSliceList
	if err := routerClient.List(context.Background(), &epsList, client.InNamespace(ns)); err != nil {
		t.Fatalf("router role must allow listing endpointslices: %v", err)
	}

	// The same four operations, unbound: all rejected. Otherwise this test
	// would pass against a cluster with no RBAC enforcement at all.
	otherUserKey := identity.UserKey(ns, app.Name, "dave")
	otherWorkspace := newWorkspace.DeepCopy()
	otherWorkspace.Name = identity.ChildName(app.Name, otherUserKey)
	otherWorkspace.Spec.UserKey = otherUserKey
	if err := unboundClient.Create(context.Background(), otherWorkspace); !apierrors.IsForbidden(err) {
		t.Fatalf("want Forbidden creating a Workspace as an unbound identity, got %v", err)
	}

	unboundPatchTarget := current.DeepCopy()
	if err := unboundClient.Status().Patch(context.Background(), unboundPatchTarget, client.MergeFrom(baseCurrent)); !apierrors.IsForbidden(err) {
		t.Fatalf("want Forbidden patching workspaces/status as an unbound identity, got %v", err)
	}
	if err := unboundClient.List(context.Background(), &svcList, client.InNamespace(ns)); !apierrors.IsForbidden(err) {
		t.Fatalf("want Forbidden listing services as an unbound identity, got %v", err)
	}
	if err := unboundClient.List(context.Background(), &epsList, client.InNamespace(ns)); !apierrors.IsForbidden(err) {
		t.Fatalf("want Forbidden listing endpointslices as an unbound identity, got %v", err)
	}
}

func TestWorkspaceLoopSeriesAreRecorded(t *testing.T) {
	ns := newNamespace(t)
	app, ws := newFixtures(ns)
	startReconciler(t, nil)

	// Create the Workspace before the PerUserApp: the resulting
	// app-not-found reconciles are what give puc_reconcile_errors_total a
	// non-empty sample. The happy path from here to Ready never touches it
	// otherwise.
	mustCreate(t, ws)
	waitFor(t, 10*time.Second, func() bool {
		_, found := gatherMetric(t, "puc_reconcile_errors_total", map[string]string{"namespace": ns, "app": app.Name, "kind": "app_not_found"})
		return found
	})

	mustCreate(t, app)
	waitForPhase(t, ws, v1alpha1.PhaseStarting)

	depName := identity.ChildName(app.Name, ws.Spec.UserKey)
	forgeReadyReplicas(t, ns, depName, 1)
	waitForPhase(t, ws, v1alpha1.PhaseReady)

	// Gauge-backed series: assert the VALUE, not just presence. A
	// reconciler that always wrote 0 here would still make a bare presence
	// check pass -- exactly the registered-and-always-zero mode that
	// leaves a Failed-ratio alert evaluating on a series nothing writes.
	// There is exactly one Workspace, in Ready, owning one PVC, in this
	// namespace/app.
	waitForMetricValue(t, 5*time.Second, "puc_workspaces", map[string]string{"namespace": ns, "app": app.Name, "phase": string(v1alpha1.PhaseReady)}, 1)
	waitForMetricValue(t, 5*time.Second, "puc_workspace_pvcs_total", map[string]string{"namespace": ns, "app": app.Name}, 1)
	waitForMetricValue(t, 5*time.Second, "puc_workspace_user_info", map[string]string{"namespace": ns, "app": app.Name, "user_key": ws.Spec.UserKey}, 1)

	// Counter/histogram-backed series only exist in the registry once
	// incremented/observed at least once, so presence alone is meaningful
	// here (unlike the gauges above).
	presenceChecks := []struct {
		name   string
		labels map[string]string
	}{
		{"puc_workspace_starts_total", map[string]string{"namespace": ns, "app": app.Name, "result": "admitted"}},
		{"puc_workspace_start_seconds", map[string]string{"namespace": ns, "app": app.Name}},
		{"puc_reconcile_errors_total", map[string]string{"namespace": ns, "app": app.Name, "kind": "app_not_found"}},
	}
	for _, c := range presenceChecks {
		if _, found := gatherMetric(t, c.name, c.labels); !found {
			t.Fatalf("metric %s{%v} not found in registry", c.name, c.labels)
		}
	}
}
