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

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/watch"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kettleofketchup/per-user-container-operator/api/v1alpha1"
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

	mu    sync.RWMutex
	cache map[client.ObjectKey]string
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

// watchDeletes runs until ctx is done or the watch closes, calling onDelete
// for every Deleted event observed. It is the shared plumbing behind
// WatchServiceDeletes and WatchWorkspaceDeletes.
func watchDeletes(ctx context.Context, wc client.WithWatch, list client.ObjectList, opts []client.ListOption, onDelete func(evt watch.Event)) error {
	w, err := wc.Watch(ctx, list, opts...)
	if err != nil {
		return err
	}
	defer w.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case evt, ok := <-w.ResultChan():
			if !ok {
				return nil
			}
			onDelete(evt)
		}
	}
}

// WatchServiceDeletes watches Service deletions in namespace and invalidates
// the resolver's cache for each one. It blocks until ctx is cancelled or the
// watch ends, and is meant to be run in its own goroutine by cmd/main.go.
func (r *Resolver) WatchServiceDeletes(ctx context.Context, wc client.WithWatch, namespace string) error {
	return watchDeletes(ctx, wc, &corev1.ServiceList{}, []client.ListOption{client.InNamespace(namespace)}, r.HandleServiceEvent)
}

// WatchWorkspaceDeletes watches Workspace deletions in namespace and
// invalidates the resolver's cache for each one (see HandleWorkspaceEvent).
func (r *Resolver) WatchWorkspaceDeletes(ctx context.Context, wc client.WithWatch, namespace string) error {
	return watchDeletes(ctx, wc, &v1alpha1.WorkspaceList{}, []client.ListOption{client.InNamespace(namespace)}, r.HandleWorkspaceEvent)
}
