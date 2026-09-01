package controller

import (
	"context"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/kettleofketchup/per-user-container-operator/api/v1alpha1"
	"github.com/kettleofketchup/per-user-container-operator/internal/identity"
	"github.com/kettleofketchup/per-user-container-operator/internal/metrics"
	"github.com/kettleofketchup/per-user-container-operator/internal/testfixtures"
)

// syntheticClock is a settable clock so every test in this file drives time
// explicitly instead of sleeping on a wall clock -- a real Sleep-based test
// cannot distinguish "the deadline math is right" from "the test happened to
// wait long enough this run."
type syntheticClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock(start time.Time) *syntheticClock { return &syntheticClock{t: start} }

func (c *syntheticClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *syntheticClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newAdmissionScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add corev1: %v", err)
	}
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add appsv1: %v", err)
	}
	if err := networkingv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add networkingv1: %v", err)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add v1alpha1: %v", err)
	}
	return scheme
}

func newFakeClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(newAdmissionScheme(t)).
		WithStatusSubresource(&v1alpha1.Workspace{}, &v1alpha1.PerUserApp{}, &appsv1.Deployment{}).
		WithObjects(objs...).
		Build()
}

// appWithLimits returns testfixtures.ValidApp() re-namespaced, with
// maxConcurrentStarts overridden.
func appWithLimits(ns string, maxConcurrentStarts int32) *v1alpha1.PerUserApp {
	app := testfixtures.ValidApp()
	app.Namespace = ns
	app.Spec.Limits.MaxConcurrentStarts = maxConcurrentStarts
	return app
}

// pendingWorkspace builds a Workspace for rawIdentity, in Pending phase, with
// the given creationTimestamp/enqueuedAt -- callers set whichever of the two
// matters for the scenario under test.
func pendingWorkspace(app *v1alpha1.PerUserApp, rawIdentity string, created time.Time, enqueuedAt *time.Time) *v1alpha1.Workspace {
	userKey := identity.UserKey(app.Namespace, app.Name, rawIdentity)
	ws := &v1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{
			Name:              identity.ChildName(app.Name, userKey),
			Namespace:         app.Namespace,
			CreationTimestamp: metav1.NewTime(created),
			Labels:            map[string]string{v1alpha1.LabelApp: app.Name, v1alpha1.LabelUserKey: userKey},
		},
		Spec:   v1alpha1.WorkspaceSpec{AppRef: corev1.LocalObjectReference{Name: app.Name}, UserKey: userKey},
		Status: v1alpha1.WorkspaceStatus{Phase: v1alpha1.PhasePending},
	}
	if enqueuedAt != nil {
		et := metav1.NewTime(*enqueuedAt)
		ws.Status.EnqueuedAt = &et
	}
	return ws
}

// TestTryAdmitEnforcesMaxConcurrentStarts is the test the brief calls out as
// non-optional: without a real concurrency bound every other assertion in
// this file passes vacuously (an unbounded admitter also has the earliest
// enqueuedAt "admitted first," trivially, and never holds a slot on anyone).
func TestTryAdmitEnforcesMaxConcurrentStarts(t *testing.T) {
	ns := "ns-bound"
	app := appWithLimits(ns, 2)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	e1 := base
	e2 := base.Add(1 * time.Second)
	e3 := base.Add(2 * time.Second)
	ws1 := pendingWorkspace(app, "alice", base, &e1)
	ws2 := pendingWorkspace(app, "bob", base, &e2)
	ws3 := pendingWorkspace(app, "carol", base, &e3)

	c := newFakeClient(t, app, ws1, ws2, ws3)
	admitter := NewAdmitter(c)

	admitted1, err := admitter.TryAdmit(context.Background(), ws1, app)
	if err != nil || !admitted1 {
		t.Fatalf("ws1 (earliest) should be admitted: admitted=%v err=%v", admitted1, err)
	}
	admitted2, err := admitter.TryAdmit(context.Background(), ws2, app)
	if err != nil || !admitted2 {
		t.Fatalf("ws2 (second earliest) should be admitted: admitted=%v err=%v", admitted2, err)
	}
	admitted3, err := admitter.TryAdmit(context.Background(), ws3, app)
	if err != nil || admitted3 {
		t.Fatalf("ws3 must NOT be admitted while ws1 and ws2 occupy both slots: admitted=%v err=%v", admitted3, err)
	}

	// Move ws1 out of Starting (as the reconciler would once it reaches
	// Ready) -- ws3 is then admitted, proving the slot is actually bounded
	// and actually released, not merely "checked and ignored."
	ws1.Status.Phase = v1alpha1.PhaseReady
	if err := c.Status().Update(context.Background(), ws1); err != nil {
		t.Fatalf("update ws1 to Ready: %v", err)
	}

	admitted3b, err := admitter.TryAdmit(context.Background(), ws3, app)
	if err != nil || !admitted3b {
		t.Fatalf("ws3 should be admitted once a slot frees up: admitted=%v err=%v", admitted3b, err)
	}
}

