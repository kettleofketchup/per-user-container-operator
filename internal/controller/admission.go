package controller

import (
	"context"
	"sort"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kettleofketchup/per-user-container-operator/api/v1alpha1"
	"github.com/kettleofketchup/per-user-container-operator/internal/identity"
	"github.com/kettleofketchup/per-user-container-operator/internal/metrics"
)

// admissionBaseBackoff is the first backoff interval on a workspace's first
// recorded start failure; RecordStartFailure doubles it per additional
// consecutive failure, capped at the app's lifecycle.backoffMax.
const admissionBaseBackoff = 5 * time.Second

// Admitter is the real implementation of the WorkspaceReconciler's Admitter
// seam (declared in workspace_controller.go): FIFO admission on
// status.enqueuedAt bounded by spec.limits.maxConcurrentStarts, the
// spec.limits/MigrationComplete gate, and the exponential backoff
// arithmetic. It is controller-side and lease-scoped rather than
// router-side: a semaphore living in the router's Deployment bounds nothing
// across two replicas and forgets every in-flight start on a router
// restart, whereas this type reads and writes Workspace.status through the
// same client the reconciler uses, so the bound holds across both router
// replicas and across controller restarts.
type Admitter struct {
	Client client.Client
	// Clock returns the current time; defaults to time.Now when nil. Tests
	// inject a synthetic clock instead of sleeping on wall-clock time.
	Clock func() time.Time
}

// NewAdmitter returns an Admitter backed by c, using time.Now.
func NewAdmitter(c client.Client) *Admitter {
	return &Admitter{Client: c}
}

func (a *Admitter) now() time.Time {
	if a.Clock != nil {
		return a.Clock()
	}
	return time.Now()
}

// TryAdmit reports whether ws may transition Pending -> Starting. It refuses
// in three cases, checked in order: app.status carries MigrationComplete in
// a non-True state (spec 623); ws.status.backoffUntil is still in the
// future; or ws is not among the earliest
// spec.limits.maxConcurrentStarts-minus-currently-Starting Pending
// workspaces for this app, ranked by status.enqueuedAt (falling back to
// creationTimestamp when enqueuedAt is unset, which only ever changes the
// outcome when ws has no competing Pending sibling).
//
// A Starting workspace whose backoffUntil is already armed does NOT count
// against maxConcurrentStarts: RecordStartFailure (including the
// OnLeaseAcquired orphan sweep) arms backoffUntil the moment a start fails,
// and that non-nil field is what actually releases the slot -- the
// reconciler's own Starting->Failed transition, which flips status.phase,
// runs on the next reconcile of that object and would otherwise leave a
// visible gap where the zombie still blocks new admissions.
func (a *Admitter) TryAdmit(ctx context.Context, ws *v1alpha1.Workspace, app *v1alpha1.PerUserApp) (bool, error) {
	if cond := apimeta.FindStatusCondition(app.Status.Conditions, v1alpha1.CondMigrationComplete); cond != nil && cond.Status != metav1.ConditionTrue {
		return false, nil
	}

	if ws.Status.BackoffUntil != nil && a.now().Before(ws.Status.BackoffUntil.Time) {
		return false, nil
	}

	var list v1alpha1.WorkspaceList
	if err := a.Client.List(ctx, &list, client.InNamespace(ws.Namespace), client.MatchingLabels{v1alpha1.LabelApp: app.Name}); err != nil {
		return false, err
	}

	occupied := 0
	var candidates []*v1alpha1.Workspace
	for i := range list.Items {
		w := &list.Items[i]
		switch w.Status.Phase {
		case v1alpha1.PhaseStarting:
			if w.Status.BackoffUntil == nil {
				occupied++
			}
		case v1alpha1.PhasePending:
			candidates = append(candidates, w)
		}
	}

	slots := int(app.Spec.Limits.MaxConcurrentStarts) - occupied
	if slots <= 0 {
		return false, nil
	}

	sort.SliceStable(candidates, func(i, j int) bool {
		return enqueueKey(candidates[i]).Before(enqueueKey(candidates[j]))
	})

	for i, w := range candidates {
		if i >= slots {
			break
		}
		if w.Namespace == ws.Namespace && w.Name == ws.Name {
			return true, nil
		}
	}
	return false, nil
}

// enqueueKey returns w's FIFO ordering key: status.enqueuedAt when set, else
// creationTimestamp. creationTimestamp is second-granular and so gives no
// total order across a burst of simultaneous first requests, but it is a
// harmless fallback here because it only decides ranking among Pending
// workspaces that have not yet had enqueuedAt written -- the common case
// where the fallback matters is a single workspace with no competing
// sibling, where any deterministic key produces the same admission result.
func enqueueKey(w *v1alpha1.Workspace) time.Time {
	if w.Status.EnqueuedAt != nil {
		return w.Status.EnqueuedAt.Time
	}
	return w.CreationTimestamp.Time
}

