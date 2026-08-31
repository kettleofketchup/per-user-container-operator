// Package examples holds the operator's example PerUserApp CRs and the
// in-process checks that keep them honest against the consumer charts they
// transcribe. This package carries NO build tag: it runs under `make test`
// (`go test -count=1 ./...`) like every other package, unlike
// test/e2e/workspace_app_test.go's `TestWorkspaceAppColdStart`, which needs a live
// cluster and stays behind `//go:build e2e`. See task-14-brief.md for why:
// these checks are pure golden-file/struct-reflection walks with no cluster
// and no API server, and the walk itself is the mechanism credited with
// finding two fields (workspace.upstreamAuth, workspace.livenessProbe) that
// were missing from the API entirely.
package examples

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"

	"github.com/kettleofketchup/per-user-container-operator/api/v1alpha1"
	"github.com/kettleofketchup/per-user-container-operator/internal/controller"
	"github.com/kettleofketchup/per-user-container-operator/internal/testfixtures"
)

// packageDir returns this source file's own directory, independent of the
// test binary's working directory (see test/e2e/env_test.go's identical
// helper and its reasoning).
func packageDir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Dir(file)
}

// loadGoldenPodSpec reads the golden PodSpec rendered from the real workspace-app
// consumer chart (test/e2e/testdata/workspace-app-podspec.yaml — see that file's
// own header for exact provenance: chart version, image tag, render date
// and command). It is NEVER hand-edited to match this package's CR or the
// operator's API: if a re-render disagrees with it, the golden must be
// regenerated from `helm template`, not patched here.
func loadGoldenPodSpec(t *testing.T) corev1.PodSpec {
	t.Helper()
	path := filepath.Join(packageDir(), "..", "test", "e2e", "testdata", "workspace-app-podspec.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden PodSpec %s: %v", path, err)
	}
	var spec corev1.PodSpec
	if err := yaml.UnmarshalStrict(raw, &spec); err != nil {
		t.Fatalf("unmarshal golden PodSpec %s: %v", path, err)
	}
	return spec
}

// loadWorkspaceAppCR reads examples/workspace-app.yaml as a v1alpha1.PerUserApp. It is
// the CR this task creates at Step 3 -- until then, every test depending on
// it fails with a plain file-not-found error, which is Step 2's "run,
// expect failure" for those checks.
func loadWorkspaceAppCR(t *testing.T) *v1alpha1.PerUserApp {
	t.Helper()
	path := filepath.Join(packageDir(), "workspace-app.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v (Step 3 of task-14-brief.md creates this file)", path, err)
	}
	var app v1alpha1.PerUserApp
	if err := yaml.UnmarshalStrict(raw, &app); err != nil {
		t.Fatalf("unmarshal %s: %v", path, err)
	}
	return &app
}

// workspaceTemplateFieldNames returns the exported Go field names on
// v1alpha1.WorkspaceTemplateSpec, computed via reflection rather than
// hand-enumerated, so a future rename or removal on that type changes what
// this walk accepts automatically instead of silently going stale.
func workspaceTemplateFieldNames() map[string]bool {
	t := reflect.TypeOf(v1alpha1.WorkspaceTemplateSpec{})
	out := make(map[string]bool, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		out[t.Field(i).Name] = true
	}
	return out
}

