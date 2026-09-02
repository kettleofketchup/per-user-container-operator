package controller

import (
	"context"
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/kettleofketchup/per-user-container-operator/api/v1alpha1"
	"github.com/kettleofketchup/per-user-container-operator/internal/identity"
	"github.com/kettleofketchup/per-user-container-operator/internal/metrics"
)

// reclaimApp returns a fixture app with reclamation enabled: target 2, a one
// hour idle floor and a one hour sweep interval. Callers that need a
// different shape mutate the returned spec.
func reclaimableApp(ns string, target int32) *v1alpha1.PerUserApp {
	app := appWithLimits(ns, 5)
	app.Spec.Reclaim = &v1alpha1.ReclaimSpec{
		Enabled:          true,
		TargetWorkspaces: target,
		MinIdleAge:       metav1.Duration{Duration: time.Hour},
		Interval:         metav1.Duration{Duration: time.Hour},
	}
	return app
}

// idleWorkspace builds an Idle-phase Workspace whose lastActivity is idleFor
// before now, with zero status.connections.
func idleWorkspace(app *v1alpha1.PerUserApp, rawIdentity string, now time.Time, idleFor time.Duration) *v1alpha1.Workspace {
	ws := readyWorkspace(app, rawIdentity)
	ws.Status.Phase = v1alpha1.PhaseIdle
	ws.Status.ScaledDown = true
	ws.Status.LastActivity = ptrTime(now.Add(-idleFor))
	ws.Status.PVCRef = ws.Name
	return ws
}

// claimFor builds the per-user claim the WorkspaceReconciler would have
// rendered for ws, bound to volumeName.
func claimFor(ws *v1alpha1.Workspace, volumeName string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ws.Name,
			Namespace: ws.Namespace,
			Labels: map[string]string{
				v1alpha1.LabelApp:     ws.Spec.AppRef.Name,
				v1alpha1.LabelUserKey: ws.Spec.UserKey,
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{VolumeName: volumeName},
	}
}

func retainedVolume(name string) *corev1.PersistentVolume {
	return &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
		},
	}
}

func workspaceExists(t *testing.T, c client.Client, ws *v1alpha1.Workspace) bool {
	t.Helper()
	var got v1alpha1.Workspace
	err := c.Get(context.Background(), client.ObjectKeyFromObject(ws), &got)
	if apierrors.IsNotFound(err) {
		return false
	}
	if err != nil {
		t.Fatalf("get workspace %s: %v", ws.Name, err)
	}
	return true
}

func claimExists(t *testing.T, c client.Client, ns, name string) bool {
	t.Helper()
	var got corev1.PersistentVolumeClaim
	err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, &got)
	if apierrors.IsNotFound(err) {
		return false
	}
	if err != nil {
		t.Fatalf("get pvc %s: %v", name, err)
	}
	return true
}

// TestReclaimDeletesOnlyTheOverageAndOnlyTheOldest is the core LRU assertion:
// five workspaces against a target of two means an overage of three, so the
// three with the OLDEST lastActivity go and the two most recent stay -- even
// though all five are equally past minIdleAge and every one of them would
// satisfy the predicate on its own. Reclaiming down to target, not
// reclaiming everything eligible, is the whole difference between this and a
// TTL sweep.
func TestReclaimDeletesOnlyTheOverageAndOnlyTheOldest(t *testing.T) {
	metrics.ResetForTest()
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	app := reclaimableApp("ns-lru", 2)

	// Deliberately not in age order, so a pass that skipped the sort and
	// simply took the first three of the List would pick the wrong set.
	idleFor := map[string]time.Duration{
		"carol": 10 * time.Hour,
		"alice": 2 * time.Hour,
		"erin":  6 * time.Hour,
		"bob":   3 * time.Hour,
		"dave":  8 * time.Hour,
	}
	objs := []client.Object{app}
	byUser := map[string]*v1alpha1.Workspace{}
	for user, age := range idleFor {
		ws := idleWorkspace(app, user, now, age)
		byUser[user] = ws
		objs = append(objs, ws, claimFor(ws, "pv-"+user))
	}

	c := newFakeClient(t, objs...)
	r := &Reclaimer{Client: c, Clock: func() time.Time { return now }}
	if err := r.Reclaim(context.Background()); err != nil {
		t.Fatalf("Reclaim: %v", err)
	}

	// carol (10h), dave (8h), erin (6h) are the three least recently used.
	for _, user := range []string{"carol", "dave", "erin"} {
		if workspaceExists(t, c, byUser[user]) {
			t.Errorf("workspace for %s survived: it is among the three least recently used and the app is three over target", user)
		}
		if claimExists(t, c, app.Namespace, byUser[user].Name) {
			t.Errorf("claim for %s survived its reclaimed workspace: the PVC has no ownerReference, so nothing else will ever delete it", user)
		}
	}
	for _, user := range []string{"alice", "bob"} {
		if !workspaceExists(t, c, byUser[user]) {
			t.Errorf("workspace for %s was reclaimed: only the overage (3) may go, and %s is among the two most recently used", user, user)
		}
		if !claimExists(t, c, app.Namespace, byUser[user].Name) {
			t.Errorf("claim for %s was deleted without its workspace", user)
		}
	}

	if got, ok := gatherReapedTotal(t, app.Namespace, app.Name, v1alpha1.ReapReasonLRU); !ok || got != 3 {
		t.Errorf("puc_workspace_reaped_total{reason=lru} = %v (present=%v), want 3", got, ok)
	}
	if _, ok := gatherReapedTotal(t, app.Namespace, app.Name, v1alpha1.ReapReasonIdle); ok {
		t.Error("reclamation recorded reason=idle: a data deletion must never be countable as a scale-down")
	}
}

