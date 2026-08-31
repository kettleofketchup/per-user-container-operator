//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kettleofketchup/per-user-container-operator/api/v1alpha1"
	"github.com/kettleofketchup/per-user-container-operator/internal/identity"
)

// deleteWatcher runs in the background for the duration of a bounded
// window and reports every DELETE event it observes for a specific
// name/namespace, on either a Workspace or a PersistentVolumeClaim. It is
// this test's "a garbage collector actually runs" observation channel
// (task-13b-brief.md assertion 5): a live Watch against the real apiserver,
// which is backed by the SAME real Kubernetes garbage-collector controller
// this kind cluster runs as part of its embedded kube-controller-manager --
// the exact process envtest (Task 6's suite) does not run, which is why
// that suite structurally cannot see this property.
type deleteWatcher struct {
	deletes chan string // object description string, sent on every DELETE event
	stop    func()
}

// watchWorkspaceDeletes watches every Workspace in ns and reports a
// DELETE event's name over the returned channel until ctx is done or Stop
// is called.
func watchWorkspaceDeletes(ctx context.Context, ns string) (*deleteWatcher, error) {
	w, err := globalRuntimeClient.Watch(ctx, &v1alpha1.WorkspaceList{}, client.InNamespace(ns))
	if err != nil {
		return nil, fmt.Errorf("watch Workspaces in %s: %w", ns, err)
	}
	out := make(chan string, 16)
	go func() {
		for ev := range w.ResultChan() {
			if ev.Type != watch.Deleted {
				continue
			}
			if ws, ok := ev.Object.(*v1alpha1.Workspace); ok {
				out <- fmt.Sprintf("Workspace %s/%s (uid %s)", ws.Namespace, ws.Name, ws.UID)
			}
		}
		close(out)
	}()
	return &deleteWatcher{deletes: out, stop: w.Stop}, nil
}

// watchPVCDeletes watches every PersistentVolumeClaim in ns the same way.
func watchPVCDeletes(ctx context.Context, ns string) (*deleteWatcher, error) {
	w, err := globalClient.CoreV1().PersistentVolumeClaims(ns).Watch(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("watch PersistentVolumeClaims in %s: %w", ns, err)
	}
	out := make(chan string, 16)
	go func() {
		for ev := range w.ResultChan() {
			if ev.Type != watch.Deleted {
				continue
			}
			if pvc, ok := ev.Object.(*corev1.PersistentVolumeClaim); ok {
				out <- fmt.Sprintf("PersistentVolumeClaim %s/%s (uid %s)", pvc.Namespace, pvc.Name, pvc.UID)
			}
		}
		close(out)
	}()
	return &deleteWatcher{deletes: out, stop: w.Stop}, nil
}

