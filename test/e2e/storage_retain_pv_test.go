//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/kettleofketchup/per-user-container-operator/api/v1alpha1"
	"github.com/kettleofketchup/per-user-container-operator/internal/controller"
	"github.com/kettleofketchup/per-user-container-operator/internal/identity"
)

// TestIsolationRetainPVRebindProhibition is the plan's assertion 6 (spec
// 292-303): README.md's runbook for recovering a Released Retain PV -- pre-
// bind spec.claimRef to the SPECIFIC intended claim's namespace/name/uid,
// never clear it -- must never hand that volume to an unrelated
// cold-starting user, and must correctly hand it to the one claim it was
// pre-bound to.
//
// Per this Step 1's own preamble, "does not bind it" is satisfied vacuously
// by a cold start that never happens at all, which would tell nobody the
// README runbook is actually safe. So this test's positive control is
// explicit and load-bearing, not decorative: the unrelated user's own
// workspace must reach Ready with its PVC Bound to a DIFFERENT PersistentVolume
// (proved by comparing PV uids, not just names), in the very same test that
// proves the intended claim binds the released one. "Bound elsewhere" and
// "never bound" only differ if both are actually observed.
//
// StorageClass binding mode: this cluster's puc-e2e-retain StorageClass is
// WaitForFirstConsumer (kind-up.sh; rancher.io/local-path cannot honor
// Immediate at all), not the Immediate task-13a-brief.md's Step 0 text
// names. This works in this test's favor rather than against it: a
// WaitForFirstConsumer PVC sits Pending, with no bind attempt at all, until
// a pod referencing it is actually scheduled -- so creating the intended
// claim and patching the Released PV's claimRef onto it is race-free by
// construction (there is no window where a provisioner could dynamically
// provision a fresh volume for it first, the way there would be under
// Immediate binding). See below for the exact sequencing this bought.
func TestIsolationRetainPVRebindProhibition(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	ns := globalEnv.Namespaces[0]
	clientPod := "puc-e2e-client"
	nonce := time.Now().UnixNano()
	identityR := fmt.Sprintf("retain-old-%d", nonce)
	identityM := fmt.Sprintf("retain-intended-%d", nonce)
	identityU := fmt.Sprintf("retain-unrelated-%d", nonce)

	// --- Provision old user R normally, and record the PV bound to R's
	// claim. ---
	if code, err := coldStart(globalEnv, ns, clientPod, smokeApp, identityR); err != nil || code != "200" {
		t.Fatalf("cold start old identity R: code=%q err=%v", code, err)
	}
	userKeyR := identity.UserKey(ns, smokeApp, identityR)
	nameR := identity.ChildName(smokeApp, userKeyR)
	pvcR, err := globalClient.CoreV1().PersistentVolumeClaims(ns).Get(ctx, nameR, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get R's PVC: %v", err)
	}
	if pvcR.Status.Phase != corev1.ClaimBound || pvcR.Spec.VolumeName == "" {
		t.Fatalf("R's PVC is not Bound (phase=%s, volumeName=%q)", pvcR.Status.Phase, pvcR.Spec.VolumeName)
	}
	pvName := pvcR.Spec.VolumeName
	pvBefore, err := globalClient.CoreV1().PersistentVolumes().Get(ctx, pvName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get PV %s bound to R: %v", pvName, err)
	}
	pvUID := pvBefore.UID
	t.Logf("old user R's claim is bound to PV %s (uid %s)", pvName, pvUID)

	// --- README.md step 1: scale R's Deployment to zero. ---
	if err := scaleDeployment(ctx, globalClient, ns, nameR, 0); err != nil {
		t.Fatalf("scale R's Deployment to 0: %v", err)
	}

	// --- README.md step 2: delete R's Workspace object and its (owned)
	// children. The controller is still running here, so the Workspace's
	// finalizer is processed and removal completes normally.
	//
	// NOTE (operational gap worth closing in the README): "its children"
	// does not, by itself, release the PV -- the per-user PVC deliberately
	// carries NO ownerReference to the Workspace (render.go's own comment:
	// a cascade delete here would be exactly the data-loss bug this whole
	// PVC design avoids), so deleting the Workspace alone leaves the PVC
	// (and therefore the PV, still Bound) untouched. Reaching "Released" at
	// all requires a further, explicit delete of the PVC, which this test
	// performs but the README's step 2 does not currently spell out. ---
	var wsR v1alpha1.Workspace
	if err := globalRuntimeClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: nameR}, &wsR); err != nil {
		t.Fatalf("get R's Workspace: %v", err)
	}
	if err := globalRuntimeClient.Delete(ctx, &wsR); err != nil {
		t.Fatalf("delete R's Workspace: %v", err)
	}
	if err := pollUntil(ctx, 2*time.Second, func() (bool, error) {
		var got v1alpha1.Workspace
		err := globalRuntimeClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: nameR}, &got)
		return apierrors.IsNotFound(err), nil
	}); err != nil {
		t.Fatalf("wait for R's Workspace to be fully deleted (finalizer processed): %v", err)
	}

	// Explicit PVC delete -- see the NOTE above.
	if err := globalClient.CoreV1().PersistentVolumeClaims(ns).Delete(ctx, nameR, metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete R's PVC: %v", err)
	}
	if err := pollUntil(ctx, 2*time.Second, func() (bool, error) {
		pv, err := globalClient.CoreV1().PersistentVolumes().Get(ctx, pvName, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		return pv.Status.Phase == corev1.VolumeReleased, nil
	}); err != nil {
		t.Fatalf("wait for PV %s to reach Released: %v", pvName, err)
	}
	t.Logf("PV %s reached Released after R's Workspace and PVC were deleted", pvName)

	// --- Quiesce the controller BEFORE creating anything for the intended
	// new user M: WorkspaceReconciler's reconcilePVC creates a claim the
	// instant it sees a Pending Workspace with none, and that race would
	// have to be won for the pre-bind patch below to land on the RIGHT
	// PVC's uid. WaitForFirstConsumer (see this test's doc comment) means
	// even an unquiesced reconciler's own PVC would just sit Pending
	// harmlessly rather than dynamically provisioning a competing PV, but
	// quiescing keeps this test's sequencing unambiguous rather than relying
	// on that. ---
	restore := quiesceController(t, ctx)

	var app v1alpha1.PerUserApp
	if err := globalRuntimeClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: smokeApp}, &app); err != nil {
		t.Fatalf("get PerUserApp %s: %v", smokeApp, err)
	}

	userKeyM := identity.UserKey(ns, smokeApp, identityM)
	nameM := identity.ChildName(smokeApp, userKeyM)
	wsM := &v1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{
			Name:        nameM,
			Namespace:   ns,
			Labels:      map[string]string{v1alpha1.LabelApp: smokeApp, v1alpha1.LabelUserKey: userKeyM},
			Annotations: map[string]string{v1alpha1.AnnUserDisplay: identityM},
		},
		Spec: v1alpha1.WorkspaceSpec{AppRef: corev1.LocalObjectReference{Name: smokeApp}, UserKey: userKeyM},
	}
	if err := globalRuntimeClient.Create(ctx, wsM); err != nil {
		t.Fatalf("create intended new user M's Workspace: %v", err)
	}

	// The intended claim, rendered by the SAME function the reconciler
	// itself uses, so its shape (labels, storageClassName, accessModes,
	// size) is byte-identical to what WorkspaceReconciler would otherwise
	// have created -- reconcilePVC lists by app+userKey labels and adopts
	// this one instead of creating a second claim once the controller is
	// restored below.
	pvcM := controller.RenderWorkspacePVC(&app, wsM)
	createdPVCM, err := globalClient.CoreV1().PersistentVolumeClaims(ns).Create(ctx, pvcM, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create intended new user M's PVC: %v", err)
	}

	// --- README.md step 3: patch the Released PV's claimRef to pre-bind it
	// directly to M's claim (namespace, name AND uid) -- never clear it. ---
	claimRefPatch := fmt.Sprintf(`{"spec":{"claimRef":{"apiVersion":"v1","kind":"PersistentVolumeClaim","namespace":%q,"name":%q,"uid":%q,"resourceVersion":null}}}`,
		createdPVCM.Namespace, createdPVCM.Name, createdPVCM.UID)
	if _, err := globalClient.CoreV1().PersistentVolumes().Patch(ctx, pvName, types.MergePatchType, []byte(claimRefPatch), metav1.PatchOptions{}); err != nil {
		t.Fatalf("pre-bind PV %s's claimRef to M's claim: %v", pvName, err)
	}
	t.Logf("pre-bound PV %s's claimRef to M's claim %s/%s (uid %s)", pvName, ns, createdPVCM.Name, createdPVCM.UID)

	restore()

	// --- Intended claim binds the released volume, and M's workspace
	// reaches Ready. ---
	if _, err := waitWorkspaceReady(ctx, ns, nameM); err != nil {
		t.Fatalf("M's workspace did not reach Ready: %v", err)
	}
	pvcMFinal, err := globalClient.CoreV1().PersistentVolumeClaims(ns).Get(ctx, nameM, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get M's PVC: %v", err)
	}
	if pvcMFinal.Status.Phase != corev1.ClaimBound {
		t.Fatalf("M's PVC is not Bound (phase=%s)", pvcMFinal.Status.Phase)
	}
	if pvcMFinal.Spec.VolumeName != pvName {
		t.Fatalf("M's PVC bound to volume %q, want the pre-bound released volume %q", pvcMFinal.Spec.VolumeName, pvName)
	}
	pvAfterM, err := globalClient.CoreV1().PersistentVolumes().Get(ctx, pvName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get PV %s after M binds: %v", pvName, err)
	}
	if pvAfterM.UID != pvUID {
		t.Fatalf("PV %s's uid changed (%s vs %s) -- this is not the same volume R's data lived on", pvName, pvAfterM.UID, pvUID)
	}
	t.Logf("assertion observed passing: M's claim bound the released volume %s (same uid %s)", pvName, pvUID)

	// --- Positive control: an unrelated, ordinary cold-starting user U
	// reaches Ready with its PVC Bound to a DIFFERENT PersistentVolume
	// (different uid, not just a different name) -- proving "bound
	// elsewhere" rather than "never bound". ---
	if code, err := coldStart(globalEnv, ns, clientPod, smokeApp, identityU); err != nil || code != "200" {
		t.Fatalf("cold start unrelated identity U: code=%q err=%v", code, err)
	}
	userKeyU := identity.UserKey(ns, smokeApp, identityU)
	nameU := identity.ChildName(smokeApp, userKeyU)
	if _, err := waitWorkspaceReady(ctx, ns, nameU); err != nil {
		t.Fatalf("POSITIVE CONTROL FAILED: unrelated user U's workspace did not reach Ready: %v", err)
	}
	pvcU, err := globalClient.CoreV1().PersistentVolumeClaims(ns).Get(ctx, nameU, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get U's PVC: %v", err)
	}
	if pvcU.Status.Phase != corev1.ClaimBound {
		t.Fatalf("POSITIVE CONTROL FAILED: unrelated user U's PVC is not Bound (phase=%s) -- 'does not bind the released PV' is meaningless if U never bound anything at all", pvcU.Status.Phase)
	}
	pvU, err := globalClient.CoreV1().PersistentVolumes().Get(ctx, pvcU.Spec.VolumeName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get U's bound PV: %v", err)
	}
	if pvU.UID == pvUID {
		t.Fatalf("ISOLATION VIOLATION: unrelated user U was bound to the SAME PersistentVolume (uid %s) the README procedure pre-bound to M", pvUID)
	}
	t.Logf("positive control observed passing: unrelated user U bound a DIFFERENT PersistentVolume (%s, uid %s) -- 'bound elsewhere', not 'never bound'", pvcU.Spec.VolumeName, pvU.UID)
}