// TestTryAdmitIsFIFOOnEnqueuedAtNotCreationTimestamp pins the ordering key:
// a workspace created LATER but enqueued EARLIER must win the single
// available slot. creationTimestamp order alone would pick the wrong one.
func TestTryAdmitIsFIFOOnEnqueuedAtNotCreationTimestamp(t *testing.T) {
	ns := "ns-fifo"
	app := appWithLimits(ns, 1)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// ws1 was created FIRST but enqueued SECOND.
	enq1 := base.Add(10 * time.Second)
	ws1 := pendingWorkspace(app, "alice", base, &enq1)

	// ws2 was created SECOND but enqueued FIRST.
	enq2 := base.Add(1 * time.Second)
	ws2 := pendingWorkspace(app, "bob", base.Add(5*time.Second), &enq2)

	c := newFakeClient(t, app, ws1, ws2)
	admitter := NewAdmitter(c)

	admittedWS1, err := admitter.TryAdmit(context.Background(), ws1, app)
	if err != nil {
		t.Fatalf("TryAdmit ws1: %v", err)
	}
	if admittedWS1 {
		t.Fatalf("ws1 was created first but enqueued second; it must NOT win the one slot")
	}

	admittedWS2, err := admitter.TryAdmit(context.Background(), ws2, app)
	if err != nil {
		t.Fatalf("TryAdmit ws2: %v", err)
	}
	if !admittedWS2 {
		t.Fatalf("ws2 has the earliest enqueuedAt; it must win the one slot despite a later creationTimestamp")
	}
}

// TestMigrationGate covers the three cases from Task 16/spec 623: absent ->
// admitted, False -> refused, True -> admitted.
func TestMigrationGate(t *testing.T) {
	ns := "ns-migration"

	t.Run("condition absent admits", func(t *testing.T) {
		app := appWithLimits(ns, 5)
		ws := pendingWorkspace(app, "alice", time.Now(), nil)
		c := newFakeClient(t, app, ws)
		admitter := NewAdmitter(c)
		admitted, err := admitter.TryAdmit(context.Background(), ws, app)
		if err != nil || !admitted {
			t.Fatalf("absent MigrationComplete condition must admit: admitted=%v err=%v", admitted, err)
		}
	})

	t.Run("condition false refuses", func(t *testing.T) {
		app := appWithLimits(ns, 5)
		app.Status.Conditions = []metav1.Condition{{
			Type: v1alpha1.CondMigrationComplete, Status: metav1.ConditionFalse, Reason: "InProgress", Message: "migrating",
		}}
		ws := pendingWorkspace(app, "bob", time.Now(), nil)
		c := newFakeClient(t, app, ws)
		admitter := NewAdmitter(c)
		admitted, err := admitter.TryAdmit(context.Background(), ws, app)
		if err != nil || admitted {
			t.Fatalf("MigrationComplete=False must refuse admission: admitted=%v err=%v", admitted, err)
		}
	})

	t.Run("condition true admits", func(t *testing.T) {
		app := appWithLimits(ns, 5)
		app.Status.Conditions = []metav1.Condition{{
			Type: v1alpha1.CondMigrationComplete, Status: metav1.ConditionTrue, Reason: "Done", Message: "migrated",
		}}
		ws := pendingWorkspace(app, "carol", time.Now(), nil)
		c := newFakeClient(t, app, ws)
		admitter := NewAdmitter(c)
		admitted, err := admitter.TryAdmit(context.Background(), ws, app)
		if err != nil || !admitted {
			t.Fatalf("MigrationComplete=True must admit: admitted=%v err=%v", admitted, err)
		}
	})
}

