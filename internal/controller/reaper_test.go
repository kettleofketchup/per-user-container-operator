package controller

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/kettleofketchup/per-user-container-operator/api/v1alpha1"
	"github.com/kettleofketchup/per-user-container-operator/internal/identity"
	"github.com/kettleofketchup/per-user-container-operator/internal/metrics"
	"github.com/kettleofketchup/per-user-container-operator/internal/testfixtures"
)

// readyWorkspace builds a Ready-phase, not-yet-scaled-down Workspace for
// rawIdentity under app, with zero status.connections so tests can add
// exactly the entries their scenario needs without a stray default masking
// the result.
func readyWorkspace(app *v1alpha1.PerUserApp, rawIdentity string) *v1alpha1.Workspace {
	userKey := identity.UserKey(app.Namespace, app.Name, rawIdentity)
	return &v1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{
			Name:      identity.ChildName(app.Name, userKey),
			Namespace: app.Namespace,
			Labels:    map[string]string{v1alpha1.LabelApp: app.Name, v1alpha1.LabelUserKey: userKey},
		},
		Spec:   v1alpha1.WorkspaceSpec{AppRef: corev1.LocalObjectReference{Name: app.Name}, UserKey: userKey},
		Status: v1alpha1.WorkspaceStatus{Phase: v1alpha1.PhaseReady},
	}
}

func gatherReapedTotal(t *testing.T, ns, app, reason string) (float64, bool) {
	t.Helper()
	mfs, err := metrics.Gatherer().Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != "puc_workspace_reaped_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			labels := map[string]string{}
			for _, lp := range m.GetLabel() {
				labels[lp.GetName()] = lp.GetValue()
			}
			if labels["namespace"] == ns && labels["app"] == app && labels["reason"] == reason {
				return m.GetCounter().GetValue(), true
			}
		}
	}
	return 0, false
}

// TestIdleTimeoutDecidesReapAgainstASyntheticClock is spec 693's named test
// and the only assertion on the idleTimeout half of the reap predicate.
// Both directions are exercised, with ZERO status.connections entries on
// either workspace so the connection half of the predicate cannot mask the
// result either way.
func TestIdleTimeoutDecidesReapAgainstASyntheticClock(t *testing.T) {
	metrics.ResetForTest()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	notYetIdleApp := appWithLimits("ns-idle-not-yet", 5)
	notYetIdleWS := readyWorkspace(notYetIdleApp, "alice")
	notYetIdleWS.Status.LastActivity = ptrTime(now.Add(-(notYetIdleApp.Spec.Lifecycle.IdleTimeout.Duration - time.Second)))

	pastIdleApp := appWithLimits("ns-idle-past", 5)
	pastIdleWS := readyWorkspace(pastIdleApp, "bob")
	pastIdleWS.Status.LastActivity = ptrTime(now.Add(-(pastIdleApp.Spec.Lifecycle.IdleTimeout.Duration + time.Second)))

	c := newFakeClient(t, notYetIdleApp, notYetIdleWS, pastIdleApp, pastIdleWS)
	r := &Reaper{Client: c, Clock: func() time.Time { return now }}

	if err := r.Reap(context.Background()); err != nil {
		t.Fatalf("Reap: %v", err)
	}

	var gotNotYetIdle v1alpha1.Workspace
	mustGetInto(t, c, notYetIdleWS, &gotNotYetIdle)
	if gotNotYetIdle.Status.ScaledDown {
		t.Fatalf("a workspace idle for LESS than idleTimeout must NOT be reaped")
	}
	if v, found := gatherReapedTotal(t, notYetIdleApp.Namespace, notYetIdleApp.Name, v1alpha1.ReapReasonIdle); found && v != 0 {
		t.Fatalf("puc_workspace_reaped_total for the not-yet-idle workspace = %v (found=%v), want 0/absent", v, found)
	}

	var gotPastIdle v1alpha1.Workspace
	mustGetInto(t, c, pastIdleWS, &gotPastIdle)
	if !gotPastIdle.Status.ScaledDown {
		t.Fatalf("a workspace idle for MORE than idleTimeout must be reaped: status.scaledDown must be true")
	}
	if v, found := gatherReapedTotal(t, pastIdleApp.Namespace, pastIdleApp.Name, v1alpha1.ReapReasonIdle); !found || v != 1 {
		t.Fatalf("puc_workspace_reaped_total{reason=idle} for the idle workspace = %v (found=%v), want exactly 1", v, found)
	}
}

