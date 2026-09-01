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
	//
	// A persistentVolumeClaim volume is the one source that is expressible
	// WITHOUT being on that allowlist, because spec.workspace.volumes is
	// not where it is expressed: the golden's single shared claim is
	// precisely what this operator replaces with a per-user claim of its
	// own, declared under spec.storage and rendered by the operator rather
	// than named by the CR. Feeding it to ValidateApp would report the
	// design's central move as an inexpressible field. It is partitioned
	// out here and checked against spec.storage instead.
	var workspaceVols []corev1.Volume
	var claimVols []string
	for _, v := range spec.Volumes {
		if v.PersistentVolumeClaim != nil {
			claimVols = append(claimVols, v.Name)
			continue
		}
		workspaceVols = append(workspaceVols, v)
	}
	if len(workspaceVols) > 0 {
		probe := testfixtures.ValidApp()
		probe.Spec.Workspace.Volumes = workspaceVols
		if _, err := controller.ValidateApp(probe); err != nil {
			bad = append(bad, fmt.Sprintf("spec.template.spec.Volumes: %v", err))
		}
	}
	if len(claimVols) > 0 {
		// Assert the claim really is expressible where this walk claims it
		// is, against the live type rather than this file's belief about
		// it: a rename or removal of these fields must fail the walk, not
		// be papered over by the partition above.
		st := reflect.TypeOf(v1alpha1.StorageSpec{})
		for _, want := range []string{"Size", "StorageClassName", "MountPath"} {
			if _, ok := st.FieldByName(want); !ok {
				t.Fatalf("internal test error: golden volume(s) %v were treated as expressible via spec.storage, but v1alpha1.StorageSpec has no %s field", claimVols, want)
			}
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
	goldenPath := filepath.Join(packageDir(), "..", "test", "e2e", "testdata", "workspace-app-podspec.yaml")
	if len(golden.Containers) < 1 || len(golden.InitContainers) < 1 || len(golden.Volumes) < 1 {
		t.Fatalf("golden PodSpec %s parsed to %d container(s), %d init container(s), %d volume(s) -- want at least 1, 1, 1 (truncated or wrongly-parsed fixture?)", goldenPath, len(golden.Containers), len(golden.InitContainers), len(golden.Volumes))
	}
	if bad := walkPodSpec(t, golden); len(bad) > 0 {
		t.Fatalf("golden PodSpec has %d field(s) not expressible via spec.workspace or a CR value:\n  %s", len(bad), strings.Join(bad, "\n  "))
	}
}

// TestWorkspaceAppCRAssertions is task-14-brief.md Step 3's "assertions the
// walk cannot make": the expressibility walk above proves the golden's shape
// CAN be expressed, never that examples/workspace-app.yaml actually expresses
// it -- and never that it makes the substitutions the CR's header claims to
// make. Each subtest below pins one of those claims against the checked-in
// CR, so a later edit that quietly reverts one fails here rather than in a
// cluster.
func TestWorkspaceAppCRAssertions(t *testing.T) {
	app := loadWorkspaceAppCR(t)
	golden := loadGoldenPodSpec(t)

	t.Run("StorageMountPath", func(t *testing.T) {
		// The chart mounts its shared claim at /home, which shadows the
		// image's baked /home/user and is the entire reason it needs a root
		// init container. One claim per user mounts a level deeper and needs
		// no such workaround, so this path is load-bearing, not cosmetic.
		if got := app.Spec.Storage.MountPath; got != "/home/user" {
			t.Fatalf("spec.storage.mountPath = %q, want /home/user", got)
		}
		for _, m := range golden.Containers[0].VolumeMounts {
			if m.MountPath == app.Spec.Storage.MountPath {
				t.Fatalf("spec.storage.mountPath = %q is the chart's own shared mount point; the per-user claim must mount below it, not shadow the image's home the same way", m.MountPath)
			}
		}
	})

	t.Run("Seed", func(t *testing.T) {
		seed := app.Spec.Storage.Seed
		if seed == nil {
			t.Fatal("spec.storage.seed is nil -- it is what replaces the chart's root init-home container")
		}
		if seed.StagingMountPath != "/mnt/home" {
			t.Fatalf("spec.storage.seed.stagingMountPath = %q, want /mnt/home", seed.StagingMountPath)
		}
		if seed.StagingMountPath == app.Spec.Storage.MountPath {
			t.Fatalf("spec.storage.seed.stagingMountPath = %q is the claim's own mountPath -- mounting there shadows the copy source and every user gets an empty home", seed.StagingMountPath)
		}
		// The seeder runs `cp -an <from> <staging>/`, so a bare directory
		// nests one level deep (/home/user/user); the trailing /. copies the
		// skeleton's CONTENTS. Getting this wrong is silent: the pod starts
		// and the home is merely wrong.
		if seed.From != "/home/user/." {
			t.Fatalf("spec.storage.seed.from = %q, want /home/user/. (the trailing /. copies the skeleton's contents rather than nesting it)", seed.From)
		}
	})

	t.Run("MultiUserEnvAbsent", func(t *testing.T) {
		// The claim the CR's header makes: container-per-user removes the
		// need for the app's in-container multi-user mode, whose documented
		// failure mode is falling back to the SHARED filesystem, silently,
		// when the identity header is absent. Find the env var the golden
		// sets rather than naming it here, so a rename in the chart cannot
		// make this check quietly vacuous.
		var multiUser string
		for _, e := range golden.Containers[0].Env {
			if strings.HasSuffix(e.Name, "_MULTI_USER") {
				multiUser = e.Name
			}
		}
		if multiUser == "" {
			t.Fatal("golden container sets no *_MULTI_USER env var -- the chart's shape changed and this assertion no longer means anything")
		}
		for _, e := range app.Spec.Workspace.Env {
			if e.Name == multiUser {
				t.Fatalf("spec.workspace.env sets %s -- container-per-user makes the app's own account mapping dead weight; see the CR's header", multiUser)
			}
		}
	})

	t.Run("NoRootInitContainer", func(t *testing.T) {
		// The chart runs init-home as uid 0 only to create and chown the
		// directory its own mount shadowed. spec.storage.seed does that job
		// without root, so nothing here may ask for it back.
		for _, c := range app.Spec.Workspace.InitContainers {
			sc := c.SecurityContext
			if sc != nil && sc.RunAsUser != nil && *sc.RunAsUser == 0 {
				t.Fatalf("initContainer %q runs as uid 0; spec.storage.seed exists precisely so the chart's root init-home container is unnecessary", c.Name)
			}
		}
	})

	t.Run("PortMatchesChart", func(t *testing.T) {
		ports := golden.Containers[0].Ports
		if len(ports) != 1 {
			t.Fatalf("golden container has %d ports, want exactly 1", len(ports))
		}
		if want, got := ports[0].ContainerPort, app.Spec.Workspace.Port; got != want {
			t.Fatalf("spec.workspace.port = %d, want %d (the chart's own containerPort)", got, want)
		}
	})

	t.Run("Resources", func(t *testing.T) {
		// Transcribed from the chart, which applies these per DEPLOYMENT
		// shared by everyone; the CR applies the same numbers per POD. Read
		// them off the golden rather than repeating literals, so a chart
		// change surfaces as a mismatch here.
		want := golden.Containers[0].Resources
		got := app.Spec.Workspace.Resources
		for _, k := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
			w, g := want.Requests[k], got.Requests[k]
			if g.Cmp(w) != 0 {
				t.Errorf("spec.workspace.resources.requests.%s = %s, want %s (the chart's own)", k, g.String(), w.String())
			}
			w, g = want.Limits[k], got.Limits[k]
			if g.Cmp(w) != 0 {
				t.Errorf("spec.workspace.resources.limits.%s = %s, want %s (the chart's own)", k, g.String(), w.String())
			}
		}
	})

	t.Run("Probes", func(t *testing.T) {
		// Both probes and their timings are the chart's; the CR is a
		// transcription, so any divergence is a transcription error.
		for _, tc := range []struct {
			name      string
			want, got *corev1.Probe
		}{
			{"readinessProbe", golden.Containers[0].ReadinessProbe, app.Spec.Workspace.ReadinessProbe},
			{"livenessProbe", golden.Containers[0].LivenessProbe, app.Spec.Workspace.LivenessProbe},
		} {
			if tc.want == nil {
				t.Fatalf("golden container has no %s -- the chart's shape changed", tc.name)
			}
			if tc.got == nil {
				t.Errorf("spec.workspace.%s is nil, want the chart's own", tc.name)
				continue
			}
			if !reflect.DeepEqual(tc.want, tc.got) {
				t.Errorf("spec.workspace.%s = %+v, want the chart's own %+v", tc.name, tc.got, tc.want)
			}
		}
	})

	t.Run("PodSecurityContext", func(t *testing.T) {
		// The chart sets NO pod securityContext -- it leans on the image's
		// USER directive, which the operator cannot accept, because
		// runAsNonRoot with a non-numeric USER is a CreateContainerConfigError
		// for every user of the app. docs/measurements.md Step 3 read the
		// numbers off the running container so they could be written down
		// here; this is the only place they exist as numbers.
		if golden.SecurityContext != nil {
			t.Fatalf("golden PodSpec now HAS a securityContext (%+v) -- the CR's explicit uid/gid no longer stand in for the image's USER directive and should be re-derived from the chart", golden.SecurityContext)
		}
		psc := app.Spec.Workspace.PodSecurityContext
		if psc.RunAsNonRoot == nil || !*psc.RunAsNonRoot {
			t.Errorf("spec.workspace.podSecurityContext.runAsNonRoot = %v, want true", psc.RunAsNonRoot)
		}
		for _, tc := range []struct {
			name string
			got  *int64
		}{
			{"runAsUser", psc.RunAsUser},
			{"runAsGroup", psc.RunAsGroup},
			{"fsGroup", psc.FSGroup},
		} {
			if tc.got == nil {
				t.Errorf("spec.workspace.podSecurityContext.%s is nil, want 1000 (measured: uid=1000(user) gid=1000(user))", tc.name)
				continue
			}
			if *tc.got != 1000 {
				t.Errorf("spec.workspace.podSecurityContext.%s = %d, want 1000 (measured: uid=1000(user) gid=1000(user))", tc.name, *tc.got)
			}
		}
	})

	t.Run("ReservedPVCVolumeName", func(t *testing.T) {
		for _, v := range app.Spec.Workspace.Volumes {
			if v.Name == v1alpha1.PVCVolumeName {
				t.Fatalf("spec.workspace.volumes declares a volume literally named %q, the operator's reserved per-user PVC volume name -- ValidateApp rejects any user volume that reuses it", v1alpha1.PVCVolumeName)
			}
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
// The uid/gid/fsGroup asserted here are the ONLY place those numbers exist:
// the consumer chart sets no pod securityContext at all and relies on the
// image's USER directive, so a renderer that silently dropped these fields
// would break every workspace at once and would otherwise ship untested (no
// other task's render tests exercise this CR; Task 5's use a generic fixture).
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