// TestBackoffExponentialCappedAndAdmitsAfterExpiry drives RecordStartFailure
// four times against a synthetic clock: the first three backoffUntil values
// must strictly increase, the fourth must be capped at lifecycle.backoffMax,
// and TryAdmit must refuse while backoffUntil is in the future and admit
// once the clock passes it.
func TestBackoffExponentialCappedAndAdmitsAfterExpiry(t *testing.T) {
	ns := "ns-backoff"
	app := appWithLimits(ns, 5)
	app.Spec.Lifecycle.BackoffMax = metav1.Duration{Duration: 25 * time.Second}
	ws := pendingWorkspace(app, "alice", time.Now(), nil)

	c := newFakeClient(t, app, ws)
	clock := newClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	admitter := NewAdmitter(c)
	admitter.Clock = clock.Now

	var deadlines []time.Time
	for i := 0; i < 4; i++ {
		if err := admitter.RecordStartFailure(context.Background(), ws, app, v1alpha1.StartFailureTimeout); err != nil {
			t.Fatalf("RecordStartFailure #%d: %v", i+1, err)
		}
		var got v1alpha1.Workspace
		if err := c.Get(context.Background(), client.ObjectKeyFromObject(ws), &got); err != nil {
			t.Fatalf("get workspace: %v", err)
		}
		if got.Status.BackoffUntil == nil {
			t.Fatalf("backoffUntil must be set after RecordStartFailure #%d", i+1)
		}
		if int32(i+1) != got.Status.ConsecutiveFailures {
			t.Fatalf("consecutiveFailures = %d, want %d", got.Status.ConsecutiveFailures, i+1)
		}
		deadlines = append(deadlines, got.Status.BackoffUntil.Time)
		ws = &got
	}

	for i := 1; i < 3; i++ {
		if !deadlines[i].After(deadlines[i-1]) {
			t.Fatalf("backoffUntil #%d (%v) must be strictly after #%d (%v)", i+1, deadlines[i], i, deadlines[i-1])
		}
	}
	maxDeadline := clock.Now().Add(25 * time.Second)
	if deadlines[3].After(maxDeadline) {
		t.Fatalf("4th backoffUntil %v exceeds backoffMax cap %v", deadlines[3], maxDeadline)
	}
	if !deadlines[3].Equal(maxDeadline) {
		t.Fatalf("4th backoffUntil %v must be capped exactly at backoffMax %v", deadlines[3], maxDeadline)
	}

	admitted, err := admitter.TryAdmit(context.Background(), ws, app)
	if err != nil || admitted {
		t.Fatalf("workspace in backoff must be refused: admitted=%v err=%v", admitted, err)
	}

	clock.Advance(26 * time.Second)
	admitted, err = admitter.TryAdmit(context.Background(), ws, app)
	if err != nil || !admitted {
		t.Fatalf("workspace must be admitted once the clock passes backoffUntil: admitted=%v err=%v", admitted, err)
	}
}