// TestLiveConnectionOnAnotherReplicaPreventsReap: two connection entries
// under distinct replica-pod-name keys. lastActivity is set PAST idleTimeout
// throughout, so the idle half of the predicate is already satisfied and the
// connection half is what decides -- otherwise this test would pass for the
// wrong reason. Replica A's entry is stale from the very start (it already
// "looks dead"); replica B's is fresh. Reaping must be blocked purely by B
// until the clock passes 2x connectionHeartbeatInterval measured from B's
// own last heartbeat, at which point the dead replica no longer pins the
// workspace.
func TestLiveConnectionOnAnotherReplicaPreventsReap(t *testing.T) {
	metrics.ResetForTest()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	app := appWithLimits("ns-conn", 5)
	heartbeat := app.Spec.Lifecycle.ConnectionHeartbeatInterval.Duration

	ws := readyWorkspace(app, "alice")
	ws.Status.LastActivity = ptrTime(now.Add(-2 * app.Spec.Lifecycle.IdleTimeout.Duration))
	ws.Status.Connections = map[string]v1alpha1.ConnectionEntry{
		"router-replica-a": {Count: 1, HeartbeatAt: metav1.NewTime(now.Add(-(2*heartbeat + 10*time.Second)))},
		"router-replica-b": {Count: 1, HeartbeatAt: metav1.NewTime(now)},
	}

	c := newFakeClient(t, app, ws)
	clock := now
	r := &Reaper{Client: c, Clock: func() time.Time { return clock }}

	if err := r.Reap(context.Background()); err != nil {
		t.Fatalf("Reap while B is fresh: %v", err)
	}
	var stillLive v1alpha1.Workspace
	mustGetInto(t, c, ws, &stillLive)
	if stillLive.Status.ScaledDown {
		t.Fatalf("a live connection on another replica (B) must prevent reap even though A already looks dead")
	}

	clock = now.Add(2*heartbeat + time.Second)
	if err := r.Reap(context.Background()); err != nil {
		t.Fatalf("Reap once B is stale: %v", err)
	}
	var nowReaped v1alpha1.Workspace
	mustGetInto(t, c, ws, &nowReaped)
	if !nowReaped.Status.ScaledDown {
		t.Fatalf("once B's heartbeat is stale past 2x connectionHeartbeatInterval, the dead replica must not pin the workspace forever")
	}
}

// TestIdleOnlyAfterPodGone drives the WorkspaceReconciler (not the Reaper)
// directly with the two inputs the reap write contract hands it:
// status.scaledDown (forged here, as the Reaper would write it) and pod
// presence/absence. "Idle" means the volume is free, so the phase must stay
// exactly Ready -- not Idle, not Pending, not Failed -- for as long as the
// pod is still terminating, and become exactly Idle only once it is gone.
func TestIdleOnlyAfterPodGone(t *testing.T) {
	app := testfixtures.ValidApp()
	ws := testfixtures.ValidWorkspace()
	ws.Finalizers = []string{workspaceFinalizer}
	ws.Status.Phase = v1alpha1.PhaseReady
	ws.Status.ScaledDown = true

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "workspace-pod",
			Namespace: ws.Namespace,
			Labels:    WorkspacePodLabels(app.Name, ws.Spec.UserKey),
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "workspace", Image: app.Spec.Workspace.Image}}},
	}

	c := newFakeClient(t, app, ws, pod)
	rec := &WorkspaceReconciler{Client: c, Scheme: newAdmissionScheme(t)}

	if _, err := rec.Reconcile(context.Background(), reconcileRequest(ws)); err != nil {
		t.Fatalf("reconcile with pod still present: %v", err)
	}
	var whilePodPresent v1alpha1.Workspace
	mustGetInto(t, c, ws, &whilePodPresent)
	if whilePodPresent.Status.Phase != v1alpha1.PhaseReady {
		t.Fatalf("phase with scaledDown=true but pod still terminating = %q, want exactly Ready", whilePodPresent.Status.Phase)
	}

	if err := c.Delete(context.Background(), pod); err != nil {
		t.Fatalf("delete pod: %v", err)
	}

	if _, err := rec.Reconcile(context.Background(), reconcileRequest(ws)); err != nil {
		t.Fatalf("reconcile with pod gone: %v", err)
	}
	var afterPodGone v1alpha1.Workspace
	mustGetInto(t, c, ws, &afterPodGone)
	if afterPodGone.Status.Phase != v1alpha1.PhaseIdle {
		t.Fatalf("phase with scaledDown=true and pod gone = %q, want exactly Idle", afterPodGone.Status.Phase)
	}
}

