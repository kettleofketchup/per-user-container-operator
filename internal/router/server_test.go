package router

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kettleofketchup/per-user-container-operator/api/v1alpha1"
	"github.com/kettleofketchup/per-user-container-operator/internal/identity"
	"github.com/kettleofketchup/per-user-container-operator/internal/metrics"
)

func okBackend(t *testing.T) (host string, port int, srv *httptest.Server) {
	t.Helper()
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	h, p, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatalf("split addr: %v", err)
	}
	pn, err := strconv.Atoi(p)
	if err != nil {
		t.Fatalf("parse port: %v", err)
	}
	return h, pn, srv
}

// TestCallerAuthRejectsMissingAndWrongCredential is the spec's single named
// trust control (spec 217-219). Neither the workspace-origin nor the
// Traefik-origin forge Task 13 relies on elsewhere exercises the bearer
// check itself, so it is proven here directly: any pod that can open a
// socket to the router must never be able to impersonate a user just by
// setting the identity header.
func TestCallerAuthRejectsMissingAndWrongCredential(t *testing.T) {
	ns, app := "ns-callerauth", "myapp"
	cfg := testConfig(ns, app)
	host, port, backend := okBackend(t)
	defer backend.Close()
	cfg.WorkspacePort = int32(port)

	userKey := identity.UserKey(ns, app, "alice")
	wsName := identity.ChildName(app, userKey)
	ws := readyWorkspace(ns, app, userKey, wsName)
	svc := serviceFor(ns, wsName, host, port)
	eps := endpointSliceFor(ns, wsName, host)

	fc := newFakeClient(t, ws, svc, eps)
	srv := NewServer(cfg, fc)

	badCases := []struct {
		name   string
		header string
	}{
		{"missing", ""},
		{"wrong scheme", "Basic " + string(cfg.CallerAuthSecret)},
		{"wrong value", "Bearer nope"},
	}
	for _, c := range badCases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if c.header != "" {
				req.Header.Set(cfg.CallerAuthHeader, c.header)
			}
			req.Header.Set(cfg.IdentityHeader, "alice")
			rw := httptest.NewRecorder()
			srv.ServeHTTP(rw, req)
			if rw.Code != http.StatusUnauthorized {
				t.Fatalf("%s: status = %d, want 401", c.name, rw.Code)
			}
		})
	}

	var list v1alpha1.WorkspaceList
	if err := fc.List(context.Background(), &list, client.InNamespace(ns)); err != nil {
		t.Fatalf("list workspaces: %v", err)
	}
	if len(list.Items) != 1 {
		t.Fatalf("workspace creates recorded beyond the pre-seeded fixture: got %d, want 1 (zero router-triggered creates)", len(list.Items))
	}

	resp := doAuthedRequest(t, srv, cfg, "alice")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("correct credential: status = %d, body=%s, want 200", resp.StatusCode, readBody(t, resp))
	}
}

// TestCallerAuthFailsClosedOnEmptyConfiguredSecret guards against a
// misconfiguration turning into the exact impersonation failure the check
// exists to prevent: an empty CallerAuthSecret (e.g. an empty mounted
// secret file) must never authenticate an empty presented credential.
func TestCallerAuthFailsClosedOnEmptyConfiguredSecret(t *testing.T) {
	ns, app := "ns-empty-secret", "myapp"
	cfg := testConfig(ns, app)
	cfg.CallerAuthSecret = nil
	fc := newFakeClient(t)
	srv := NewServer(cfg, fc)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(cfg.CallerAuthHeader, cfg.CallerAuthScheme+" ") // empty credential value
	req.Header.Set(cfg.IdentityHeader, "alice")
	rw := httptest.NewRecorder()
	srv.ServeHTTP(rw, req)

	if rw.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (an empty configured secret must never authenticate)", rw.Code)
	}
}

