package controller

import (
	"context"
	"fmt"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/kettleofketchup/per-user-container-operator/api/v1alpha1"
	"github.com/kettleofketchup/per-user-container-operator/internal/metrics"
)

// reclaimerTickInterval is the fixed controller cadence the Reclaimer's
// Runnable ticks at, and it is deliberately six times coarser than
// reaperTickInterval. A manager.Runnable is registered before any PerUserApp
// has been read, so as with the Reaper the per-app spec.reclaim.interval
// cannot be the ticker period -- it gates whether a given app is evaluated on
// a given tick. The Reaper's 10s exists because a scale-down is cheap,
// reversible and wanted promptly after idleTimeout; reclamation is none of
// those. Its intervals are measured in hours, and every tick costs a List of
// every Workspace in every watched namespace, so a fast tick would buy
// nothing but API traffic.
const reclaimerTickInterval = 60 * time.Second

// Reclaimer deletes the least recently used Workspaces of an app that is over
// its reclaim target, together with their per-user PersistentVolumeClaims.
//
// This is the only component in the operator that destroys a user's data, and
// the whole of it is opt-in: an app with no spec.reclaim, or with
// reclaim.enabled false, is skipped entirely and no Workspace of it is ever
// deleted by this code path.
//
// Reclaim predicate. A workspace is a candidate iff ALL of:
//   - it is not already being deleted (nil deletionTimestamp)
//   - status.phase is Idle or Failed. Idle means the Reaper scaled it down
//     and the reconciler observed the Pod actually gone, so nothing is
//     writing to the claim. Failed is included because a workspace that never
//     recovers never reaches Idle, and excluding it would let a permanently
//     broken workspace hold a limits.maxWorkspaces slot forever -- the exact
//     outage targetWorkspaces exists to prevent. Ready and Starting are never
//     candidates: a live pod is a user at work.
//   - status.lastActivity is non-nil and older than reclaim.minIdleAge
//   - the fresh connection count is zero, on exactly the Reaper's definition
//
// Candidates are then sorted by status.lastActivity ascending (least recently
// used first) and only the first (total - targetWorkspaces) are reclaimed. If
// fewer candidates pass minIdleAge than that, FEWER are reclaimed and the app
// stays over target: minIdleAge is a floor, never a budget to be met.
//
// Write contract. Unlike the Reaper -- which writes one status field and
// deletes nothing -- the Reclaimer deletes Workspaces and PersistentVolumeClaims,
// and with reclaim.deleteVolumeData patches the bound PersistentVolume's
// reclaim policy. It still never writes a Deployment, a Service, or any
// Workspace status field: the WorkspaceReconciler remains the sole writer of
// those, and the Deployment and Service go away through their ownerReferences
// when the Workspace does. The PVC has no ownerReference by design (see
// RenderWorkspacePVC), which is precisely why deleting it has to be explicit
// here rather than a cascade nobody chose.
type Reclaimer struct {
	Client client.Client

	// Clock returns the current time; defaults to time.Now when nil. Tests
	// inject a synthetic clock rather than sleeping out a minIdleAge.
	Clock func() time.Time

	// TickInterval overrides the fixed controller cadence (default
	// reclaimerTickInterval).
	TickInterval time.Duration

	// lastPass tracks, per PerUserApp, the last time this process evaluated
	// it. In-memory only, exactly as the Reaper's is: every app is due on the
	// first pass after a restart. That is safe here for the same reason the
	// predicate is safe at all -- being due only means being evaluated, and
	// an evaluation reclaims nothing unless the app is over target with
	// candidates past minIdleAge.
	lastPass map[types.NamespacedName]time.Time
}

var (
	_ manager.Runnable               = (*Reclaimer)(nil)
	_ manager.LeaderElectionRunnable = (*Reclaimer)(nil)
)

// NewReclaimer returns a Reclaimer backed by c, using time.Now and the fixed
// 60s tick cadence.
func NewReclaimer(c client.Client) *Reclaimer {
	return &Reclaimer{Client: c}
}

