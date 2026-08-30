package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
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

// namespacedRoleOnlyReactor simulates the exact shape Finding 1 named: a
// namespaced Role granting `verb` on storageclasses answers Allowed:true
// for a SelfSubjectAccessReview that carries a Namespace, regardless of
// hasClusterRoleGrant -- only a SAR with NO Namespace asks the question the
// real cluster-scoped client.Get() actually makes, and only that shape is
// gated on hasClusterRoleGrant. Every other (namespaced) resource is always
// allowed, so a test using this reactor isolates the storageclasses
// decision as the sole variable.
//
// It fails the test outright the moment probeNamespace issues a NAMESPACED
// SAR for storageclasses: that is the bug itself, not a value this reactor
// should silently score as denied.
func namespacedRoleOnlyReactor(t *testing.T, hasClusterRoleGrant bool) clienttesting.ReactionFunc {
	return func(action clienttesting.Action) (bool, runtime.Object, error) {
		ca, ok := action.(clienttesting.CreateAction)
		if !ok {
			return false, nil, nil
		}
		review, ok := ca.GetObject().(*authorizationv1.SelfSubjectAccessReview)
		if !ok || review.Spec.ResourceAttributes == nil {
			return false, nil, nil
		}
		attrs := review.Spec.ResourceAttributes
		out := review.DeepCopy()
		if attrs.Resource == "storageclasses" {
			if attrs.Namespace != "" {
				t.Fatalf("probeNamespace issued a NAMESPACED SelfSubjectAccessReview (namespace=%q) for cluster-scoped resource %q; the real client.Get() against a cluster-scoped object is never namespaced, so this SAR asks an easier question than the request ValidateStorageClass actually makes", attrs.Namespace, attrs.Resource)
			}
			out.Status.Allowed = hasClusterRoleGrant
			return true, out, nil
		}
		out.Status.Allowed = true
		return true, out, nil
	}
}

// TestProbeNamespaceUsesClusterScopeCorrectlyForStorageClasses is Finding
// 1's proof: constructed first against the namespaced-Role-only case (no
// ClusterRole at all), then against a proper ClusterRole grant.
func TestProbeNamespaceUsesClusterScopeCorrectlyForStorageClasses(t *testing.T) {
	t.Run("namespaced Role only: probe reports NOT ready", func(t *testing.T) {
		fc := newFakeClientsetForProbe(namespacedRoleOnlyReactor(t, false))
		ok, failed := probeNamespace(context.Background(), fc.AuthorizationV1(), "ns1")
		if ok {
			t.Fatalf("probe reported ready with no ClusterRole granting storageclasses; failed=%v", failed)
		}
		var namesStorageClasses bool
		for _, f := range failed {
			if strings.Contains(f, "storageclasses") {
				namesStorageClasses = true
			}
		}
		if !namesStorageClasses {
			t.Fatalf("failed list does not name storageclasses: %v", failed)
		}
	})

	t.Run("proper ClusterRole: probe reports ready", func(t *testing.T) {
		fc := newFakeClientsetForProbe(namespacedRoleOnlyReactor(t, true))
		ok, failed := probeNamespace(context.Background(), fc.AuthorizationV1(), "ns1")
		if !ok {
			t.Fatalf("probe reported not ready with a proper ClusterRole granting storageclasses: failed=%v", failed)
		}
	})
}

// rbacMarkerRe parses one +kubebuilder:rbac:groups=<g>,resources=<r1>;<r2>,verbs=<v1>;<v2>
// marker line. groups may be a bare word (apps.kettleofketchup) or the
// literal `""` for the core group.
var rbacMarkerRe = regexp.MustCompile(`\+kubebuilder:rbac:groups=([^,]*),resources=([^,]+),verbs=(\S+)`)

// markerChecksFromControllerFiles mechanically derives the expected
// (group, resource, verb) set from every +kubebuilder:rbac marker in
// internal/controller/*_controller.go, expanding the semicolon-joined
// resources and verbs lists on each marker line.
func markerChecksFromControllerFiles(t *testing.T) []namespaceCheck {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("..", "internal", "controller", "*_controller.go"))
	if err != nil {
		t.Fatalf("glob controller files: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no *_controller.go files found; this drift check has nothing to compare against")
	}
	var checks []namespaceCheck
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for _, line := range strings.Split(string(b), "\n") {
			m := rbacMarkerRe.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			group := strings.Trim(m[1], `"`)
			for _, res := range strings.Split(m[2], ";") {
				for _, verb := range strings.Split(m[3], ";") {
					checks = append(checks, namespaceCheck{group: group, resource: res, verb: verb})
				}
			}
		}
	}
	return checks
}

func namespaceCheckKey(c namespaceCheck) string {
	return fmt.Sprintf("%s|%s|%s", c.group, c.resource, c.verb)
}

// TestProbeNamespaceChecksMatchRBACMarkers is a drift guard in the same
// shape as internal/metrics/callsite_coverage_test.go: it mechanically
// derives the expected check set from the +kubebuilder:rbac markers on
// WorkspaceReconciler and PerUserAppReconciler and asserts set-equality
// (by group/resource/verb, ignoring clusterScoped) against
// controllerNamespaceChecks, so a reconciler that gains or loses an RBAC
// verb cannot silently drift from what the startup probe checks.
//
// This catches ONLY hand-maintenance drift between the markers and the
// check list. It does NOT catch the cluster-scope blind spot Finding 1
// fixed: a marker and a check can agree perfectly on group/resource/verb
// while the check still asks the SelfSubjectAccessReview the wrong scope
// question (namespaced vs. cluster-scoped). Those are two independent
// failure modes -- passing this test says nothing about whether
// clusterScoped is set correctly on any entry.
func TestProbeNamespaceChecksMatchRBACMarkers(t *testing.T) {
	derived := markerChecksFromControllerFiles(t)

	want := map[string]bool{}
	for _, c := range derived {
		want[namespaceCheckKey(c)] = true
	}
	got := map[string]bool{}
	for _, c := range controllerNamespaceChecks {
		got[namespaceCheckKey(c)] = true
	}

	var missing, extra []string
	for k := range want {
		if !got[k] {
			missing = append(missing, k)
		}
	}
	for k := range got {
		if !want[k] {
			extra = append(extra, k)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) > 0 {
		t.Errorf("controllerNamespaceChecks is missing entries an RBAC marker grants: %v", missing)
	}
	if len(extra) > 0 {
		t.Errorf("controllerNamespaceChecks probes a verb no RBAC marker grants: %v", extra)
	}
}
