//go:build envtest

// router_identity_test.go holds exactly TestRawIdentityIsAnnotatedNeverLabelled
// (task-10-brief.md's Files block lists this test explicitly, here, for the
// same reason Task 9 lists test/envtest/reaper_test.go: an implementer
// working only from internal/router's own suite would write this test
// against the fake client its siblings use, and a fake client accepts a
// 200-character label value outright — passing over the exact bug this
// test exists to catch. Only a real API server enforces Kubernetes' label
// VALUE syntax (DNS-1123-ish, <=63 chars), which is a property of core
// object metadata, not of this operator's CRD schema.
package envtest

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kettleofketchup/per-user-container-operator/api/v1alpha1"
	"github.com/kettleofketchup/per-user-container-operator/internal/identity"
	"github.com/kettleofketchup/per-user-container-operator/internal/router"
)

func routerConfigFor(ns, app string) router.Config {
	return router.Config{
		App:                         app,
		Namespace:                   ns,
		IdentityHeader:              "X-User-Id",
		IdentityMaxLength:           256,
		CallerAuthHeader:            "Authorization",
		CallerAuthScheme:            "Bearer",
		CallerAuthSecret:            []byte("caller-secret"),
		WorkspacePort:               8000,
		ColdStartHold:               60 * time.Millisecond,
		ConnectionHeartbeatInterval: 30 * time.Second,
		MaxWorkspaces:               10,
		PodName:                     "router-test-pod",
	}
}

// TestRawIdentityIsAnnotatedNeverLabelled is spec 198-202: only userKey is a
// label. An accepted raw identity is any printable ASCII up to maxLength,
// which includes every email address and any 200-character token — none of
// which is a legal label VALUE. A router that stamped it as a label 422s
// AFTER the identity path succeeded: one user permanently broken while
// everyone else works, the hardest class of failure to notice. The label
// set on the created Workspace must be EXACTLY {LabelApp, LabelUserKey}:
// LabelUserKey alone would drop LabelApp, which is what the controller's
// label-selected List adopts on.
func TestRawIdentityIsAnnotatedNeverLabelled(t *testing.T) {
	ns := newNamespace(t)
	app := "workspace-app"

	cfg := routerConfigFor(ns, app)
	srv := router.NewServer(cfg, k8sClient)
	srv.PollInterval = 20 * time.Millisecond

	cases := []struct {
		name string
		raw  string
	}{
		{"email", "alice@corp.example"},
		{"long_token", strings.Repeat("t", 200)},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Header.Set(cfg.CallerAuthHeader, cfg.CallerAuthScheme+" "+string(cfg.CallerAuthSecret))
			req.Header.Set(cfg.IdentityHeader, c.raw)
			rw := httptest.NewRecorder()
			srv.ServeHTTP(rw, req)

			userKey := identity.UserKey(ns, app, c.raw)
			name := identity.ChildName(app, userKey)

			var ws v1alpha1.Workspace
			mustGetObj(t, client.ObjectKey{Namespace: ns, Name: name}, &ws)

			if got := ws.Annotations[v1alpha1.AnnUserDisplay]; got != c.raw {
				t.Fatalf("annotation %s = %q, want raw identity %q", v1alpha1.AnnUserDisplay, got, c.raw)
			}

			wantLabels := map[string]string{v1alpha1.LabelApp: app, v1alpha1.LabelUserKey: userKey}
			if len(ws.Labels) != len(wantLabels) {
				t.Fatalf("label set = %v, want exactly %v", ws.Labels, wantLabels)
			}
			for k, v := range wantLabels {
				if ws.Labels[k] != v {
					t.Fatalf("label %s = %q, want %q", k, ws.Labels[k], v)
				}
			}
			for k, v := range ws.Labels {
				if strings.Contains(v, c.raw) {
					t.Fatalf("label %s=%q carries the raw identity value %q; only the annotation may", k, v, c.raw)
				}
			}
		})
	}
}
