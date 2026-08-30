package router

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/watch"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/kettleofketchup/per-user-container-operator/internal/identity"
)

// TestRecycledClusterIPDoesNotCrossUsers is property 1: a deleted
// workspace Service releases its ClusterIP back to the allocator, and a
// router replica that kept dialling a memoised address for it would then
// dial whichever pod that address now belongs to — past every other
// control, since a workspace's own NetworkPolicy admits any router pod and
// upstreamAuth is one shared secret for the whole app.
func TestRecycledClusterIPDoesNotCrossUsers(t *testing.T) {
	ns, app := "ns-recycle", "myapp"
	cfg := testConfig(ns, app)

	// A single real backend whose CONTENT is swapped from "A" to "B" at the
	// same network address — exactly what "the ClusterIP was reassigned to
	// a different pod" looks like from the router's point of view, without
	// depending on the OS actually handing back the same ephemeral port.
	var current atomic.Value
	current.Store("A")
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(current.Load().(string)))
	}))
	defer backend.Close()
	host, portStr, err := net.SplitHostPort(backend.Listener.Addr().String())
	if err != nil {
		t.Fatalf("split addr: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parse port: %v", err)
	}
	cfg.WorkspacePort = int32(port)

	userKeyA := identity.UserKey(ns, app, "alice")
	wsNameA := identity.ChildName(app, userKeyA)
	wsA := readyWorkspace(ns, app, userKeyA, wsNameA)
	svcA := serviceFor(ns, wsNameA, host, port)
	epsA := endpointSliceFor(ns, wsNameA, host)

	fc := newFakeClient(t, wsA, svcA, epsA)
	srv := NewServer(cfg, fc)

	// Record A's address (populates the resolver's cache).
	addr1, err := srv.Resolver.Resolve(context.Background(), ns, wsNameA, cfg.WorkspacePort)
	if err != nil {
		t.Fatalf("resolve A: %v", err)
	}

	// The address now belongs to Bob's container.
	current.Store("B")

	// Delete A's Service and deliver the delete event to the router's watch.
	if err := fc.Delete(context.Background(), svcA); err != nil {
		t.Fatalf("delete service A: %v", err)
	}
	srv.Resolver.HandleServiceEvent(watch.Event{Type: watch.Deleted, Object: svcA})

	// Create B's Service holding the same ClusterIP value.
	userKeyB := identity.UserKey(ns, app, "bob")
	wsNameB := identity.ChildName(app, userKeyB)
	svcB := serviceFor(ns, wsNameB, host, port)
	if err := fc.Create(context.Background(), svcB); err != nil {
		t.Fatalf("create service B: %v", err)
	}

	// A fresh resolve for A must fail now — Service A is gone — rather than
	// silently reusing the invalidated address (which now serves Bob).
	if _, err := srv.Resolver.Resolve(context.Background(), ns, wsNameA, cfg.WorkspacePort); err == nil {
		t.Fatalf("resolve for deleted Service A must fail after invalidation, not silently return the recycled address %s", addr1)
	}

	// End to end: a request for A must not reach B's backend.
	resp := doAuthedRequest(t, srv, cfg, "alice")
	defer func() { _ = resp.Body.Close() }()
	body := readBody(t, resp)
	if body == "B" {
		t.Fatalf("request for A reached B's backend (body=%q) -- the recycled ClusterIP crossed users", body)
	}
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("request for A must not succeed once its Service is gone, got 200 with body %q", body)
	}
}

// TestResolveCachesUntilInvalidated is the companion property: a second
// Resolve for the SAME still-valid Service must not need another Get — this
// is what makes the cache worth invalidating correctly rather than simply
// never caching at all.
func TestResolveCachesUntilInvalidated(t *testing.T) {
	ns := "ns-cache"
	svc := serviceFor(ns, "svc-a", "10.0.0.5", 8000)
	fc := newFakeClient(t, svc)
	r := NewResolver(fc)

	addr1, err := r.Resolve(context.Background(), ns, "svc-a", 8000)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if addr1 != "10.0.0.5:8000" {
		t.Fatalf("addr = %q, want 10.0.0.5:8000", addr1)
	}

	// Delete the Service from the client WITHOUT invalidating: the cache
	// must still serve the (now stale, but not yet invalidated) address —
	// proving the cache is real and not accidentally a passthrough Get.
	if err := fc.Delete(context.Background(), svc); err != nil {
		t.Fatalf("delete: %v", err)
	}
	addr2, err := r.Resolve(context.Background(), ns, "svc-a", 8000)
	if err != nil || addr2 != addr1 {
		t.Fatalf("cached resolve after delete (pre-invalidation) = (%q, %v), want (%q, nil)", addr2, err, addr1)
	}

	r.Invalidate(ns, "svc-a")
	if _, err := r.Resolve(context.Background(), ns, "svc-a", 8000); err == nil {
		t.Fatalf("resolve after invalidation must re-Get and fail (the Service no longer exists)")
	}
}

