package controller

import (
	"fmt"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/kettleofketchup/per-user-container-operator/api/v1alpha1"
	"github.com/kettleofketchup/per-user-container-operator/internal/identity"
	"github.com/kettleofketchup/per-user-container-operator/internal/testfixtures"
)

var (
	validApp       = testfixtures.ValidApp
	validWorkspace = testfixtures.ValidWorkspace
	// testfixtures.Ptr is generic; Go requires instantiation to bind it to a
	// variable (a bare alias, as used for the others here, does not compile).
	// The only call site in this file needs intstr.IntOrString.
	//
	// testfixtures.Dur has no call site in this file's test bodies (none of
	// them touch LifecycleSpec durations), so it is not aliased here — an
	// unused package-level var is a lint failure, not a harmless leftover.
	ptr = testfixtures.Ptr[intstr.IntOrString]
)

// ingressPodSelector finds the ingress rule's from[].podSelector across all
// rendered policies and fails the test if none is found.
func ingressPodSelector(t *testing.T, pols []*networkingv1.NetworkPolicy) *metav1.LabelSelector {
	t.Helper()
	for _, p := range pols {
		for _, r := range p.Spec.Ingress {
			for _, peer := range r.From {
				if peer.PodSelector != nil {
					return peer.PodSelector
				}
			}
		}
	}
	t.Fatal("no ingress rule carries a from[].podSelector")
	return nil
}

// admitsNodeCIDR reports whether any ingress rule across pols admits the
// given CIDR as an ipBlock peer.
func admitsNodeCIDR(t *testing.T, pols []*networkingv1.NetworkPolicy, cidr string) bool {
	t.Helper()
	for _, p := range pols {
		for _, r := range p.Spec.Ingress {
			for _, peer := range r.From {
				if peer.IPBlock != nil && peer.IPBlock.CIDR == cidr {
					return true
				}
			}
		}
	}
	return false
}

// nodeCIDRIngressPort returns the numeric port admitted on the ingress rule
// whose peer is an ipBlock matching cidr, failing the test if no such rule
// exists or it names no port. Unlike admitsNodeCIDR, this reads r.Ports: a
// resolver that reintroduces the port-0 bug (reading a named probe port's
// IntVal directly instead of resolving it) renders a rule for port 0, which
// admitsNodeCIDR alone cannot see since it only checks the CIDR peer.
func nodeCIDRIngressPort(t *testing.T, pols []*networkingv1.NetworkPolicy, cidr string) int32 {
	t.Helper()
	for _, p := range pols {
		for _, r := range p.Spec.Ingress {
			for _, peer := range r.From {
				if peer.IPBlock == nil || peer.IPBlock.CIDR != cidr {
					continue
				}
				if len(r.Ports) == 0 || r.Ports[0].Port == nil {
					t.Fatalf("ingress rule admitting CIDR %q names no port", cidr)
				}
				return r.Ports[0].Port.IntVal
			}
		}
	}
	t.Fatalf("no ingress rule admits CIDR %q", cidr)
	return 0
}

// mountPathOf returns the mount path of the named volume mount on c, failing
// the test if it is not mounted there.
func mountPathOf(t *testing.T, c corev1.Container, volumeName string) string {
	t.Helper()
	for _, m := range c.VolumeMounts {
		if m.Name == volumeName {
			return m.MountPath
		}
	}
	t.Fatalf("container %q does not mount volume %q", c.Name, volumeName)
	return ""
}

// volumeByName returns the named volume from the Deployment's pod spec,
// failing the test if it is absent.
func volumeByName(t *testing.T, d *appsv1.Deployment, name string) corev1.Volume {
	t.Helper()
	for _, v := range d.Spec.Template.Spec.Volumes {
		if v.Name == name {
			return v
		}
	}
	t.Fatalf("no volume named %q", name)
	return corev1.Volume{}
}

// Allowlist, not denylist. Three denial cases pass identically against a
// three-entry denylist and against the allowlist the spec requires, so the
// positive half is what makes this test meaningful: with a denylist, csi/nfs/
// iscsi/downwardAPI sail through and are the same reach-another-user's-files
// primitive the test was meant to close.
func TestValidateVolumeSourceAllowlist(t *testing.T) {
	allowed := []corev1.Volume{
		{Name: "a", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{}}},
		{Name: "b", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{}}},
		{Name: "c", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: "d", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{}}},
	}
	app := validApp()
	app.Spec.Workspace.Volumes = allowed
	if _, err := ValidateApp(app); err != nil {
		t.Fatalf("allowed sources rejected: %v", err)
	}
	for _, tc := range []struct {
		name string
		v    corev1.Volume
	}{
		{"hostPath", corev1.Volume{Name: "x", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/"}}}},
		{"foreign pvc", corev1.Volume{Name: "x", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "workspace-app-u-bob"}}}},
		{"sa token projection", corev1.Volume{Name: "x", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{Sources: []corev1.VolumeProjection{{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{}}}}}}},
		{"csi (not on the allowlist)", corev1.Volume{Name: "x", VolumeSource: corev1.VolumeSource{CSI: &corev1.CSIVolumeSource{Driver: "d"}}}},
		// A user volume reusing the operator's own PVC volume name is a
		// ConfigInvalid, not a rendered duplicate: the alternative surfaces
		// as an API-server "Duplicate value" on the Deployment, per user,
		// with nothing naming the operator's own volume as the other half.
		{"reserved volume name", corev1.Volume{Name: v1alpha1.PVCVolumeName, VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := validApp()
			app.Spec.Workspace.Volumes = []corev1.Volume{tc.v}
			if _, err := ValidateApp(app); err == nil {
				t.Fatalf("%s volume accepted; it reaches another user's files", tc.name)
			}
		})
	}
}

// The controller SA can create pods and read PVCs across served namespaces,
// which inside a workspace is `kubectl create pod` with claimName:
// workspace-app-u-<bob>. Naming it is rejected.
func TestValidateRejectsOperatorServiceAccountName(t *testing.T) {
	app := validApp()
	app.Spec.Workspace.ServiceAccountName = v1alpha1.OperatorServiceAccountName
	if _, err := ValidateApp(app); err == nil {
		t.Fatal("operator SA accepted as the workspace SA")
	}
}