// TestOnLeaseAcquiredOrphanSweepReleasesSlotWithoutTouchingPhase reproduces
// "controller killed between the Starting status write and the Deployment
// create, restarted": ws1 sits Starting with no Deployment, occupying a
// slot. Before the sweep runs, a second workspace must be refused (the slot
// is genuinely occupied, not a phantom bound). OnLeaseAcquired must then
// record start_orphaned -- consecutiveFailures incremented, backoffUntil
// armed, the metric emitted exactly once -- and leave status.phase alone
// (Task 6's transition table derives Failed from the armed backoff on its
// own next pass; a sweep that set Failed here would race the reconciler and
// violate the sole-writer invariant). Only after that does the slot free up.
func TestOnLeaseAcquiredOrphanSweepReleasesSlotWithoutTouchingPhase(t *testing.T) {
	metrics.ResetForTest()
	ns := "ns-orphan"
	app := appWithLimits(ns, 1)
	ws1 := pendingWorkspace(app, "alice", time.Now(), nil)
	ws1.Status.Phase = v1alpha1.PhaseStarting
	deadline := metav1.NewTime(time.Now().Add(time.Minute))
	ws1.Status.StartDeadline = &deadline
	// No Deployment object created for ws1: this IS the zombie shape.

	ws2 := pendingWorkspace(app, "bob", time.Now(), nil)

	c := newFakeClient(t, app, ws1, ws2)
	admitter := NewAdmitter(c)

	admitted, err := admitter.TryAdmit(context.Background(), ws2, app)
	if err != nil || admitted {
		t.Fatalf("before the sweep, ws1's zombie Starting slot must still block ws2: admitted=%v err=%v", admitted, err)
	}

	if err := admitter.OnLeaseAcquired(context.Background(), 3*time.Minute); err != nil {
		t.Fatalf("OnLeaseAcquired: %v", err)
	}

	var gotWS1 v1alpha1.Workspace
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(ws1), &gotWS1); err != nil {
		t.Fatalf("get ws1: %v", err)
	}
	if gotWS1.Status.Phase != v1alpha1.PhaseStarting {
		t.Fatalf("OnLeaseAcquired must never write status.phase; got %q, want %q", gotWS1.Status.Phase, v1alpha1.PhaseStarting)
	}
	if gotWS1.Status.ConsecutiveFailures != 1 {
		t.Fatalf("consecutiveFailures = %d, want 1", gotWS1.Status.ConsecutiveFailures)
	}
	if gotWS1.Status.BackoffUntil == nil || !gotWS1.Status.BackoffUntil.After(time.Now()) {
		t.Fatalf("backoffUntil must be armed in the future, got %v", gotWS1.Status.BackoffUntil)
	}

	if v, found := gatherStartFailures(t, ns, app.Name, v1alpha1.StartFailureOrphaned); !found || v != 1 {
		t.Fatalf("puc_workspace_start_failures_total{reason=start_orphaned} = %v (found=%v), want 1", v, found)
	}

	admittedAfter, err := admitter.TryAdmit(context.Background(), ws2, app)
	if err != nil || !admittedAfter {
		t.Fatalf("the zombie's slot must be released by the sweep: admitted=%v err=%v", admittedAfter, err)
	}
}

