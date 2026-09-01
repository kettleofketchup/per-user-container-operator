//go:build envtest

package envtest

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/kettleofketchup/per-user-container-operator/api/v1alpha1"
	"github.com/kettleofketchup/per-user-container-operator/internal/controller"
)

// TestWakeDuringReapIsNotLost is spec 463's envtest-only property: the
// reaper's scale-down is a conditional write on the resourceVersion it read,
// against the status subresource. A fake client need not honor
// resourceVersion/optimistic-concurrency semantics at all (Task 6 Step 0's
// vacuity hazard), so this is only meaningfully provable against the real
// apiserver envtest starts.
//
// No WorkspaceReconciler or Reaper.Start ticking loop runs in this test:
// controller.Reaper.ScaleDown is driven directly against a deliberately
// stale Workspace snapshot, exactly reproducing "the reaper listed this
// workspace, then before it got around to writing, a wake landed."
func TestWakeDuringReapIsNotLost(t *testing.T) {
	ns := newNamespace(t)
	app, ws := newFixtures(ns)
	mustCreate(t, app)
	mustCreate(t, ws)

	// Forge the two inputs that make ws reapable: Ready, idle well past
	// idleTimeout, zero live connections. No reconciler runs in this test,
	// so the phase transition that would normally produce Ready is forged
	// directly, per test/envtest/helpers_test.go's patchWorkspaceStatus
	// convention for inputs this suite has no other producer of.
	patchWorkspaceStatus(t, ws, func(s *v1alpha1.WorkspaceStatus) {
		s.Phase = v1alpha1.PhaseReady
		s.ScaledDown = false
		past := metav1.NewTime(time.Now().Add(-(app.Spec.Lifecycle.IdleTimeout.Duration + time.Minute)))
		s.LastActivity = &past
	})

	// The reaper's own read: this is the resourceVersion its later
	// conditional write will carry.
	var stale v1alpha1.Workspace
	mustGet(t, ws, &stale)

	// The wake lands from a second client, after the reaper's read.
	wake := metav1.NewTime(time.Now())
	patchWorkspaceStatus(t, ws, func(s *v1alpha1.WorkspaceStatus) {
		s.WakeRequestedAt = &wake
	})

	// Intercept every write this client issues so clause (iii) --  zero
	// writes touching the Deployment that would set spec.replicas to 0 -- is
	// directly observable, not inferred from the Reaper's documented
	// contract.
	var deploymentWrites int
	watchClient, err := client.NewWithWatch(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("new watch client: %v", err)
	}
	recording := interceptor.NewClient(watchClient, interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if _, ok := obj.(*appsv1.Deployment); ok {
				deploymentWrites++
			}
			return c.Update(ctx, obj, opts...)
		},
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if _, ok := obj.(*appsv1.Deployment); ok {
				deploymentWrites++
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	})

	reaper := controller.NewReaper(recording)

	// (i) the reaper's conditional write, against the stale resourceVersion
	// it read before the wake landed, must return a Conflict.
	err = reaper.ScaleDown(context.Background(), &stale)
	if err == nil {
		t.Fatalf("ScaleDown against a stale resourceVersion must fail, got nil error")
	}
	if !apierrors.IsConflict(err) {
		t.Fatalf("ScaleDown against a stale resourceVersion must return a Conflict, got: %v", err)
	}

	// (ii) the wake must survive intact: still set, and scaledDown still
	// false -- the losing write must not have partially landed.
	var afterConflict v1alpha1.Workspace
	if err := k8sClient.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: ws.Name}, &afterConflict); err != nil {
		t.Fatalf("get workspace after conflict: %v", err)
	}
	if afterConflict.Status.WakeRequestedAt == nil {
		t.Fatalf("status.wakeRequestedAt must still be set after the reaper lost the race, got nil")
	}
	if afterConflict.Status.ScaledDown {
		t.Fatalf("status.scaledDown must still be false after the reaper lost the race, got true")
	}

	// (iii) zero writes of any kind touched the Deployment: a lost-then-
	// restored wake is indistinguishable from a wake that never lost the
	// race under a Pod- or replicas-based assertion (envtest runs no
	// ReplicaSet controller or kubelet), so this is the only assertion that
	// actually distinguishes the two.
	if deploymentWrites != 0 {
		t.Fatalf("want zero writes targeting the Deployment, got %d", deploymentWrites)
	}
}
