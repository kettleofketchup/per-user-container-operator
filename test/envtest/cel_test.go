//go:build envtest

// cel_test.go exercises the CEL rules Task 4 declared that a grep canary
// cannot enforce: the API server, not this operator's Go code, is what
// rejects these requests.
package envtest

import (
	"context"
	"strings"
	"testing"
	"time"

	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kettleofketchup/per-user-container-operator/api/v1alpha1"
	"github.com/kettleofketchup/per-user-container-operator/internal/testfixtures"
)

func validAppFor(ns string) *v1alpha1.PerUserApp {
	app := testfixtures.ValidApp()
	app.Namespace = ns
	return app
}

func TestNameLengthCELIsEnforcedByTheAPIServer(t *testing.T) {
	ns := newNamespace(t)

	ok := validAppFor(ns)
	ok.Name = strings.Repeat("a", 27)
	if err := k8sClient.Create(context.Background(), ok); err != nil {
		t.Fatalf("a 27-char name must be accepted: %v", err)
	}

	tooLong := validAppFor(ns)
	tooLong.Name = strings.Repeat("a", 28)
	err := k8sClient.Create(context.Background(), tooLong)
	if err == nil {
		t.Fatal("a 28-char name must be rejected")
	}
	if !strings.Contains(err.Error(), "pod name budget") {
		t.Fatalf("rejection message must name the pod-name-budget arithmetic, got: %v", err)
	}
}

func TestStorageShrinkCELRejectsAnUpdate(t *testing.T) {
	ns := newNamespace(t)
	app := validAppFor(ns)
	app.Spec.Storage.Size = resource.MustParse("10Gi")
	mustCreate(t, app)

	// 10Gi -> 9Gi must be REJECTED. This is the lexicographic trap: "9Gi" >=
	// "10Gi" as a string compare, so a bare self.size >= oldSelf.size passes
	// this shrink.
	var shrink v1alpha1.PerUserApp
	mustGetObj(t, client.ObjectKeyFromObject(app), &shrink)
	shrinkBase := shrink.DeepCopy()
	shrink.Spec.Storage.Size = resource.MustParse("9Gi")
	if err := k8sClient.Patch(context.Background(), &shrink, client.MergeFrom(shrinkBase)); err == nil {
		t.Fatal("10Gi -> 9Gi must be rejected by the CEL transition rule")
	}

	// 10Gi -> 20Gi must be ACCEPTED: growth is the one mutation the
	// controller performs, and an over-strict or erroring rule would fire
	// StorageSpecDrift on every workspace at once.
	var grow v1alpha1.PerUserApp
	mustGetObj(t, client.ObjectKeyFromObject(app), &grow)
	growBase := grow.DeepCopy()
	grow.Spec.Storage.Size = resource.MustParse("20Gi")
	if err := k8sClient.Patch(context.Background(), &grow, client.MergeFrom(growBase)); err != nil {
		t.Fatalf("10Gi -> 20Gi must be accepted by the CEL transition rule: %v", err)
	}
}

// TestSelectorEgressPeerIsAcceptedByTheAPIServer: a podSelector (or
// namespaceSelector) workspaceEgress peer is valid -- NetworkPolicy egress
// is evaluated against the POST-DNAT destination, so a selector peer
// correctly resolves to its backing pods' real IPs. It is an ipBlock naming
// a Service's ClusterIP that silently fails instead (see
// NetworkSpec.WorkspaceEgress's doc comment in api/v1alpha1/peruserapp_types.go),
// which is why this CEL rule does not single out selector peers as unsafe.
// A peer with none of ipBlock/podSelector/namespaceSelector set, and a
// ports-only rule with `to` omitted entirely (allow-to-anywhere), must both
// still be rejected.
func TestSelectorEgressPeerIsAcceptedByTheAPIServer(t *testing.T) {
	ns := newNamespace(t)

	selectorPeer := validAppFor(ns)
	selectorPeer.Name = "cel-selector-peer"
	selectorPeer.Spec.Network.WorkspaceEgress = []networkingv1.NetworkPolicyEgressRule{{
		To: []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "x"}}}},
	}}
	if err := k8sClient.Create(context.Background(), selectorPeer); err != nil {
		t.Fatalf("a podSelector egress peer must be accepted: %v", err)
	}

	emptyPeer := validAppFor(ns)
	emptyPeer.Name = "cel-empty-peer"
	emptyPeer.Spec.Network.WorkspaceEgress = []networkingv1.NetworkPolicyEgressRule{{
		To: []networkingv1.NetworkPolicyPeer{{}},
	}}
	if err := k8sClient.Create(context.Background(), emptyPeer); err == nil {
		t.Fatal("a peer with none of ipBlock/podSelector/namespaceSelector must be rejected")
	}

	allowAnywhere := validAppFor(ns)
	allowAnywhere.Name = "cel-allow-anywhere"
	port := intstr.FromInt32(443)
	allowAnywhere.Spec.Network.WorkspaceEgress = []networkingv1.NetworkPolicyEgressRule{{
		Ports: []networkingv1.NetworkPolicyPort{{Port: &port}},
	}}
	if err := k8sClient.Create(context.Background(), allowAnywhere); err == nil {
		t.Fatal("a ports-only egress rule with 'to' omitted (allow-to-anywhere) must be rejected")
	}
}