// RenderWorkspaceDeployment always renders automountServiceAccountToken:
// false regardless of this field (render.go), so an explicit true is dead
// config the API server would otherwise accept silently. nil and explicit
// false both describe the actual rendered behavior and must stay valid.
func TestValidateRejectsAutomountServiceAccountTokenTrue(t *testing.T) {
	t.Run("explicit true rejected", func(t *testing.T) {
		app := validApp()
		app.Spec.Workspace.AutomountServiceAccountToken = testfixtures.Ptr(true)
		if _, err := ValidateApp(app); err == nil {
			t.Fatal("automountServiceAccountToken:true accepted; the value is always rendered false, so this is dead config the API server silently ignores")
		}
	})
	t.Run("explicit false stays valid", func(t *testing.T) {
		app := validApp()
		app.Spec.Workspace.AutomountServiceAccountToken = testfixtures.Ptr(false)
		if _, err := ValidateApp(app); err != nil {
			t.Fatalf("automountServiceAccountToken:false rejected: %v", err)
		}
	})
	t.Run("nil stays valid", func(t *testing.T) {
		app := validApp()
		app.Spec.Workspace.AutomountServiceAccountToken = nil
		if _, err := ValidateApp(app); err != nil {
			t.Fatalf("automountServiceAccountToken:nil rejected: %v", err)
		}
	})
}

func TestValidateRequiresFsGroupAndNumericUser(t *testing.T) {
	t.Run("positive control", func(t *testing.T) {
		if _, err := ValidateApp(validApp()); err != nil {
			t.Fatalf("valid app rejected: %v", err)
		}
	})
	t.Run("fsGroup nil with a rendered mount", func(t *testing.T) {
		app := validApp()
		app.Spec.Workspace.PodSecurityContext.FSGroup = nil
		if _, err := ValidateApp(app); err == nil {
			t.Fatal("missing fsGroup accepted; a fresh RBD volume is root-owned")
		}
	})
	t.Run("runAsNonRoot with no numeric runAsUser", func(t *testing.T) {
		app := validApp()
		app.Spec.Workspace.PodSecurityContext.RunAsUser = nil
		if _, err := ValidateApp(app); err == nil {
			t.Fatal("runAsNonRoot with a non-numeric image USER is a CreateContainerConfigError for every user; it must be a reconcile-time ConfigInvalid")
		}
	})
}

// Egress: a selector peer (podSelector or namespaceSelector) is accepted --
// NetworkPolicy egress is evaluated against the POST-DNAT destination, so a
// selector peer correctly resolves to its backing pods' real IPs. It is
// ipBlock naming a Service's ClusterIP that silently fails (kube-proxy has
// already rewritten the destination by the time policy is evaluated), which
// is exactly why this validation does NOT single out selector peers as the
// unsafe case; see NetworkSpec.WorkspaceEgress's doc comment
// (api/v1alpha1/peruserapp_types.go).
func TestValidateAcceptsSelectorEgressAndRejectsEmptyRouterIngress(t *testing.T) {
	t.Run("selector egress peer accepted", func(t *testing.T) {
		app := validApp()
		app.Spec.Network.WorkspaceEgress = []networkingv1.NetworkPolicyEgressRule{
			{To: []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "prometheus"}}}}},
		}
		if _, err := ValidateApp(app); err != nil {
			t.Fatalf("podSelector egress peer rejected: %v", err)
		}
	})
	t.Run("namespaceSelector egress peer accepted", func(t *testing.T) {
		app := validApp()
		app.Spec.Network.WorkspaceEgress = []networkingv1.NetworkPolicyEgressRule{
			{To: []networkingv1.NetworkPolicyPeer{{NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": "monitoring"}}}}},
		}
		if _, err := ValidateApp(app); err != nil {
			t.Fatalf("namespaceSelector egress peer rejected: %v", err)
		}
	})
	t.Run("peer with no ipBlock/podSelector/namespaceSelector rejected", func(t *testing.T) {
		app := validApp()
		app.Spec.Network.WorkspaceEgress = []networkingv1.NetworkPolicyEgressRule{
			{To: []networkingv1.NetworkPolicyPeer{{}}},
		}
		if _, err := ValidateApp(app); err == nil {
			t.Fatal("empty peer (no ipBlock, podSelector or namespaceSelector) accepted; it selects nothing meaningful and is not a peer any user meant to write")
		}
	})
	t.Run("egress rule with no to", func(t *testing.T) {
		app := validApp()
		app.Spec.Network.WorkspaceEgress = []networkingv1.NetworkPolicyEgressRule{
			{Ports: []networkingv1.NetworkPolicyPort{{Port: ptr(intstr.FromInt(443))}}},
		}
		if _, err := ValidateApp(app); err == nil {
			t.Fatal("ports-only egress rule accepted; an absent `to` is allow-to-anywhere, which reaches every other workspace Service and the router")
		}
	})
	t.Run("routerIngress default-allow", func(t *testing.T) {
		app := validApp()
		app.Spec.Network.RouterIngress = v1alpha1.RouterIngressSpec{FromTraefik: false}
		if _, err := ValidateApp(app); err == nil {
			t.Fatal("empty from with fromTraefik:false accepted; that is a silent default-allow")
		}
	})
	t.Run("callerAuth absent", func(t *testing.T) {
		app := validApp()
		app.Spec.CallerAuth = v1alpha1.SecretHeaderRef{}
		if _, err := ValidateApp(app); err == nil {
			t.Fatal("callerAuth is mandatory: without it anyone who can open a socket to the router becomes any user")
		}
	})
}

// TestValidateRejectsSelectAllWorkspaceEgressPeers: an empty podSelector,
// namespaceSelector or ipBlock 0.0.0.0/0 (or ::/0) all satisfy "peer sets
// one of ipBlock/podSelector/namespaceSelector" -- the CEL rule and the
// check just above both admit them -- but each one silently reaches every
// other user's workspace and the router, defeating the exact isolation
// property this operator exists for. This gate has to live here, not in
// CEL: the CEL cost estimator already prices the nested
// self.workspaceEgress.all(r, r.to.all(...)) at its budget limit (see
// NetworkSpec's doc comment, api/v1alpha1/peruserapp_types.go), and any
// additional condition risks an uninstallable CRD.
//
// The two "broad but deliberately targeted" shapes below are legitimate and
// must stay accepted: an empty podSelector paired with a NON-empty
// namespaceSelector ("every pod in these namespaces") and a NON-empty
// podSelector paired with an empty namespaceSelector ("these labelled pods
// in every namespace"). Rejecting either would be overreach against a
// normal way to allow egress to a whole namespace or a labelled workload
// class across namespaces.
func TestValidateRejectsSelectAllWorkspaceEgressPeers(t *testing.T) {
	t.Run("empty podSelector alone reaches every pod in the workspace's own namespace", func(t *testing.T) {
		app := validApp()
		app.Spec.Network.WorkspaceEgress = []networkingv1.NetworkPolicyEgressRule{
			{To: []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{}}}},
		}
		if _, err := ValidateApp(app); err == nil {
			t.Fatal("empty podSelector with no namespaceSelector accepted; in an egress peer this is every pod in the workspace's own namespace -- every other user's workspace and the router")
		}
	})
	t.Run("empty namespaceSelector with no podSelector reaches every pod in every namespace", func(t *testing.T) {
		app := validApp()
		app.Spec.Network.WorkspaceEgress = []networkingv1.NetworkPolicyEgressRule{
			{To: []networkingv1.NetworkPolicyPeer{{NamespaceSelector: &metav1.LabelSelector{}}}},
		}
		if _, err := ValidateApp(app); err == nil {
			t.Fatal("empty namespaceSelector with no podSelector accepted; this is every pod in every namespace in the cluster")
		}
	})
	t.Run("empty namespaceSelector with empty podSelector reaches every pod in every namespace", func(t *testing.T) {
		app := validApp()
		app.Spec.Network.WorkspaceEgress = []networkingv1.NetworkPolicyEgressRule{
			{To: []networkingv1.NetworkPolicyPeer{{NamespaceSelector: &metav1.LabelSelector{}, PodSelector: &metav1.LabelSelector{}}}},
		}
		if _, err := ValidateApp(app); err == nil {
			t.Fatal("empty namespaceSelector with an also-empty podSelector accepted; this is every pod in every namespace in the cluster")
		}
	})
	t.Run("ipBlock 0.0.0.0/0 reaches everything", func(t *testing.T) {
		app := validApp()
		app.Spec.Network.WorkspaceEgress = []networkingv1.NetworkPolicyEgressRule{
			{To: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: "0.0.0.0/0"}}}},
		}
		if _, err := ValidateApp(app); err == nil {
			t.Fatal("ipBlock 0.0.0.0/0 accepted; this reaches every other workspace and the router")
		}
	})
	t.Run("ipBlock ::/0 reaches everything", func(t *testing.T) {
		app := validApp()
		app.Spec.Network.WorkspaceEgress = []networkingv1.NetworkPolicyEgressRule{
			{To: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: "::/0"}}}},
		}
		if _, err := ValidateApp(app); err == nil {
			t.Fatal("ipBlock ::/0 accepted; this reaches every other workspace and the router")
		}
	})
	t.Run("ipBlock 0.0.0.0/0 with an except is still rejected", func(t *testing.T) {
		app := validApp()
		app.Spec.Network.WorkspaceEgress = []networkingv1.NetworkPolicyEgressRule{
			{To: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: "0.0.0.0/0", Except: []string{"10.244.0.0/16"}}}}},
		}
		if _, err := ValidateApp(app); err == nil {
			t.Fatal("ipBlock 0.0.0.0/0 with an except accepted; ValidateApp has no access to the actual pod CIDR to confirm the except excludes it, so an unverifiable except must not rescue this")
		}
	})
	t.Run("empty podSelector with a non-empty namespaceSelector is legitimate and accepted", func(t *testing.T) {
		app := validApp()
		app.Spec.Network.WorkspaceEgress = []networkingv1.NetworkPolicyEgressRule{
			{To: []networkingv1.NetworkPolicyPeer{{
				PodSelector:       &metav1.LabelSelector{},
				NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": "monitoring"}},
			}}},
		}
		if _, err := ValidateApp(app); err != nil {
			t.Fatalf("empty podSelector + non-empty namespaceSelector rejected: %v -- this is a normal way to allow egress to every pod in a specific namespace and must stay accepted", err)
		}
	})
	t.Run("non-empty podSelector with an empty namespaceSelector is legitimate and accepted", func(t *testing.T) {
		app := validApp()
		app.Spec.Network.WorkspaceEgress = []networkingv1.NetworkPolicyEgressRule{
			{To: []networkingv1.NetworkPolicyPeer{{
				PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": "prometheus"}},
				NamespaceSelector: &metav1.LabelSelector{},
			}}},
		}
		if _, err := ValidateApp(app); err != nil {
			t.Fatalf("non-empty podSelector + empty namespaceSelector rejected: %v -- this is a normal way to allow egress to a labelled workload class across every namespace and must stay accepted", err)
		}
	})
}

