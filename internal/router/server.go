package router

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kettleofketchup/per-user-container-operator/api/v1alpha1"
	"github.com/kettleofketchup/per-user-container-operator/internal/identity"
	"github.com/kettleofketchup/per-user-container-operator/internal/metrics"
)

// errWorkspaceLimit is ensureWorkspace's sentinel for "this would create a
// workspace beyond spec.limits.maxWorkspaces" — distinguished from every
// other error so ServeHTTP can map it onto RejectWorkspaceLimit without
// creating anything.
var errWorkspaceLimit = errors.New("workspace limit reached")

// Server is the router's http.Handler: authenticate the caller, derive
// identity (fail-closed, via internal/identity — never reimplemented here),
// ensure that user's Workspace exists and is servable, and proxy.
type Server struct {
	Cfg      Config
	Client   client.Client
	Resolver *Resolver
	Conns    *ConnectionTracker

	// Clock returns the current time; defaults to time.Now when nil.
	Clock func() time.Time
	// PollInterval overrides the cold-start hold's poll cadence (default
	// 200ms); tests shorten it instead of waiting on production timing.
	PollInterval time.Duration
}

// NewServer returns a Server wired to c, with its own Resolver and
// ConnectionTracker.
func NewServer(cfg Config, c client.Client) *Server {
	return &Server{
		Cfg:      cfg,
		Client:   c,
		Resolver: NewResolver(c),
		Conns:    NewConnectionTracker(c, cfg.App, cfg.PodName, cfg.ConnectionHeartbeatInterval),
	}
}

func (s *Server) now() time.Time {
	if s.Clock != nil {
		return s.Clock()
	}
	return time.Now()
}

func (s *Server) pollInterval() time.Duration {
	if s.PollInterval > 0 {
		return s.PollInterval
	}
	return 200 * time.Millisecond
}

var _ http.Handler = (*Server)(nil)

// ServeHTTP implements the router's entire request path. Order matters:
// caller auth is checked BEFORE identity is derived (spec 217-219 is the
// single named trust control standing between any pod on the network and
// impersonating any user), and no Workspace is created or touched until
// both have succeeded.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rec := &statusRecorder{ResponseWriter: w}
	ctx := r.Context()
	ns, app := s.Cfg.Namespace, s.Cfg.App

	if !s.checkCallerAuth(r.Header) {
		s.reject(rec, http.StatusUnauthorized, "unauthorized")
		metrics.RecordRouterRequest(ns, app, strconv.Itoa(rec.code()))
		return
	}

	raw, err := identity.Extract(r.Header, s.Cfg.IdentityHeader, s.Cfg.IdentityMaxLength)
	if err != nil {
		var rej *identity.Rejection
		if errors.As(err, &rej) {
			metrics.RecordIdentityRejection(ns, app, rej.Reason)
			s.reject(rec, rej.Status, "identity rejected: "+string(rej.Reason))
		} else {
			s.reject(rec, http.StatusUnauthorized, "identity rejected")
		}
		metrics.RecordRouterRequest(ns, app, strconv.Itoa(rec.code()))
		return
	}

	userKey := identity.UserKey(ns, app, raw)
	ws, err := s.ensureWorkspace(ctx, raw, userKey)
	switch {
	case errors.Is(err, errWorkspaceLimit):
		metrics.RecordRequestRejection(ns, app, v1alpha1.RejectWorkspaceLimit)
		s.reject(rec, http.StatusServiceUnavailable, v1alpha1.RejectWorkspaceLimit)
		metrics.RecordRouterRequest(ns, app, strconv.Itoa(rec.code()))
		return
	case err != nil:
		s.reject(rec, http.StatusBadGateway, "internal error")
		metrics.RecordRouterRequest(ns, app, strconv.Itoa(rec.code()))
		return
	}

	if reason, rejected := classify(ws, s.now()); rejected {
		metrics.RecordRequestRejection(ns, app, reason)
		s.reject(rec, http.StatusServiceUnavailable, reason)
		metrics.RecordRouterRequest(ns, app, strconv.Itoa(rec.code()))
		return
	}

	key := types.NamespacedName{Namespace: ws.Namespace, Name: ws.Name}

	servable, err := s.isServable(ctx, ws)
	if err != nil {
		s.reject(rec, http.StatusBadGateway, "internal error")
		metrics.RecordRouterRequest(ns, app, strconv.Itoa(rec.code()))
		return
	}
	if !servable {
		ws, servable, err = s.coldStartHold(ctx, ws)
		if err != nil {
			s.reject(rec, http.StatusBadGateway, "internal error")
			metrics.RecordRouterRequest(ns, app, strconv.Itoa(rec.code()))
			return
		}
		if !servable {
			metrics.RecordRequestRejection(ns, app, v1alpha1.RejectHoldExpired)
			s.reject(rec, http.StatusServiceUnavailable, v1alpha1.RejectHoldExpired)
			metrics.RecordRouterRequest(ns, app, strconv.Itoa(rec.code()))
			return
		}
	}

	_ = touchActivity(ctx, s.Client, key, s.now())

	s.proxy(rec, r, ws)
	metrics.RecordRouterRequest(ns, app, strconv.Itoa(rec.code()))
}

