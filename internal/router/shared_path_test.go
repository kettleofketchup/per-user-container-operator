package router

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kettleofketchup/per-user-container-operator/api/v1alpha1"
	"github.com/kettleofketchup/per-user-container-operator/internal/identity"
)

const sharedTestPath = "/openapi.json"

// sharedPathServer wires a Server whose only configured shared path is
// sharedTestPath, with the reserved identity's Workspace already Ready so a
// substituted request proxies rather than cold-starting.
func sharedPathServer(t *testing.T, ns, app string) (*Server, func()) {
	t.Helper()
	host, port, backend := okBackend(t)

	cfg := testConfig(ns, app)
	cfg.WorkspacePort = int32(port)
	cfg.SharedPaths = []string{sharedTestPath}

	sharedKey := identity.UserKey(ns, app, identity.Shared)
	sharedName := identity.ChildName(app, sharedKey)

	aliceKey := identity.UserKey(ns, app, "alice")
	aliceName := identity.ChildName(app, aliceKey)

	fc := newFakeClient(t,
		readyWorkspace(ns, app, sharedKey, sharedName),
		serviceFor(ns, sharedName, host, port),
		endpointSliceFor(ns, sharedName, host),
		readyWorkspace(ns, app, aliceKey, aliceName),
		serviceFor(ns, aliceName, host, port),
		endpointSliceFor(ns, aliceName, host),
	)
	return NewServer(cfg, fc), backend.Close
}

func sharedPathRequest(method, path string) *http.Request {
	r := httptest.NewRequest(method, path, nil)
	r.Header.Set("Authorization", "Bearer caller-secret")
	return r
}

// TestSharedPathServesIdentitylessRequest is the whole point of the field: a
// client discovering the app's API has no user in scope and so cannot send an
// identity header on that one fetch. Before sharedPaths existed this was a
// flat 401, which left the app looking unreachable to a client whose per-user
// requests would all have worked.
func TestSharedPathServesIdentitylessRequest(t *testing.T) {
	srv, closeBackend := sharedPathServer(t, "ns-shared", "myapp")
	defer closeBackend()

	for _, method := range []string{http.MethodGet, http.MethodHead} {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, sharedPathRequest(method, sharedTestPath))
		if rec.Code != http.StatusOK {
			t.Errorf("%s %s: got %d, want 200", method, sharedTestPath, rec.Code)
		}
	}
}

// TestSharedPathDoesNotLeakToOtherPaths pins the blast radius. The match is
// exact by design, so every neighbouring spelling — a subtree, a traversal, a
// doubled slash, an unlisted sibling — must still demand an identity.
func TestSharedPathDoesNotLeakToOtherPaths(t *testing.T) {
	srv, closeBackend := sharedPathServer(t, "ns-shared-leak", "myapp")
	defer closeBackend()

	for _, path := range []string{
		"/",
		"/files/list",
		"/openapi.json/../files/list",
		"//openapi.json",
		"/openapi.jsonx",
		"/openapi.json/sub",
	} {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, sharedPathRequest(http.MethodGet, path))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("GET %s: got %d, want 401 — only the exact listed path is identity-less", path, rec.Code)
		}
	}
}

// TestSharedPathRejectsWriteMethods keeps a shared path read-only. The paths
// worth listing are user-independent metadata; a write arriving without a
// user attached has no correct workspace to land in.
func TestSharedPathRejectsWriteMethods(t *testing.T) {
	srv, closeBackend := sharedPathServer(t, "ns-shared-write", "myapp")
	defer closeBackend()

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, sharedPathRequest(method, sharedTestPath))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s: got %d, want 401", method, sharedTestPath, rec.Code)
		}
	}
}

// TestSharedPathStillRequiresCallerAuth: the substitution happens after the
// caller-auth check and must not become a way around it. A shared path is
// still reachable only by something holding the credential.
func TestSharedPathStillRequiresCallerAuth(t *testing.T) {
	srv, closeBackend := sharedPathServer(t, "ns-shared-auth", "myapp")
	defer closeBackend()

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, sharedTestPath, nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no caller credential: got %d, want 401", rec.Code)
	}

	rec = httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, sharedTestPath, nil)
	r.Header.Set("Authorization", "Bearer wrong-secret")
	srv.ServeHTTP(rec, r)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong caller credential: got %d, want 401", rec.Code)
	}
}