// Warnings, not errors: empty egress is legal (spec 137) and a nested volatile
// mount is deliberate in workspace-app (spec 670-675).
func TestValidateWarnsOnEmptyEgressAndNestedVolatileMount(t *testing.T) {
	app := validApp()
	app.Spec.Network.WorkspaceEgress = nil
	w, err := ValidateApp(app)
	if err != nil || len(w) == 0 {
		t.Fatal("empty workspaceEgress must warn, not fail")
	}
	app = validApp()
	app.Spec.Workspace.Volumes = []corev1.Volume{{Name: "cfg", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory}}}}
	app.Spec.Workspace.VolumeMounts = []corev1.VolumeMount{{Name: "cfg", MountPath: "/home/user/.cfg"}}
	w, err = ValidateApp(app)
	if err != nil {
		t.Fatalf("nested volatile mount must warn, not fail: %v", err)
	}
	if len(w) == 0 || !strings.Contains(w[0], "/home/user/.cfg") || !strings.Contains(w[0], "/home/user") {
		t.Fatal("warning must name BOTH mounts: a file left there looks like workspace and vanishes on the next reap")
	}
}

func TestPVCHasNoOwnerRefAndCarriesKeepPolicies(t *testing.T) {
	pvc := RenderWorkspacePVC(validApp(), validWorkspace())
	if len(pvc.OwnerReferences) != 0 {
		t.Fatal("PVC has an ownerReference; a cascade would destroy user data")
	}
	if pvc.Annotations["argocd.argoproj.io/sync-options"] != "Prune=false" {
		t.Fatal("PVC missing Prune=false")
	}
	if pvc.Annotations["helm.sh/resource-policy"] != "keep" {
		t.Fatal("PVC missing resource-policy: keep")
	}
	if pvc.Spec.AccessModes[0] != corev1.ReadWriteOncePod {
		t.Fatal("PVC must be ReadWriteOncePod; RWO is per-node and a rollout double-attaches")
	}
	if pvc.Labels[v1alpha1.LabelUserKey] == "" || pvc.Annotations[v1alpha1.AnnUserDisplay] == "" {
		t.Fatal("PVC must carry user-key label and user-display annotation so an orphaned volume can be matched back to a human")
	}
}

// The two app-agnostic labels alone make the policy per-namespace while the
// guarantee is per-user: a second app's router would match every existing
// workspace, and callerAuth does not save that since it holds a valid bearer
// for its own app.
func TestWorkspaceIngressSelectsAllThreeRouterLabels(t *testing.T) {
	pols := RenderWorkspaceNetworkPolicies(validApp(), validWorkspace(), "10.42.0.0/16", "10.0.0.0/24")
	sel := ingressPodSelector(t, pols) // helper: the ingress rule's from[].podSelector
	for k, want := range RouterPodLabels("app") {
		if sel.MatchLabels[k] != want {
			t.Fatalf("router podSelector missing %s=%s", k, want)
		}
	}
	if sel.MatchLabels[v1alpha1.LabelApp] != "app" {
		t.Fatal("policy does not pin puc.kettleofketchup/app; a second app's router would match every workspace here")
	}
	// A router pod for a different app must not satisfy this selector.
	// Direction matters: Set(A).AsSelector().Matches(B) is true iff A is a
	// SUBSET of B, so the reversed form asks whether another app's router
	// labels are a subset of the policy's selector — which is false for every
	// input, including the bug being hunted, making the line decorative.
	if labels.Set(sel.MatchLabels).AsSelector().Matches(labels.Set(RouterPodLabels("other"))) {
		t.Fatal("another app's router pod satisfies this workspace's ingress policy")
	}
	// Kubelet probes must be admitted or every cold start dies at startupTimeout.
	if !admitsNodeCIDR(t, pols, "10.0.0.0/24") {
		t.Fatal("node CIDR not admitted on the probe port")
	}
}

