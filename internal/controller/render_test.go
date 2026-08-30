package controller

import (
	"strings"
	"testing"

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

// Egress: a selector peer renders and applies and silently drops on Calico.
// Also enforced by CEL (Task 4); both layers, because the API-server rule is
// bypassed by prune-and-recreate and the Go rule runs after admission.
func TestValidateRejectsSelectorEgressAndEmptyRouterIngress(t *testing.T) {
	t.Run("selector egress peer", func(t *testing.T) {
		app := validApp()
		app.Spec.Network.WorkspaceEgress = []networkingv1.NetworkPolicyEgressRule{
			{To: []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{}}}},
		}
		if _, err := ValidateApp(app); err == nil {
			t.Fatal("selector egress peer accepted; Calico drops it silently")
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
	// workspace-app's render-config init container is the only thing that writes
	// /workspace/.app/config.yaml from the mirrored litellm secret, and Task
	// 14's remaining coverage never reads InitContainers. Worse, dropping it
	// makes Task 14 Step 4's kind run GREENER: the real render-config blocks on
	// `until [ -s /secret/master-key ]`, which never resolves on kind, so the
	// correct rendering is the one that looks broken.
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
	// The corpus keeps its directory name: `cp -an <from> <staging>/`, no
	// trailing "/.". See Step 3 — the layout is decided there and Task 14
	// Step 4 asserts the resulting path.
	if !strings.Contains(strings.Join(d.Spec.Template.Spec.InitContainers[0].Command, " "), "cp -an /workspace/corpus /mnt/ws/") {
		t.Fatal("seeder command must copy the source DIRECTORY, not its contents; a trailing /. flattens the corpus to the volume root permanently")
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
// default-deny + DNS + the declared ipBlock peers, but every assertion that
// looks like it covers this is about ingress:
// TestWorkspaceIngressSelectsAllThreeRouterLabels reads the ingress rule and
// the node-CIDR admission, and TestValidateRejectsSelectorEgressAndEmptyRouterIngress
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