func (r *Reclaimer) now() time.Time {
	if r.Clock != nil {
		return r.Clock()
	}
	return time.Now()
}

func (r *Reclaimer) tickInterval() time.Duration {
	if r.TickInterval > 0 {
		return r.TickInterval
	}
	return reclaimerTickInterval
}

// SetupWithManager registers the Reclaimer as a manager.Runnable so it starts
// and stops with the rest of the controller process.
func (r *Reclaimer) SetupWithManager(mgr ctrl.Manager) error {
	if r.Client == nil {
		r.Client = mgr.GetClient()
	}
	return mgr.Add(r)
}

// NeedLeaderElection reports that the Reclaimer only runs on the leader. This
// matters more here than it does for the Reaper: two replicas independently
// computing "the N least recently used workspaces" from two slightly
// different Lists would each delete their own N, and the app would be
// reclaimed twice as far below target as anyone asked for.
func (r *Reclaimer) NeedLeaderElection() bool {
	return true
}

// Start implements manager.Runnable, running one reclaim pass per tick for as
// long as ctx is live. A failed pass is swallowed rather than returned, so one
// bad tick never wedges reclamation for every app; the next tick retries.
func (r *Reclaimer) Start(ctx context.Context) error {
	ticker := time.NewTicker(r.tickInterval())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			_ = r.Reclaim(ctx)
		}
	}
}

// Reclaim runs one gated pass: every PerUserApp with reclamation enabled is
// evaluated only if its own spec.reclaim.interval has elapsed since this
// process last evaluated it. Exported so both Start's loop and tests can drive
// a pass directly against a synthetic clock.
//
// lastPass is advanced for an app only when reclaimApp genuinely succeeds, on
// the Reaper's reasoning: a failed pass that advanced the gate would defer the
// next real attempt by a full interval, which for an interval measured in
// hours means an app sits over target for most of a day after one transient
// List error.
func (r *Reclaimer) Reclaim(ctx context.Context) error {
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
		if app.Spec.Reclaim == nil || !app.Spec.Reclaim.Enabled {
			continue
		}
		key := types.NamespacedName{Namespace: app.Namespace, Name: app.Name}
		if last, seen := r.lastPass[key]; seen && now.Sub(last) < app.Spec.Reclaim.Interval.Duration {
			continue
		}

		if err := r.reclaimApp(ctx, app, now); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			logf.FromContext(ctx).Error(err, "reclaim pass failed; not advancing lastPass so the next tick retries rather than waiting a full reclaim interval",
				"namespace", app.Namespace, "app", app.Name)
			continue
		}
		r.lastPass[key] = now
	}
	return firstErr
}

// reclaimApp finishes any reclamation a previous pass left half-done, then
// reclaims app down to reclaim.targetWorkspaces if it is over it.
func (r *Reclaimer) reclaimApp(ctx context.Context, app *v1alpha1.PerUserApp, now time.Time) error {
	var wsList v1alpha1.WorkspaceList
	if err := r.Client.List(ctx, &wsList, client.InNamespace(app.Namespace), client.MatchingLabels{v1alpha1.LabelApp: app.Name}); err != nil {
		return err
	}

	if err := r.reclaimOrphanedPVCs(ctx, app, &wsList); err != nil {
		return err
	}

	// A workspace already carrying a deletionTimestamp is on its way out and
	// no longer occupies a slot; counting it would make a pass that is
	// waiting on finalizers look still-over-target and reclaim a second
	// round of workspaces for the same overage.
	var live []*v1alpha1.Workspace
	for i := range wsList.Items {
		ws := &wsList.Items[i]
		if ws.DeletionTimestamp != nil {
			continue
		}
		live = append(live, ws)
	}

	over := len(live) - int(app.Spec.Reclaim.TargetWorkspaces)
	if over <= 0 {
		return nil
	}

	var candidates []*v1alpha1.Workspace
	for _, ws := range live {
		if isReclaimable(ws, app, now) {
			candidates = append(candidates, ws)
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].Status.LastActivity.Time.Before(candidates[j].Status.LastActivity.Time)
	})
	if len(candidates) > over {
		candidates = candidates[:over]
	}

	var firstErr error
	for _, ws := range candidates {
		if err := r.reclaimWorkspace(ctx, app, ws); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			logf.FromContext(ctx).Error(err, "reclaiming workspace failed",
				"namespace", ws.Namespace, "workspace", ws.Name)
		}
	}
	return firstErr
}