// admitsNodeCIDR only checks the CIDR peer, not the port the rule admits —
// a resolver that renders a rule for port 0 (the port-0 bug resolveProbePort
// exists to prevent) would still pass that check. This test reads the port
// value directly, with a named readinessProbe port that only resolves
// correctly if it is looked up against the declared container port rather
// than read as a raw IntOrString.
func TestWorkspaceIngressAdmitsResolvedProbePort(t *testing.T) {
	app := validApp()
	app.Spec.Workspace.Port = 9000
	app.Spec.Workspace.ReadinessProbe = &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Port: intstr.FromString("http")}},
	}
	pols := RenderWorkspaceNetworkPolicies(app, validWorkspace(), "10.42.0.0/16", "10.0.0.0/24")
	if got := nodeCIDRIngressPort(t, pols, "10.0.0.0/24"); got != 9000 {
		t.Fatalf("node CIDR ingress port = %d, want 9000 (resolved via the declared %q container port); a naive raw read of a named IntOrString yields 0 and blocks every kubelet probe", got, "http")
	}
}

// resolveProbePort is exercised only indirectly by the network-policy tests
// above (which never varied the probe declaration), so its branches are
// tested directly here: absent probe, a named port that resolves, a named
// port that does not resolve, and a numeric literal.
func TestResolveProbePort(t *testing.T) {
	t.Run("no probe falls back to workspace port", func(t *testing.T) {
		app := validApp()
		app.Spec.Workspace.Port = 9000
		app.Spec.Workspace.ReadinessProbe = nil
		if got := resolveProbePort(app); got != 9000 {
			t.Fatalf("resolveProbePort = %d, want 9000", got)
		}
	})
	t.Run("named port resolves against the declared container port", func(t *testing.T) {
		app := validApp()
		app.Spec.Workspace.Port = 9000
		app.Spec.Workspace.ReadinessProbe = &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Port: intstr.FromString("http")}},
		}
		if got := resolveProbePort(app); got != 9000 {
			t.Fatalf(`resolveProbePort = %d, want 9000 (resolved via the declared "http" container port)`, got)
		}
	})
	t.Run("unknown port name falls back to workspace port", func(t *testing.T) {
		app := validApp()
		app.Spec.Workspace.Port = 9000
		app.Spec.Workspace.ReadinessProbe = &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Port: intstr.FromString("metrics")}},
		}
		if got := resolveProbePort(app); got != 9000 {
			t.Fatalf("resolveProbePort = %d, want fallback 9000", got)
		}
	})
	t.Run("numeric literal port used verbatim", func(t *testing.T) {
		app := validApp()
		app.Spec.Workspace.Port = 9000
		app.Spec.Workspace.ReadinessProbe = &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Port: intstr.FromInt(7777)}},
		}
		if got := resolveProbePort(app); got != 7777 {
			t.Fatalf("resolveProbePort = %d, want the literal numeric port 7777", got)
		}
	})
}

// RenderRouterNetworkPolicy had zero coverage: a divergence here is
// invisible to every unit suite and only surfaces at Task 13 as a Service
// with no reachable endpoint. Ports come from the v1alpha1 constants, not
// re-typed, so a drift between this render and Task 11's Deployment/Service
// would be caught here too.
func TestRouterNetworkPolicyAdmitsDeclaredIngressAndScopesEgress(t *testing.T) {
	app := validApp()
	pol := RenderRouterNetworkPolicy(app, "10.42.0.0/16", "10.0.0.0/24")

	var admitsDeclaredPeer, admitsPodCIDRMetrics bool
	for _, r := range pol.Spec.Ingress {
		for _, peer := range r.From {
			if peer.PodSelector != nil && peer.PodSelector.MatchLabels["app.kubernetes.io/name"] == "caller" {
				for _, p := range r.Ports {
					if p.Port != nil && p.Port.IntVal == v1alpha1.RouterPort {
						admitsDeclaredPeer = true
					}
				}
			}
			if peer.IPBlock != nil && peer.IPBlock.CIDR == "10.42.0.0/16" {
				for _, p := range r.Ports {
					if p.Port != nil && p.Port.IntVal == v1alpha1.MetricsPort {
						admitsPodCIDRMetrics = true
					}
				}
			}
		}
	}
	if !admitsDeclaredPeer {
		t.Fatal("router ingress does not admit the declared routerIngress.from peer on RouterPort")
	}
	if !admitsPodCIDRMetrics {
		t.Fatal("router ingress does not admit the pod CIDR on MetricsPort; Task 13's Prometheus assertion has no data without this")
	}

	var udp53, tcp53, admitsNode, admitsPod bool
	for _, r := range pol.Spec.Egress {
		for _, port := range r.Ports {
			if port.Port == nil || port.Port.IntValue() != 53 {
				continue
			}
			if port.Protocol != nil && *port.Protocol == corev1.ProtocolUDP {
				udp53 = true
			}
			if port.Protocol != nil && *port.Protocol == corev1.ProtocolTCP {
				tcp53 = true
			}
		}
		for _, peer := range r.To {
			if peer.IPBlock == nil {
				continue
			}
			if peer.IPBlock.CIDR == "10.0.0.0/24" {
				admitsNode = true
			}
			if peer.IPBlock.CIDR == "10.42.0.0/16" {
				admitsPod = true
			}
		}
	}
	if !udp53 || !tcp53 {
		t.Fatalf("router egress missing DNS rule (udp53=%v tcp53=%v)", udp53, tcp53)
	}
	if !admitsNode {
		t.Fatal("router egress does not admit the node CIDR; the router cannot reach the apiserver and every Workspace create/status patch fails")
	}
	if !admitsPod {
		t.Fatal("router egress does not admit the pod CIDR; the router cannot reach any workspace Service")
	}
}

func TestWorkspaceDeploymentUsesRecreateStrategy(t *testing.T) {
	d := RenderWorkspaceDeployment(validApp(), validWorkspace())
	if d.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType {
		t.Fatal("RollingUpdate would have RWOP reject the second pod; a single-user interactive session has no rolling update")
	}
}