// TestIsolationStorageSurvivesAppDeleteRecreate is the plan's assertion 5
// (spec 371-374): deleting and recreating a PerUserApp must cascade-delete
// NEITHER a user's Workspace NOR its PersistentVolumeClaim. Task 6's envtest
// suite cannot see this property (envtest does not run a garbage-collector
// controller at all), and Task 5's render-time check covers only the
// rendered object's own ownerReferences, not an actual controller-issued
// delete or an ArgoCD prune against a live apiserver.
func TestIsolationStorageSurvivesAppDeleteRecreate(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	ns := globalEnv.Namespaces[0]
	clientPod := "puc-e2e-client"
	identitySurvivor := fmt.Sprintf("iso-survivor-%d", time.Now().UnixNano())
	const marker = "/workspace/marker-survivor"
	const content = "still-here-after-recreate"

	// --- Set up: cold start a workspace and write a marker, with a
	// positive control on the write (matching assertion 1's shape): read it
	// back before trusting anything about it surviving. ---
	if code, err := coldStart(globalEnv, ns, clientPod, smokeApp, identitySurvivor); err != nil || code != "200" {
		t.Fatalf("cold start survivor identity: code=%q err=%v", code, err)
	}
	userKey := identity.UserKey(ns, smokeApp, identitySurvivor)
	childName := identity.ChildName(smokeApp, userKey)
	pod, err := findWorkspacePod(ctx, globalClient, ns, smokeApp, userKey)
	if err != nil {
		t.Fatalf("find survivor's workspace pod: %v", err)
	}
	if err := writeMarker(globalEnv.Kubeconfig, ns, pod.Name, marker, content); err != nil {
		t.Fatalf("write survivor marker: %v", err)
	}
	if got, err := readMarker(globalEnv.Kubeconfig, ns, pod.Name, marker); err != nil || got != content {
		t.Fatalf("POSITIVE CONTROL FAILED: could not read back the marker just written (got %q, err %v)", got, err)
	}
	t.Log("positive control observed passing: survivor marker written and read back before the delete/recreate cycle")

	var wsBefore v1alpha1.Workspace
	if err := globalRuntimeClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: childName}, &wsBefore); err != nil {
		t.Fatalf("get Workspace before delete/recreate: %v", err)
	}
	pvcBefore, err := globalClient.CoreV1().PersistentVolumeClaims(ns).Get(ctx, childName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get PVC before delete/recreate: %v", err)
	}

	// --- Snapshot the PerUserApp's spec so it can be recreated identically,
	// then start the delete-event watches BEFORE deleting it. ---
	var app v1alpha1.PerUserApp
	if err := globalRuntimeClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: smokeApp}, &app); err != nil {
		t.Fatalf("get PerUserApp %s before delete/recreate: %v", smokeApp, err)
	}
	recreated := &v1alpha1.PerUserApp{
		ObjectMeta: metav1.ObjectMeta{Name: app.Name, Namespace: app.Namespace, Labels: app.Labels, Annotations: app.Annotations},
		Spec:       *app.Spec.DeepCopy(),
	}

	wsWatcher, err := watchWorkspaceDeletes(ctx, ns)
	if err != nil {
		t.Fatalf("start Workspace delete watch: %v", err)
	}
	defer wsWatcher.stop()
	pvcWatcher, err := watchPVCDeletes(ctx, ns)
	if err != nil {
		t.Fatalf("start PVC delete watch: %v", err)
	}
	defer pvcWatcher.stop()

	// --- Positive control: prove BOTH watch channels actually observe a
	// real delete before trusting either one's silence below. Two separate
	// client machinery paths back these watches (globalRuntimeClient's
	// generic Watch for the Workspace CRD, the typed clientset's Watch for
	// PersistentVolumeClaim), so one working is not evidence the other
	// does -- matching this dispatch's assertion 1 requiring a positive
	// control on each half of ITS observation channel. The canary Workspace
	// names a nonexistent PerUserApp in spec.appRef so WorkspaceReconciler's
	// "app not found" branch (a Get that returns NotFound, then just
	// requeues) is its only interaction with it -- it never reaches
	// reconcilePending, so it never provisions a real Deployment/PVC of its
	// own. ---
	canaryName := fmt.Sprintf("canary-delete-probe-%d", time.Now().UnixNano())
	canaryWS := &v1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: canaryName, Namespace: ns},
		Spec:       v1alpha1.WorkspaceSpec{AppRef: corev1.LocalObjectReference{Name: "canary-nonexistent-app"}, UserKey: "u-canary000000"},
	}
	if err := globalRuntimeClient.Create(ctx, canaryWS); err != nil {
		t.Fatalf("create canary Workspace: %v", err)
	}
	storageClass := app.Spec.Storage.StorageClassName
	canaryPVC := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: canaryName, Namespace: ns},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOncePod},
			StorageClassName: &storageClass,
			Resources:        corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: app.Spec.Storage.Size}},
		},
	}
	createdCanaryPVC, err := globalClient.CoreV1().PersistentVolumeClaims(ns).Create(ctx, canaryPVC, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create canary PVC: %v", err)
	}
	canaryPVC = createdCanaryPVC
	// The canary Workspace carries the same finalizer every real Workspace
	// does; strip it before deleting or the delete just hangs pending
	// finalizer removal, which only a running controller reconcile does.
	base := canaryWS.DeepCopy()
	canaryWS.Finalizers = nil
	if err := globalRuntimeClient.Patch(ctx, canaryWS, client.MergeFrom(base)); err != nil {
		t.Fatalf("strip canary Workspace's finalizer: %v", err)
	}
	if err := globalRuntimeClient.Delete(ctx, canaryWS); err != nil {
		t.Fatalf("delete canary Workspace: %v", err)
	}
	if err := globalClient.CoreV1().PersistentVolumeClaims(ns).Delete(ctx, canaryName, metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete canary PVC: %v", err)
	}
	canaryCtx, canaryCancel := context.WithTimeout(ctx, 20*time.Second)
	var sawWSDelete, sawPVCDelete bool
	for !sawWSDelete || !sawPVCDelete {
		select {
		case d := <-wsWatcher.deletes:
			if d == fmt.Sprintf("Workspace %s/%s (uid %s)", ns, canaryName, canaryWS.UID) {
				sawWSDelete = true
			}
		case d := <-pvcWatcher.deletes:
			if d == fmt.Sprintf("PersistentVolumeClaim %s/%s (uid %s)", ns, canaryName, canaryPVC.UID) {
				sawPVCDelete = true
			}
		case <-canaryCtx.Done():
			canaryCancel()
			t.Fatalf("POSITIVE CONTROL FAILED: watch channel(s) never reported the canary delete (Workspace seen=%v, PVC seen=%v) -- the observation channel itself is broken, so a later 'zero events' result would prove nothing", sawWSDelete, sawPVCDelete)
		}
	}
	canaryCancel()
	t.Log("positive control observed passing: both delete-watch channels reported the canary Workspace and PVC deletions")

	// --- Delete and recreate the PerUserApp. ---
	if err := globalRuntimeClient.Delete(ctx, &app); err != nil {
		t.Fatalf("delete PerUserApp %s: %v", smokeApp, err)
	}
	// Recreate promptly: this is the shared fixture other assertions in
	// this dispatch also use, and every other test in this package expects
	// it to exist.
	if err := globalRuntimeClient.Create(ctx, recreated); err != nil {
		t.Fatalf("recreate PerUserApp %s: %v", smokeApp, err)
	}

	// --- Observe a bounded window: zero delete events for the survivor's
	// Workspace or PVC, across the whole delete+recreate+reconcile cycle. ---
	observeWindow := 45 * time.Second
	windowCtx, windowCancel := context.WithTimeout(ctx, observeWindow)
	defer windowCancel()
	var violations []string
