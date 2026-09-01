//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/kettleofketchup/per-user-container-operator/api/v1alpha1"
	"github.com/kettleofketchup/per-user-container-operator/internal/identity"
)

// workspaceAppName is the name examples/workspace-app.yaml declares (metadata.name).
const workspaceAppName = "workspace-app"

// workspaceAppConfigTemplateName is the ConfigMap examples/workspace-app.yaml's
// config-template volume names (transcribed verbatim from the real
// workspace-app chart's own ConfigMap, test/e2e/testdata/workspace-app-podspec.yaml).
// In production this ConfigMap is provisioned by whatever deploys workspace-app
// itself alongside this operator's CR -- this operator only renders the
// Deployment/Service/NetworkPolicy from spec.workspace and never manages a
// ConfigMap merely named in spec.workspace.volumes. This harness has no
// such deployment, so ensureWorkspaceAppConfigTemplate creates one directly with
// the same content the golden render carries.
const workspaceAppConfigTemplateName = "workspace-app-config-template"

// workspaceAppConfigTemplateData is copied verbatim from the golden's own
// ConfigMap (see this package's kind-up.sh testdata and
// examples/workspace-app.yaml's header for the render command/date). The
// __LITELLM_API_KEY__ placeholder is exactly what loadWorkspaceAppCRForKind's
// rewritten render-config command still substitutes.
const workspaceAppConfigTemplateData = `llm:
  servers:
    - name: "litellm"
      api_base: "http://litellm.litellm.svc:4000/v1"
      api_key: "__LITELLM_API_KEY__"
      api_protocol: completions
      context_size: 120000
default_persona: "default"
logging:
  level: "INFO"
`

// ensureWorkspaceAppConfigTemplate creates (or replaces) the config-template
// ConfigMap examples/workspace-app.yaml's render-config init container mounts.
// Without it the pod never leaves Init: the volume simply fails to mount
// ("configmap %q not found"), which is exactly what this harness observed
// before this helper was added.
func ensureWorkspaceAppConfigTemplate(ctx context.Context, ns string) error {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: workspaceAppConfigTemplateName, Namespace: ns},
		Data:       map[string]string{"config.yaml": workspaceAppConfigTemplateData},
	}
	if _, err := globalClient.CoreV1().ConfigMaps(ns).Create(ctx, cm, metav1.CreateOptions{}); err != nil {
		if apierrors.IsAlreadyExists(err) {
			_, err = globalClient.CoreV1().ConfigMaps(ns).Update(ctx, cm, metav1.UpdateOptions{})
			return err
		}
		return err
	}
	return nil
}

// workspaceAppFixtureSeedContent is fixture-workspace-seed/samples/sample.txt's
// content, baked into the kind fixture image
// (test/e2e/fixture-workspace.Dockerfile) at workspaceAppFixtureSeedSourcePath.
// The file on disk ends with a trailing newline, which `cp -an` faithfully
// preserves; the assertion below trims it rather than encoding it here,
// because what this step must prove is that the corpus landed at the exact
// path task-14-brief.md names -- not how a fixture text file is framed.
const workspaceAppFixtureSeedContent = "puc-e2e-seed-fixture"

// workspaceAppFixtureSeedSourcePath is where the kind fixture image bakes its
// seed corpus (test/e2e/fixture-workspace.Dockerfile), which is NOT the
// same path examples/workspace-app.yaml's own spec.storage.seed.from names
// (/workspace/samples -- the real workspace-app image's own baked path). See
// loadWorkspaceAppCRForKind's doc comment for why this test repoints seed.from
// at apply time rather than either editing the checked-in CR or the
// fixture image.
const workspaceAppFixtureSeedSourcePath = "/opt/puc-e2e-seed/samples"

// workspaceAppSeedDestPath is where the seed corpus lands on the per-user PVC
// regardless of which of the two source paths above produced it: cp -an
// SRC DST/ copies SRC into DST under SRC's own basename, and both
// candidate source directories are named "samples". task-14-brief.md Step
// 4 requires this EXACT destination path, not merely "contains the seeded
// samples".
const workspaceAppSeedDestPath = "/workspace/samples/sample.txt"