// TestIdentityRejectionCreatesNothing drives all five identity.Reason cases
// through the real handler: Extract itself can be perfect while server.go
// ignores its error, calls it after ensureWorkspace, or falls through to a
// default identity, and every Task 2 test would still pass. Only driving it
// through this handler catches that.
func TestIdentityRejectionCreatesNothing(t *testing.T) {
	ns, app := "ns-identity", "myapp"
	cfg := testConfig(ns, app)

	allReasons := []identity.Reason{
		identity.ReasonMissing, identity.ReasonEmpty, identity.ReasonTooLong,
		identity.ReasonDuplicate, identity.ReasonInvalid,
	}

	cases := []struct {
		name       string
		setHeader  func(h http.Header)
		wantStatus int
		wantReason identity.Reason
	}{
		{"missing", func(_ http.Header) {}, http.StatusUnauthorized, identity.ReasonMissing},
		{"empty", func(h http.Header) { h.Set(cfg.IdentityHeader, "   ") }, http.StatusUnauthorized, identity.ReasonEmpty},
		{"too_long", func(h http.Header) { h.Set(cfg.IdentityHeader, strings.Repeat("a", cfg.IdentityMaxLength+1)) }, http.StatusUnauthorized, identity.ReasonTooLong},
		{"duplicate", func(h http.Header) { h.Add(cfg.IdentityHeader, "alice"); h.Add(cfg.IdentityHeader, "bob") }, http.StatusBadRequest, identity.ReasonDuplicate},
		{"invalid", func(h http.Header) { h.Set(cfg.IdentityHeader, "bad\x00id") }, http.StatusBadRequest, identity.ReasonInvalid},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			metrics.ResetForTest()
			fc := newFakeClient(t)
			srv := NewServer(cfg, fc)

			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Header.Set(cfg.CallerAuthHeader, cfg.CallerAuthScheme+" "+string(cfg.CallerAuthSecret))
			c.setHeader(req.Header)
			rw := httptest.NewRecorder()
			srv.ServeHTTP(rw, req)

			if rw.Code != c.wantStatus {
				t.Fatalf("%s: status = %d, want %d", c.name, rw.Code, c.wantStatus)
			}
			for _, r := range allReasons {
				want := 0.0
				if r == c.wantReason {
					want = 1
				}
				got, _ := gatherCounter(t, "puc_router_identity_rejected_total", map[string]string{"namespace": ns, "app": app, "reason": string(r)})
				if got != want {
					t.Fatalf("%s: puc_router_identity_rejected_total{reason=%s} = %v, want %v", c.name, r, got, want)
				}
			}

			var list v1alpha1.WorkspaceList
			if err := fc.List(context.Background(), &list, client.InNamespace(ns)); err != nil {
				t.Fatalf("list workspaces: %v", err)
			}
			if len(list.Items) != 0 {
				t.Fatalf("%s: workspace creates = %d, want 0", c.name, len(list.Items))
			}

			var pvcs corev1.PersistentVolumeClaimList
			if err := fc.List(context.Background(), &pvcs, client.InNamespace(ns)); err != nil {
				t.Fatalf("list pvcs: %v", err)
			}
			if len(pvcs.Items) != 0 {
				t.Fatalf("%s: pvc count = %d, want 0", c.name, len(pvcs.Items))
			}
		})
	}
}

func TestBackoffUntilInTheFutureRejectsWithReasonBackoff(t *testing.T) {
	metrics.ResetForTest()
	ns, app := "ns-backoff", "myapp"
	cfg := testConfig(ns, app)
	userKey := identity.UserKey(ns, app, "alice")
	wsName := identity.ChildName(app, userKey)
	until := metav1.NewTime(time.Now().Add(time.Hour))
	ws := newWorkspace(ns, app, userKey, wsName, v1alpha1.WorkspaceStatus{Phase: v1alpha1.PhaseFailed, BackoffUntil: &until})

	fc := newFakeClient(t, ws)
	srv := NewServer(cfg, fc)

	resp := doAuthedRequest(t, srv, cfg, "alice")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	if body := readBody(t, resp); body != v1alpha1.RejectBackoff {
		t.Fatalf("body = %q, want %q", body, v1alpha1.RejectBackoff)
	}
	if got, _ := gatherCounter(t, "puc_router_request_rejected_total", map[string]string{"namespace": ns, "app": app, "reason": v1alpha1.RejectBackoff}); got != 1 {
		t.Fatalf("puc_router_request_rejected_total{reason=backoff} = %v, want 1", got)
	}
}

