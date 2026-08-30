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
		"self.workspaceEgress.all(r, r.to.size() > 0 && r.to.all(p, has(p.ipBlock)))",
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