func TestReapIntervalAboveIdleTimeoutIsRejected(t *testing.T) {
	ns := newNamespace(t)

	equal := validAppFor(ns)
	equal.Name = "cel-reap-equal"
	equal.Spec.Lifecycle.IdleTimeout = testfixtures.Dur("60s")
	equal.Spec.Lifecycle.ReapInterval = testfixtures.Dur("60s")
	if err := k8sClient.Create(context.Background(), equal); err == nil {
		t.Fatal("reapInterval == idleTimeout must be rejected")
	}

	greater := validAppFor(ns)
	greater.Name = "cel-reap-greater"
	greater.Spec.Lifecycle.IdleTimeout = testfixtures.Dur("60s")
	greater.Spec.Lifecycle.ReapInterval = testfixtures.Dur("90s")
	if err := k8sClient.Create(context.Background(), greater); err == nil {
		t.Fatal("reapInterval > idleTimeout must be rejected")
	}
}

func TestWorkspaceUserKeyIsImmutable(t *testing.T) {
	ns := newNamespace(t)
	app, ws := newFixtures(ns)
	mustCreate(t, app)
	mustCreate(t, ws)

	var current v1alpha1.Workspace
	mustGetObj(t, client.ObjectKeyFromObject(ws), &current)
	base := current.DeepCopy()
	current.Spec.UserKey = "u-0000000000000000"
	if err := k8sClient.Patch(context.Background(), &current, client.MergeFrom(base)); err == nil {
		t.Fatal("spec.userKey must be immutable after create")
	}
}