// reject writes a plain-text response whose body IS the closed-set reason
// string: the same string that goes into puc_router_request_rejected_total
// (or "identity rejected: <reason>" for the identity path), so a screenshot
// of the response maps directly onto a series.
func (s *Server) reject(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

// checkCallerAuth is spec 217-219's single named trust control: exactly one
// occurrence of the configured header, matching the configured scheme and
// secret. It is checked before anything else in ServeHTTP runs — including
// identity derivation — because without it any pod that can open a socket
// to the router can set the identity header and become any user.
func (s *Server) checkCallerAuth(h http.Header) bool {
	// Fail closed on a misconfigured (empty) secret: without this, an empty
	// configured secret would make an empty presented credential (e.g. a
	// bare "Bearer " with nothing after it) compare equal and authenticate
	// every caller — turning a misconfiguration into the exact
	// impersonation-by-anyone-on-the-network failure this check exists to
	// prevent.
	if len(s.Cfg.CallerAuthSecret) == 0 {
		return false
	}
	values := h.Values(s.Cfg.CallerAuthHeader)
	if len(values) != 1 {
		return false
	}
	got := values[0]
	if s.Cfg.CallerAuthScheme != "" {
		prefix := s.Cfg.CallerAuthScheme + " "
		if !strings.HasPrefix(got, prefix) {
			return false
		}
		got = strings.TrimPrefix(got, prefix)
	}
	return subtle.ConstantTimeCompare([]byte(got), s.Cfg.CallerAuthSecret) == 1
}

// ensureWorkspace implements the two-call sequence spec status.enqueuedAt
// requires: Create (AlreadyExists treated as success), then a
// Status().Patch of enqueuedAt if and only if it is still unset. Workspace
// carries +kubebuilder:subresource:status, so writing status on the Create
// body itself is silently dropped by the API server; skipping the second
// call would leave enqueuedAt nil forever and give Task 8's FIFO admission
// no ordering key at all.
//
// A brand-new user is gated on spec.limits.maxWorkspaces BEFORE creation:
// an existing workspace is never re-counted against the limit (a lowered
// limit must not evict an existing user), but a request that would create
// the (maxWorkspaces+1)-th workspace is refused before any Create call.
func (s *Server) ensureWorkspace(ctx context.Context, raw, userKey string) (*v1alpha1.Workspace, error) {
	name := identity.ChildName(s.Cfg.App, userKey)
	key := types.NamespacedName{Namespace: s.Cfg.Namespace, Name: name}

	var existing v1alpha1.Workspace
	switch err := s.Client.Get(ctx, key, &existing); {
	case err == nil:
		return s.ensureEnqueuedAt(ctx, &existing)
	case !apierrors.IsNotFound(err):
		return nil, err
	}

	var list v1alpha1.WorkspaceList
	if err := s.Client.List(ctx, &list, client.InNamespace(s.Cfg.Namespace), client.MatchingLabels{v1alpha1.LabelApp: s.Cfg.App}); err != nil {
		return nil, err
	}
	if s.Cfg.MaxWorkspaces > 0 && int32(len(list.Items)) >= s.Cfg.MaxWorkspaces {
		return nil, errWorkspaceLimit
	}

	ws := &v1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: s.Cfg.Namespace,
			Labels: map[string]string{
				// Exactly these two labels — never the raw identity, which
				// is any printable ASCII up to maxLength and so is not a
				// legal label value. LabelApp must be present alongside
				// LabelUserKey: the controller's label-selected List adopts
				// on LabelApp, and a Workspace missing it is never
				// reconciled at all.
				v1alpha1.LabelApp:     s.Cfg.App,
				v1alpha1.LabelUserKey: userKey,
			},
			Annotations: map[string]string{v1alpha1.AnnUserDisplay: raw},
		},
		Spec: v1alpha1.WorkspaceSpec{
			AppRef:  corev1.LocalObjectReference{Name: s.Cfg.App},
			UserKey: userKey,
		},
	}
	if err := s.Client.Create(ctx, ws); err != nil {
		if apierrors.IsAlreadyExists(err) {
			var got v1alpha1.Workspace
			if gerr := s.Client.Get(ctx, key, &got); gerr != nil {
				return nil, gerr
			}
			return s.ensureEnqueuedAt(ctx, &got)
		}
		return nil, err
	}
	return s.ensureEnqueuedAt(ctx, ws)
}

