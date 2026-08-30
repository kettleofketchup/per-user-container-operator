//go:build envtest

package envtest

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"

	"github.com/kettleofketchup/per-user-container-operator/api/v1alpha1"
)

// chartDir resolves charts/per-user-container-operator relative to this
// file, mirroring test/chart/render_test.go's chartPath -- kept as its own
// small copy rather than an import across test packages, since
// test/chart's helpers are unexported _test.go internals.
func chartDir(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(thisFile), "..", "..", "charts", "per-user-container-operator")
}

var chartDocSeparator = regexp.MustCompile(`(?m)^---\s*$`)

// renderRouterRoleFromChart runs `helm template` against
// charts/per-user-container-operator with a single watched namespace ns and
// extracts the rendered router Role -- the SAME object
// TestRouterRoleGrantsExactlyWhatTheRouterDoes applies to a real
// impersonated client below. Task 12 renders this Role; Task 6 originally
// asserted against a hand-written fixture that could never catch a chart
// Role missing (for example) workspaces/status patch, since the fixture
// only ever asserted itself.
func renderRouterRoleFromChart(t *testing.T, ns string) *rbacv1.Role {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Fatalf("helm not found on PATH: %v (required to render the router Role from the chart)", err)
	}

	valuesYAML := fmt.Sprintf("clusterPodCIDR: \"10.42.0.0/16\"\nnodeCIDR: \"10.0.0.0/24\"\nwatchNamespaces:\n  - %s\n", ns)
	valuesFile := filepath.Join(t.TempDir(), "values.yaml")
	if err := os.WriteFile(valuesFile, []byte(valuesYAML), 0o600); err != nil {
		t.Fatalf("write values file: %v", err)
	}

	cmd := exec.Command("helm", "template", "envtest-under-test", chartDir(t), "-f", valuesFile) //nolint:gosec // fixed argv, test-only
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("helm template failed: %v\nstderr: %s", err, stderr.String())
	}

	for _, doc := range chartDocSeparator.Split(stdout.String(), -1) {
		doc = strings.TrimSpace(doc)
		if doc == "" {
			continue
		}
		var tm struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Name      string `json:"name"`
				Namespace string `json:"namespace"`
			} `json:"metadata"`
		}
		if err := yaml.Unmarshal([]byte(doc), &tm); err != nil {
			t.Fatalf("decode rendered doc: %v", err)
		}
		if tm.Kind != "Role" || tm.Metadata.Name != v1alpha1.RouterRoleName || tm.Metadata.Namespace != ns {
			continue
		}
		var role rbacv1.Role
		if err := yaml.Unmarshal([]byte(doc), &role); err != nil {
			t.Fatalf("decode router Role: %v", err)
		}
		// The chart renders this Role cluster-wide-namespace-agnostic; pin
		// it to the caller's actual namespace so mustCreate targets the
		// envtest-created namespace under test rather than the one the
		// chart happened to render for.
		role.Namespace = ns
		role.ResourceVersion = ""
		return &role
	}
	t.Fatalf("chart did not render a Role named %q in namespace %q", v1alpha1.RouterRoleName, ns)
	return nil
}