// TestReclaimCELRulesAreEnforcedByTheAPIServer covers the three cross-field
// rules guarding the one API in this operator that destroys user data. All
// three are CEL because none is expressible as a per-field bound: each
// compares two fields that live in different structs, and Go-side validation
// would let a bad CR sit admitted in etcd until a controller happened to read
// it -- by which time the reclaim loop is already running against it.
func TestReclaimCELRulesAreEnforcedByTheAPIServer(t *testing.T) {
	ns := newNamespace(t)

	sane := func() *v1alpha1.ReclaimSpec {
		return &v1alpha1.ReclaimSpec{
			Enabled:          true,
			TargetWorkspaces: 3,
			MinIdleAge:       metav1.Duration{Duration: 24 * time.Hour},
			Interval:         metav1.Duration{Duration: time.Hour},
		}
	}

	// The whole struct is optional: absence must remain admissible, since
	// that is what "no workspace is ever reclaimed" looks like on the wire.
	absent := validAppFor(ns)
	absent.Name = "reclaim-absent"
	if err := k8sClient.Create(context.Background(), absent); err != nil {
		t.Fatalf("an app with no spec.reclaim must be accepted: %v", err)
	}

	ok := validAppFor(ns)
	ok.Name = "reclaim-ok"
	ok.Spec.Limits.MaxWorkspaces = 10
	ok.Spec.Reclaim = sane()
	if err := k8sClient.Create(context.Background(), ok); err != nil {
		t.Fatalf("a well-formed reclaim spec must be accepted: %v", err)
	}

	for _, tc := range []struct {
		name    string
		mutate  func(*v1alpha1.PerUserApp)
		wantMsg string
	}{
		{
			// At equal values the first sweep only runs once the app is
			// already at its hard cap, i.e. once the router is already
			// answering 503 workspace_limit -- reclamation would start at
			// the outage it exists to prevent.
			name: "target equal to the hard cap",
			mutate: func(a *v1alpha1.PerUserApp) {
				a.Spec.Limits.MaxWorkspaces = 3
				a.Spec.Reclaim = sane()
			},
			wantMsg: "strictly below limits.maxWorkspaces",
		},
		{
			// A floor at or below idleTimeout deletes the files of a user
			// the Reaper had only just scaled down.
			name: "idle floor below the reap timeout",
			mutate: func(a *v1alpha1.PerUserApp) {
				a.Spec.Limits.MaxWorkspaces = 10
				a.Spec.Lifecycle.IdleTimeout = metav1.Duration{Duration: 2 * time.Hour}
				a.Spec.Reclaim = sane()
				a.Spec.Reclaim.MinIdleAge = metav1.Duration{Duration: time.Hour}
			},
			wantMsg: "must exceed lifecycle.idleTimeout",
		},
		{
			// With a sweep interval longer than the floor, a workspace can
			// cross minIdleAge and be reclaimed within a single tick of
			// having gone idle.
			name: "sweep interval longer than the idle floor",
			mutate: func(a *v1alpha1.PerUserApp) {
				a.Spec.Limits.MaxWorkspaces = 10
				a.Spec.Reclaim = sane()
				a.Spec.Reclaim.MinIdleAge = metav1.Duration{Duration: time.Hour}
				a.Spec.Reclaim.Interval = metav1.Duration{Duration: 2 * time.Hour}
			},
			wantMsg: "minIdleAge must exceed interval",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := validAppFor(ns)
			bad.Name = "reclaim-bad-" + strings.ReplaceAll(tc.name, " ", "-")
			tc.mutate(bad)
			err := k8sClient.Create(context.Background(), bad)
			if err == nil {
				t.Fatalf("%s must be rejected by CEL", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Fatalf("rejection must explain the failure mode (want %q), got: %v", tc.wantMsg, err)
			}
		})
	}
}

// TestSharedPathsCELRequiresAbsolutePaths: a sharedPaths entry is compared
// verbatim against the request path, so a relative one can never match and
// would leave the app quietly undiscoverable rather than visibly misconfigured.
// The API server is what has to catch that.
func TestSharedPathsCELRequiresAbsolutePaths(t *testing.T) {
	ns := newNamespace(t)

	ok := validAppFor(ns)
	ok.Name = "shared-ok"
	ok.Spec.Router.SharedPaths = []string{"/openapi.json", "/.well-known/schema"}
	if err := k8sClient.Create(context.Background(), ok); err != nil {
		t.Fatalf("absolute sharedPaths must be accepted: %v", err)
	}

	bad := validAppFor(ns)
	bad.Name = "shared-bad"
	bad.Spec.Router.SharedPaths = []string{"/openapi.json", "openapi.json"}
	err := k8sClient.Create(context.Background(), bad)
	if err == nil {
		t.Fatal("a relative sharedPaths entry must be rejected")
	}
	if !strings.Contains(err.Error(), "absolute path") {
		t.Fatalf("rejection must name the absolute-path requirement, got: %v", err)
	}
}

// TestSharedPathsIsOptional: the field was added after the CRD shipped, so
// every existing PerUserApp that omits it must still be accepted.
func TestSharedPathsIsOptional(t *testing.T) {
	ns := newNamespace(t)
	app := validAppFor(ns)
	if err := k8sClient.Create(context.Background(), app); err != nil {
		t.Fatalf("omitting sharedPaths must be accepted: %v", err)
	}
	var got v1alpha1.PerUserApp
	mustGetObj(t, client.ObjectKeyFromObject(app), &got)
	if len(got.Spec.Router.SharedPaths) != 0 {
		t.Fatalf("SharedPaths defaulted to %v, want empty", got.Spec.Router.SharedPaths)
	}
}