// TestMinIdleAgeIsAFloorNotABudget: the app is over target, but no workspace
// is idle long enough. The correct outcome is that NOTHING is reclaimed and
// the app simply stays over target -- deleting the freshest-but-still-oldest
// workspace to hit the number would delete the files of somebody who used the
// app minutes ago.
func TestMinIdleAgeIsAFloorNotABudget(t *testing.T) {
	metrics.ResetForTest()
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	app := reclaimableApp("ns-floor", 1)

	objs := []client.Object{app}
	var all []*v1alpha1.Workspace
	for i, user := range []string{"alice", "bob", "carol"} {
		// 59, 58 and 57 minutes: all under the one hour floor.
		ws := idleWorkspace(app, user, now, time.Hour-time.Duration(i+1)*time.Minute)
		all = append(all, ws)
		objs = append(objs, ws, claimFor(ws, fmt.Sprintf("pv-%d", i)))
	}

	c := newFakeClient(t, objs...)
	r := &Reclaimer{Client: c, Clock: func() time.Time { return now }}
	if err := r.Reclaim(context.Background()); err != nil {
		t.Fatalf("Reclaim: %v", err)
	}

	for _, ws := range all {
		if !workspaceExists(t, c, ws) {
			t.Errorf("workspace %s was reclaimed while idle for less than minIdleAge; being over target is not on its own a licence to delete", ws.Name)
		}
	}
}

// TestOnlyIdleAndFailedPhasesAreReclaimable pins the phase clause in both
// directions. A Ready workspace has a live pod writing to the claim, so it is
// never a candidate however old its lastActivity; a Failed one never reaches
// Idle, so excluding it would let a permanently broken workspace hold a
// maxWorkspaces slot forever.
func TestOnlyIdleAndFailedPhasesAreReclaimable(t *testing.T) {
	metrics.ResetForTest()
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	app := reclaimableApp("ns-phase", 1)

	ready := idleWorkspace(app, "alice", now, 100*time.Hour)
	ready.Status.Phase = v1alpha1.PhaseReady
	ready.Status.ScaledDown = false
	failed := idleWorkspace(app, "bob", now, 2*time.Hour)
	failed.Status.Phase = v1alpha1.PhaseFailed
	keeper := idleWorkspace(app, "carol", now, 90*time.Hour)

	c := newFakeClient(t, app, ready, claimFor(ready, "pv-a"), failed, claimFor(failed, "pv-b"), keeper, claimFor(keeper, "pv-c"))
	r := &Reclaimer{Client: c, Clock: func() time.Time { return now }}
	if err := r.Reclaim(context.Background()); err != nil {
		t.Fatalf("Reclaim: %v", err)
	}

	if !workspaceExists(t, c, ready) {
		t.Error("a Ready workspace was reclaimed: its pod is live and holding the claim, and 100h of no router activity does not change that")
	}
	if workspaceExists(t, c, failed) {
		t.Error("a Failed workspace was not reclaimed: it never reaches Idle, so excluding it holds a maxWorkspaces slot forever")
	}
	// keeper (90h) is older than failed (2h) but the overage is 2 (three
	// workspaces, target 1) and only two workspaces are candidates at all,
	// so both candidates go.
	if workspaceExists(t, c, keeper) {
		t.Error("the oldest Idle workspace was not reclaimed while the app was two over target")
	}
}

