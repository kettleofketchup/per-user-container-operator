package controller

import (
	"context"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/kettleofketchup/per-user-container-operator/api/v1alpha1"
	"github.com/kettleofketchup/per-user-container-operator/internal/metrics"
)

// reaperTickInterval is the fixed controller cadence the Reaper's Runnable
// ticks at. A manager.Runnable is registered once, before any PerUserApp has
// been read, so "tick every lifecycle.reapInterval" is not registrable --
// see task-9-brief.md's "Tick shape" note. Each app's own reapInterval gates
// whether THAT app is evaluated on a given tick (see Reap).
const reaperTickInterval = 10 * time.Second

// Reaper scales idle Workspaces to zero.
//
// Write contract (task-9-brief.md): the Reaper NEVER writes to a Deployment,
// Service or PersistentVolumeClaim, and never deletes anything. It writes
// exactly one field -- Workspace.status.scaledDown = true -- as a
// conditional write on the resourceVersion it read. The WorkspaceReconciler
// (the sole writer of Deployment.spec.replicas and status.phase) observes
// that write and does the rest: scaling the Deployment to 0, waiting for the
// Pod to actually be gone, and only then marking the Workspace Idle. A
// Reaper that patched the Deployment directly would be silently reverted in
// production by the reconciler's own re-render from status.scaledDown, while
// every test that forges its own inputs (rather than running both
// controllers together) stays green -- see the brief for the full argument.
//
// Reap predicate (task-9-brief.md): a workspace is reapable iff ALL of:
//   - status.phase == Ready
//   - status.scaledDown == false
//   - now - status.lastActivity > app.spec.lifecycle.idleTimeout
//   - no status.connections entry has heartbeatAt fresher than
//     2 x lifecycle.connectionHeartbeatInterval
//
// status.connections is keyed by router replica pod name and self-expiring:
// each entry is live only while its own heartbeatAt is fresh. A dead
// replica's entry simply ages out -- it never needs to be zeroed by another
// replica, which is what makes the per-replica keying safe where a single
// absolute counter would not be (see WorkspaceStatus.Connections' doc
// comment).
type Reaper struct {
	Client client.Client

	// Clock returns the current time; defaults to time.Now when nil. Tests
	// inject a synthetic clock instead of sleeping on wall-clock time.
	Clock func() time.Time

	// TickInterval overrides the fixed controller cadence (default
	// reaperTickInterval); tests shorten it instead of waiting on a
	// production-scale ticker.
	TickInterval time.Duration

	// lastPass tracks, per PerUserApp, the last time this process evaluated
	// it for reaping. In-memory only: there is nothing to seed it from after
	// a restart (puc_reaper_last_completion_timestamp_seconds is a gauge in
	// this same process's registry and has never been set at startup), so
	// every app is due on the first pass.
	lastPass map[types.NamespacedName]time.Time
}

var (
	_ manager.Runnable               = (*Reaper)(nil)
	_ manager.LeaderElectionRunnable = (*Reaper)(nil)
)

// NewReaper returns a Reaper backed by c, using time.Now and the fixed 10s
// tick cadence.
func NewReaper(c client.Client) *Reaper {
	return &Reaper{Client: c}
}

func (r *Reaper) now() time.Time {
	if r.Clock != nil {
		return r.Clock()
	}
	return time.Now()
}

func (r *Reaper) tickInterval() time.Duration {
	if r.TickInterval > 0 {
		return r.TickInterval
	}
	return reaperTickInterval
}

// SetupWithManager registers the Reaper as a manager.Runnable so it starts
// and stops with the rest of the controller process.
func (r *Reaper) SetupWithManager(mgr ctrl.Manager) error {
	if r.Client == nil {
		r.Client = mgr.GetClient()
	}
	return mgr.Add(r)
}

// NeedLeaderElection reports that the Reaper only runs on the leader:
// several controller replicas independently scaling down the same
// Workspaces would race the same conditional write for no benefit.
func (r *Reaper) NeedLeaderElection() bool {
	return true
}

// Start implements manager.Runnable: it ticks at the fixed controller
// cadence for as long as ctx is live, running one reap pass per tick. A
// failed pass (a transient List/Get/Update error) is swallowed rather than
// returned, so one bad tick never wedges reaping for every app forever; the
// next tick simply tries again.
func (r *Reaper) Start(ctx context.Context) error {
	ticker := time.NewTicker(r.tickInterval())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			_ = r.Reap(ctx)
		}
	}
}