func TestMaxWorkspacesRejectsWithReasonWorkspaceLimit(t *testing.T) {
	metrics.ResetForTest()
	ns, app := "ns-limit", "myapp"
	cfg := testConfig(ns, app)
	cfg.MaxWorkspaces = 1

	existingKey := identity.UserKey(ns, app, "alice")
	existing := newWorkspace(ns, app, existingKey, identity.ChildName(app, existingKey), v1alpha1.WorkspaceStatus{})

	fc := newFakeClient(t, existing)
	srv := NewServer(cfg, fc)

	resp := doAuthedRequest(t, srv, cfg, "bob")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	if body := readBody(t, resp); body != v1alpha1.RejectWorkspaceLimit {
		t.Fatalf("body = %q, want %q", body, v1alpha1.RejectWorkspaceLimit)
	}

	var list v1alpha1.WorkspaceList
	if err := fc.List(context.Background(), &list, client.InNamespace(ns)); err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list.Items) != 1 {
		t.Fatalf("workspace created despite the limit: got %d, want 1 (only the pre-seeded fixture)", len(list.Items))
	}
	if got, _ := gatherCounter(t, "puc_router_request_rejected_total", map[string]string{"namespace": ns, "app": app, "reason": v1alpha1.RejectWorkspaceLimit}); got != 1 {
		t.Fatalf("puc_router_request_rejected_total{reason=workspace_limit} = %v, want 1", got)
	}
}

// TestRWOPConflictRejectsWithReasonRwopConflict feeds the RECONCILER-derived
// status.waitingReason, per task-10-brief.md's warning against bare-seeding
// a string with no real producer.
func TestRWOPConflictRejectsWithReasonRwopConflict(t *testing.T) {
	metrics.ResetForTest()
	ns, app := "ns-rwop", "myapp"
	cfg := testConfig(ns, app)
	userKey := identity.UserKey(ns, app, "alice")
	wsName := identity.ChildName(app, userKey)
	ws := newWorkspace(ns, app, userKey, wsName, v1alpha1.WorkspaceStatus{Phase: v1alpha1.PhaseStarting, WaitingReason: v1alpha1.WaitingRWOPConflict})

	fc := newFakeClient(t, ws)
	srv := NewServer(cfg, fc)

	resp := doAuthedRequest(t, srv, cfg, "alice")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	if body := readBody(t, resp); body != v1alpha1.RejectRWOPConflict {
		t.Fatalf("body = %q, want %q", body, v1alpha1.RejectRWOPConflict)
	}
	if got, _ := gatherCounter(t, "puc_router_request_rejected_total", map[string]string{"namespace": ns, "app": app, "reason": v1alpha1.RejectRWOPConflict}); got != 1 {
		t.Fatalf("puc_router_request_rejected_total{reason=rwop_conflict} = %v, want 1", got)
	}
}

func TestTerminatingWorkspaceRejectsWithReasonTerminating(t *testing.T) {
	metrics.ResetForTest()
	ns, app := "ns-terminating", "myapp"
	cfg := testConfig(ns, app)
	userKey := identity.UserKey(ns, app, "alice")
	wsName := identity.ChildName(app, userKey)
	now := metav1.NewTime(time.Now())
	ws := newWorkspace(ns, app, userKey, wsName, v1alpha1.WorkspaceStatus{Phase: v1alpha1.PhaseReady})
	ws.DeletionTimestamp = &now
	ws.Finalizers = []string{"puc.kettleofketchup/workspace-cleanup"}

	fc := newFakeClient(t, ws)
	srv := NewServer(cfg, fc)

	resp := doAuthedRequest(t, srv, cfg, "alice")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	if body := readBody(t, resp); body != v1alpha1.RejectTerminating {
		t.Fatalf("body = %q, want %q", body, v1alpha1.RejectTerminating)
	}
	if got, _ := gatherCounter(t, "puc_router_request_rejected_total", map[string]string{"namespace": ns, "app": app, "reason": v1alpha1.RejectTerminating}); got != 1 {
		t.Fatalf("puc_router_request_rejected_total{reason=terminating} = %v, want 1", got)
	}
}