// TestReapScalesToZeroAndDeletesNothing asserts the write contract's
// load-bearing half: the reaper writes ONLY status.scaledDown on the
// Workspace status subresource, issues zero writes of any kind against the
// Deployment or the PersistentVolumeClaim, and zero Deletes against any
// object at all. The Deployment and Service must still exist afterward, the
// Service with its spec.clusterIP unchanged -- a reaper that deleted and
// relied on recreation would pass every other assertion here.
func TestReapScalesToZeroAndDeletesNothing(t *testing.T) {
	metrics.ResetForTest()
	app := testfixtures.ValidApp()
	ws := testfixtures.ValidWorkspace()
	ws.Status.Phase = v1alpha1.PhaseReady
	ws.Status.ScaledDown = false
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	ws.Status.LastActivity = ptrTime(now.Add(-(app.Spec.Lifecycle.IdleTimeout.Duration + time.Minute)))

	dep := RenderWorkspaceDeployment(app, ws)
	svc := RenderWorkspaceService(app, ws)
	svc.Spec.ClusterIP = "10.96.0.55"
	pvc := RenderWorkspacePVC(app, ws)

	var writes []string
	c := fake.NewClientBuilder().
		WithScheme(newAdmissionScheme(t)).
		WithStatusSubresource(&v1alpha1.Workspace{}, &v1alpha1.PerUserApp{}, &appsv1.Deployment{}).
		WithObjects(app, ws, dep, svc, pvc).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				writes = append(writes, fmt.Sprintf("Create:%T", obj))
				return cl.Create(ctx, obj, opts...)
			},
			Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				writes = append(writes, fmt.Sprintf("Update:%T", obj))
				return cl.Update(ctx, obj, opts...)
			},
			Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				writes = append(writes, fmt.Sprintf("Patch:%T", obj))
				return cl.Patch(ctx, obj, patch, opts...)
			},
			Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				writes = append(writes, fmt.Sprintf("Delete:%T", obj))
				return cl.Delete(ctx, obj, opts...)
			},
			SubResourceUpdate: func(ctx context.Context, cl client.Client, subResourceName string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				writes = append(writes, fmt.Sprintf("SubResourceUpdate:%s:%T", subResourceName, obj))
				return cl.SubResource(subResourceName).Update(ctx, obj, opts...)
			},
			SubResourcePatch: func(ctx context.Context, cl client.Client, subResourceName string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
				writes = append(writes, fmt.Sprintf("SubResourcePatch:%s:%T", subResourceName, obj))
				return cl.SubResource(subResourceName).Patch(ctx, obj, patch, opts...)
			},
		}).
		Build()

	r := &Reaper{Client: c, Clock: func() time.Time { return now }}
	if err := r.Reap(context.Background()); err != nil {
		t.Fatalf("Reap: %v", err)
	}

	var deploymentWrites, pvcWrites, deletes, statusUpdates int
	for _, w := range writes {
		if strings.Contains(w, "Deployment") {
			deploymentWrites++
		}
		if strings.Contains(w, "PersistentVolumeClaim") {
			pvcWrites++
		}
		if strings.HasPrefix(w, "Delete:") {
			deletes++
		}
		if w == "SubResourceUpdate:status:*v1alpha1.Workspace" {
			statusUpdates++
		}
	}
	if deploymentWrites != 0 {
		t.Fatalf("want zero writes of any kind targeting the Deployment, got %d: %v", deploymentWrites, writes)
	}
	if pvcWrites != 0 {
		t.Fatalf("want zero writes of any kind targeting the PersistentVolumeClaim, got %d: %v", pvcWrites, writes)
	}
	if deletes != 0 {
		t.Fatalf("want zero Deletes of ANY object, got %d: %v", deletes, writes)
	}
	if statusUpdates != 1 {
		t.Fatalf("want exactly one Workspace status subresource write, got %d: %v", statusUpdates, writes)
	}

	var gotWS v1alpha1.Workspace
	mustGetInto(t, c, ws, &gotWS)
	if !gotWS.Status.ScaledDown {
		t.Fatalf("want status.scaledDown = true after reaping")
	}

	var gotDep appsv1.Deployment
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(dep), &gotDep); err != nil {
		t.Fatalf("Deployment must still exist after reaping: %v", err)
	}
	var gotSvc corev1.Service
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(svc), &gotSvc); err != nil {
		t.Fatalf("Service must still exist after reaping: %v", err)
	}
	if gotSvc.Spec.ClusterIP != "10.96.0.55" {
		t.Fatalf("Service clusterIP must be unchanged, got %q", gotSvc.Spec.ClusterIP)
	}
	var gotPVC corev1.PersistentVolumeClaim
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(pvc), &gotPVC); err != nil {
		t.Fatalf("PVC must still exist after reaping: %v", err)
	}
}

