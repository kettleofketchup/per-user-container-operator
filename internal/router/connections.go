package router

import (
	"context"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kettleofketchup/per-user-container-operator/api/v1alpha1"
	"github.com/kettleofketchup/per-user-container-operator/internal/metrics"
)

// ConnectionTracker publishes THIS router replica's own live per-user
// connection count into Workspace.status.connections, keyed by POD_NAME —
// the binding ruling from Task 9: each replica owns exactly one entry,
// writes its OWN count, and must refresh HeartbeatAt on schedule even when
// Count is 0. The reaper sums Count across entries fresher than
// 2*connectionHeartbeatInterval; a replica that stops heartbeating while
// merely idle (rather than dead) would age out and let the reaper kill a
// live-but-quiet session it can no longer see, while a replica that keyed
// its writes on "" instead of its own pod name would collide with every
// other replica on one shared, last-writer-wins count.
type ConnectionTracker struct {
	Client            client.Client
	PodName           string
	HeartbeatInterval time.Duration
	App               string

	// Clock returns the current time; defaults to time.Now when nil.
	Clock func() time.Time

	mu     sync.Mutex
	counts map[types.NamespacedName]int32
}

// NewConnectionTracker returns a ConnectionTracker backed by c, publishing
// under podName.
func NewConnectionTracker(c client.Client, app, podName string, heartbeat time.Duration) *ConnectionTracker {
	return &ConnectionTracker{Client: c, App: app, PodName: podName, HeartbeatInterval: heartbeat, counts: map[types.NamespacedName]int32{}}
}

func (t *ConnectionTracker) now() time.Time {
	if t.Clock != nil {
		return t.Clock()
	}
	return time.Now()
}

// Open increments the in-memory count for key and immediately republishes
// it, so a newly opened connection is visible to the reaper without waiting
// for the next heartbeat tick.
func (t *ConnectionTracker) Open(ctx context.Context, key types.NamespacedName) error {
	t.mu.Lock()
	if t.counts == nil {
		t.counts = map[types.NamespacedName]int32{}
	}
	t.counts[key]++
	n := t.counts[key]
	t.mu.Unlock()
	return t.publish(ctx, key, n)
}

// Close decrements the in-memory count for key (never below zero) and
// republishes it.
func (t *ConnectionTracker) Close(ctx context.Context, key types.NamespacedName) error {
	t.mu.Lock()
	if t.counts[key] > 0 {
		t.counts[key]--
	}
	n := t.counts[key]
	t.mu.Unlock()
	return t.publish(ctx, key, n)
}

// publish writes status.connections[PodName] = {n, now}. It always writes,
// even when n is 0: a live, idle replica must keep heartbeating so the
// reaper's freshness check tells it apart from a dead one whose stale entry
// should age out.
func (t *ConnectionTracker) publish(ctx context.Context, key types.NamespacedName, n int32) error {
	var ws v1alpha1.Workspace
	if err := t.Client.Get(ctx, key, &ws); err != nil {
		return err
	}
	base := ws.DeepCopy()
	if ws.Status.Connections == nil {
		ws.Status.Connections = map[string]v1alpha1.ConnectionEntry{}
	}
	ws.Status.Connections[t.PodName] = v1alpha1.ConnectionEntry{Count: n, HeartbeatAt: metav1.NewTime(t.now())}
	if err := t.Client.Status().Patch(ctx, &ws, client.MergeFrom(base)); err != nil {
		return err
	}
	metrics.SetOpenUpgradedConnections(key.Namespace, t.App, t.PodName, float64(n))
	return nil
}

// HeartbeatAll republishes the current in-memory count — possibly 0 — for
// every workspace this replica has ever opened a connection for. It is
// exported so a test can drive one tick directly instead of waiting on
// Run's ticker.
func (t *ConnectionTracker) HeartbeatAll(ctx context.Context) {
	t.mu.Lock()
	keys := make([]types.NamespacedName, 0, len(t.counts))
	vals := make(map[types.NamespacedName]int32, len(t.counts))
	for k, v := range t.counts {
		keys = append(keys, k)
		vals[k] = v
	}
	t.mu.Unlock()

	for _, k := range keys {
		_ = t.publish(ctx, k, vals[k])
	}
}

// Run ticks HeartbeatAll at HeartbeatInterval until ctx is done.
func (t *ConnectionTracker) Run(ctx context.Context) {
	interval := t.HeartbeatInterval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			t.HeartbeatAll(ctx)
		}
	}
}

// touchActivity performs the compare-and-set write spec 450 requires:
// status.lastActivity is a merge-patch that must be last-writer-wins by
// TIME, not by arrival order, so it re-Gets the workspace and skips the
// write entirely whenever the persisted value is already at or after at. A
// blind merge patch here would let a delayed write from another replica
// move the field backwards past idleTimeout and cause the reaper to scale
// down a workspace with a genuinely open connection.
func touchActivity(ctx context.Context, c client.Client, key types.NamespacedName, at time.Time) error {
	var ws v1alpha1.Workspace
	if err := c.Get(ctx, key, &ws); err != nil {
		return err
	}
	if ws.Status.LastActivity != nil && !ws.Status.LastActivity.Time.Before(at) {
		return nil
	}
	base := ws.DeepCopy()
	t := metav1.NewTime(at)
	ws.Status.LastActivity = &t
	return c.Status().Patch(ctx, &ws, client.MergeFrom(base))
}