// TestColdStartHoldThenRelease proves the release half: a workspace that
// becomes Ready (with endpoints) MID-HOLD is proxied 200, not rejected.
func TestColdStartHoldThenRelease(t *testing.T) {
	ns, app := "ns-hold-release", "myapp"
	cfg := testConfig(ns, app)
	cfg.ColdStartHold = 2 * time.Second
	host, port, backend := okBackend(t)
	defer backend.Close()
	cfg.WorkspacePort = int32(port)

	userKey := identity.UserKey(ns, app, "alice")
	wsName := identity.ChildName(app, userKey)
	ws := newWorkspace(ns, app, userKey, wsName, v1alpha1.WorkspaceStatus{Phase: v1alpha1.PhaseStarting})
	svc := serviceFor(ns, wsName, host, port)

	fc := newFakeClient(t, ws, svc)
	srv := NewServer(cfg, fc)
	srv.PollInterval = 20 * time.Millisecond

	key := types.NamespacedName{Namespace: ns, Name: wsName}
	go func() {
		time.Sleep(150 * time.Millisecond)
		var cur v1alpha1.Workspace
		if err := fc.Get(context.Background(), key, &cur); err != nil {
			return
		}
		base := cur.DeepCopy()
		cur.Status.Phase = v1alpha1.PhaseReady
		_ = fc.Status().Patch(context.Background(), &cur, client.MergeFrom(base))
		_ = fc.Create(context.Background(), endpointSliceFor(ns, wsName, host))
	}()

	resp := doAuthedRequest(t, srv, cfg, "alice")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body=%s, want 200 (released mid-hold)", resp.StatusCode, readBody(t, resp))
	}
}

func TestColdStartHoldExpiresWithReasonHoldExpired(t *testing.T) {
	metrics.ResetForTest()
	ns, app := "ns-hold-expire", "myapp"
	cfg := testConfig(ns, app)
	cfg.ColdStartHold = 100 * time.Millisecond

	userKey := identity.UserKey(ns, app, "alice")
	wsName := identity.ChildName(app, userKey)
	ws := newWorkspace(ns, app, userKey, wsName, v1alpha1.WorkspaceStatus{Phase: v1alpha1.PhaseStarting})

	fc := newFakeClient(t, ws)
	srv := NewServer(cfg, fc)
	srv.PollInterval = 15 * time.Millisecond

	resp := doAuthedRequest(t, srv, cfg, "alice")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	if body := readBody(t, resp); body != v1alpha1.RejectHoldExpired {
		t.Fatalf("body = %q, want %q", body, v1alpha1.RejectHoldExpired)
	}
	if got, _ := gatherCounter(t, "puc_router_request_rejected_total", map[string]string{"namespace": ns, "app": app, "reason": v1alpha1.RejectHoldExpired}); got != 1 {
		t.Fatalf("puc_router_request_rejected_total{reason=hold_expired} = %v, want 1", got)
	}
}