// podSpecFieldMap maps every corev1.PodSpec field to the
// v1alpha1.WorkspaceTemplateSpec field it is expressible through, or to one
// of two sentinels: "" (never expressible -- a populated field fails the
// walk) or a SPECIAL_* marker the walker below handles by hand. A
// corev1.PodSpec field with no entry here is an internal test error (see
// walkPodSpec): this map must be extended, not the field silently ignored,
// the day upstream Kubernetes adds a new PodSpec field.
var podSpecFieldMap = map[string]string{
	"Volumes":                       "Volumes",
	"InitContainers":                "InitContainers", // pass-through type (see walkPodSpec)
	"Containers":                    "SPECIAL_CONTAINERS",
	"EphemeralContainers":           "",
	"RestartPolicy":                 "",
	"TerminationGracePeriodSeconds": "TerminationGracePeriodSeconds",
	"ActiveDeadlineSeconds":         "",
	"DNSPolicy":                     "",
	"NodeSelector":                  "NodeSelector",
	"ServiceAccountName":            "ServiceAccountName",
	"DeprecatedServiceAccount":      "",
	"AutomountServiceAccountToken":  "AutomountServiceAccountToken",
	"NodeName":                      "",
	"HostNetwork":                   "",
	"HostPID":                       "",
	"HostIPC":                       "",
	"ShareProcessNamespace":         "",
	"SecurityContext":               "PodSecurityContext",
	"ImagePullSecrets":              "",
	"Hostname":                      "",
	"Subdomain":                     "",
	"Affinity":                      "",
	"SchedulerName":                 "",
	"Tolerations":                   "Tolerations",
	"HostAliases":                   "",
	"PriorityClassName":             "",
	"Priority":                      "",
	"DNSConfig":                     "",
	"ReadinessGates":                "",
	"RuntimeClassName":              "",
	"EnableServiceLinks":            "",
	"PreemptionPolicy":              "",
	"Overhead":                      "",
	"TopologySpreadConstraints":     "",
	"SetHostnameAsFQDN":             "",
	"OS":                            "",
	"HostUsers":                     "",
	"SchedulingGates":               "",
	"ResourceClaims":                "",
}

// containerFieldMap is podSpecFieldMap's counterpart for corev1.Container,
// applied to the golden's single main container (spec.template.spec.containers[0]).
// It is NOT applied to initContainers: WorkspaceTemplateSpec.InitContainers
// is a literal []corev1.Container, so every field of an init container is
// expressible unconditionally by construction (see walkPodSpec) -- walking
// its fields against this map would only ever find field names it already
// knows are fine, which is why the brief calls that half of the walk a
// proof of expressibility, not a live check.
var containerFieldMap = map[string]string{
	"Name":                     "SPECIAL_SKIP", // renderer always names it workspaceContainerName, never from the CR
	"Image":                    "Image",
	"Command":                  "Command",
	"Args":                     "Args",
	"WorkingDir":               "",
	"Ports":                    "SPECIAL_PORT",
	"EnvFrom":                  "EnvFrom",
	"Env":                      "Env",
	"Resources":                "Resources",
	"ResizePolicy":             "",
	"RestartPolicy":            "",
	"VolumeMounts":             "VolumeMounts",
	"VolumeDevices":            "",
	"LivenessProbe":            "LivenessProbe",
	"ReadinessProbe":           "ReadinessProbe",
	"StartupProbe":             "",
	"Lifecycle":                "",
	"TerminationMessagePath":   "",
	"TerminationMessagePolicy": "",
	"ImagePullPolicy":          "ImagePullPolicy",
	"SecurityContext":          "SecurityContext",
	"Stdin":                    "",
	"StdinOnce":                "",
	"TTY":                      "",
}

// walkContainer walks every populated field of c (the golden's main
// container) and appends prefix-qualified field paths to *bad for anything
// containerFieldMap marks unexpressible ("") or for a port shape
// SPECIAL_PORT cannot express (more than one container port; spec.workspace
// only carries a single int32 Port).
func walkContainer(t *testing.T, prefix string, c corev1.Container, wtFields map[string]bool, bad *[]string) {
	t.Helper()
	cVal := reflect.ValueOf(c)
	cType := cVal.Type()
	for i := 0; i < cType.NumField(); i++ {
		f := cType.Field(i)
		fv := cVal.Field(i)
		if fv.IsZero() {
			continue
		}
		mapped, known := containerFieldMap[f.Name]
		if !known {
			t.Fatalf("internal test error: corev1.Container has a field %q with no entry in containerFieldMap -- add one (see that map's doc comment)", f.Name)
		}
		path := prefix + "." + f.Name
		switch mapped {
		case "SPECIAL_SKIP":
			continue
		case "SPECIAL_PORT":
			if !wtFields["Port"] {
				t.Fatalf("internal test error: v1alpha1.WorkspaceTemplateSpec has no Port field any more")
			}
			ports, _ := fv.Interface().([]corev1.ContainerPort)
			if len(ports) != 1 {
				*bad = append(*bad, fmt.Sprintf("%s (only exactly one container port is expressible, via spec.workspace.port; golden has %d)", path, len(ports)))
			}
		case "":
			*bad = append(*bad, path)
		default:
			if !wtFields[mapped] {
				t.Fatalf("internal test error: containerFieldMap claims WorkspaceTemplateSpec.%s expresses Container.%s, but no such field exists (type may have been renamed)", mapped, f.Name)
			}
		}
	}
}

