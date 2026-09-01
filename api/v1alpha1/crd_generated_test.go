package v1alpha1

import (
	"os"
	"path/filepath"
	"testing"

	"sigs.k8s.io/yaml"
)

// findRule walks a decoded CRD document looking for an
// x-kubernetes-validations entry whose rule contains want.
func findRule(t *testing.T, doc map[string]interface{}, want string) bool {
	t.Helper()
	var walk func(interface{}) bool
	walk = func(v interface{}) bool {
		switch val := v.(type) {
		case map[string]interface{}:
			if rules, ok := val["x-kubernetes-validations"].([]interface{}); ok {
				for _, r := range rules {
					rm, ok := r.(map[string]interface{})
					if !ok {
						continue
					}
					rule, _ := rm["rule"].(string)
					if rule == want {
						return true
					}
				}
			}
			for _, child := range val {
				if walk(child) {
					return true
				}
			}
		case []interface{}:
			for _, child := range val {
				if walk(child) {
					return true
				}
			}
		}
		return false
	}
	return walk(doc)
}

// TestGeneratedCRDContainsCELRules asserts the CEL rules actually landed in
// the generated CRD schema in config/crd/, not merely as source text. This
// is the real check Task 4 Step 4 requires ("config/crd/ contains both
// CRDs"); Task 6's envtest is the check against a running API server.
func TestGeneratedCRDContainsCELRules(t *testing.T) {
	appCRDPath := filepath.Join("..", "..", "config", "crd", "apps.kettleofketchup_peruserapps.yaml")
	wsCRDPath := filepath.Join("..", "..", "config", "crd", "apps.kettleofketchup_workspaces.yaml")

	appBytes, err := os.ReadFile(appCRDPath)
	if err != nil {
		t.Fatalf("read %s: %v (did `make manifests` run?)", appCRDPath, err)
	}
	var appDoc map[string]interface{}
	if err := yaml.Unmarshal(appBytes, &appDoc); err != nil {
		t.Fatalf("parse %s: %v", appCRDPath, err)
	}
	if kind, _ := appDoc["kind"].(string); kind != "CustomResourceDefinition" {
		t.Fatalf("%s: kind = %q, want CustomResourceDefinition", appCRDPath, kind)
	}

	for _, rule := range []string{
		"self.metadata.name.size() <= 27",
		"quantity(self.size).compareTo(quantity(oldSelf.size)) >= 0",
		"self.workspaceEgress.all(r, r.to.size() > 0 && r.to.all(p, has(p.ipBlock) || has(p.podSelector) || has(p.namespaceSelector)))",
		"duration(self.reapInterval) < duration(self.idleTimeout)",
	} {
		if !findRule(t, appDoc, rule) {
			t.Errorf("generated PerUserApp CRD missing CEL rule %q", rule)
		}
	}

	wsBytes, err := os.ReadFile(wsCRDPath)
	if err != nil {
		t.Fatalf("read %s: %v (did `make manifests` run?)", wsCRDPath, err)
	}
	var wsDoc map[string]interface{}
	if err := yaml.Unmarshal(wsBytes, &wsDoc); err != nil {
		t.Fatalf("parse %s: %v", wsCRDPath, err)
	}
	if kind, _ := wsDoc["kind"].(string); kind != "CustomResourceDefinition" {
		t.Fatalf("%s: kind = %q, want CustomResourceDefinition", wsCRDPath, kind)
	}
	if !findRule(t, wsDoc, "self == oldSelf") {
		t.Error("generated Workspace CRD missing userKey immutability rule (self == oldSelf)")
	}
}

// maxWorkspaceEgressItems mirrors hack/patch-crds.py's bound. The CEL rule
// on NetworkSpec nests two unbounded arrays (workspaceEgress itself, and
// each rule's `to` peers): self.workspaceEgress.all(r, r.to.all(p, ...)).
// The API server's CEL cost estimator prices that as N*M and refuses to
// install the CRD at all without a maxItems bound on BOTH -- this is the
// bug test/envtest found the hard way (the CRD would not install). Bump
// this constant only alongside api/v1alpha1/peruserapp_types.go's
// +kubebuilder:validation:MaxItems marker and hack/patch-crds.py's inner
// bound, together.
const maxWorkspaceEgressItems = 20