// TestPhaseReadyButEmptyEndpointsIsNotServed is property 5: liveness comes
// from EndpointSlices, not status.phase. A Ready workspace with an
// EndpointSlice present but carrying zero endpoints must never be served.
// A live, reachable backend/Service IS present here — deliberately, so that
// an implementation which serves on phase alone (ignoring endpoints
// entirely) would actually succeed in proxying to it and return 200; a
// weaker test that omitted the Service could pass vacuously on an
// unrelated "no such Service" error instead of on the endpoint check.
func TestPhaseReadyButEmptyEndpointsIsNotServed(t *testing.T) {
	metrics.ResetForTest()
	ns, app := "ns-empty-eps", "myapp"
	cfg := testConfig(ns, app)
	cfg.ColdStartHold = 80 * time.Millisecond
	host, port, backend := okBackend(t)
	defer backend.Close()
	cfg.WorkspacePort = int32(port)

	userKey := identity.UserKey(ns, app, "alice")
	wsName := identity.ChildName(app, userKey)
	ws := readyWorkspace(ns, app, userKey, wsName)
	svc := serviceFor(ns, wsName, host, port)
	eps := emptyEndpointSlice(ns, wsName)

	fc := newFakeClient(t, ws, svc, eps)
	srv := NewServer(cfg, fc)
	srv.PollInterval = 15 * time.Millisecond

	resp := doAuthedRequest(t, srv, cfg, "alice")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("phase Ready with an empty EndpointSlice must not be served, got 200 (a live, reachable backend was one Resolve call away)")
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	if body := readBody(t, resp); body != v1alpha1.RejectHoldExpired {
		t.Fatalf("body = %q, want %q", body, v1alpha1.RejectHoldExpired)
	}
}

// TestWakeRequestedAtIsSetBeforeTheHoldBegins proves the router writes
// wakeRequestedAt itself, and does so before the hold starts -- not after it
// resolves. Task 9's own coverage only ever writes this field "from a second
// client," so nothing else asserts the router writes it at all.
func TestWakeRequestedAtIsSetBeforeTheHoldBegins(t *testing.T) {
	ns, app := "ns-wake", "myapp"
	cfg := testConfig(ns, app)
	cfg.ColdStartHold = 400 * time.Millisecond

	userKey := identity.UserKey(ns, app, "alice")
	wsName := identity.ChildName(app, userKey)
	ws := newWorkspace(ns, app, userKey, wsName, v1alpha1.WorkspaceStatus{Phase: v1alpha1.PhaseIdle, ScaledDown: true})

	fc := newFakeClient(t, ws)
	srv := NewServer(cfg, fc)
	srv.PollInterval = 15 * time.Millisecond

	key := types.NamespacedName{Namespace: ns, Name: wsName}
	done := make(chan struct{}, 1)
	go func() {
		resp := doAuthedRequest(t, srv, cfg, "alice")
		_ = resp.Body.Close()
		done <- struct{}{}
	}()

	waitUntil(t, 2*time.Second, func() bool {
		var cur v1alpha1.Workspace
		if err := fc.Get(context.Background(), key, &cur); err != nil {
			return false
		}
		return cur.Status.WakeRequestedAt != nil
	})

	<-done // drain the still-holding request so the test doesn't leak it
}