// TestOnLeaseAcquiredExtendsDeadlinesByTheLeaderlessInterval is the other
// half of the same sweep: a HEALTHY Starting workspace (its Deployment
// exists) must have startDeadline pushed forward by exactly the leaderless
// interval, never reset to a fresh now()+startupTimeout -- a routine chart
// upgrade (a brief leader gap) must not silently shrink or reset a live
// workspace's remaining runway and mark a healthy fleet Failed.
func TestOnLeaseAcquiredExtendsDeadlinesByTheLeaderlessInterval(t *testing.T) {
	ns := "ns-extend"
	app := appWithLimits(ns, 5)
	ws := pendingWorkspace(app, "alice", time.Now(), nil)
	ws.Status.Phase = v1alpha1.PhaseStarting
	originalDeadline := time.Date(2026, 1, 1, 0, 5, 0, 0, time.UTC)
	dl := metav1.NewTime(originalDeadline)
	ws.Status.StartDeadline = &dl

	depName := identity.ChildName(app.Name, ws.Spec.UserKey)
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: depName, Namespace: ns}}

	c := newFakeClient(t, app, ws, dep)
	admitter := NewAdmitter(c)

	leaderless := 7 * time.Minute
	if err := admitter.OnLeaseAcquired(context.Background(), leaderless); err != nil {
		t.Fatalf("OnLeaseAcquired: %v", err)
	}

	var got v1alpha1.Workspace
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(ws), &got); err != nil {
		t.Fatalf("get workspace: %v", err)
	}
	want := originalDeadline.Add(leaderless)
	if got.Status.StartDeadline == nil || !got.Status.StartDeadline.Time.Equal(want) {
		t.Fatalf("startDeadline = %v, want exactly the original deadline extended by the leaderless interval (%v)", got.Status.StartDeadline, want)
	}
	if got.Status.Phase != v1alpha1.PhaseStarting {
		t.Fatalf("phase must be untouched by the sweep, got %q", got.Status.Phase)
	}
	if got.Status.ConsecutiveFailures != 0 || got.Status.BackoffUntil != nil {
		t.Fatalf("a healthy Starting workspace must not be treated as a failure: consecutiveFailures=%d backoffUntil=%v", got.Status.ConsecutiveFailures, got.Status.BackoffUntil)
	}
}