// Reap runs one gated reap pass: every PerUserApp is evaluated only if that
// app's own lifecycle.reapInterval has elapsed since this process last
// evaluated it (tracked in-memory in r.lastPass; an app never seen before is
// always due). Reap is exported so both Start's ticking loop and tests can
// drive a pass directly against a synthetic clock.
func (r *Reaper) Reap(ctx context.Context) error {
	now := r.now()

	var apps v1alpha1.PerUserAppList
	if err := r.Client.List(ctx, &apps); err != nil {
		return err
	}

	if r.lastPass == nil {
		r.lastPass = map[types.NamespacedName]time.Time{}
	}

	var firstErr error
	for i := range apps.Items {
		app := &apps.Items[i]
		key := types.NamespacedName{Namespace: app.Namespace, Name: app.Name}
		if last, seen := r.lastPass[key]; seen && now.Sub(last) < app.Spec.Lifecycle.ReapInterval.Duration {
			continue
		}

		if err := r.reapApp(ctx, app, now); err != nil && firstErr == nil {
			firstErr = err
		}
		r.lastPass[key] = now
		metrics.SetReaperLastCompletion(app.Namespace, app.Name, float64(now.Unix()))
	}
	return firstErr
}

// reapApp evaluates every Workspace belonging to app and scales down each
// one the predicate on Reaper's doc comment finds reapable.
func (r *Reaper) reapApp(ctx context.Context, app *v1alpha1.PerUserApp, now time.Time) error {
	var wsList v1alpha1.WorkspaceList
	if err := r.Client.List(ctx, &wsList, client.InNamespace(app.Namespace), client.MatchingLabels{v1alpha1.LabelApp: app.Name}); err != nil {
		return err
	}

	var firstErr error
	for i := range wsList.Items {
		ws := &wsList.Items[i]
		if !isReapable(ws, app, now) {
			continue
		}
		if err := r.ScaleDown(ctx, ws); err != nil {
			if apierrors.IsConflict(err) {
				// Lost the race to a concurrent write (e.g. a wake landing
				// mid-reap): the intended outcome, not a failure to surface.
				continue
			}
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// isReapable implements the four-clause reap predicate from the Reaper doc
// comment. A nil status.lastActivity (a Ready workspace on which the router
// has not yet recorded any activity) is treated as NOT idle -- there is no
// timestamp to measure "now - lastActivity" against, and reaping on absence
// of data would be the same silent-degradation failure mode the identity
// and connection contracts elsewhere in this operator exist to avoid.
func isReapable(ws *v1alpha1.Workspace, app *v1alpha1.PerUserApp, now time.Time) bool {
	if ws.Status.Phase != v1alpha1.PhaseReady {
		return false
	}
	if ws.Status.ScaledDown {
		return false
	}
	if ws.Status.LastActivity == nil {
		return false
	}
	if now.Sub(ws.Status.LastActivity.Time) <= app.Spec.Lifecycle.IdleTimeout.Duration {
		return false
	}
	return !hasLiveConnection(ws, app, now)
}

// hasLiveConnection reports whether any status.connections entry (keyed by
// router replica pod name) has a heartbeatAt fresher than
// 2 x lifecycle.connectionHeartbeatInterval. Both the router (Task 10) and
// this predicate use exactly this freshness threshold and nothing else --
// see WorkspaceStatus.Connections' doc comment for why a dead replica's
// stale entry must never pin a workspace forever, and why a live replica's
// fresh entry must never be masked by another replica's absence.
func hasLiveConnection(ws *v1alpha1.Workspace, app *v1alpha1.PerUserApp, now time.Time) bool {
	threshold := 2 * app.Spec.Lifecycle.ConnectionHeartbeatInterval.Duration
	for _, entry := range ws.Status.Connections {
		if now.Sub(entry.HeartbeatAt.Time) < threshold {
			return true
		}
	}
	return false
}

// ScaleDown performs the Reaper's one and only write: a conditional
// status.scaledDown = true on the resourceVersion ws carries. It is exported
// (rather than folded invisibly into reapApp's loop) because it is the exact
// unit the write contract's optimistic-concurrency guarantee is about, and
// TestWakeDuringReapIsNotLost (test/envtest/reaper_test.go) drives it
// directly against a deliberately stale ws to prove that guarantee holds
// against a real API server -- a fake client need not honor resourceVersion
// semantics at all, so that property is only meaningfully testable there.
//
// It patches a DEEP COPY of ws (never ws itself -- the same aliasing hazard
// Task 8's RecordStartFailure documents), so a caller iterating
// wsList.Items is never surprised by an in-place mutation. Because this is a
// full Status().Update (not a diff-based Patch), the API server rejects the
// whole write with a Conflict if ws.ResourceVersion is stale -- e.g. because
// the router just wrote status.wakeRequestedAt -- and the wake is never
// partially applied over: it simply wins the race outright, with zero
// fields on the live object touched by the losing write.
func (r *Reaper) ScaleDown(ctx context.Context, ws *v1alpha1.Workspace) error {
	patchTarget := ws.DeepCopy()
	patchTarget.Status.ScaledDown = true
	if err := r.Client.Status().Update(ctx, patchTarget); err != nil {
		return err
	}
	metrics.RecordWorkspaceReaped(ws.Namespace, ws.Spec.AppRef.Name, v1alpha1.ReapReasonIdle)
	return nil
}