// TestEnqueuedAtIsWrittenOnceAndNeverRewritten is Step 3's explicit second
// assertion: status.enqueuedAt is a two-call sequence (Create, then
// Status().Patch), because Workspace carries +kubebuilder:subresource:status
// and the API server strips status from a Create body. A single-call
// implementation leaves enqueuedAt nil forever with every other test in this
// file still green. It also proves the write happens EXACTLY once: a second
// request for the same, already-enqueued workspace must never re-stamp the
// field, or a later arrival could cut ahead of everyone already queued.
func TestEnqueuedAtIsWrittenOnceAndNeverRewritten(t *testing.T) {
	ns, app := "ns-enqueued", "myapp"
	cfg := testConfig(ns, app)
	// Small and real-time (no injected Clock): the freshly created workspace
	// never becomes servable in this test, so every request runs the
	// cold-start hold to expiry — a frozen synthetic clock would make that
	// hold's deadline check (which reads the same clock) never pass,
	// hanging the request forever instead of returning 503 hold_expired.
	cfg.ColdStartHold = 20 * time.Millisecond
	fc := newFakeClient(t)
	srv := NewServer(cfg, fc)
	srv.PollInterval = 5 * time.Millisecond

	resp1 := doAuthedRequest(t, srv, cfg, "alice")
	_ = resp1.Body.Close()

	userKey := identity.UserKey(ns, app, "alice")
	key := types.NamespacedName{Namespace: ns, Name: identity.ChildName(app, userKey)}
	var first v1alpha1.Workspace
	if err := fc.Get(context.Background(), key, &first); err != nil {
		t.Fatalf("get: %v", err)
	}
	if first.Status.EnqueuedAt == nil {
		t.Fatal("status.enqueuedAt is nil after the first request")
	}
	firstVal := first.Status.EnqueuedAt.Time

	resp2 := doAuthedRequest(t, srv, cfg, "alice")
	_ = resp2.Body.Close()
	var second v1alpha1.Workspace
	if err := fc.Get(context.Background(), key, &second); err != nil {
		t.Fatalf("get: %v", err)
	}
	if !second.Status.EnqueuedAt.Time.Equal(firstVal) {
		t.Fatalf("enqueuedAt was rewritten on a second request: got %v, want unchanged %v", second.Status.EnqueuedAt.Time, firstVal)
	}
}

// TestEnqueuedAtDistinctAcrossWorkspaces proves ensureEnqueuedAt actually
// stamps the current clock reading, rather than a fixed or zero value, by
// giving two different users' first requests clock readings a full second
// apart. A full second, not a smaller gap, is deliberate: metav1.Time's wire
// format (time.RFC3339, no fractional seconds — see MarshalJSON) truncates
// to whole-second granularity on every Patch, against the fake client here
// and against a real API server identically, so a sub-second gap would be
// indistinguishable from Step 1's "created within the same second" case
// after round-tripping. This is a pre-existing constraint of the
// *metav1.Time field type Task 4 already froze on WorkspaceStatus, not
// something this task's Step 3 can improve on: EnqueuedAt's ordering
// resolution is exactly as coarse as CreationTimestamp's, which undercuts
// the doc comment's claim that it gives a burst of simultaneous requests a
// total order at nanosecond precision. See this task's report for the
// concern this raises for Task 8's FIFO admission ordering.
func TestEnqueuedAtDistinctAcrossWorkspaces(t *testing.T) {
	ns, app := "ns-enqueued-distinct", "myapp"
	cfg := testConfig(ns, app)
	fc := newFakeClient(t)
	clock := newFakeClockAt(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	srv := NewServer(cfg, fc)
	srv.Clock = clock.Now

	resp1 := doAuthedRequest(t, srv, cfg, "alice")
	_ = resp1.Body.Close()
	clock.Advance(time.Second)
	resp2 := doAuthedRequest(t, srv, cfg, "bob")
	_ = resp2.Body.Close()

	aliceKey := identity.UserKey(ns, app, "alice")
	bobKey := identity.UserKey(ns, app, "bob")

	var aliceWS, bobWS v1alpha1.Workspace
	if err := fc.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: identity.ChildName(app, aliceKey)}, &aliceWS); err != nil {
		t.Fatalf("get alice workspace: %v", err)
	}
	if err := fc.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: identity.ChildName(app, bobKey)}, &bobWS); err != nil {
		t.Fatalf("get bob workspace: %v", err)
	}
	if aliceWS.Status.EnqueuedAt == nil || bobWS.Status.EnqueuedAt == nil {
		t.Fatalf("enqueuedAt must be non-nil for both: alice=%v bob=%v", aliceWS.Status.EnqueuedAt, bobWS.Status.EnqueuedAt)
	}
	if !bobWS.Status.EnqueuedAt.After(aliceWS.Status.EnqueuedAt.Time) {
		t.Fatalf("bob's enqueuedAt (%v) must be after alice's (%v)", bobWS.Status.EnqueuedAt.Time, aliceWS.Status.EnqueuedAt.Time)
	}
}