// isReclaimable implements the candidate predicate from the Reclaimer doc
// comment. A nil status.lastActivity is NOT reclaimable, on the Reaper's
// reasoning and more so: there is no timestamp to measure minIdleAge against,
// and here acting on absent data deletes files rather than scaling a pod.
func isReclaimable(ws *v1alpha1.Workspace, app *v1alpha1.PerUserApp, now time.Time) bool {
	if ws.DeletionTimestamp != nil {
		return false
	}
	if ws.Status.Phase != v1alpha1.PhaseIdle && ws.Status.Phase != v1alpha1.PhaseFailed {
		return false
	}
	if ws.Status.LastActivity == nil {
		return false
	}
	if now.Sub(ws.Status.LastActivity.Time) < app.Spec.Reclaim.MinIdleAge.Duration {
		return false
	}
	return freshConnectionCount(ws, app, now) == 0
}

// reclaimWorkspace performs the deletion, in an order chosen for what each
// possible crash leaves behind rather than for brevity:
//
//  1. If deleteVolumeData, flip the bound PersistentVolume's reclaim policy
//     from Retain to Delete. This MUST precede the claim's deletion: the
//     policy is read when the claim releases the volume, so patching it
//     afterwards patches an already-Released volume and frees nothing. If
//     this step fails -- most plausibly because the controller was not
//     granted the cluster-scoped persistentvolumes patch -- the whole
//     reclamation aborts having deleted nothing.
//  2. Stamp AnnReclaiming on the claim, so a crash after step 3 leaves
//     evidence that this operator, and not a migration, owns the orphan.
//  3. Delete the Workspace. This frees the limits.maxWorkspaces slot and
//     cascades the Deployment and Service through their ownerReferences.
//  4. Delete the claim.
//
// Steps 3 and 4 cannot be atomic. Deleting the claim first would be worse: the
// WorkspaceReconciler would re-create an empty claim the moment it reconciled
// the still-live Workspace, and the workspace would return to Pending -- where
// it is no longer reclaimable, holds its slot forever, and has lost the user's
// files anyway. In this order the only casualty of a crash between them is an
// orphaned claim, which reclaimOrphanedPVCs finishes on the next pass.
func (r *Reclaimer) reclaimWorkspace(ctx context.Context, app *v1alpha1.PerUserApp, ws *v1alpha1.Workspace) error {
	pvcName := childName(app, ws)
	var pvc corev1.PersistentVolumeClaim
	err := r.Client.Get(ctx, types.NamespacedName{Namespace: ws.Namespace, Name: pvcName}, &pvc)
	switch {
	case apierrors.IsNotFound(err):
		// Nothing to free; the Workspace deletion below is the whole job.
	case err != nil:
		return err
	default:
		if app.Spec.Reclaim.DeleteVolumeData {
			if err := r.releaseVolume(ctx, &pvc); err != nil {
				return err
			}
		}
		if err := r.markReclaiming(ctx, &pvc); err != nil {
			return err
		}
	}

	if err := r.Client.Delete(ctx, ws); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	if err := r.Client.Delete(ctx, &pvc); err != nil && !apierrors.IsNotFound(err) {
		return err
	}

	metrics.RecordWorkspaceReaped(ws.Namespace, app.Name, v1alpha1.ReapReasonLRU)
	return nil
}