// TestSharedPathSubstitutesOnlyMissingIdentity: every other rejection reason
// is the shape a header takes when something is trying to be somebody, and a
// shared path is exactly where that is worth trying. Only an absent header is
// substituted; a malformed one fails here as it does on any other path.
func TestSharedPathSubstitutesOnlyMissingIdentity(t *testing.T) {
	srv, closeBackend := sharedPathServer(t, "ns-shared-reasons", "myapp")
	defer closeBackend()

	cases := []struct {
		name   string
		values []string
		want   int
	}{
		{"empty", []string{""}, http.StatusUnauthorized},
		{"whitespace", []string{"   "}, http.StatusUnauthorized},
		{"duplicate", []string{"alice", "bob"}, http.StatusBadRequest},
		{"comma merged", []string{"alice,bob"}, http.StatusBadRequest},
		{"non ascii", []string{"alice\x01"}, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := sharedPathRequest(http.MethodGet, sharedTestPath)
			for _, v := range tc.values {
				r.Header.Add("X-User-Id", v)
			}
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, r)
			if rec.Code != tc.want {
				t.Fatalf("got %d, want %d", rec.Code, tc.want)
			}
		})
	}
}

// TestSharedIdentityIsNotClaimable is the control that makes the reserved
// identity safe to introduce at all. If a caller could simply send it, the
// shared workspace would be a shared session anyone holding the caller
// credential could join.
func TestSharedIdentityIsNotClaimable(t *testing.T) {
	srv, closeBackend := sharedPathServer(t, "ns-shared-claim", "myapp")
	defer closeBackend()

	for _, path := range []string{sharedTestPath, "/files/list"} {
		r := sharedPathRequest(http.MethodGet, path)
		r.Header.Set("X-User-Id", identity.Shared)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, r)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("GET %s claiming the reserved identity: got %d, want 400", path, rec.Code)
		}
	}
}

// TestSharedPathHonoursSuppliedIdentity: a real user fetching a shared path
// is still that user. Substitution fills an absence; it never overrides.
func TestSharedPathHonoursSuppliedIdentity(t *testing.T) {
	ns, app := "ns-shared-real", "myapp"
	srv, closeBackend := sharedPathServer(t, ns, app)
	defer closeBackend()

	r := sharedPathRequest(http.MethodGet, sharedTestPath)
	r.Header.Set("X-User-Id", "alice")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", rec.Code)
	}

	// alice's own Workspace served it, not the reserved one.
	var ws v1alpha1.WorkspaceList
	if err := srv.Client.List(context.Background(), &ws); err != nil {
		t.Fatalf("list workspaces: %v", err)
	}
	sharedKey := identity.UserKey(ns, app, identity.Shared)
	for _, w := range ws.Items {
		if w.Spec.UserKey == sharedKey && w.Status.Phase != v1alpha1.PhaseReady {
			t.Fatalf("reserved workspace was disturbed by an identified request")
		}
	}
}

// TestNoSharedPathsMeansEveryPathNeedsIdentity: the field is optional and its
// zero value must be the behaviour every app had before it existed.
func TestNoSharedPathsMeansEveryPathNeedsIdentity(t *testing.T) {
	ns, app := "ns-no-shared", "myapp"
	host, port, backend := okBackend(t)
	defer backend.Close()

	cfg := testConfig(ns, app)
	cfg.WorkspacePort = int32(port)

	sharedKey := identity.UserKey(ns, app, identity.Shared)
	sharedName := identity.ChildName(app, sharedKey)
	fc := newFakeClient(t,
		readyWorkspace(ns, app, sharedKey, sharedName),
		serviceFor(ns, sharedName, host, port),
		endpointSliceFor(ns, sharedName, host),
	)
	srv := NewServer(cfg, fc)

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, sharedPathRequest(http.MethodGet, sharedTestPath))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401 with no sharedPaths configured", rec.Code)
	}
}
