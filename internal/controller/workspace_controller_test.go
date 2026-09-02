package controller

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kettleofketchup/per-user-container-operator/api/v1alpha1"
	"github.com/kettleofketchup/per-user-container-operator/internal/identity"
)

// getWorkspaceDeployment reads back the Deployment ensureDeployment acts on,
// so a test can assert against what the API server holds rather than against
// the object it handed in.
func getWorkspaceDeployment(t *testing.T, c client.Client, ns, app, userKey string) *appsv1.Deployment {
	t.Helper()
	var dep appsv1.Deployment
	key := types.NamespacedName{Namespace: ns, Name: identity.ChildName(app, userKey)}
	if err := c.Get(context.Background(), key, &dep); err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	return &dep
}

// TestEnsureDeploymentAppliesATemplateChangeToAnExistingWorkspace is the
// reason ensureDeployment exists. A workspace Deployment is created once and
// then outlives many PerUserApp revisions; before this, an image bump reached
// only users who had no workspace yet.
func TestEnsureDeploymentAppliesATemplateChangeToAnExistingWorkspace(t *testing.T) {
	ns := "ns-template-change"
	app := appWithLimits(ns, 5)
	ws := readyWorkspace(app, "alice")

	old := app.DeepCopy()
	old.Spec.Workspace.Image = "example.invalid/workspace:v1"
	live := RenderWorkspaceDeployment(old, ws)

	app.Spec.Workspace.Image = "example.invalid/workspace:v2"

	c := newFakeClient(t, app, ws, live)
	rec := &WorkspaceReconciler{Client: c, Scheme: newAdmissionScheme(t)}

	if err := rec.ensureDeployment(context.Background(), ws, app, 1); err != nil {
		t.Fatalf("ensureDeployment: %v", err)
	}

	got := getWorkspaceDeployment(t, c, ns, app.Name, ws.Spec.UserKey)
	if img := got.Spec.Template.Spec.Containers[0].Image; img != "example.invalid/workspace:v2" {
		t.Errorf("image = %q, want the current render's image", img)
	}
	want := RenderWorkspaceDeployment(app, ws).Annotations[v1alpha1.AnnSpecHash]
	if got.Annotations[v1alpha1.AnnSpecHash] != want {
		t.Errorf("spec hash = %q, want %q", got.Annotations[v1alpha1.AnnSpecHash], want)
	}
	if got.Spec.Replicas == nil || *got.Spec.Replicas != 1 {
		t.Errorf("replicas = %v, want 1", got.Spec.Replicas)
	}
}

// TestEnsureDeploymentLeavesAnUnchangedWorkspaceUntouched guards the other
// half: a reconcile that finds nothing to do must write nothing at all.
// Patching unconditionally would restart every workspace on every pass.
func TestEnsureDeploymentLeavesAnUnchangedWorkspaceUntouched(t *testing.T) {
	ns := "ns-no-change"
	app := appWithLimits(ns, 5)
	ws := readyWorkspace(app, "bob")
	live := RenderWorkspaceDeployment(app, ws)

	c := newFakeClient(t, app, ws, live)
	rec := &WorkspaceReconciler{Client: c, Scheme: newAdmissionScheme(t)}

	before := getWorkspaceDeployment(t, c, ns, app.Name, ws.Spec.UserKey).ResourceVersion
	if err := rec.ensureDeployment(context.Background(), ws, app, 1); err != nil {
		t.Fatalf("ensureDeployment: %v", err)
	}
	after := getWorkspaceDeployment(t, c, ns, app.Name, ws.Spec.UserKey).ResourceVersion
	if before != after {
		t.Errorf("resourceVersion moved from %s to %s; an unchanged workspace was written", before, after)
	}
}

// TestEnsureDeploymentScalesAWorkspaceItDidNotRender covers the idle path:
// the spec is current, only replicas differ, and the workspace must scale
// without its template being rewritten.
func TestEnsureDeploymentScalesAWorkspaceItDidNotRender(t *testing.T) {
	ns := "ns-scale"
	app := appWithLimits(ns, 5)
	ws := readyWorkspace(app, "carol")
	live := RenderWorkspaceDeployment(app, ws)

	c := newFakeClient(t, app, ws, live)
	rec := &WorkspaceReconciler{Client: c, Scheme: newAdmissionScheme(t)}

	if err := rec.ensureDeployment(context.Background(), ws, app, 0); err != nil {
		t.Fatalf("ensureDeployment: %v", err)
	}
	got := getWorkspaceDeployment(t, c, ns, app.Name, ws.Spec.UserKey)
	if got.Spec.Replicas == nil || *got.Spec.Replicas != 0 {
		t.Fatalf("replicas = %v, want 0", got.Spec.Replicas)
	}
}

// TestWorkspaceSpecHashIgnoresReplicas is what keeps the idle cycle from
// reading as a template change: this operator drives replicas itself, so a
// hash that covered them would mark every scaled-down workspace stale and
// rewrite its template on the way back up.
func TestWorkspaceSpecHashIgnoresReplicas(t *testing.T) {
	app := appWithLimits("ns-hash", 5)
	ws := readyWorkspace(app, "dave")

	one := RenderWorkspaceDeployment(app, ws)
	zero := RenderWorkspaceDeployment(app, ws)
	var scaled int32
	zero.Spec.Replicas = &scaled

	if WorkspaceSpecHash(one) != WorkspaceSpecHash(zero) {
		t.Error("replicas changed the spec hash; idle cycles will look like template changes")
	}

	changed := app.DeepCopy()
	changed.Spec.Workspace.Image = "example.invalid/workspace:other"
	if WorkspaceSpecHash(one) == WorkspaceSpecHash(RenderWorkspaceDeployment(changed, ws)) {
		t.Error("a different image produced the same spec hash; drift would go undetected")
	}
}
