package main

import (
	"context"
	"testing"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

func TestParseControllerFlagsRequiresWatchNamespaces(t *testing.T) {
	t.Setenv("RELATED_IMAGE_ROUTER", "example/router:v1")
	t.Setenv("POD_NAME", "controller-0")
	t.Setenv("POD_NAMESPACE", "puc-system")
	_, err := parseControllerFlags([]string{"--pod-cidr", "10.42.0.0/16", "--node-cidr", "10.0.0.0/24"})
	if err == nil {
		t.Fatal("missing --watch-namespaces must error")
	}
}

func TestParseControllerFlagsRequiresPodCIDRAndNodeCIDR(t *testing.T) {
	t.Setenv("RELATED_IMAGE_ROUTER", "example/router:v1")
	t.Setenv("POD_NAME", "controller-0")
	t.Setenv("POD_NAMESPACE", "puc-system")
	_, err := parseControllerFlags([]string{"--watch-namespaces", "ns1"})
	if err == nil {
		t.Fatal("missing --pod-cidr/--node-cidr must error")
	}
}

// TestParseControllerFlagsFailsFastWithoutRelatedImageRouter is the fail-fast
// property this task exists to add: the controller creates the router
// Deployment at runtime, so an unset image is not a startup nicety -- it is
// a Deployment that never becomes ready, diagnosed at 3am.
func TestParseControllerFlagsFailsFastWithoutRelatedImageRouter(t *testing.T) {
	t.Setenv("RELATED_IMAGE_ROUTER", "")
	t.Setenv("POD_NAME", "controller-0")
	t.Setenv("POD_NAMESPACE", "puc-system")
	_, err := parseControllerFlags([]string{"--watch-namespaces", "ns1", "--pod-cidr", "10.42.0.0/16", "--node-cidr", "10.0.0.0/24"})
	if err == nil {
		t.Fatal("unset RELATED_IMAGE_ROUTER must fail fast at startup")
	}
}

func TestParseControllerFlagsRequiresPodNameAndNamespace(t *testing.T) {
	t.Setenv("RELATED_IMAGE_ROUTER", "example/router:v1")
	t.Setenv("POD_NAME", "")
	t.Setenv("POD_NAMESPACE", "")
	_, err := parseControllerFlags([]string{"--watch-namespaces", "ns1", "--pod-cidr", "10.42.0.0/16", "--node-cidr", "10.0.0.0/24"})
	if err == nil {
		t.Fatal("missing POD_NAME/POD_NAMESPACE (downward API) must error")
	}
}

// TestParseControllerFlagsSplitsAndTrimsWatchNamespaces: --watch-namespaces
// is rendered from the same chart watchNamespaces list that renders the
// Roles and both ServiceMonitors (Task 12), so it must survive a comma-space
// join without any namespace silently gaining whitespace.
func TestParseControllerFlagsSplitsAndTrimsWatchNamespaces(t *testing.T) {
	t.Setenv("RELATED_IMAGE_ROUTER", "example/router:v1")
	t.Setenv("POD_NAME", "controller-0")
	t.Setenv("POD_NAMESPACE", "puc-system")
	cfg, err := parseControllerFlags([]string{
		"--watch-namespaces", "ns1, ns2 ,ns3",
		"--pod-cidr", "10.42.0.0/16",
		"--node-cidr", "10.0.0.0/24",
	})
	if err != nil {
		t.Fatalf("parseControllerFlags: %v", err)
	}
	want := []string{"ns1", "ns2", "ns3"}
	if len(cfg.WatchNamespaces) != len(want) {
		t.Fatalf("watch namespaces = %v, want %v", cfg.WatchNamespaces, want)
	}
	for i, ns := range want {
		if cfg.WatchNamespaces[i] != ns {
			t.Fatalf("watch namespaces = %v, want %v", cfg.WatchNamespaces, want)
		}
	}
}

func TestParseControllerFlagsDefaultsMetricsAddrAndThreadsCIDRsAndImage(t *testing.T) {
	t.Setenv("RELATED_IMAGE_ROUTER", "example/router:v1")
	t.Setenv("POD_NAME", "controller-0")
	t.Setenv("POD_NAMESPACE", "puc-system")
	cfg, err := parseControllerFlags([]string{
		"--watch-namespaces", "ns1",
		"--pod-cidr", "10.42.0.0/16",
		"--node-cidr", "10.0.0.0/24",
	})
	if err != nil {
		t.Fatalf("parseControllerFlags: %v", err)
	}
	if cfg.MetricsAddr != ":9090" {
		t.Fatalf("metrics addr = %q, want :9090 (v1alpha1.MetricsPort)", cfg.MetricsAddr)
	}
	if cfg.PodCIDR != "10.42.0.0/16" || cfg.NodeCIDR != "10.0.0.0/24" {
		t.Fatalf("CIDRs not threaded through: %+v", cfg)
	}
	if cfg.RouterImage != "example/router:v1" {
		t.Fatalf("router image = %q, want the RELATED_IMAGE_ROUTER value", cfg.RouterImage)
	}
	if cfg.PodName != "controller-0" || cfg.PodNamespace != "puc-system" {
		t.Fatalf("downward API values not threaded through: %+v", cfg)
	}
}

// TestLeaseIsSameHolderComparesByPodNamePrefix: controller-runtime's default
// leader-election identity is "<hostname>_<uuid>", and a Pod's hostname is
// its own name by default -- so "the previous holder is THIS pod" is a
// prefix match against podName+"_", never an exact string match (which
// would only match a re-acquisition within the same process, not the same
// pod after a container restart).
func TestLeaseIsSameHolderComparesByPodNamePrefix(t *testing.T) {
	if !leaseIsSameHolder("controller-0_abc-123-uuid", "controller-0") {
		t.Fatal("holder identity with the pod-name prefix must be treated as the same holder")
	}
	if leaseIsSameHolder("controller-1_abc-123-uuid", "controller-0") {
		t.Fatal("a different pod's holder identity must not be treated as the same holder")
	}
	if leaseIsSameHolder("controller-0-extra_abc-123-uuid", "controller-0") {
		t.Fatal("a holder identity for a DIFFERENT pod that merely shares a prefix (no trailing underscore boundary) must not match")
	}
}

// TestComputeLeaderlessZeroWhenNoPriorLeaseOrSameHolder locks the two zero
// cases: no prior Lease at all (first-ever leader; nothing to compute a gap
// against), and a re-acquisition by the same pod (never actually leaderless,
// even across a brief renewal gap).
func TestComputeLeaderlessZeroWhenNoPriorLeaseOrSameHolder(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 10, 0, 0, time.UTC)
	renew := now.Add(-3 * time.Minute)

	if got := computeLeaderless(now, leaseSnapshot{found: false}, "controller-0"); got != 0 {
		t.Fatalf("no prior lease: leaderless = %v, want 0", got)
	}
	if got := computeLeaderless(now, leaseSnapshot{found: true, holderIdentity: "controller-0_uuid-a", renewTime: renew}, "controller-0"); got != 0 {
		t.Fatalf("same-pod re-acquisition: leaderless = %v, want 0", got)
	}
}