// releaseVolume patches the PersistentVolume bound to pvc so that releasing
// the claim destroys the backing volume.
//
// The operator only accepts Retain-reclaim StorageClasses, which is what makes
// ordinary reaping non-destructive: deleting a claim leaves its volume
// Released with the data intact. That same property means deleting the claim
// -- or the PersistentVolume object -- frees no disk whatsoever; the second
// merely orphans the backing image with nothing in the cluster referencing it.
// Flipping persistentVolumeReclaimPolicy to Delete first is the only thing
// that makes the CSI driver actually destroy it, and it is irreversible once
// the release happens.
//
// An unbound claim (empty spec.volumeName) has no volume to free and is not an
// error: the claim is deleted and there was never any data.
func (r *Reclaimer) releaseVolume(ctx context.Context, pvc *corev1.PersistentVolumeClaim) error {
	if pvc.Spec.VolumeName == "" {
		return nil
	}
	var pv corev1.PersistentVolume
	if err := r.Client.Get(ctx, types.NamespacedName{Name: pvc.Spec.VolumeName}, &pv); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("reading bound PersistentVolume %s: %w", pvc.Spec.VolumeName, err)
	}
	if pv.Spec.PersistentVolumeReclaimPolicy == corev1.PersistentVolumeReclaimDelete {
		return nil
	}
	patch := client.MergeFrom(pv.DeepCopy())
	pv.Spec.PersistentVolumeReclaimPolicy = corev1.PersistentVolumeReclaimDelete
	if err := r.Client.Patch(ctx, &pv, patch); err != nil {
		return fmt.Errorf("setting reclaim policy Delete on PersistentVolume %s: %w", pv.Name, err)
	}
	return nil
}

// markReclaiming stamps AnnReclaiming on pvc. It is a merge patch rather than
// an Update so it cannot lose a concurrent write by the WorkspaceReconciler
// (which owns the claim's resize path) to a field this call never read.
func (r *Reclaimer) markReclaiming(ctx context.Context, pvc *corev1.PersistentVolumeClaim) error {
	if _, ok := pvc.Annotations[v1alpha1.AnnReclaiming]; ok {
		return nil
	}
	patch := client.MergeFrom(pvc.DeepCopy())
	if pvc.Annotations == nil {
		pvc.Annotations = map[string]string{}
	}
	pvc.Annotations[v1alpha1.AnnReclaiming] = r.now().UTC().Format(time.RFC3339)
	return r.Client.Patch(ctx, pvc, patch)
}

// reclaimOrphanedPVCs deletes claims that a previous pass stamped
// AnnReclaiming and whose Workspace is already gone -- the one state a crash
// between reclaimWorkspace's last two deletes can leave.
//
// The stamp is the entire eligibility test, and nothing weaker would do.
// "Labelled for this app and has no Workspace" also describes a claim seeded
// ahead of its Workspace, so a sweep on that condition would delete migrated
// user data that no one asked to reclaim. Requiring the annotation means the
// sweep only ever finishes work this operator itself began.
func (r *Reclaimer) reclaimOrphanedPVCs(ctx context.Context, app *v1alpha1.PerUserApp, wsList *v1alpha1.WorkspaceList) error {
	var pvcs corev1.PersistentVolumeClaimList
	if err := r.Client.List(ctx, &pvcs, client.InNamespace(app.Namespace), client.MatchingLabels{v1alpha1.LabelApp: app.Name}); err != nil {
		return err
	}

	liveNames := make(map[string]struct{}, len(wsList.Items))
	for i := range wsList.Items {
		liveNames[childName(app, &wsList.Items[i])] = struct{}{}
	}

	var firstErr error
	for i := range pvcs.Items {
		pvc := &pvcs.Items[i]
		if pvc.DeletionTimestamp != nil {
			continue
		}
		if _, stamped := pvc.Annotations[v1alpha1.AnnReclaiming]; !stamped {
			continue
		}
		if _, live := liveNames[pvc.Name]; live {
			continue
		}
		if err := r.Client.Delete(ctx, pvc); err != nil && !apierrors.IsNotFound(err) {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		logf.FromContext(ctx).Info("deleted orphaned per-user claim left by an interrupted reclamation",
			"namespace", pvc.Namespace, "pvc", pvc.Name)
	}
	return firstErr
}