func gatherStartFailures(t *testing.T, ns, app, reason string) (float64, bool) {
	t.Helper()
	mfs, err := metrics.Gatherer().Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != "puc_workspace_start_failures_total" {
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

// TestStartDeadlineExpiryArmsBackoffAndReleasesTheSlot is the half Task 6's
// own TestStartDeadlineExpiryFailsWithReasonStartupTimeout does not make:
// driving the WorkspaceReconciler with the REAL Admitter (not a recording
// stub) through a timeout, and asserting the admission-side effects --
// consecutiveFailures, backoffUntil, the metric's exact delta, and that the
// freed slot actually admits a second workspace on the next pass. A
// successful start afterwards resets consecutiveFailures to 0.
func TestStartDeadlineExpiryArmsBackoffAndReleasesTheSlot(t *testing.T) {
	metrics.ResetForTest()
	ns := "ns-e2e-timeout"
	app := appWithLimits(ns, 1)
	app.Spec.Lifecycle.StartupTimeout = metav1.Duration{Duration: time.Minute}
	app.Spec.Lifecycle.BackoffMax = metav1.Duration{Duration: time.Hour}

	clock := newClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))

	ws1 := pendingWorkspace(app, "alice", clock.Now(), nil)
	enq1 := clock.Now()
	ws1.Status.EnqueuedAt = ptrTime(enq1)
	ws2 := pendingWorkspace(app, "bob", clock.Now(), nil)
	enq2 := clock.Now().Add(time.Second)
	ws2.Status.EnqueuedAt = ptrTime(enq2)

	c := newFakeClient(t, app, ws1, ws2)
	admitter := NewAdmitter(c)
	admitter.Clock = clock.Now

	rec := &WorkspaceReconciler{
		Client:   c,
		Scheme:   newAdmissionScheme(t),
		Admitter: admitter,
		Clock:    clock.Now,
	}

	// Driving Reconcile directly against a fake client (no informer/watch
	// loop re-invoking it) means each meaningful transition needs its own
	// pass: the first call on a fresh object only adds the finalizer and
	// returns early. Three passes is enough headroom for finalizer-add +
	// the phase transition + one settle pass, and re-running an already
	// idempotent reconcile a spare time changes nothing.
	reconcile := func(ws *v1alpha1.Workspace) {
		t.Helper()
		for i := 0; i < 3; i++ {
			if _, err := rec.Reconcile(context.Background(), reconcileRequest(ws)); err != nil {
				t.Fatalf("reconcile %s (pass %d): %v", ws.Name, i+1, err)
			}
		}
	}

	// ws1 is admitted (the only slot); ws2 stays Pending behind it.
	reconcile(ws1)
	var got1 v1alpha1.Workspace
	mustGetInto(t, c, ws1, &got1)
	if got1.Status.Phase != v1alpha1.PhaseStarting {
		t.Fatalf("ws1 phase = %q, want Starting", got1.Status.Phase)
	}

	reconcile(ws2)
	var got2 v1alpha1.Workspace
	mustGetInto(t, c, ws2, &got2)
	if got2.Status.Phase != v1alpha1.PhasePending {
		t.Fatalf("ws2 phase = %q, want Pending (the slot is taken by ws1)", got2.Status.Phase)
	}

	// Advance the clock past ws1's startDeadline and reconcile it again: the
	// timeout row fires, failStarting calls RecordStartFailure through the
	// REAL Admitter this time.
	clock.Advance(2 * time.Minute)
	reconcile(&got1)

	var afterTimeout v1alpha1.Workspace
	mustGetInto(t, c, ws1, &afterTimeout)
	if afterTimeout.Status.Phase != v1alpha1.PhaseFailed {
		t.Fatalf("ws1 phase after timeout = %q, want Failed", afterTimeout.Status.Phase)
	}
	if afterTimeout.Status.ConsecutiveFailures != 1 {
		t.Fatalf("ws1 consecutiveFailures = %d, want 1", afterTimeout.Status.ConsecutiveFailures)
	}
	if afterTimeout.Status.BackoffUntil == nil || !afterTimeout.Status.BackoffUntil.After(clock.Now()) {
		t.Fatalf("ws1 backoffUntil must be armed in the future, got %v (clock=%v)", afterTimeout.Status.BackoffUntil, clock.Now())
	}
	if v, found := gatherStartFailures(t, ns, app.Name, v1alpha1.StartFailureTimeout); !found || v != 1 {
		t.Fatalf("puc_workspace_start_failures_total{reason=startup_timeout} = %v (found=%v), want exactly 1", v, found)
	}

	// The slot is released: ws2 is admitted on the next pass.
	reconcile(&got2)
	var got2b v1alpha1.Workspace
	mustGetInto(t, c, ws2, &got2b)
	if got2b.Status.Phase != v1alpha1.PhaseStarting {
		t.Fatalf("ws2 phase = %q, want Starting once ws1's slot is released", got2b.Status.Phase)
	}

	// A successful start resets consecutiveFailures to 0: advance past
	// ws1's backoff, let it retry, and forge readyReplicas to reach Ready.
	clock.Advance(2 * time.Hour)
	reconcile(&afterTimeout) // Failed -> Pending once backoffUntil has passed
	var backToPending v1alpha1.Workspace
	mustGetInto(t, c, ws1, &backToPending)
	if backToPending.Status.Phase != v1alpha1.PhasePending {
		t.Fatalf("ws1 phase after backoff expiry = %q, want Pending", backToPending.Status.Phase)
	}

	// Free ws2's slot so ws1 can be re-admitted (maxConcurrentStarts=1 here).
	got2b.Status.Phase = v1alpha1.PhaseReady
	if err := c.Status().Update(context.Background(), &got2b); err != nil {
		t.Fatalf("update ws2 to Ready: %v", err)
	}

	reconcile(&backToPending)
	var restarted v1alpha1.Workspace
	mustGetInto(t, c, ws1, &restarted)
	if restarted.Status.Phase != v1alpha1.PhaseStarting {
		t.Fatalf("ws1 phase = %q, want Starting on retry", restarted.Status.Phase)
	}

	depName := identity.ChildName(app.Name, ws1.Spec.UserKey)
	forgeReadyDeployment(t, c, ns, depName)
	reconcile(&restarted)

	var readyAgain v1alpha1.Workspace
	mustGetInto(t, c, ws1, &readyAgain)
	if readyAgain.Status.Phase != v1alpha1.PhaseReady {
		t.Fatalf("ws1 phase = %q, want Ready", readyAgain.Status.Phase)
	}
	if readyAgain.Status.ConsecutiveFailures != 0 {
		t.Fatalf("a successful start must reset consecutiveFailures to 0, got %d", readyAgain.Status.ConsecutiveFailures)
	}
}