func (s *Server) ensureEnqueuedAt(ctx context.Context, ws *v1alpha1.Workspace) (*v1alpha1.Workspace, error) {
	if ws.Status.EnqueuedAt != nil {
		return ws, nil
	}
	base := ws.DeepCopy()
	t := metav1.NewTime(s.now())
	ws.Status.EnqueuedAt = &t
	if err := s.Client.Status().Patch(ctx, ws, client.MergeFrom(base)); err != nil {
		return nil, err
	}
	return ws, nil
}

// classify implements the four request-rejection reasons that are pure
// functions of an already-fetched Workspace's status (the fifth,
// hold_expired, is a property of the cold-start wait and lives in
// coldStartHold instead). Order does not encode precedence the spec
// requires; each caller-visible reason arises from a disjoint status shape.
func classify(ws *v1alpha1.Workspace, now time.Time) (string, bool) {
	if !ws.DeletionTimestamp.IsZero() {
		return v1alpha1.RejectTerminating, true
	}
	if ws.Status.BackoffUntil != nil && now.Before(ws.Status.BackoffUntil.Time) {
		return v1alpha1.RejectBackoff, true
	}
	if ws.Status.WaitingReason == v1alpha1.WaitingRWOPConflict {
		return v1alpha1.RejectRWOPConflict, true
	}
	return "", false
}

// isServable is property 5: liveness comes from EndpointSlices, not
// status.phase. phase Ready with an empty endpoint set is a routine race
// after a node event, and serving on phase alone sends a user to nothing.
func (s *Server) isServable(ctx context.Context, ws *v1alpha1.Workspace) (bool, error) {
	if ws.Status.Phase != v1alpha1.PhaseReady {
		return false, nil
	}
	return s.Resolver.HasEndpoints(ctx, ws.Namespace, ws.Name)
}

// coldStartHold implements the scale-from-zero wait: status.wakeRequestedAt
// is set BEFORE the hold begins (spec 460-461 makes the router its sole
// writer), then this polls until the workspace becomes servable or
// coldStartHold elapses. Losing the race ends in RejectHoldExpired at the
// caller.
func (s *Server) coldStartHold(ctx context.Context, ws *v1alpha1.Workspace) (*v1alpha1.Workspace, bool, error) {
	key := types.NamespacedName{Namespace: ws.Namespace, Name: ws.Name}

	if ws.Status.WakeRequestedAt == nil {
		base := ws.DeepCopy()
		t := metav1.NewTime(s.now())
		ws.Status.WakeRequestedAt = &t
		if err := s.Client.Status().Patch(ctx, ws, client.MergeFrom(base)); err != nil {
			return ws, false, err
		}
	}

	// The hold's own deadline is deliberately real wall-clock time
	// (time.Now), NOT s.now(): s.now() is injectable so status timestamps
	// (WakeRequestedAt, EnqueuedAt, LastActivity) are deterministically
	// testable, but this loop's job is to bound an actual wait on a real
	// pod starting. Computing the deadline from an injected, frozen clock
	// would make it un-passable — the exact deadlock a synthetic-clock test
	// hit during development of this file.
	deadline := time.Now().Add(s.Cfg.ColdStartHold)
	ticker := time.NewTicker(s.pollInterval())
	defer ticker.Stop()

	for {
		var cur v1alpha1.Workspace
		if err := s.Client.Get(ctx, key, &cur); err != nil {
			return ws, false, err
		}
		ok, err := s.isServable(ctx, &cur)
		if err != nil {
			return ws, false, err
		}
		if ok {
			return &cur, true, nil
		}
		if !time.Now().Before(deadline) {
			return &cur, false, nil
		}
		select {
		case <-ctx.Done():
			return &cur, false, ctx.Err()
		case <-ticker.C:
		}
	}
}

// proxy resolves ws's Service by NAME (never a memoised ClusterIP — see
// Resolver's doc comment) and forwards the request. Only upgrade requests
// are tracked in status.connections: a plain HTTP or SSE request is not the
// kind of long-lived per-user session that metric and the reaper's
// freshness predicate are about.
func (s *Server) proxy(w http.ResponseWriter, r *http.Request, ws *v1alpha1.Workspace) {
	addr, err := s.Resolver.Resolve(r.Context(), ws.Namespace, ws.Name, s.Cfg.WorkspacePort)
	if err != nil {
		s.reject(w, http.StatusBadGateway, "workspace unreachable")
		return
	}

	if isUpgradeRequest(r) && s.Conns != nil {
		key := types.NamespacedName{Namespace: ws.Namespace, Name: ws.Name}
		if err := s.Conns.Open(r.Context(), key); err == nil {
			defer func() { _ = s.Conns.Close(context.Background(), key) }()
		}
	}

	newReverseProxy(addr, s.Cfg).ServeHTTP(w, r)
}