// The claim is mounted at stagingMountPath in the seeder, never at mountPath —
// mounting at mountPath shadows the copy source, so cp reads an empty directory
// and every user gets an empty workspace with the baked corpus silently gone.
func TestSeederMountsClaimAtStagingPathAndRunsFirst(t *testing.T) {
	app := validApp()
	app.Spec.Storage.Seed = &v1alpha1.SeedSpec{StagingMountPath: "/mnt/ws", From: "/workspace/corpus"}
	app.Spec.Workspace.InitContainers = []corev1.Container{{Name: "user-init", Image: "x"}}
	d := RenderWorkspaceDeployment(app, validWorkspace())
	// Ordering alone is not enough. A renderer that DROPS
	// spec.workspace.initContainers and emits only the seeder satisfies every
	// other assertion in this test — InitContainers[0] is the seeder, so the
	// name, staging-mount, image, command and pull-policy checks all pass.
	// A consumer's own initContainers are the only thing that prepares state
	// the workload needs before it starts, and no other coverage in this suite
	// reads InitContainers past index 0 — so a renderer that dropped them
	// would fail nowhere else.
	if len(d.Spec.Template.Spec.InitContainers) != 2 {
		t.Fatalf("InitContainers = %d, want 2; the user's initContainers were dropped", len(d.Spec.Template.Spec.InitContainers))
	}
	if d.Spec.Template.Spec.InitContainers[0].Name == "user-init" {
		t.Fatal("seeder must run before user initContainers")
	}
	if d.Spec.Template.Spec.InitContainers[1].Name != "user-init" {
		t.Fatalf("InitContainers[1] = %q, want the user's own", d.Spec.Template.Spec.InitContainers[1].Name)
	}
	// "puc-workspace", not "workspace" — Step 3 item 1 reserves the prefixed
	// name because both consumer charts already declare a volume literally
	// called `workspace`.
	if mountPathOf(t, d.Spec.Template.Spec.InitContainers[0], v1alpha1.PVCVolumeName) != "/mnt/ws" {
		t.Fatal("seeder mounts the claim at mountPath; cp would read an empty directory")
	}
	// The corpus being copied is baked into the WORKSPACE image. A generic
	// utility image has nothing at seed.from, so cp succeeds over an empty
	// directory and every user gets an empty workspace — the same silent
	// outcome as shadowing the source, reached a different way.
	if d.Spec.Template.Spec.InitContainers[0].Image != app.Spec.Workspace.Image {
		t.Fatal("seeder must run the workspace image; seed.from only exists inside it")
	}
	// The renderer passes seed.from through verbatim: `cp -an <from>
	// <staging>/`, appending nothing. Whether the source arrives under its own
	// directory name or as loose contents is then the CR author's choice, made
	// by writing a trailing "/." or not (examples/workspace-app.yaml writes
	// one, to lay a home skeleton at the volume root). A renderer that
	// appended "/." itself would take that choice away and flatten every
	// consumer's corpus to the root permanently.
	if !strings.Contains(strings.Join(d.Spec.Template.Spec.InitContainers[0].Command, " "), "cp -an '/workspace/corpus' '/mnt/ws/'") {
		t.Fatal("seeder command must pass seed.from through verbatim; appending /. takes the layout choice away from the CR and flattens the corpus to the volume root permanently")
	}
	// A seed.from ending in "/." copies into the staging directory ITSELF, so
	// a single `cp -a` would stamp ownership and timestamps onto the volume's
	// mount root. fsGroup leaves that root owned by root:<fsGroup> -- group
	// writable, but not chownable by the workspace user -- so cp exits 1 and
	// the init container crash-loops before the workspace ever starts. The
	// entries have to be copied one at a time so the mount root is never
	// itself a copy target.
	app.Spec.Storage.Seed = &v1alpha1.SeedSpec{StagingMountPath: "/mnt/ws", From: "/home/skel/."}
	d = RenderWorkspaceDeployment(app, validWorkspace())
	seedCmd := strings.Join(d.Spec.Template.Spec.InitContainers[0].Command, " ")
	if strings.Contains(seedCmd, "cp -an '/home/skel/.' '/mnt/ws/'") {
		t.Fatal("a trailing-/. seed.from must not be copied with a single cp: -a stamps the mount root, which fsGroup leaves unchownable, and the seeder crash-loops")
	}
	if !strings.Contains(seedCmd, `cp -an "$e" '/mnt/ws/'`) {
		t.Fatal("a trailing-/. seed.from must copy entries individually so the mount root is never a copy target")
	}
	if !strings.Contains(seedCmd, "cd '/home/skel/.'") {
		t.Fatal("the per-entry seeder must cd to seed.from so the entries it globs are the corpus, not the init container's cwd")
	}
	// Airgap: the overlay patches imagePullPolicy to Never, and it has to
	// reach the seeder too. Left at the default, an airgapped node sits in
	// ImagePullBackOff behind DNS timeouts in the init container instead of
	// failing fast as ErrImageNeverPull (spec 580-586).
	app.Spec.Workspace.ImagePullPolicy = corev1.PullNever
	d = RenderWorkspaceDeployment(app, validWorkspace())
	if d.Spec.Template.Spec.Containers[0].ImagePullPolicy != corev1.PullNever ||
		d.Spec.Template.Spec.InitContainers[0].ImagePullPolicy != corev1.PullNever {
		t.Fatal("imagePullPolicy must reach BOTH the workspace container and the seeder")
	}
	// "No seed" must not become "no init containers".
	app = validApp()
	app.Spec.Workspace.InitContainers = []corev1.Container{{Name: "user-init", Image: "x"}}
	d = RenderWorkspaceDeployment(app, validWorkspace()) // Storage.Seed is nil
	if len(d.Spec.Template.Spec.InitContainers) != 1 || d.Spec.Template.Spec.InitContainers[0].Name != "user-init" {
		t.Fatal("with no seed the user's initContainers must render verbatim")
	}
}

