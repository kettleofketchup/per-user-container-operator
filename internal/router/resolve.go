// Package router implements the per-app router: it authenticates the
// caller, derives the requesting user's identity, ensures that user's
// Workspace exists and is servable, and proxies the request — WebSocket and
// SSE included — to that user's workspace Service.
package router

import (
	"context"
	"net"
	"strconv"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/watch"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kettleofketchup/per-user-container-operator/api/v1alpha1"
)

// Default bounds for the watch-reconnect backoff in WatchServiceDeletes /
// WatchWorkspaceDeletes. 200ms is short enough that a routine
// --min-request-timeout close (the common case) reconnects almost
// immediately; 30s caps how hard a persistently broken connection (e.g. a
// control-plane outage) is hammered.
const (
	defaultReconnectMinBackoff = 200 * time.Millisecond
	defaultReconnectMaxBackoff = 30 * time.Second
)

// Resolver addresses a workspace's Service by NAME, never by a memoised
// ClusterIP: a deleted Service releases its ClusterIP back to the
// allocator, and a router replica that kept dialling a cached address after
// that could end up dialling a completely different user's pod once the
// address is reassigned. The cache below exists purely to avoid a Service
// Get on every single proxied request; every entry is invalidated the
// moment this process learns (via Invalidate, driven by a delete event on
// either the Service or the Workspace) that the name might now resolve
// differently, so the next Resolve call always re-reads the live object
// rather than ever serving a stale address.
type Resolver struct {
	Client client.Client

	// ReconnectMinBackoff/ReconnectMaxBackoff bound the delay between watch
	// reconnect attempts in WatchServiceDeletes/WatchWorkspaceDeletes,
	// defaulting to defaultReconnectMinBackoff/MaxBackoff when zero. Tests
	// shorten these instead of waiting on production-scale backoff.
	ReconnectMinBackoff time.Duration
	ReconnectMaxBackoff time.Duration

	mu    sync.RWMutex
	cache map[client.ObjectKey]string
}

func (r *Resolver) reconnectMinBackoff() time.Duration {
	if r.ReconnectMinBackoff > 0 {
		return r.ReconnectMinBackoff
	}
	return defaultReconnectMinBackoff
}

func (r *Resolver) reconnectMaxBackoff() time.Duration {
	if r.ReconnectMaxBackoff > 0 {
		return r.ReconnectMaxBackoff
	}
	return defaultReconnectMaxBackoff
}

// NewResolver returns a Resolver backed by c.
func NewResolver(c client.Client) *Resolver {
	return &Resolver{Client: c, cache: map[client.ObjectKey]string{}}
}

// Resolve returns the "host:port" dial address for the named Service,
// consulting the cache first and falling back to a live Get on a miss. It
// never returns an address for a Service that no longer exists: a cache
// miss that Gets NotFound propagates the error to the caller instead of
// inventing or reusing any address.
func (r *Resolver) Resolve(ctx context.Context, namespace, name string, port int32) (string, error) {
	key := client.ObjectKey{Namespace: namespace, Name: name}

	r.mu.RLock()
	if addr, ok := r.cache[key]; ok {
		r.mu.RUnlock()
		return addr, nil
	}
	r.mu.RUnlock()

	var svc corev1.Service
	if err := r.Client.Get(ctx, key, &svc); err != nil {
		return "", err
	}
	addr := net.JoinHostPort(svc.Spec.ClusterIP, strconv.Itoa(int(port)))

	r.mu.Lock()
	r.cache[key] = addr
	r.mu.Unlock()
	return addr, nil
}

// Invalidate purges any cached address for namespace/name. It is the sole
// mutation this type performs outside of Resolve's own cache-fill, and it
// is safe to call for a key that was never cached.
func (r *Resolver) Invalidate(namespace, name string) {
	r.mu.Lock()
	delete(r.cache, client.ObjectKey{Namespace: namespace, Name: name})
	r.mu.Unlock()
}

// HasEndpoints reports whether any EndpointSlice for the Service named name
// carries at least one ready address. This — not status.phase — is the
// router's liveness signal: a Workspace can read Ready with no endpoints at
// all immediately after a routine node event, and phase alone would send a
// user's request to nothing.
func (r *Resolver) HasEndpoints(ctx context.Context, namespace, name string) (bool, error) {
	var slices discoveryv1.EndpointSliceList
	if err := r.Client.List(ctx, &slices,
		client.InNamespace(namespace),
		client.MatchingLabels{discoveryv1.LabelServiceName: name},
	); err != nil {
		return false, err
	}
	for _, sl := range slices.Items {
		for _, ep := range sl.Endpoints {
			if len(ep.Addresses) == 0 {
				continue
			}
			if ep.Conditions.Ready == nil || *ep.Conditions.Ready {
				return true, nil
			}
		}
	}
	return false, nil
}

// HandleServiceEvent invalidates the cache entry for a deleted Service. Any
// other event type is ignored: an Add/Update never makes a cached address
// stale (the ClusterIP a Service is assigned is immutable for its
// lifetime), only its deletion does.
func (r *Resolver) HandleServiceEvent(evt watch.Event) {
	if evt.Type != watch.Deleted {
		return
	}
	if svc, ok := evt.Object.(*corev1.Service); ok {
		r.Invalidate(svc.Namespace, svc.Name)
	}
}