// fakeManager is a minimal manager.Manager double that records what is
// registered via Add and hands back a fixed client via GetClient. Every
// other manager.Manager method is unimplemented (the embedded interface is
// nil) -- fine, because Reaper.SetupWithManager never calls them.
type fakeManager struct {
	manager.Manager
	client client.Client
	added  []manager.Runnable
}

func (f *fakeManager) GetClient() client.Client { return f.client }
func (f *fakeManager) Add(r manager.Runnable) error {
	f.added = append(f.added, r)
	return nil
}

// TestReaperTicksAtTheFixedCadence is the registration half of the Tick
// shape contract: SetupWithManager must register the Reaper itself as a
// Runnable (not wrap it or register something else), it must run only on
// the leader (several replicas independently scaling down the same
// Workspaces would race each other for no benefit), and its default tick
// cadence must be the fixed interval the brief specifies -- a per-CR
// reapInterval cannot drive registration because a Runnable is registered
// once, before any PerUserApp has been read.
func TestReaperTicksAtTheFixedCadence(t *testing.T) {
	c := newFakeClient(t)
	r := NewReaper(c)
	fm := &fakeManager{client: c}

	if err := r.SetupWithManager(fm); err != nil {
		t.Fatalf("SetupWithManager: %v", err)
	}
	if len(fm.added) != 1 {
		t.Fatalf("want exactly one Runnable registered with the manager, got %d", len(fm.added))
	}
	if fm.added[0] != r {
		t.Fatalf("the Reaper itself must be the registered Runnable")
	}
	if got := r.tickInterval(); got != reaperTickInterval {
		t.Fatalf("tick interval = %v, want the fixed %v cadence", got, reaperTickInterval)
	}
	if !r.NeedLeaderElection() {
		t.Fatalf("Reaper must only run on the leader")
	}
}

// TestPerAppReapIntervalGatesEachPass is the per-app gating half of the Tick
// shape contract: an app whose lifecycle.reapInterval has not yet elapsed
// since this process last evaluated it must not be evaluated again --
// proven here by counting actual Workspace List calls per app, not by
// inspecting outcomes that idempotency could produce for the wrong reason.
func TestPerAppReapIntervalGatesEachPass(t *testing.T) {
	appFast := appWithLimits("ns-fast", 5)
	appFast.Spec.Lifecycle.ReapInterval = metav1.Duration{Duration: 5 * time.Second}
	appSlow := appWithLimits("ns-slow", 5)
	appSlow.Spec.Lifecycle.ReapInterval = metav1.Duration{Duration: 100 * time.Second}

	base := newFakeClient(t, appFast, appSlow)
	wc, ok := base.(client.WithWatch)
	if !ok {
		t.Fatalf("fake client does not implement client.WithWatch")
	}
	var wsListCalls int
	c := interceptor.NewClient(wc, interceptor.Funcs{
		List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*v1alpha1.WorkspaceList); ok {
				wsListCalls++
			}
			return cl.List(ctx, list, opts...)
		},
	})

	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	r := &Reaper{Client: c, Clock: func() time.Time { return clock }}

	if err := r.Reap(context.Background()); err != nil {
		t.Fatalf("first Reap: %v", err)
	}
	if wsListCalls != 2 {
		t.Fatalf("first pass: both apps are due for the first time ever, want 2 Workspace Lists, got %d", wsListCalls)
	}

	clock = clock.Add(10 * time.Second)
	if err := r.Reap(context.Background()); err != nil {
		t.Fatalf("second Reap: %v", err)
	}
	if wsListCalls != 3 {
		t.Fatalf("second pass (10s elapsed): only the 5s-interval app is due, want 3 total Workspace Lists, got %d", wsListCalls)
	}

	clock = clock.Add(95 * time.Second) // 105s since appSlow's last pass
	if err := r.Reap(context.Background()); err != nil {
		t.Fatalf("third Reap: %v", err)
	}
	if wsListCalls != 5 {
		t.Fatalf("third pass: both apps due again (100s elapsed for the slow app), want 5 total Workspace Lists, got %d", wsListCalls)
	}
}