// TestComputeLeaderlessMeasuresFromSnapshotWhenHolderDiffers is the
// arithmetic assertion Task 8's own test cannot make (it injects a duration
// directly into OnLeaseAcquired): a snapshot taken BEFORE contending, with a
// different previous holder, must yield exactly now-minus-snapshotRenewTime
// -- reading the Lease again at/after acquisition would see this process's
// own just-written renewTime instead, and the equivalent bug (Task 8) proves
// what that reads as: a few milliseconds no matter how long the fleet was
// actually leaderless.
func TestComputeLeaderlessMeasuresFromSnapshotWhenHolderDiffers(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 10, 0, 0, time.UTC)
	renew := now.Add(-7 * time.Minute)
	got := computeLeaderless(now, leaseSnapshot{found: true, holderIdentity: "controller-1_uuid-b", renewTime: renew}, "controller-0")
	if got != 7*time.Minute {
		t.Fatalf("leaderless = %v, want exactly 7m (now - snapshotted renewTime)", got)
	}
}

func newFakeClientsetForProbe(reactor clienttesting.ReactionFunc) *fake.Clientset {
	fc := fake.NewSimpleClientset()
	fc.PrependReactor("create", "selfsubjectaccessreviews", reactor)
	return fc
}

func selfSARReactor(allow func(res authorizationv1.ResourceAttributes) bool) clienttesting.ReactionFunc {
	return func(action clienttesting.Action) (bool, runtime.Object, error) {
		ca, ok := action.(clienttesting.CreateAction)
		if !ok {
			return false, nil, nil
		}
		review, ok := ca.GetObject().(*authorizationv1.SelfSubjectAccessReview)
		if !ok {
			return false, nil, nil
		}
		out := review.DeepCopy()
		if review.Spec.ResourceAttributes != nil {
			out.Status.Allowed = allow(*review.Spec.ResourceAttributes)
		}
		return true, out, nil
	}
}

// TestProbeNamespaceAllAllowedIsReady and its inverse lock the startup verb
// probe's aggregation: ready iff every check this controller actually needs
// is allowed, not merely the first one -- a probe that short-circuited on
// the first Allowed check would report ready with, say, no grant to create
// Deployments at all, and that gap is discovered per-user at 03:00 instead
// of at startup.
func TestProbeNamespaceAllAllowedIsReady(t *testing.T) {
	fc := newFakeClientsetForProbe(selfSARReactor(func(authorizationv1.ResourceAttributes) bool { return true }))
	ok, failed := probeNamespace(context.Background(), fc.AuthorizationV1(), "ns1")
	if !ok || len(failed) != 0 {
		t.Fatalf("probeNamespace = ok=%v failed=%v, want ok=true failed=[]", ok, failed)
	}
}

func TestProbeNamespaceOneDeniedIsNotReady(t *testing.T) {
	fc := newFakeClientsetForProbe(selfSARReactor(func(res authorizationv1.ResourceAttributes) bool {
		// Deny exactly the grant the router's identity create depends on;
		// everything else allowed.
		return res.Resource != "workspaces" || res.Verb != "create"
	}))
	ok, failed := probeNamespace(context.Background(), fc.AuthorizationV1(), "ns1")
	if ok || len(failed) == 0 {
		t.Fatalf("probeNamespace = ok=%v failed=%v, want ok=false with at least one failure", ok, failed)
	}
}