// navigate walks doc following a sequence of map keys, returning nil if any
// step is missing or not itself a map.
func navigate(doc map[string]interface{}, keys ...string) map[string]interface{} {
	cur := doc
	for _, k := range keys {
		next, ok := cur[k]
		if !ok {
			return nil
		}
		m, ok := next.(map[string]interface{})
		if !ok {
			return nil
		}
		cur = m
	}
	return cur
}

// assertBoundedMaxItems requires schema to carry a maxItems bound in
// (0, maxWorkspaceEgressItems] -- present at all is not enough; an
// unbounded neighbor still blows the CEL cost budget, and a bound too
// permissive to actually cap the estimator's cost is the same failure with
// extra steps.
func assertBoundedMaxItems(t *testing.T, schema map[string]interface{}, path string) {
	t.Helper()
	raw, ok := schema["maxItems"]
	if !ok {
		t.Fatalf("%s: no maxItems bound; the CEL cost estimator refuses to install this CRD without one on every array the workspaceEgress CEL rule nests (see hack/patch-crds.py)", path)
	}
	n, ok := raw.(float64)
	if !ok {
		t.Fatalf("%s: maxItems is %T (%v), want a number", path, raw, raw)
	}
	if n <= 0 || n > maxWorkspaceEgressItems {
		t.Fatalf("%s: maxItems = %v, want > 0 and <= %d", path, n, maxWorkspaceEgressItems)
	}
}

// TestGeneratedCRDBoundsWorkspaceEgressCELCost asserts network.workspaceEgress
// AND its nested `to` peer list both carry a maxItems bound, not just one of
// the two: bounding only the outer array (the one controller-gen can reach
// from a marker on WorkspaceEgress) still leaves N*bounded * M*unbounded,
// which is exactly the shape that failed to install before this task fixed
// it. This assertion is CI-gated (`make lint test`) — unlike test/envtest,
// which requires KUBEBUILDER_ASSETS and is not wired into any CI job — so it
// is what actually catches a regression if hack/patch-crds.py is ever
// skipped or someone hand-edits NetworkSpec without re-running `make
// manifests`.
func TestGeneratedCRDBoundsWorkspaceEgressCELCost(t *testing.T) {
	appCRDPath := filepath.Join("..", "..", "config", "crd", "apps.kettleofketchup_peruserapps.yaml")
	appBytes, err := os.ReadFile(appCRDPath)
	if err != nil {
		t.Fatalf("read %s: %v (did `make manifests` run?)", appCRDPath, err)
	}
	var appDoc map[string]interface{}
	if err := yaml.Unmarshal(appBytes, &appDoc); err != nil {
		t.Fatalf("parse %s: %v", appCRDPath, err)
	}

	crdSpec, _ := appDoc["spec"].(map[string]interface{})
	versions, _ := crdSpec["versions"].([]interface{})
	if len(versions) == 0 {
		t.Fatal("CRD has no spec.versions")
	}
	version, ok := versions[0].(map[string]interface{})
	if !ok {
		t.Fatal("spec.versions[0] is not an object")
	}

	workspaceEgress := navigate(version, "schema", "openAPIV3Schema", "properties", "spec", "properties", "network", "properties", "workspaceEgress")
	if workspaceEgress == nil {
		t.Fatal("could not find network.workspaceEgress in the generated schema")
	}
	assertBoundedMaxItems(t, workspaceEgress, "network.workspaceEgress")

	toSchema := navigate(workspaceEgress, "items", "properties", "to")
	if toSchema == nil {
		t.Fatal("could not find network.workspaceEgress[].to in the generated schema")
	}
	assertBoundedMaxItems(t, toSchema, "network.workspaceEgress[].to")
}