// backoffDuration returns the backoff interval for the failures-th
// consecutive failure (1-indexed), doubling from admissionBaseBackoff and
// capped at max.
func backoffDuration(failures int32, backoffMax time.Duration) time.Duration {
	if failures < 1 {
		failures = 1
	}
	d := admissionBaseBackoff
	for i := int32(1); i < failures; i++ {
		if d > backoffMax/2 {
			return backoffMax
		}
		d *= 2
	}
	if d > backoffMax {
		return backoffMax
	}
	return d
}

// RecordStartFailure implements the Admitter interface's other method. It
// increments status.consecutiveFailures, arms status.backoffUntil
// exponentially (capped at app.spec.lifecycle.backoffMax), and records
// puc_workspace_start_failures_total{reason} -- the sole emitter of that
// series, per the interface's own documentation in workspace_controller.go.
// It never writes status.phase: the WorkspaceReconciler is the sole writer
// of that field (Task 6 Step 3), and armed backoffUntil on a Starting
// workspace with no Deployment is the signal that field's transition table
// reads to derive Failed on its own next pass.
//
// It patches a DEEP COPY of ws, not ws itself: a client.Patch call decodes
// the server's response back into whatever object it was given, and the
// caller (failStarting) has already set ws.Status.Phase = Failed in memory
// before calling this -- patching ws directly would silently revert that
// local mutation back to whatever phase the server still holds (this call's
// own diff never touches phase), so the reconciler's own subsequent status
// patch would then find no phase change to persist. Only the two fields this
// type owns are copied back onto the caller's ws.
func (a *Admitter) RecordStartFailure(ctx context.Context, ws *v1alpha1.Workspace, app *v1alpha1.PerUserApp, reason string) error {
	patchTarget := ws.DeepCopy()
	base := patchTarget.DeepCopy()
	patchTarget.Status.ConsecutiveFailures++
	until := metav1.NewTime(a.now().Add(backoffDuration(patchTarget.Status.ConsecutiveFailures, app.Spec.Lifecycle.BackoffMax.Duration)))
	patchTarget.Status.BackoffUntil = &until
	if err := a.Client.Status().Patch(ctx, patchTarget, client.MergeFrom(base)); err != nil {
		return err
	}
	ws.Status.ConsecutiveFailures = patchTarget.Status.ConsecutiveFailures
	ws.Status.BackoffUntil = patchTarget.Status.BackoffUntil
	ws.ResourceVersion = patchTarget.ResourceVersion
	metrics.RecordStartFailure(ws.Namespace, app.Name, reason)
	return nil
}

// OnLeaseAcquired is called once by cmd/main.go (Task 11) whenever this
// controller pod acquires (or re-acquires, after a gap) leadership, with
// leaderless = now - lease.spec.renewTime measured at acquisition. It walks
// every Starting Workspace across all served namespaces and does exactly one
// of two things:
//
//   - No Deployment exists for it: this is the zombie shape left behind by a
//     controller killed between the Starting status write and the
//     Deployment create (or by any other gap where the Starting write
//     landed but the child objects never did). It is recorded as a start
//     failure with reason start_orphaned through RecordStartFailure, which
//     arms backoffUntil, increments consecutiveFailures, emits the metric,
//     and -- critically -- never touches status.phase.
//   - A Deployment exists: this workspace is healthy and mid-start; its
//     startDeadline is EXTENDED by the leaderless interval, never reset to a
//     fresh now()+startupTimeout. startDeadline is absolute specifically so
//     it survives leader failover; resetting it here would make repeated
//     short leader gaps (a routine chart upgrade, say) erase real elapsed
//     time and let a workspace run past its true timeout unnoticed, while
//     leaving it untouched would fail a healthy fleet the moment any single
//     leaderless gap outlives whatever startupTimeout remained.
func (a *Admitter) OnLeaseAcquired(ctx context.Context, leaderless time.Duration) error {
	var list v1alpha1.WorkspaceList
	if err := a.Client.List(ctx, &list); err != nil {
		return err
	}

	for i := range list.Items {
		ws := &list.Items[i]
		if ws.Status.Phase != v1alpha1.PhaseStarting {
			continue
		}

		var app v1alpha1.PerUserApp
		if err := a.Client.Get(ctx, types.NamespacedName{Namespace: ws.Namespace, Name: ws.Spec.AppRef.Name}, &app); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return err
		}

		depName := identity.ChildName(app.Name, ws.Spec.UserKey)
		var dep appsv1.Deployment
		switch err := a.Client.Get(ctx, types.NamespacedName{Namespace: ws.Namespace, Name: depName}, &dep); {
		case apierrors.IsNotFound(err):
			if rerr := a.RecordStartFailure(ctx, ws, &app, v1alpha1.StartFailureOrphaned); rerr != nil {
				return rerr
			}
		case err != nil:
			return err
		default:
			if ws.Status.StartDeadline == nil {
				continue
			}
			base := ws.DeepCopy()
			extended := metav1.NewTime(ws.Status.StartDeadline.Add(leaderless))
			ws.Status.StartDeadline = &extended
			if err := a.Client.Status().Patch(ctx, ws, client.MergeFrom(base)); err != nil {
				return err
			}
		}
	}
	return nil
}