// loadWorkspaceAppCRForKind reads examples/workspace-app.yaml and adapts it for this
// kind harness, exactly the way task-14-brief.md Step 4 requires and one
// step further it does not spell out but which follows directly from it.
// Nothing here EVER touches the checked-in file:
//
//  1. spec.workspace.image AND every spec.workspace.initContainers[].image
//     are overlaid to PUC_E2E_WORKSPACE_IMAGE (env.WorkspaceImage) -- the
//     brief's explicit contract. workspace-app's own image
//     (registry.example.com/workspace-app/workspace-app-web:v0.6.1)
//     is private and unreachable from this repo's public CI; Task 15 Step 4
//     runs this same test against the live edge cluster with that image
//     substituted back in via the identical environment variable.
//
//  2. spec.storage.seed.from is repointed from /workspace/samples (the
//     real workspace-app image's own baked path) to
//     workspaceAppFixtureSeedSourcePath (the kind fixture's baked path,
//     test/e2e/fixture-workspace.Dockerfile) -- a path that exists only
//     inside the substitute image, never the real one, so it cannot be a
//     value examples/workspace-app.yaml itself carries. Not overlaying it would
//     make the seed init container's `cp -an` run against a source that is
//     simply absent from the fixture image, and this operator's own seed
//     container mounts the PVC only at stagingMountPath -- never at
//     mountPath -- so there is no path under which the real value would
//     resolve by coincidence.
//
//  3. spec.workspace.port is overlaid from PUC_E2E_WORKSPACE_PORT when that
//     variable is set (kind-up.sh sets it; a live-cluster run leaves it
//     unset and keeps the CR's own port). The substitute image listens
//     where it listens -- nginx-unprivileged on 8080 -- and workspace-app's own
//     7399 is a property of workspace-app's image, not of this CR's design. Left
//     alone, the workspace pod runs but never passes readiness on a closed
//     port, the router answers 503 for the whole coldStartHoldSeconds hold,
//     and nothing in that failure names the port. The probes need no
//     overlay of their own: they address the port by NAME ("http"), which
//     the renderer assigns to spec.workspace.port
//     (internal/controller/render.go's workspacePortName).
//
//  4. The render-config init container's command is rewritten from
//     workspace-app's own python3 templating step to a functionally identical
//     `sed` substitution. Verified directly against the fixture's own base
//     image (`docker run --rm nginxinc/nginx-unprivileged:alpine sh -c
//     'which python3'` -- not found; `sed` is present), so the real
//     command would exit 127 under `set -e` and the pod would
//     Init:CrashLoopBackOff forever on kind. The `until [ -s
//     /secret/master-key ]` wait is preserved BYTE FOR BYTE: that loop is
//     exactly what this task's kind-up.sh addition (a non-empty
//     litellm-secret/master-key Secret per namespace) exists to unblock,
//     and this test needs to prove that property regardless of which tool
//     renders the template afterward.
func loadWorkspaceAppCRForKind(t *testing.T, env e2eEnv, ns string) *v1alpha1.PerUserApp {
	t.Helper()
	path := filepath.Join(packageDir(), "..", "..", "examples", "workspace-app.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var app v1alpha1.PerUserApp
	if err := yaml.UnmarshalStrict(raw, &app); err != nil {
		t.Fatalf("unmarshal %s: %v", path, err)
	}
	app.Namespace = ns

	app.Spec.Workspace.Image = env.WorkspaceImage
	for i := range app.Spec.Workspace.InitContainers {
		app.Spec.Workspace.InitContainers[i].Image = env.WorkspaceImage
	}
	if env.WorkspacePort != 0 {
		app.Spec.Workspace.Port = int32(env.WorkspacePort)
	}

	if seed := app.Spec.Storage.Seed; seed != nil {
		seed.From = workspaceAppFixtureSeedSourcePath
	} else {
		t.Fatal("examples/workspace-app.yaml has no spec.storage.seed; this test's seeded-corpus assertion has nothing to check")
	}

	var sawRenderConfig bool
	for i := range app.Spec.Workspace.InitContainers {
		c := &app.Spec.Workspace.InitContainers[i]
		if c.Name != "render-config" {
			continue
		}
		sawRenderConfig = true
		c.Command = []string{"/bin/sh", "-c", `set -e
echo "waiting for litellm master-key to be mirrored..."
until [ -s /secret/master-key ]; do
  echo "  litellm-secret/master-key not present yet; retrying in 5s"
  sleep 5
done
mkdir -p /workspace/.app
key="$(cat /secret/master-key)"
sed "s#__LITELLM_API_KEY__#$key#" /tmp/workspace-app-template/config.yaml > /workspace/.app/config.yaml
echo "wrote /workspace/.app/config.yaml"
`}
	}
	if !sawRenderConfig {
		t.Fatal("examples/workspace-app.yaml has no render-config initContainer any more; this test's kind-only command rewrite has nothing to rewrite")
	}

	return &app
}

// waitWorkspaceIdle polls the Workspace named name in ns until its
// status.phase is Idle, or ctx expires. The WorkspaceReconciler only ever
// sets Idle once it has confirmed the scaled-down Deployment's pod is
// actually gone (workspace_controller.go), so this is also this test's
// "pod object is gone" wait, not merely a status-field poll.
func waitWorkspaceIdle(ctx context.Context, ns, name string) error {
	var ws v1alpha1.Workspace
	err := pollUntil(ctx, 3*time.Second, func() (bool, error) {
		if err := globalRuntimeClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &ws); err != nil {
			return false, err
		}
		return ws.Status.Phase == v1alpha1.PhaseIdle, nil
	})
	if err != nil {
		return fmt.Errorf("workspace %s/%s did not reach Idle: %w (last phase %q)", ns, name, err, ws.Status.Phase)
	}
	return nil
}