// walkPodSpec is the expressibility walk itself (task-14-brief.md Step 1):
// every populated field of the golden PodSpec, its single main container,
// and (trivially) its init containers must be expressible either as a
// v1alpha1.WorkspaceTemplateSpec field or as a CR value elsewhere, or the
// walk fails naming the exact field path. It is deny-by-default: a
// corev1.PodSpec or corev1.Container field this file has never seen before
// is an internal test error demanding a human decide whether it needs a new
// mapping, not a silently-ignored pass.
func walkPodSpec(t *testing.T, spec corev1.PodSpec) []string {
	t.Helper()
	wtFields := workspaceTemplateFieldNames()
	var bad []string

	psVal := reflect.ValueOf(spec)
	psType := psVal.Type()
	for i := 0; i < psType.NumField(); i++ {
		f := psType.Field(i)
		fv := psVal.Field(i)
		if fv.IsZero() {
			continue
		}
		mapped, known := podSpecFieldMap[f.Name]
		if !known {
			t.Fatalf("internal test error: corev1.PodSpec has a field %q with no entry in podSpecFieldMap -- add one (see that map's doc comment)", f.Name)
		}
		path := "spec.template.spec." + f.Name

		switch f.Name {
		case "Containers":
			containers, _ := fv.Interface().([]corev1.Container)
			for ci, c := range containers {
				walkContainer(t, fmt.Sprintf("%s[%d]", path, ci), c, wtFields, &bad)
			}
			continue
		case "InitContainers":
			// WorkspaceTemplateSpec.InitContainers is a literal
			// []corev1.Container -- every field of every init container is
			// expressible unconditionally, by type identity, with no
			// per-field mapping needed. This is the walk PROVING that
			// (task-14-brief.md's own framing), not skipping it: an empty
			// containerFieldMap-style restriction here would be false, not
			// conservative.
			continue
		}

		switch mapped {
		case "":
			bad = append(bad, path)
		default:
			if !wtFields[mapped] {
				t.Fatalf("internal test error: podSpecFieldMap claims WorkspaceTemplateSpec.%s expresses PodSpec.%s, but no such field exists (type may have been renamed)", mapped, f.Name)
			}
		}
	}

	// Volumes are a literal []corev1.Volume by TYPE (so the field-level walk
	// above accepts them unconditionally), but the operator's actual
	// admission-time rule (ValidateApp / allowedVolumeSource) restricts
	// their VolumeSource to configMap/secret/emptyDir/projected. Checking
	// that restriction here by calling the real ValidateApp -- rather than
	// re-implementing the allowlist in this test file -- is what makes "an
	// emptyDir is expressible because emptyDir is on the volume allowlist"
	// (task-14-brief.md Step 3's own reasoning) a fact about production
	// code, not this test's opinion of it.
	if len(spec.Volumes) > 0 {
		probe := testfixtures.ValidApp()
		probe.Spec.Workspace.Volumes = spec.Volumes
		if _, err := controller.ValidateApp(probe); err != nil {
			bad = append(bad, fmt.Sprintf("spec.template.spec.Volumes: %v", err))
		}
	}

	return bad
}

// TestWorkspaceAppGoldenPodSpecIsExpressible is task-14-brief.md Step 1's
// coverage test: it walks every field of the golden PodSpec rendered from
// the real workspace-app chart and fails, naming the field path, for anything not
// expressible in spec.workspace or a CR value. See walkPodSpec's doc
// comment for the walk itself and the two field-map vars above for the
// current expressibility mapping.
func TestWorkspaceAppGoldenPodSpecIsExpressible(t *testing.T) {
	golden := loadGoldenPodSpec(t)
	if bad := walkPodSpec(t, golden); len(bad) > 0 {
		t.Fatalf("golden PodSpec has %d field(s) not expressible via spec.workspace or a CR value:\n  %s", len(bad), strings.Join(bad, "\n  "))
	}
}