// TestReclaimIsOptInAtEveryLevel: an app with no spec.reclaim, and an app
// with reclaim.enabled false, are both left completely alone even though each
// is far over any target and has ancient idle workspaces.
func TestReclaimIsOptInAtEveryLevel(t *testing.T) {
	metrics.ResetForTest()
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	absent := appWithLimits("ns-absent", 5)
	absentWS := idleWorkspace(absent, "alice", now, 500*time.Hour)

	disabled := reclaimableApp("ns-disabled", 0)
	disabled.Spec.Reclaim.Enabled = false
	disabledWS := idleWorkspace(disabled, "bob", now, 500*time.Hour)

	c := newFakeClient(t, absent, absentWS, claimFor(absentWS, "pv-a"), disabled, disabledWS, claimFor(disabledWS, "pv-b"))
	r := &Reclaimer{Client: c, Clock: func() time.Time { return now }}
	if err := r.Reclaim(context.Background()); err != nil {
		t.Fatalf("Reclaim: %v", err)
	}

	if !workspaceExists(t, c, absentWS) {
		t.Error("an app with no spec.reclaim had a workspace deleted: absent must mean nothing is ever reclaimed")
	}
	if !workspaceExists(t, c, disabledWS) {
		t.Error("an app with reclaim.enabled false had a workspace deleted")
	}
}

// TestDeleteVolumeDataFlipsTheVolumePolicyBeforeReleasingIt is the assertion
// that the destructive option is actually destructive, and that the harmless
// default is actually harmless. Deleting the claim under a Retain volume
// frees NO disk -- the volume just goes Released with the data intact -- so
// the policy flip is the entire mechanism, and it has to land before the
// claim goes away because the policy is read at release time.
func TestDeleteVolumeDataFlipsTheVolumePolicyBeforeReleasingIt(t *testing.T) {
	for _, tc := range []struct {
		name       string
		deleteData bool
		wantPolicy corev1.PersistentVolumeReclaimPolicy
	}{
		{"default leaves the volume Retained and recoverable", false, corev1.PersistentVolumeReclaimRetain},
		{"deleteVolumeData makes the driver destroy the volume on release", true, corev1.PersistentVolumeReclaimDelete},
	} {
		t.Run(tc.name, func(t *testing.T) {
			metrics.ResetForTest()
			now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
			app := reclaimableApp("ns-vol", 0)
			app.Spec.Reclaim.DeleteVolumeData = tc.deleteData
			ws := idleWorkspace(app, "alice", now, 5*time.Hour)

			c := newFakeClient(t, app, ws, claimFor(ws, "pv-alice"), retainedVolume("pv-alice"))
			r := &Reclaimer{Client: c, Clock: func() time.Time { return now }}
			if err := r.Reclaim(context.Background()); err != nil {
				t.Fatalf("Reclaim: %v", err)
			}

			if claimExists(t, c, app.Namespace, ws.Name) {
				t.Fatal("claim survived reclamation")
			}
			var pv corev1.PersistentVolume
			if err := c.Get(context.Background(), client.ObjectKey{Name: "pv-alice"}, &pv); err != nil {
				t.Fatalf("get pv: %v", err)
			}
			if pv.Spec.PersistentVolumeReclaimPolicy != tc.wantPolicy {
				t.Errorf("PersistentVolume reclaim policy = %q, want %q", pv.Spec.PersistentVolumeReclaimPolicy, tc.wantPolicy)
			}
		})
	}
}

// TestVolumeReleaseFailureAbortsBeforeAnythingIsDeleted: if the policy patch
// fails -- overwhelmingly likely to be a missing cluster-scoped
// persistentvolumes grant -- the reclamation must fail CLOSED. The
// alternative is the worst outcome available: the claim deleted, the
// maxWorkspaces slot freed, and the volume left Retained and orphaned, so the
// operator reports having freed disk it did not free.
func TestVolumeReleaseFailureAbortsBeforeAnythingIsDeleted(t *testing.T) {
	metrics.ResetForTest()
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	app := reclaimableApp("ns-forbidden", 0)
	app.Spec.Reclaim.DeleteVolumeData = true
	ws := idleWorkspace(app, "alice", now, 5*time.Hour)

	base, ok := newFakeClient(t, app, ws, claimFor(ws, "pv-alice"), retainedVolume("pv-alice")).(client.WithWatch)
	if !ok {
		t.Fatalf("fake client does not implement client.WithWatch")
	}
	c := interceptor.NewClient(base, interceptor.Funcs{
		Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if _, isPV := obj.(*corev1.PersistentVolume); isPV {
				return apierrors.NewForbidden(corev1.Resource("persistentvolumes"), obj.GetName(), fmt.Errorf("no grant"))
			}
			return cl.Patch(ctx, obj, patch, opts...)
		},
	})

	r := &Reclaimer{Client: c, Clock: func() time.Time { return now }}
	if err := r.Reclaim(context.Background()); err == nil {
		t.Fatal("Reclaim returned nil after the volume policy patch was Forbidden; a silently-swallowed failure here is a claim deleted with nothing freed")
	}

	if !workspaceExists(t, c, ws) {
		t.Error("workspace was deleted even though the volume could not be released")
	}
	if !claimExists(t, c, app.Namespace, ws.Name) {
		t.Error("claim was deleted even though the volume could not be released: this frees the slot while the volume stays Retained and orphaned")
	}
	if got, ok := gatherReapedTotal(t, app.Namespace, app.Name, v1alpha1.ReapReasonLRU); ok {
		t.Errorf("puc_workspace_reaped_total{reason=lru} = %v after a failed reclamation; nothing was reclaimed", got)
	}
}