// TestPVCNotCreatedUntilAdmitted is the create-only half spec 380-382
// requires: while admission is refused, a Get on the deterministic PVC name
// returns NotFound; once the workspace transitions to Starting, the same Get
// succeeds.
func TestPVCNotCreatedUntilAdmitted(t *testing.T) {
	ns := "ns-pvc-gate"
	app := appWithLimits(ns, 5)
	app.Status.Conditions = []metav1.Condition{{
		Type: v1alpha1.CondMigrationComplete, Status: metav1.ConditionFalse, Reason: "InProgress", Message: "migrating",
	}}
	ws := pendingWorkspace(app, "alice", time.Now(), nil)

	c := newFakeClient(t, app, ws)
	admitter := NewAdmitter(c)
	rec := &WorkspaceReconciler{Client: c, Scheme: newAdmissionScheme(t), Admitter: admitter}

	// The first pass only adds the finalizer; the second processes the
	// Pending phase and finds admission refused.
	for i := 0; i < 2; i++ {
		if _, err := rec.Reconcile(context.Background(), reconcileRequest(ws)); err != nil {
			t.Fatalf("reconcile (pass %d): %v", i+1, err)
		}
	}

	pvcName := identity.ChildName(app.Name, ws.Spec.UserKey)
	var pvc corev1.PersistentVolumeClaim
	err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: pvcName}, &pvc)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("PVC must not exist while admission is refused, got err=%v", err)
	}

	var current v1alpha1.PerUserApp
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(app), &current); err != nil {
		t.Fatalf("get app: %v", err)
	}
	baseApp := current.DeepCopy()
	current.Status.Conditions = []metav1.Condition{{
		Type: v1alpha1.CondMigrationComplete, Status: metav1.ConditionTrue, Reason: "Done", Message: "migrated",
	}}
	if err := c.Status().Patch(context.Background(), &current, client.MergeFrom(baseApp)); err != nil {
		t.Fatalf("patch app migration condition: %v", err)
	}

	if _, err := rec.Reconcile(context.Background(), reconcileRequest(ws)); err != nil {
		t.Fatalf("reconcile after migration complete: %v", err)
	}

	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: pvcName}, &pvc); err != nil {
		t.Fatalf("PVC must exist once admitted, got err=%v", err)
	}

	var afterWS v1alpha1.Workspace
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(ws), &afterWS); err != nil {
		t.Fatalf("get ws: %v", err)
	}
	if afterWS.Status.Phase != v1alpha1.PhaseStarting {
		t.Fatalf("ws phase = %q, want Starting", afterWS.Status.Phase)
	}
}

func ptrTime(tt time.Time) *metav1.Time {
	mt := metav1.NewTime(tt)
	return &mt
}

func reconcileRequest(ws *v1alpha1.Workspace) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ws.Namespace, Name: ws.Name}}
}

func mustGetInto(t *testing.T, c client.Client, ws *v1alpha1.Workspace, out *v1alpha1.Workspace) {
	t.Helper()
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(ws), out); err != nil {
		t.Fatalf("get %s: %v", ws.Name, err)
	}
}

func forgeReadyDeployment(t *testing.T, c client.Client, ns, name string) {
	t.Helper()
	var dep appsv1.Deployment
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, &dep); err != nil {
		t.Fatalf("get deployment %s: %v", name, err)
	}
	base := dep.DeepCopy()
	dep.Status.Replicas = 1
	dep.Status.ReadyReplicas = 1
	if err := c.Status().Patch(context.Background(), &dep, client.MergeFrom(base)); err != nil {
		t.Fatalf("forge ready deployment: %v", err)
	}
}