// forceReap patches ws.status.lastActivity into the past, far enough beyond
// idleTimeout that the very next Reaper tick (test/e2e/checks_test.go's
// preamble already confirms the controller is running -- this needs no
// quiesce/restore dance, unlike the destructive assertions in this
// package's other files) finds it idle. This is the standard way this
// operator's own unit suite (internal/controller/reaper_test.go) forces the
// same predicate without waiting real wall-clock idleTimeout minutes; here
// it is a real Status().Patch against the live apiserver instead of a fake
// client.
func forceReap(ctx context.Context, ns, name string, idleTimeout time.Duration) error {
	var ws v1alpha1.Workspace
	if err := globalRuntimeClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &ws); err != nil {
		return fmt.Errorf("get workspace %s/%s: %w", ns, name, err)
	}
	base := ws.DeepCopy()
	past := metav1.NewTime(time.Now().Add(-(idleTimeout + 2*time.Minute)))
	ws.Status.LastActivity = &past
	if err := globalRuntimeClient.Status().Patch(ctx, &ws, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("patch workspace %s/%s status.lastActivity: %w", ns, name, err)
	}
	return nil
}

// TestWorkspaceAppColdStart is task-14-brief.md Step 4, named (not a subtest)
// because Task 15 Step 4 invokes it by `-run TestWorkspaceAppColdStart` against
// the live edge cluster. It provisions env.ColdStartIdentities distinct
// identities against examples/workspace-app.yaml (adapted for kind by
// loadWorkspaceAppCRForKind), confirms the first one's workspace has the seeded
// corpus at the exact fixed path task-14-brief.md names, then forces a reap
// and asserts all four halves spec 445-448 names, in order: the workspace
// pod object is gone, the PVC is still Bound, a file written under
// /workspace before the reap is readable after re-requesting, and a file
// under /workspace/.app is absent.
func TestWorkspaceAppColdStart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	ns := globalEnv.Namespaces[0]
	clientPod := "puc-e2e-client"
	nonce := time.Now().UnixNano()

	if err := ensureWorkspaceAppConfigTemplate(ctx, ns); err != nil {
		t.Fatalf("ensure %s ConfigMap: %v", workspaceAppConfigTemplateName, err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		_ = globalClient.CoreV1().ConfigMaps(ns).Delete(cleanupCtx, workspaceAppConfigTemplateName, metav1.DeleteOptions{})
	})

	app := loadWorkspaceAppCRForKind(t, globalEnv, ns)
	if err := globalRuntimeClient.Create(ctx, app); err != nil {
		t.Fatalf("create PerUserApp %s: %v", app.Name, err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cleanupCancel()
		_ = globalRuntimeClient.Delete(cleanupCtx, app)
	})

	if err := waitDeploymentReady(ctx, globalClient, ns, workspaceAppName+"-router"); err != nil {
		t.Fatalf("wait for %s-router to become Ready: %v", workspaceAppName, err)
	}

	identities := make([]string, globalEnv.ColdStartIdentities)
	for i := range identities {
		identities[i] = fmt.Sprintf("workspace-app-cold-%d-%d", nonce, i)
	}
	if len(identities) == 0 {
		t.Fatal("PUC_E2E_COLD_START_IDENTITIES resolved to zero identities")
	}

	// --- Provision every distinct identity Task 15 needs a distribution
	// over. Workspaces are named ChildName(app, userKey), so re-running one
	// identity would be one cold start plus N-1 idle-resumes, not N cold
	// starts. ---
	for _, id := range identities {
		if code, err := coldStart(globalEnv, ns, clientPod, workspaceAppName, id); err != nil || code != "200" {
			t.Fatalf("cold start identity %q: code=%q err=%v", id, code, err)
		}
	}
	t.Logf("provisioned %d distinct cold-start identities against %s", len(identities), workspaceAppName)

	// --- The remaining assertions all run against the FIRST identity's
	// workspace. ---
	primary := identities[0]
	userKey := identity.UserKey(ns, workspaceAppName, primary)
	name := identity.ChildName(workspaceAppName, userKey)

	if _, err := waitWorkspaceReady(ctx, ns, name); err != nil {
		t.Fatalf("wait for %s/%s to become Ready: %v", ns, name, err)
	}
	pod, err := findWorkspacePod(ctx, globalClient, ns, workspaceAppName, userKey)
	if err != nil {
		t.Fatalf("find primary workspace pod: %v", err)
	}

	// --- Seeded corpus lands at the exact fixed path, not merely
	// "somewhere under /workspace". ---
	got, err := readMarker(globalEnv.Kubeconfig, ns, pod.Name, workspaceAppSeedDestPath)
	if err != nil {
		t.Fatalf("read seeded corpus at %s: %v", workspaceAppSeedDestPath, err)
	}
	if strings.TrimRight(got, "\n") != workspaceAppFixtureSeedContent {
		t.Fatalf("seeded corpus at %s = %q, want %q", workspaceAppSeedDestPath, got, workspaceAppFixtureSeedContent)
	}
	t.Logf("confirmed seeded corpus at %s", workspaceAppSeedDestPath)

	// --- Positive controls for the reap assertions below: write one marker
	// on the persistent volume (expected to survive the reap) and one under
	// the ephemeral .app scratch volume (expected NOT to survive it), and
	// read each back before trusting anything about what happens to them. ---
	const (
		persistentMarker = "/workspace/reap-marker"
		persistentValue  = "reap-survivor"
		scratchMarker       = "/workspace/.app/reap-marker"
		scratchValue        = "reap-casualty"
	)
	if err := writeMarker(globalEnv.Kubeconfig, ns, pod.Name, persistentMarker, persistentValue); err != nil {
		t.Fatalf("write persistent marker: %v", err)
	}
	if got, err := readMarker(globalEnv.Kubeconfig, ns, pod.Name, persistentMarker); err != nil || got != persistentValue {
		t.Fatalf("POSITIVE CONTROL FAILED: could not read back persistent marker (got %q, err %v)", got, err)
	}
	if err := writeMarker(globalEnv.Kubeconfig, ns, pod.Name, scratchMarker, scratchValue); err != nil {
		t.Fatalf("write .app marker: %v", err)
	}
	if got, err := readMarker(globalEnv.Kubeconfig, ns, pod.Name, scratchMarker); err != nil || got != scratchValue {
		t.Fatalf("POSITIVE CONTROL FAILED: could not read back .app marker (got %q, err %v)", got, err)
	}
	t.Log("positive control observed passing: both markers written and read back before the forced reap")

	pvcBefore, err := globalClient.CoreV1().PersistentVolumeClaims(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get PVC %s/%s before reap: %v", ns, name, err)
	}
	if pvcBefore.Status.Phase != corev1.ClaimBound {
		t.Fatalf("PVC %s/%s is not Bound before reap (phase=%s)", ns, name, pvcBefore.Status.Phase)
	}

	// --- Force the reap and wait for the reconciler to confirm Idle. ---
	if err := forceReap(ctx, ns, name, app.Spec.Lifecycle.IdleTimeout.Duration); err != nil {
		t.Fatalf("force reap: %v", err)
	}
	if err := waitWorkspaceIdle(ctx, ns, name); err != nil {
		t.Fatalf("wait for workspace to reach Idle after forced reap: %v", err)
	}

	// --- Half 1: the workspace pod object is gone. waitWorkspaceIdle
	// already implies this (see its doc comment), but assert it directly
	// too: it is one of the four halves spec 445-448 names explicitly, and
	// Task 9's equivalent (TestReapScalesToZeroAndDeletesNothing) is a
	// fake-client unit test that cannot observe a real pod deletion at all.
	if err := waitNoPods(ctx, globalClient, ns, workspacePodLabelSelector(workspaceAppName, userKey)); err != nil {
		t.Fatalf("workspace pod did not disappear after reap: %v", err)
	}
	t.Log("confirmed: workspace pod object is gone")

	// --- Half 2: the PVC is still Bound. Unobservable in envtest (Task 6
	// Step 0: no PV binder there) -- this is the only place in the whole
	// suite that checks it against a real cluster. ---
	pvcAfter, err := globalClient.CoreV1().PersistentVolumeClaims(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get PVC %s/%s after reap: %v", ns, name, err)
	}
	if pvcAfter.Status.Phase != corev1.ClaimBound {
		t.Fatalf("PVC %s/%s is not Bound after reap (phase=%s)", ns, name, pvcAfter.Status.Phase)
	}
	if pvcAfter.UID != pvcBefore.UID {
		t.Fatalf("PVC %s/%s has a DIFFERENT uid after reap (%s vs %s): it was deleted and recreated, not preserved", ns, name, pvcAfter.UID, pvcBefore.UID)
	}
	t.Log("confirmed: PVC is still Bound (same uid) after reap")

	// --- Re-request: this is also the RWOP fast-wake path's precondition
	// (spec 479-484) -- Idle means the volume is free, tested only against
	// a synthetic clock in Task 9. ---
	if code, err := coldStart(globalEnv, ns, clientPod, workspaceAppName, primary); err != nil || code != "200" {
		t.Fatalf("re-request (wake) for primary identity: code=%q err=%v", code, err)
	}
	if _, err := waitWorkspaceReady(ctx, ns, name); err != nil {
		t.Fatalf("wait for workspace to become Ready again after wake: %v", err)
	}
	newPod, err := findWorkspacePod(ctx, globalClient, ns, workspaceAppName, userKey)
	if err != nil {
		t.Fatalf("find re-woken workspace pod: %v", err)
	}

	// --- Half 3: a file written under /workspace before the reap is
	// readable after re-requesting. ---
	if got, err := readMarker(globalEnv.Kubeconfig, ns, newPod.Name, persistentMarker); err != nil || got != persistentValue {
		t.Fatalf("persistent marker did not survive reap+re-request (got %q, err %v)", got, err)
	}
	t.Log("confirmed: persistent marker survived reap+re-request")

	// --- Half 4: a file under /workspace/.app is absent -- it is an
	// emptyDir, recreated empty with the new pod. ---
	if got, err := readMarker(globalEnv.Kubeconfig, ns, newPod.Name, scratchMarker); err == nil {
		t.Fatalf(".app marker survived reap+re-request (got %q), want it absent: the emptyDir did not get a fresh volume", got)
	}
	t.Log("confirmed: .app marker is absent after reap+re-request (fresh emptyDir)")
}