// watchCapture records every watch.Interface a wrapped client.WithWatch
// hands out, via an interceptor, so a test can reach in and Stop() one from
// outside the goroutine that's consuming it.
type watchCapture struct {
	mu      sync.Mutex
	watches []watch.Interface
}

func (c *watchCapture) record(w watch.Interface) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.watches = append(c.watches, w)
}

func (c *watchCapture) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.watches)
}

func (c *watchCapture) at(i int) watch.Interface {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.watches[i]
}

func newCapturingWatchClient(t *testing.T, base client.WithWatch) (client.WithWatch, *watchCapture) {
	t.Helper()
	capture := &watchCapture{}
	wrapped := interceptor.NewClient(base, interceptor.Funcs{
		Watch: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) (watch.Interface, error) {
			w, err := c.Watch(ctx, list, opts...)
			if err == nil {
				capture.record(w)
			}
			return w, err
		},
	})
	return wrapped, capture
}

// TestWatchServiceDeletesReconnectsAfterChannelCloses is the fix for the
// CRITICAL finding on this task: Kubernetes closes watches routinely
// (--min-request-timeout expiry, a 410 resourceVersion-too-old, any network
// blip). A fire-and-forget watchDeletes that returns the moment its
// ResultChan closes stops invalidating the resolver's cache for the
// remaining life of the process — silently, with no error surfaced anywhere
// — which is exactly the recycled-ClusterIP path TestRecycledClusterIPDoesNotCrossUsers
// guards against, except here nothing ever calls Invalidate a second time to
// catch it.
//
// This test closes the FIRST watch's channel out from under
// WatchServiceDeletes (simulating the server-initiated close above), then
// deletes the Service for real and waits for that delete to still land: the
// resolver's cache must eventually invalidate, meaning the watch loop must
// have reconnected and be listening again, not have exited.
func TestWatchServiceDeletesReconnectsAfterChannelCloses(t *testing.T) {
	ns := "ns-reconnect"
	svc := serviceFor(ns, "svc-a", "10.0.0.5", 8000)
	fc := newFakeClient(t, svc)
	wrapped, watches := newCapturingWatchClient(t, fc)

	r := NewResolver(fc)
	r.ReconnectMinBackoff = 10 * time.Millisecond
	r.ReconnectMaxBackoff = 50 * time.Millisecond

	if _, err := r.Resolve(context.Background(), ns, "svc-a", 8000); err != nil {
		t.Fatalf("prime cache: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = r.WatchServiceDeletes(ctx, wrapped, ns) }()

	waitUntil(t, 2*time.Second, func() bool { return watches.count() >= 1 })

	// Simulate the API server closing the watch out from under the router.
	// A fake client's watch.Interface.Stop() closes its ResultChan exactly
	// like a real server-initiated close does from the consumer's side.
	watches.at(0).Stop()

	// A correct implementation reconnects (with backoff) after the channel
	// closes. That reconnect must be observed BEFORE the delete below: a
	// NEW watch only sees events that occur after it starts, so delivering
	// the delete first would prove nothing either way.
	waitUntil(t, 2*time.Second, func() bool { return watches.count() >= 2 })

	// The Service is deleted for real AFTER the reconnect. If the watch
	// loop never reconnected (the fire-and-forget bug), nothing is
	// listening when this happens and the resolver's cache never
	// invalidates.
	if err := fc.Delete(context.Background(), svc); err != nil {
		t.Fatalf("delete service: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := r.Resolve(context.Background(), ns, "svc-a", 8000); err != nil {
			return // invalidated, and the re-Get correctly failed: reconnect worked.
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("cache was never invalidated after the watch reconnected and observed the delete -- the watch loop must reconnect on channel close, not exit (fire-and-forget bug)")
}