// TestWorkspaceAppCRAssertions is task-14-brief.md Step 3's "assertions the walk
// cannot make": the expressibility walk above proves a value like an
// emptyDir volume or a mounted Secret CAN be expressed, never that
// examples/workspace-app.yaml actually expresses it. These four checks close that
// gap directly against the checked-in CR.
func TestWorkspaceAppCRAssertions(t *testing.T) {
	app := loadWorkspaceAppCR(t)

	t.Run("StorageMountPath", func(t *testing.T) {
		if got := app.Spec.Storage.MountPath; got != "/workspace" {
			t.Fatalf("spec.storage.mountPath = %q, want /workspace", got)
		}
	})

	t.Run("Seed", func(t *testing.T) {
		seed := app.Spec.Storage.Seed
		if seed == nil {
			t.Fatal("spec.storage.seed is nil")
		}
		if seed.StagingMountPath != "/mnt/workspace" {
			t.Fatalf("spec.storage.seed.stagingMountPath = %q, want /mnt/workspace", seed.StagingMountPath)
		}
		if seed.From != "/workspace/samples" {
			t.Fatalf("spec.storage.seed.from = %q, want /workspace/samples", seed.From)
		}
	})

	t.Run("HomeEnv", func(t *testing.T) {
		for _, e := range app.Spec.Workspace.Env {
			if e.Name == "HOME" && e.Value == "/workspace/home" {
				return
			}
		}
		t.Fatalf("spec.workspace.env does not contain HOME=/workspace/home (env=%+v)", app.Spec.Workspace.Env)
	})

	t.Run("ScratchVolume", func(t *testing.T) {
		var scratchVol *corev1.Volume
		for i := range app.Spec.Workspace.Volumes {
			v := &app.Spec.Workspace.Volumes[i]
			if v.Name == v1alpha1.PVCVolumeName {
				t.Fatalf("spec.workspace.volumes declares a volume literally named %q, the RESERVED per-user PVC volume name -- this is exactly the rename task-14-brief.md section (b) requires: the chart's own `workspace` emptyDir must be renamed to something else before it lands here", v1alpha1.PVCVolumeName)
			}
			if v.EmptyDir != nil {
				scratchVol = v
			}
		}
		if scratchVol == nil {
			t.Fatal("spec.workspace.volumes has no emptyDir volume for the .app scratch dir")
		}
		if scratchVol.EmptyDir.Medium != corev1.StorageMediumMemory {
			t.Fatalf("volume %q emptyDir.medium = %q, want Memory", scratchVol.Name, scratchVol.EmptyDir.Medium)
		}
		if scratchVol.EmptyDir.SizeLimit == nil || scratchVol.EmptyDir.SizeLimit.String() != "16Mi" {
			t.Fatalf("volume %q emptyDir.sizeLimit = %v, want 16Mi", scratchVol.Name, scratchVol.EmptyDir.SizeLimit)
		}

		var mounted bool
		for _, m := range app.Spec.Workspace.VolumeMounts {
			if m.Name == scratchVol.Name && m.MountPath == "/workspace/.app" {
				mounted = true
			}
		}
		if !mounted {
			t.Fatalf("spec.workspace.volumeMounts does not mount volume %q at /workspace/.app", scratchVol.Name)
		}
	})

	t.Run("RenderConfigInitContainerMounts", func(t *testing.T) {
		var haveConfigTemplate, haveLitellmSecret bool
		var configTemplateName, litellmSecretName string
		for _, v := range app.Spec.Workspace.Volumes {
			if v.ConfigMap != nil {
				haveConfigTemplate = true
				configTemplateName = v.Name
			}
			if v.Secret != nil {
				haveLitellmSecret = true
				litellmSecretName = v.Name
			}
		}
		if !haveConfigTemplate {
			t.Fatal("spec.workspace.volumes has no configMap-sourced volume (config-template)")
		}
		if !haveLitellmSecret {
			t.Fatal("spec.workspace.volumes has no secret-sourced volume (litellm-secret)")
		}

		var initC *corev1.Container
		for i := range app.Spec.Workspace.InitContainers {
			if app.Spec.Workspace.InitContainers[i].Name == "render-config" {
				initC = &app.Spec.Workspace.InitContainers[i]
			}
		}
		if initC == nil {
			t.Fatal("spec.workspace.initContainers has no render-config container")
		}

		mountedAt := map[string]string{}
		for _, m := range initC.VolumeMounts {
			mountedAt[m.Name] = m.MountPath
		}
		if got := mountedAt[configTemplateName]; got != "/tmp/workspace-app-template" {
			t.Fatalf("render-config mounts %q at %q, want /tmp/workspace-app-template", configTemplateName, got)
		}
		if got := mountedAt[litellmSecretName]; got != "/secret" {
			t.Fatalf("render-config mounts %q at %q, want /secret", litellmSecretName, got)
		}
	})

	t.Run("ValidateApp", func(t *testing.T) {
		if _, err := controller.ValidateApp(app); err != nil {
			t.Fatalf("ValidateApp rejected examples/workspace-app.yaml: %v", err)
		}
	})
}

