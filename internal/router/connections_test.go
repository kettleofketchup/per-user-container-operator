package router

import (
	"context"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"

	"github.com/kettleofketchup/per-user-container-operator/api/v1alpha1"
	"github.com/kettleofketchup/per-user-container-operator/internal/identity"
)

// TestConnectionsAreKeyedByPodNameAndSelfExpire is the binding ruling from
// Task 9: status.connections is keyed by ROUTER REPLICA POD NAME, each
// replica publishes its OWN live count, and HeartbeatAt is refreshed on
// schedule EVEN WHEN Count is 0. If POD_NAME were ignored, every replica
// would write connections[""] -- one shared, last-writer-wins entry -- and
// the reaper could zero out a live session it never held.
func TestConnectionsAreKeyedByPodNameAndSelfExpire(t *testing.T) {
	ns, app, pod := "ns-conn", "myapp", "router-abc"
	userKey := identity.UserKey(ns, app, "alice")
	wsName := identity.ChildName(app, userKey)
	ws := newWorkspace(ns, app, userKey, wsName, v1alpha1.WorkspaceStatus{})

	fc := newFakeClient(t, ws)
	clock := newFakeClockAt(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	tr := NewConnectionTracker(fc, app, pod, 50*time.Millisecond)
	tr.Clock = clock.Now
	key := types.NamespacedName{Namespace: ns, Name: wsName}

	if err := tr.Open(context.Background(), key); err != nil {
		t.Fatalf("open: %v", err)
	}

	var got v1alpha1.Workspace
	if err := fc.Get(context.Background(), key, &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	entry, ok := got.Status.Connections[pod]
	if !ok {
		t.Fatalf("status.connections must be keyed by POD_NAME %q, got keys %v", pod, keysOf(got.Status.Connections))
	}
	if entry.Count != 1 {
		t.Fatalf("count = %d, want 1", entry.Count)
	}
	if _, hasEmpty := got.Status.Connections[""]; hasEmpty {
		t.Fatalf("status.connections must never be keyed by the empty string")
	}
	firstHeartbeat := entry.HeartbeatAt.Time

	clock.Advance(time.Minute)
	tr.HeartbeatAll(context.Background())
	if err := fc.Get(context.Background(), key, &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	entry2 := got.Status.Connections[pod]
	if !entry2.HeartbeatAt.After(firstHeartbeat) {
		t.Fatalf("heartbeatAt must be refreshed on each tick: got %v, want after %v", entry2.HeartbeatAt.Time, firstHeartbeat)
	}
	if entry2.Count != 1 {
		t.Fatalf("a heartbeat tick must not change the count, got %d", entry2.Count)
	}

	if err := tr.Close(context.Background(), key); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := fc.Get(context.Background(), key, &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	entry3 := got.Status.Connections[pod]
	if entry3.Count != 0 {
		t.Fatalf("count after close = %d, want 0", entry3.Count)
	}

	// The load-bearing property: heartbeat keeps refreshing EVEN AT ZERO.
	clock.Advance(time.Minute)
	tr.HeartbeatAll(context.Background())
	if err := fc.Get(context.Background(), key, &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	entry4 := got.Status.Connections[pod]
	if entry4.Count != 0 {
		t.Fatalf("count = %d, want 0", entry4.Count)
	}
	if !entry4.HeartbeatAt.After(entry3.HeartbeatAt.Time) {
		t.Fatalf("heartbeatAt must still refresh while Count is 0: got %v, want after %v", entry4.HeartbeatAt.Time, entry3.HeartbeatAt.Time)
	}
}

// TestLastActivityMonotonicUnderOutOfOrderWrites: monotonicity is a
// property of the WRITE, not of the field. A delayed write carrying an
// earlier timestamp must never move status.lastActivity backwards past
// idleTimeout, or the reaper could scale down a workspace with an open
// connection.
func TestLastActivityMonotonicUnderOutOfOrderWrites(t *testing.T) {
	ns, app := "ns-activity", "myapp"
	userKey := identity.UserKey(ns, app, "alice")
	wsName := identity.ChildName(app, userKey)
	ws := newWorkspace(ns, app, userKey, wsName, v1alpha1.WorkspaceStatus{})

	fc := newFakeClient(t, ws)
	key := types.NamespacedName{Namespace: ns, Name: wsName}

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	t2 := base.Add(2 * time.Second)
	t1 := base.Add(1 * time.Second)

	// T2 lands first, then the delayed T1 arrives second.
	if err := touchActivity(context.Background(), fc, key, t2); err != nil {
		t.Fatalf("touch T2: %v", err)
	}
	if err := touchActivity(context.Background(), fc, key, t1); err != nil {
		t.Fatalf("touch T1: %v", err)
	}

	var got v1alpha1.Workspace
	if err := fc.Get(context.Background(), key, &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status.LastActivity == nil || !got.Status.LastActivity.Time.Equal(t2) {
		t.Fatalf("lastActivity = %v, want exactly T2 (%v): an out-of-order delayed write must never move it backwards", got.Status.LastActivity, t2)
	}
}
