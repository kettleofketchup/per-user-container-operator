package router

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"

	"k8s.io/apimachinery/pkg/watch"

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