// TestOrphanSweepFinishesOnlyWorkThisOperatorStarted. The sweep exists
// because deleting a Workspace and deleting its claim cannot be atomic. Its
// eligibility test is the AnnReclaiming stamp and nothing weaker: "labelled
// for this app with no Workspace" also describes a claim seeded ahead of its
// Workspace by a migration, and sweeping on that would delete migrated user
// data nobody asked to reclaim.
func TestOrphanSweepFinishesOnlyWorkThisOperatorStarted(t *testing.T) {
	metrics.ResetForTest()
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	app := reclaimableApp("ns-orphan", 10)

	stampedKey := identity.UserKey(app.Namespace, app.Name, "gone")
	stamped := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:        identity.ChildName(app.Name, stampedKey),
			Namespace:   app.Namespace,
			Labels:      map[string]string{v1alpha1.LabelApp: app.Name, v1alpha1.LabelUserKey: stampedKey},
			Annotations: map[string]string{v1alpha1.AnnReclaiming: now.Add(-time.Minute).Format(time.RFC3339)},
		},
	}
	preSeededKey := identity.UserKey(app.Namespace, app.Name, "notyet")
	preSeeded := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      identity.ChildName(app.Name, preSeededKey),
			Namespace: app.Namespace,
			Labels:    map[string]string{v1alpha1.LabelApp: app.Name, v1alpha1.LabelUserKey: preSeededKey},
		},
	}

	c := newFakeClient(t, app, stamped, preSeeded)
	r := &Reclaimer{Client: c, Clock: func() time.Time { return now }}
	if err := r.Reclaim(context.Background()); err != nil {
		t.Fatalf("Reclaim: %v", err)
	}

	if claimExists(t, c, app.Namespace, stamped.Name) {
		t.Error("a stamped orphan claim survived: the sweep exists precisely to finish an interrupted reclamation")
	}
	if !claimExists(t, c, app.Namespace, preSeeded.Name) {
		t.Fatal("an UNSTAMPED workspace-less claim was deleted: this is a claim seeded ahead of its Workspace, and deleting it destroys migrated user data")
	}
}

// TestReclaimIsGatedByThePerAppInterval: a second pass inside the app's own
// reclaim.interval must be a no-op even though the app is still over target,
// exactly as lifecycle.reapInterval gates the Reaper.
func TestReclaimIsGatedByThePerAppInterval(t *testing.T) {
	metrics.ResetForTest()
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	app := reclaimableApp("ns-gate", 1)

	first := idleWorkspace(app, "alice", now, 10*time.Hour)
	second := idleWorkspace(app, "bob", now, 9*time.Hour)
	third := idleWorkspace(app, "carol", now, 8*time.Hour)

	c := newFakeClient(t, app, first, claimFor(first, "pv-a"), second, claimFor(second, "pv-b"), third, claimFor(third, "pv-c"))
	clock := now
	r := &Reclaimer{Client: c, Clock: func() time.Time { return clock }}

	if err := r.Reclaim(context.Background()); err != nil {
		t.Fatalf("first Reclaim: %v", err)
	}
	if workspaceExists(t, c, first) || workspaceExists(t, c, second) {
		t.Fatal("first pass did not reclaim the two least recently used workspaces")
	}

	// Re-create both so the app is over target again, then sweep well
	// inside the interval.
	revived := idleWorkspace(app, "alice", now, 10*time.Hour)
	if err := c.Create(context.Background(), revived); err != nil {
		t.Fatalf("recreate: %v", err)
	}
	clock = now.Add(time.Minute)
	if err := r.Reclaim(context.Background()); err != nil {
		t.Fatalf("second Reclaim: %v", err)
	}
	if !workspaceExists(t, c, revived) {
		t.Error("a second pass one minute into a one hour reclaim.interval reclaimed a workspace; the per-app interval gates evaluation, not just the ticker")
	}

	clock = now.Add(2 * time.Hour)
	if err := r.Reclaim(context.Background()); err != nil {
		t.Fatalf("third Reclaim: %v", err)
	}
	if workspaceExists(t, c, revived) {
		t.Error("a pass after the interval elapsed did not reclaim; lastPass must gate, not latch")
	}
}