// The workspace egress policy has no other test. Step 3 item 4 makes it
// default-deny + DNS + the declared peers, but every assertion that
// looks like it covers this is about ingress:
// TestWorkspaceIngressSelectsAllThreeRouterLabels reads the ingress rule and
// the node-CIDR admission, and TestValidateAcceptsSelectorEgressAndRejectsEmptyRouterIngress
// exercises ValidateApp, not the renderer. A renderer that omits
// policyTypes: Egress, or forgets the DNS rule, or drops
// network.workspaceEgress on the floor, passes every other unit test here —
// and a missing DNS rule lands as workspace-app's LLM endpoint unreachable with
// every first chat hanging at phase Ready (spec 322-327).
func TestWorkspaceEgressIsDefaultDenyWithDNSAndDeclaredPeers(t *testing.T) {
	check := func(t *testing.T, app *v1alpha1.PerUserApp, wantDeclared bool) {
		pols := RenderWorkspaceNetworkPolicies(app, validWorkspace(), "10.42.0.0/16", "10.0.0.0/24")
		var egress *networkingv1.NetworkPolicy
		for _, p := range pols {
			for _, pt := range p.Spec.PolicyTypes {
				if pt == networkingv1.PolicyTypeEgress {
					egress = p
				}
			}
		}
		if egress == nil {
			t.Fatal("no policy carries policyTypes: Egress; the workspace has unrestricted egress")
		}
		var udp53, tcp53, declared bool
		for _, r := range egress.Spec.Egress {
			for _, port := range r.Ports {
				if port.Port == nil || port.Port.IntValue() != 53 {
					continue
				}
				if port.Protocol != nil && *port.Protocol == corev1.ProtocolUDP {
					udp53 = true
				}
				if port.Protocol != nil && *port.Protocol == corev1.ProtocolTCP {
					tcp53 = true
				}
			}
			for _, peer := range r.To {
				if peer.IPBlock != nil && peer.IPBlock.CIDR == "10.0.0.0/8" {
					declared = true
				}
			}
		}
		if !udp53 || !tcp53 {
			t.Fatalf("DNS rule missing (udp53=%v tcp53=%v); workspace-app's LLM endpoint is unreachable and every first chat hangs at phase Ready", udp53, tcp53)
		}
		if declared != wantDeclared {
			t.Fatalf("declared 10.0.0.0/8 ipBlock peer present = %v, want %v", declared, wantDeclared)
		}
	}
	// Declared peers reach the rendered policy verbatim.
	check(t, validApp(), true)
	// And with none declared the policy is still rendered default-deny + DNS,
	// not skipped entirely.
	nilEgress := validApp()
	nilEgress.Spec.Network.WorkspaceEgress = nil
	check(t, nilEgress, false)
}

// The 27 in the name bound is arithmetic Task 3 does on ChildName alone, and
// Task 6's CEL test only asserts the API server accepts 27 and rejects 28.
// Neither exercises the renderers at the boundary — Task 5's fixtures use a
// three-character app name throughout — so the arithmetic is never checked
// against the objects it was derived for.
func TestRenderedNamesValidateAtTheNameBound(t *testing.T) {
	app := validApp()
	app.Name = "a123456789012345678901234567"[:27]
	ws := validWorkspace()
	ws.Spec.UserKey = identity.UserKey("ns", app.Name, "alice")
	ws.Name = identity.ChildName(app.Name, ws.Spec.UserKey)
	d := RenderWorkspaceDeployment(app, ws)
	s := RenderWorkspaceService(app, ws)
	if errs := validation.IsDNS1123Subdomain(d.Name); len(errs) != 0 {
		t.Fatalf("Deployment name %q invalid: %v", d.Name, errs)
	}
	if errs := validation.IsDNS1035Label(s.Name); len(errs) != 0 {
		t.Fatalf("Service name %q invalid: %v", s.Name, errs)
	}
	// The pod suffix needs 17 characters: -<pod-template-hash:10>-<random:5>.
	if len(d.Name)+17 > 63 {
		t.Fatalf("Deployment name %d chars leaves no room for the pod suffix; no pod is created for any user of this app", len(d.Name))
	}
}

// tmpfs pages count against the pod memory limit.
func TestMemoryEmptyDirGetsDefaultSizeLimit(t *testing.T) {
	app := validApp()
	app.Spec.Workspace.Volumes = []corev1.Volume{{Name: "m", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory}}}}
	d := RenderWorkspaceDeployment(app, validWorkspace())
	if volumeByName(t, d, "m").EmptyDir.SizeLimit.String() != "16Mi" {
		t.Fatal("memory-medium emptyDir must default to sizeLimit 16Mi")
	}
}

// --- Task 11: router Deployment/Service/identity rendering ---

const testRouterImage = "example.registry/router:v1"

// routerArgs parses a rendered router container's Args into a flag->value
// map, skipping the leading "router" subcommand token and failing the test
// on anything that is not exactly one --flag=value.
func routerArgs(t *testing.T, args []string) map[string]string {
	t.Helper()
	if len(args) == 0 || args[0] != "router" {
		t.Fatalf("router container args must start with the %q subcommand, got %v", "router", args)
	}
	out := map[string]string{}
	for _, a := range args[1:] {
		trimmed := strings.TrimPrefix(a, "--")
		if trimmed == a {
			t.Fatalf("arg %q is not a --flag=value", a)
		}
		parts := strings.SplitN(trimmed, "=", 2)
		if len(parts) != 2 {
			t.Fatalf("arg %q is not --flag=value", a)
		}
		if _, dup := out[parts[0]]; dup {
			t.Fatalf("flag %q rendered more than once", parts[0])
		}
		out[parts[0]] = parts[1]
	}
	return out
}

func routerContainer(t *testing.T, d *appsv1.Deployment) corev1.Container {
	t.Helper()
	for _, c := range d.Spec.Template.Spec.Containers {
		if c.Name == "router" {
			return c
		}
	}
	t.Fatal(`router Deployment has no container named "router"`)
	return corev1.Container{}
}

func volumeMountByName(t *testing.T, c corev1.Container, name string) corev1.VolumeMount {
	t.Helper()
	for _, m := range c.VolumeMounts {
		if m.Name == name {
			return m
		}
	}
	t.Fatalf("container %q has no volumeMount named %q", c.Name, name)
	return corev1.VolumeMount{}
}

func podVolumeByName(t *testing.T, d *appsv1.Deployment, name string) corev1.Volume {
	t.Helper()
	for _, v := range d.Spec.Template.Spec.Volumes {
		if v.Name == name {
			return v
		}
	}
	t.Fatalf("pod spec has no volume named %q", name)
	return corev1.Volume{}
}

// TestRouterDeploymentRendersExactlyTheTask10FlagList locks the Deployment's
// Args to task-10-brief.md's startup contract verbatim: any drift here is
// invisible to Task 10's own httptest-based suite (it never inspects how the
// binary was invoked) and only surfaces as a router that fails to start or
// silently ignores a spec field, discovered at Task 13.
func TestRouterDeploymentRendersExactlyTheTask10FlagList(t *testing.T) {
	app := validApp()
	d, err := RenderRouterDeployment(app, testRouterImage)
	if err != nil {
		t.Fatalf("RenderRouterDeployment: %v", err)
	}
	got := routerArgs(t, routerContainer(t, d).Args)

	want := map[string]string{
		"app":                     app.Name,
		"namespace":               app.Namespace,
		"identity-header":         app.Spec.Identity.Header,
		"identity-max-length":     fmt.Sprintf("%d", app.Spec.Identity.MaxLength),
		"caller-auth-header":      app.Spec.CallerAuth.Header,
		"caller-auth-scheme":      app.Spec.CallerAuth.Scheme,
		"caller-auth-secret-file": "/etc/puc/caller-auth/value",
		"workspace-port":          fmt.Sprintf("%d", app.Spec.Workspace.Port),
		"cold-start-hold":         (time.Duration(app.Spec.Router.ColdStartHoldSeconds) * time.Second).String(),
		"connection-heartbeat":    app.Spec.Lifecycle.ConnectionHeartbeatInterval.Duration.String(),
		"max-workspaces":          fmt.Sprintf("%d", app.Spec.Limits.MaxWorkspaces),
		"listen-addr":             fmt.Sprintf(":%d", v1alpha1.RouterPort),
		"metrics-addr":            fmt.Sprintf(":%d", v1alpha1.MetricsPort),
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("flag %q = %q, want %q (all flags: %v)", k, got[k], v, got)
		}
	}
	for _, k := range []string{"upstream-auth-header", "upstream-auth-scheme", "upstream-auth-secret-file"} {
		if v, ok := got[k]; ok {
			t.Fatalf("flag %q=%q rendered with spec.workspace.upstreamAuth unset", k, v)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("got %d flags %v, want exactly %d", len(got), got, len(want))
	}
}