// TestWorkspaceAppRenderPreservesSecurityContext is the render test
// task-14-brief.md's Step 3 requires (spec 697): feed examples/workspace-app.yaml
// through RenderWorkspaceDeployment and confirm the pod security context and
// the full volume set survive rendering intact.
// spec.workspace.podSecurityContext.seccompProfile: Unconfined matters most
// of everything asserted here -- without it bubblewrap's clone/unshare
// preflight fails and the pod crash-loops, and a renderer that silently
// drops the field would otherwise ship untested (no other task's render
// tests exercise this CR at all; Task 5's use a generic fixture).
func TestWorkspaceAppRenderPreservesSecurityContext(t *testing.T) {
	app := loadWorkspaceAppCR(t)
	ws := &v1alpha1.Workspace{
		Spec: v1alpha1.WorkspaceSpec{
			AppRef:  corev1.LocalObjectReference{Name: app.Name},
			UserKey: "u-0000000000000000",
		},
	}
	ws.Namespace = app.Namespace
	ws.Name = app.Name + "-" + ws.Spec.UserKey

	dep := controller.RenderWorkspaceDeployment(app, ws)
	psc := dep.Spec.Template.Spec.SecurityContext
	if psc == nil {
		t.Fatal("rendered pod securityContext is nil")
	}
	if psc.RunAsNonRoot == nil || !*psc.RunAsNonRoot {
		t.Errorf("rendered pod securityContext.runAsNonRoot = %v, want true", psc.RunAsNonRoot)
	}
	wantID := app.Spec.Workspace.PodSecurityContext
	if psc.RunAsUser == nil || wantID.RunAsUser == nil || *psc.RunAsUser != *wantID.RunAsUser {
		t.Errorf("rendered pod securityContext.runAsUser = %v, want %v", psc.RunAsUser, wantID.RunAsUser)
	}
	if psc.RunAsGroup == nil || wantID.RunAsGroup == nil || *psc.RunAsGroup != *wantID.RunAsGroup {
		t.Errorf("rendered pod securityContext.runAsGroup = %v, want %v", psc.RunAsGroup, wantID.RunAsGroup)
	}
	if psc.FSGroup == nil || wantID.FSGroup == nil || *psc.FSGroup != *wantID.FSGroup {
		t.Errorf("rendered pod securityContext.fsGroup = %v, want %v", psc.FSGroup, wantID.FSGroup)
	}
	if psc.SeccompProfile == nil || psc.SeccompProfile.Type != corev1.SeccompProfileTypeUnconfined {
		t.Fatalf("rendered pod securityContext.seccompProfile = %v, want type Unconfined -- without it bubblewrap's clone/unshare preflight fails and the pod crash-loops", psc.SeccompProfile)
	}

	gotNames := map[string]bool{}
	for _, v := range dep.Spec.Template.Spec.Volumes {
		gotNames[v.Name] = true
	}
	if !gotNames[v1alpha1.PVCVolumeName] {
		t.Errorf("rendered volumes missing the per-user PVC volume %q", v1alpha1.PVCVolumeName)
	}
	for _, v := range app.Spec.Workspace.Volumes {
		if !gotNames[v.Name] {
			t.Errorf("rendered volumes missing CR-declared volume %q", v.Name)
		}
	}
	if want, got := len(app.Spec.Workspace.Volumes)+1, len(dep.Spec.Template.Spec.Volumes); want != got {
		t.Errorf("rendered volume count = %d, want %d (CR volumes + the PVC volume)", got, want)
	}
}