// HandleWorkspaceEvent invalidates the cache entry for a deleted Workspace.
// The Workspace's child Service shares its name (identity.ChildName) and is
// owned by it, so it is garbage-collected on the Workspace's deletion — but
// that Service-delete watch event can lag; invalidating here too closes
// that window rather than depending on GC latency.
func (r *Resolver) HandleWorkspaceEvent(evt watch.Event) {
	if evt.Type != watch.Deleted {
		return
	}
	obj, ok := evt.Object.(client.Object)
	if !ok {
		return
	}
	r.Invalidate(obj.GetNamespace(), obj.GetName())
}

// watchDeletes runs until ctx is done, calling onDelete for every Deleted
// event observed, and RECONNECTS — with capped exponential backoff — every
// time the watch's channel closes or the initial Watch call itself errors.
//
// Kubernetes closes watches routinely: --min-request-timeout expiry, a 410
// resourceVersion-too-old, any network blip. A raw client.WithWatch, unlike
// controller-runtime's own informer/reflector machinery, does not recover
// from any of those on its own — a single-shot "watch once, return when the
// channel closes" implementation goes silently and permanently idle the
// first time any of them happens, with no error surfaced anywhere. From
// that moment, Resolver.Invalidate is never called again for the remaining
// life of the process: cache invalidation is dead, a stale cached address
// persists, and that is exactly the recycled-ClusterIP path this package's
// doc comment describes — one user's request landing in whoever's pod that
// address was later reassigned to. Exiting only on ctx cancellation is the
// fix; newList is called fresh on every (re)connect attempt since a
// client.ObjectList used for one Watch call must not be reused for another.
//
// The backoff RESETS to minBackoff once an established watch's session
// (from a successful Watch call to its channel closing) lasts at least
// minBackoff — the simplest rule that doesn't depend on this watch ever
// seeing an event: a delete-watch on a quiet namespace can go its entire
// life without one, so "reset on first event" would leave the backoff
// ratcheted forever right alongside the bug this fixes. A session that
// merely outlives the delay we'd have backed off for anyway has
// demonstrably not just failed, and rewarding it with a reset matters
// because routine --min-request-timeout closes happen on every long-lived,
// otherwise-healthy watch: without the reset, a pod up for days ends up
// permanently reconnecting at maxBackoff instead of minBackoff, leaving a
// recurring window — after every one of those routine closes — during
// which Invalidate cannot fire. Narrower than the fire-and-forget bug above,
// but the same failure shape, repeating instead of permanent.
func watchDeletes(ctx context.Context, wc client.WithWatch, newList func() client.ObjectList, opts []client.ListOption, onDelete func(evt watch.Event), minBackoff, maxBackoff time.Duration) {
	backoff := minBackoff
	for {
		if ctx.Err() != nil {
			return
		}

		w, err := wc.Watch(ctx, newList(), opts...)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if !sleepOrDone(ctx, backoff) {
				return
			}
			backoff = nextBackoff(backoff, maxBackoff)
			continue
		}

		sessionStart := time.Now()
		drainUntilClosedOrDone(ctx, w, onDelete)
		w.Stop()
		if ctx.Err() != nil {
			return
		}

		if time.Since(sessionStart) >= minBackoff {
			// Healthy session: reset before sleeping, so the upcoming
			// reconnect uses the floor, not whatever the backoff had
			// ratcheted up to.
			backoff = minBackoff
			if !sleepOrDone(ctx, backoff) {
				return
			}
			continue
		}

		// Unhealthy session (closed/errored before proving itself): back
		// off before reconnecting so a persistently broken connection does
		// not spin, and ratchet for next time.
		if !sleepOrDone(ctx, backoff) {
			return
		}
		backoff = nextBackoff(backoff, maxBackoff)
	}
}

// drainUntilClosedOrDone forwards every event on w's ResultChan to onDelete
// until ctx is done or the channel closes.
func drainUntilClosedOrDone(ctx context.Context, w watch.Interface, onDelete func(evt watch.Event)) {
	for {
		select {
		case <-ctx.Done():
			return
		case evt, ok := <-w.ResultChan():
			if !ok {
				return
			}
			onDelete(evt)
		}
	}
}

// sleepOrDone waits for d, returning false immediately (without waiting) if
// ctx is done first.
func sleepOrDone(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func nextBackoff(d, maxD time.Duration) time.Duration {
	d *= 2
	if d > maxD {
		return maxD
	}
	return d
}

// WatchServiceDeletes watches Service deletions in namespace and invalidates
// the resolver's cache for each one, reconnecting on any disconnect. It
// blocks until ctx is cancelled, and is meant to be run in its own goroutine
// by cmd/main.go. The error return is always nil once ctx is cancelled; it
// exists so callers can use the same fire-the-goroutine-and-discard-the-
// return pattern as before without a signature change.
func (r *Resolver) WatchServiceDeletes(ctx context.Context, wc client.WithWatch, namespace string) error {
	watchDeletes(ctx, wc,
		func() client.ObjectList { return &corev1.ServiceList{} },
		[]client.ListOption{client.InNamespace(namespace)},
		r.HandleServiceEvent, r.reconnectMinBackoff(), r.reconnectMaxBackoff())
	return nil
}

// WatchWorkspaceDeletes watches Workspace deletions in namespace and
// invalidates the resolver's cache for each one (see HandleWorkspaceEvent),
// reconnecting on any disconnect exactly like WatchServiceDeletes.
func (r *Resolver) WatchWorkspaceDeletes(ctx context.Context, wc client.WithWatch, namespace string) error {
	watchDeletes(ctx, wc,
		func() client.ObjectList { return &v1alpha1.WorkspaceList{} },
		[]client.ListOption{client.InNamespace(namespace)},
		r.HandleWorkspaceEvent, r.reconnectMinBackoff(), r.reconnectMaxBackoff())
	return nil
}