func TestRouterDeploymentOmitsCallerAuthSchemeWhenUnset(t *testing.T) {
	app := validApp()
	app.Spec.CallerAuth.Scheme = ""
	d, err := RenderRouterDeployment(app, testRouterImage)
	if err != nil {
		t.Fatalf("RenderRouterDeployment: %v", err)
	}
	got := routerArgs(t, routerContainer(t, d).Args)
	if v, ok := got["caller-auth-scheme"]; ok {
		t.Fatalf("caller-auth-scheme=%q rendered with an empty spec.callerAuth.scheme", v)
	}
}

// TestRouterDeploymentSetsPodNameFromDownwardAPI: POD_NAME is in Task 10's
// startup contract but is not a flag, so the flag-list assertion above passes
// over its absence. An unset POD_NAME makes every router replica write
// status.connections[""], one shared last-writer-wins entry across every
// replica -- the reaper-kills-a-live-session failure spec 187-190 names.
func TestRouterDeploymentSetsPodNameFromDownwardAPI(t *testing.T) {
	d, err := RenderRouterDeployment(validApp(), testRouterImage)
	if err != nil {
		t.Fatalf("RenderRouterDeployment: %v", err)
	}
	c := routerContainer(t, d)
	for _, e := range c.Env {
		if e.Name != "POD_NAME" {
			continue
		}
		if e.ValueFrom == nil || e.ValueFrom.FieldRef == nil || e.ValueFrom.FieldRef.FieldPath != "metadata.name" {
			t.Fatalf("POD_NAME must come from the downward API's metadata.name, got %+v", e)
		}
		return
	}
	t.Fatal("router container missing POD_NAME env var")
}

// TestRouterPortsAreTheSharedConstantsEverywhere is the "one number, three
// dispatches" property: the Service targetPort, the container port, and (by
// re-deriving) RenderRouterNetworkPolicy's ingress port must all be
// v1alpha1.RouterPort, and the metrics port in all three must be
// v1alpha1.MetricsPort -- asserted against the constants, never re-typed
// numbers, so the three dispatches cannot silently drift from each other.
func TestRouterPortsAreTheSharedConstantsEverywhere(t *testing.T) {
	app := validApp()
	d, err := RenderRouterDeployment(app, testRouterImage)
	if err != nil {
		t.Fatalf("RenderRouterDeployment: %v", err)
	}
	svc := RenderRouterService(app)
	c := routerContainer(t, d)

	var containerRouterPort, containerMetricsPort int32
	for _, p := range c.Ports {
		switch p.Name {
		case "http":
			containerRouterPort = p.ContainerPort
		case "metrics":
			containerMetricsPort = p.ContainerPort
		}
	}
	if containerRouterPort != v1alpha1.RouterPort {
		t.Fatalf("container http port = %d, want v1alpha1.RouterPort", containerRouterPort)
	}
	if containerMetricsPort != v1alpha1.MetricsPort {
		t.Fatalf("container metrics port = %d, want v1alpha1.MetricsPort", containerMetricsPort)
	}

	var svcRouterPort, svcMetricsPort int32
	var svcRouterTarget, svcMetricsTarget intstr.IntOrString
	for _, p := range svc.Spec.Ports {
		switch p.Name {
		case "http":
			svcRouterPort, svcRouterTarget = p.Port, p.TargetPort
		case "metrics":
			svcMetricsPort, svcMetricsTarget = p.Port, p.TargetPort
		}
	}
	if svcRouterPort != v1alpha1.RouterPort || svcRouterTarget.IntVal != v1alpha1.RouterPort {
		t.Fatalf("router Service http port/targetPort must be v1alpha1.RouterPort, got port=%d targetPort=%v", svcRouterPort, svcRouterTarget)
	}
	if svcMetricsPort != v1alpha1.MetricsPort || svcMetricsTarget.IntVal != v1alpha1.MetricsPort {
		t.Fatalf("router Service metrics port/targetPort must be v1alpha1.MetricsPort, got port=%d targetPort=%v", svcMetricsPort, svcMetricsTarget)
	}

	pol := RenderRouterNetworkPolicy(app, "10.42.0.0/16", "10.0.0.0/24")
	var polRouterPort int32 = -1
	for _, r := range pol.Spec.Ingress {
		for _, peer := range r.From {
			if peer.PodSelector == nil {
				continue
			}
			for _, p := range r.Ports {
				if p.Port != nil {
					polRouterPort = p.Port.IntVal
				}
			}
		}
	}
	if polRouterPort != v1alpha1.RouterPort {
		t.Fatalf("router NetworkPolicy ingress port = %d, want v1alpha1.RouterPort", polRouterPort)
	}
}

// TestRouterDeploymentMountsCallerAuthAtFixedFilename: a plain Secret volume
// names each projected file after its key, so `key: api-key` (the value in
// every fixture and both consumer CRs) puts the file at
// /etc/puc/caller-auth/api-key unless items[0].path is pinned to "value" --
// exactly what --caller-auth-secret-file names. Asserting the flag list or
// the mount path alone passes over a Deployment whose flags name paths that
// do not exist.
func TestRouterDeploymentMountsCallerAuthAtFixedFilename(t *testing.T) {
	app := validApp()
	d, err := RenderRouterDeployment(app, testRouterImage)
	if err != nil {
		t.Fatalf("RenderRouterDeployment: %v", err)
	}
	c := routerContainer(t, d)
	mount := volumeMountByName(t, c, "caller-auth")
	if mount.MountPath != "/etc/puc/caller-auth" || !mount.ReadOnly {
		t.Fatalf("caller-auth mount = %+v, want path /etc/puc/caller-auth, readOnly", mount)
	}
	vol := podVolumeByName(t, d, "caller-auth")
	if vol.Secret == nil || vol.Secret.SecretName != app.Spec.CallerAuth.SecretRef.Name {
		t.Fatalf("caller-auth volume must be a Secret volume named %q, got %+v", app.Spec.CallerAuth.SecretRef.Name, vol)
	}
	if len(vol.Secret.Items) != 1 || vol.Secret.Items[0].Key != app.Spec.CallerAuth.SecretRef.Key || vol.Secret.Items[0].Path != "value" {
		t.Fatalf("caller-auth volume must project key %q onto path %q, got %+v", app.Spec.CallerAuth.SecretRef.Key, "value", vol.Secret.Items)
	}
}