drain:
	for {
		select {
		case d, ok := <-wsWatcher.deletes:
			if !ok {
				break drain
			}
			violations = append(violations, d)
		case d, ok := <-pvcWatcher.deletes:
			if !ok {
				break drain
			}
			violations = append(violations, d)
		case <-windowCtx.Done():
			break drain
		}
	}
	if len(violations) > 0 {
		t.Fatalf("ISOLATION VIOLATION: observed %d delete event(s) during PerUserApp delete/recreate (expected zero): %v", len(violations), violations)
	}
	t.Logf("observed zero Workspace/PVC delete events over the %s window spanning PerUserApp delete+recreate", observeWindow)

	// --- Confirm the SAME identity resolves to the SAME userKey, and the
	// original Workspace/PVC objects (by UID, not just by name) were never
	// actually replaced. ---
	userKeyAfter := identity.UserKey(ns, smokeApp, identitySurvivor)
	if userKeyAfter != userKey {
		t.Fatalf("identity %q resolved to a different userKey after recreate (%s vs %s)", identitySurvivor, userKeyAfter, userKey)
	}
	var wsAfter v1alpha1.Workspace
	if err := globalRuntimeClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: childName}, &wsAfter); err != nil {
		t.Fatalf("get Workspace after delete/recreate: %v", err)
	}
	if wsAfter.UID != wsBefore.UID {
		t.Fatalf("Workspace %s/%s has a DIFFERENT uid after recreate (%s vs %s): it was deleted and silently re-created, not preserved", ns, childName, wsAfter.UID, wsBefore.UID)
	}
	pvcAfter, err := globalClient.CoreV1().PersistentVolumeClaims(ns).Get(ctx, childName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get PVC after delete/recreate: %v", err)
	}
	if pvcAfter.UID != pvcBefore.UID {
		t.Fatalf("PVC %s/%s has a DIFFERENT uid after recreate (%s vs %s): it was deleted and silently re-created, not preserved", ns, childName, pvcAfter.UID, pvcBefore.UID)
	}
	t.Logf("confirmed same userKey (%s) and unchanged Workspace/PVC uids across the PerUserApp delete/recreate", userKeyAfter)

	// --- Confirm the file itself is intact, read directly off the surviving
	// pod/volume. ---
	if got, err := readMarker(globalEnv.Kubeconfig, ns, pod.Name, marker); err != nil || got != content {
		t.Fatalf("survivor marker did not survive the PerUserApp delete/recreate cycle (got %q, err %v)", got, err)
	}
	t.Log("confirmed survivor marker content intact after the PerUserApp delete/recreate cycle")

	// --- Wait for the shared fixture's router to come back (it DOES have
	// an ownerRef to PerUserApp and is expected to be cascade-deleted and
	// then reconciled back), so every other test in this package still has
	// a working e2e-app router. ---
	if err := waitDeploymentReady(ctx, globalClient, ns, smokeApp+"-router"); err != nil {
		t.Fatalf("wait for e2e-app-router to recover after PerUserApp recreate: %v", err)
	}
	if code, err := coldStart(globalEnv, ns, clientPod, smokeApp, identitySurvivor); err != nil || code != "200" {
		t.Fatalf("end-to-end re-check via router after recreate: code=%q err=%v", code, err)
	}
	t.Log("router recovered after PerUserApp recreate; end-to-end re-check through the router succeeded")
}