func TestRouterDeploymentOmitsUpstreamAuthWhenUnset(t *testing.T) {
	d, err := RenderRouterDeployment(validApp(), testRouterImage)
	if err != nil {
		t.Fatalf("RenderRouterDeployment: %v", err)
	}
	for _, v := range d.Spec.Template.Spec.Volumes {
		if v.Name == "upstream-auth" {
			t.Fatal("upstream-auth volume rendered with spec.workspace.upstreamAuth unset")
		}
	}
	for _, m := range routerContainer(t, d).VolumeMounts {
		if m.Name == "upstream-auth" {
			t.Fatal("upstream-auth volumeMount rendered with spec.workspace.upstreamAuth unset")
		}
	}
}

func TestRouterDeploymentMountsUpstreamAuthWhenSet(t *testing.T) {
	app := validApp()
	app.Spec.Workspace.UpstreamAuth = &v1alpha1.SecretHeaderRef{
		SecretRef: corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "upstream-secret"}, Key: "token"},
		Header:    "X-Upstream-Auth",
		Scheme:    "Bearer",
	}
	d, err := RenderRouterDeployment(app, testRouterImage)
	if err != nil {
		t.Fatalf("RenderRouterDeployment: %v", err)
	}
	c := routerContainer(t, d)
	got := routerArgs(t, c.Args)
	if got["upstream-auth-header"] != "X-Upstream-Auth" || got["upstream-auth-scheme"] != "Bearer" || got["upstream-auth-secret-file"] != "/etc/puc/upstream-auth/value" {
		t.Fatalf("upstream-auth-* flags not rendered correctly: %v", got)
	}
	mount := volumeMountByName(t, c, "upstream-auth")
	if mount.MountPath != "/etc/puc/upstream-auth" || !mount.ReadOnly {
		t.Fatalf("upstream-auth mount = %+v", mount)
	}
	vol := podVolumeByName(t, d, "upstream-auth")
	if vol.Secret == nil || vol.Secret.SecretName != "upstream-secret" || len(vol.Secret.Items) != 1 ||
		vol.Secret.Items[0].Key != "token" || vol.Secret.Items[0].Path != "value" {
		t.Fatalf("upstream-auth volume malformed: %+v", vol)
	}
}

func TestRouterDeploymentFailsFastWithNoResolvableImage(t *testing.T) {
	app := validApp()
	app.Spec.Router.Image = ""
	if _, err := RenderRouterDeployment(app, ""); err == nil {
		t.Fatal("expected error when neither spec.router.Image nor RELATED_IMAGE_ROUTER (the passed default) is set")
	}
}

func TestRouterDeploymentSpecImageOverridesDefault(t *testing.T) {
	app := validApp()
	app.Spec.Router.Image = "dev/router:local"
	d, err := RenderRouterDeployment(app, testRouterImage)
	if err != nil {
		t.Fatalf("RenderRouterDeployment: %v", err)
	}
	if got := routerContainer(t, d).Image; got != "dev/router:local" {
		t.Fatalf("container image = %q, want spec.router.Image override %q", got, "dev/router:local")
	}
}

// TestRouterDeploymentAndServiceStampedWithSharedLabels is the precondition
// for Task 12's ServiceMonitor selecting anything at all: RouterPodLabels
// (Task 5), this task's stamping, and Task 12's selector are three separate
// dispatches, and a mismatch on any of the three strings makes the
// ServiceMonitor select nothing -- no error, just no series.
func TestRouterDeploymentAndServiceStampedWithSharedLabels(t *testing.T) {
	app := validApp()
	d, err := RenderRouterDeployment(app, testRouterImage)
	if err != nil {
		t.Fatalf("RenderRouterDeployment: %v", err)
	}
	svc := RenderRouterService(app)
	want := RouterPodLabels(app.Name)
	for k, v := range want {
		if d.Spec.Template.Labels[k] != v {
			t.Fatalf("router pod template missing label %s=%s", k, v)
		}
		if svc.Labels[k] != v || svc.Spec.Selector[k] != v {
			t.Fatalf("router Service missing label/selector %s=%s", k, v)
		}
	}
	if want[v1alpha1.LabelComponent] != v1alpha1.ComponentRouter || want[v1alpha1.LabelPartOf] != v1alpha1.PartOfValue {
		t.Fatal("RouterPodLabels must carry ComponentRouter and PartOfValue")
	}
	if *d.Spec.Replicas != app.Spec.Router.Replicas {
		t.Fatalf("replicas = %d, want spec.router.replicas %d", *d.Spec.Replicas, app.Spec.Router.Replicas)
	}
	if d.Spec.Template.Spec.ServiceAccountName != app.Name+"-router" {
		t.Fatalf("router pod serviceAccountName = %q, want %q", d.Spec.Template.Spec.ServiceAccountName, app.Name+"-router")
	}
}

func TestWorkspaceServiceAccountNamedByConventionAndNoRoleBindingRendered(t *testing.T) {
	app := validApp()
	sa := RenderWorkspaceServiceAccount(app)
	if sa.Name != app.Name+"-workspace" || sa.Namespace != app.Namespace {
		t.Fatalf("workspace SA = %s/%s, want %s/%s-workspace", sa.Namespace, sa.Name, app.Namespace, app.Name)
	}
}

func TestRouterServiceAccountAndRoleBindingNamedByConvention(t *testing.T) {
	app := validApp()
	sa := RenderRouterServiceAccount(app)
	if sa.Name != app.Name+"-router" || sa.Namespace != app.Namespace {
		t.Fatalf("router SA = %s/%s, want %s/%s-router", sa.Namespace, sa.Name, app.Namespace, app.Name)
	}
	rb := RenderRouterRoleBinding(app)
	if rb.RoleRef.Name != v1alpha1.RouterRoleName || rb.RoleRef.Kind != "Role" {
		t.Fatalf("router RoleBinding roleRef = %+v, want Role/%s", rb.RoleRef, v1alpha1.RouterRoleName)
	}
	if len(rb.Subjects) != 1 || rb.Subjects[0].Name != sa.Name || rb.Subjects[0].Namespace != app.Namespace || rb.Subjects[0].Kind != "ServiceAccount" {
		t.Fatalf("router RoleBinding subject mismatch: %+v, want ServiceAccount %s/%s", rb.Subjects, app.Namespace, sa.Name)
	}
}
