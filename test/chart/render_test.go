// Package chart_test render-tests charts/per-user-container-operator with
// `helm template` and asserts the rendered objects against Task 4's
// constants (api/v1alpha1) rather than re-typed literals -- see
// task-12-brief.md. Every assertion here exists because the corresponding
// failure is invisible to every other task's test suite: a chart is not Go,
// so nothing compiles it, and "helm template exits 0" passes even when the
// Role granted is wrong, the ServiceMonitor selects nothing, or the image
// referenced by RELATED_IMAGE_ROUTER is a typo.
package chart_test

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"

	"github.com/kettleofketchup/per-user-container-operator/api/v1alpha1"
)

// chartPath resolves the chart directory relative to this test file, not the
// working directory `go test` happens to be invoked from.
func chartPath(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(thisFile), "..", "..", "charts", "per-user-container-operator")
}

// renderOpts is the input to a single `helm template` invocation. Every
// field defaults to a value production would reject outright (empty CIDRs,
// empty watchNamespaces) so a test that forgets to set one fails loudly
// rather than silently reusing another test's namespace list.
type renderOpts struct {
	watchNamespaces []string
	podCIDR         string
	nodeCIDR        string
	metricsPort     int
	serviceMonitor  *bool
	metricsEnabled  *bool
	releaseNS       string
}

func defaultOpts() renderOpts {
	return renderOpts{
		watchNamespaces: []string{"team-a", "team-b"},
		podCIDR:         "10.42.0.0/16",
		nodeCIDR:        "10.0.0.0/24",
		metricsPort:     9090,
		releaseNS:       "puc-system",
	}
}

// helmTemplate invokes `helm template` and returns every rendered
// document, split on the `---` document separators helm emits ahead of each
// `# Source:` comment. It fails the test outright (not skips) if helm is not
// on PATH: Task 12's brief requires either a real render test or a plain
// statement that helm could not be obtained, never a faked pass.
func helmTemplate(t *testing.T, o renderOpts) [][]byte {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Fatalf("helm not found on PATH: %v (required for chart render tests)", err)
	}

	valuesYAML := renderValuesYAML(o)
	valuesFile := filepath.Join(t.TempDir(), "values.yaml")
	if err := os.WriteFile(valuesFile, []byte(valuesYAML), 0o600); err != nil {
		t.Fatalf("write values file: %v", err)
	}

	args := []string{"template", "release-under-test", chartPath(t), "-f", valuesFile, "--namespace", o.releaseNS}
	cmd := exec.Command("helm", args...) //nolint:gosec // fixed argv, test-only
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("helm template failed: %v\nstderr: %s", err, stderr.String())
	}
	return splitYAMLDocs(stdout.Bytes())
}

func renderValuesYAML(o renderOpts) string {
	var b strings.Builder
	fmt.Fprintf(&b, "clusterPodCIDR: %q\n", o.podCIDR)
	fmt.Fprintf(&b, "nodeCIDR: %q\n", o.nodeCIDR)
	b.WriteString("watchNamespaces:\n")
	for _, ns := range o.watchNamespaces {
		fmt.Fprintf(&b, "  - %s\n", ns)
	}
	b.WriteString("metrics:\n")
	if o.metricsEnabled != nil {
		fmt.Fprintf(&b, "  enabled: %t\n", *o.metricsEnabled)
	}
	fmt.Fprintf(&b, "  port: %d\n", o.metricsPort)
	if o.serviceMonitor != nil {
		b.WriteString("  serviceMonitor:\n")
		fmt.Fprintf(&b, "    enabled: %t\n", *o.serviceMonitor)
	}
	return b.String()
}

var docSeparator = regexp.MustCompile(`(?m)^---\s*$`)

func splitYAMLDocs(data []byte) [][]byte {
	parts := docSeparator.Split(string(data), -1)
	var out [][]byte
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		out = append(out, []byte(p))
	}
	return out
}

// typeMeta is decoded from every document first, to route it to the right
// typed struct and to find it by kind/name/namespace without decoding twice.
type typeMeta struct {
	Kind     string `json:"kind"`
	Metadata struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	} `json:"metadata"`
}

// serviceMonitorDoc and prometheusRuleDoc are hand-rolled subsets of the
// Prometheus Operator CRDs: adding that module as a Go dependency just to
// parse two fields would pull an entire API package into this repo for a
// test-only unmarshal. Only the fields this test reads are declared.
type serviceMonitorDoc struct {
	Metadata struct {
		Name      string            `json:"name"`
		Namespace string            `json:"namespace"`
		Labels    map[string]string `json:"labels"`
	} `json:"metadata"`
	Spec struct {
		NamespaceSelector struct {
			MatchNames []string `json:"matchNames"`
		} `json:"namespaceSelector"`
		Selector struct {
			MatchLabels map[string]string `json:"matchLabels"`
		} `json:"selector"`
		Endpoints []struct {
			Port     string `json:"port"`
			Interval string `json:"interval"`
		} `json:"endpoints"`
	} `json:"spec"`
}

type prometheusRuleDoc struct {
	Spec struct {
		Groups []struct {
			Rules []struct {
				Alert string `json:"alert"`
				Expr  string `json:"expr"`
			} `json:"rules"`
		} `json:"groups"`
	} `json:"spec"`
}

// renderedSet decodes every document from a helm template run into typed
// buckets keyed by kind, so each subtest can query "give me the Role named X
// in namespace Y" without re-parsing.
type renderedSet struct {
	serviceAccounts    []corev1.ServiceAccount
	deployments        []appsv1.Deployment
	services           []corev1.Service
	roles              []rbacv1.Role
	roleBindings       []rbacv1.RoleBinding
	clusterRoles       []rbacv1.ClusterRole
	clusterRoleBinding []rbacv1.ClusterRoleBinding
	serviceMonitors    []serviceMonitorDoc
	prometheusRules    []prometheusRuleDoc
}

func decodeRendered(t *testing.T, docs [][]byte) renderedSet {
	t.Helper()
	var set renderedSet
	for _, doc := range docs {
		var tm typeMeta
		if err := yaml.Unmarshal(doc, &tm); err != nil {
			t.Fatalf("decode typeMeta: %v\ndoc:\n%s", err, doc)
		}
		switch tm.Kind {
		case "ServiceAccount":
			var o corev1.ServiceAccount
			mustUnmarshal(t, doc, &o)
			set.serviceAccounts = append(set.serviceAccounts, o)
		case "Deployment":
			var o appsv1.Deployment
			mustUnmarshal(t, doc, &o)
			set.deployments = append(set.deployments, o)
		case "Service":
			var o corev1.Service
			mustUnmarshal(t, doc, &o)
			set.services = append(set.services, o)
		case "Role":
			var o rbacv1.Role
			mustUnmarshal(t, doc, &o)
			set.roles = append(set.roles, o)
		case "RoleBinding":
			var o rbacv1.RoleBinding
			mustUnmarshal(t, doc, &o)
			set.roleBindings = append(set.roleBindings, o)
		case "ClusterRole":
			var o rbacv1.ClusterRole
			mustUnmarshal(t, doc, &o)
			set.clusterRoles = append(set.clusterRoles, o)
		case "ClusterRoleBinding":
			var o rbacv1.ClusterRoleBinding
			mustUnmarshal(t, doc, &o)
			set.clusterRoleBinding = append(set.clusterRoleBinding, o)
		case "ServiceMonitor":
			var o serviceMonitorDoc
			mustUnmarshal(t, doc, &o)
			set.serviceMonitors = append(set.serviceMonitors, o)
		case "PrometheusRule":
			var o prometheusRuleDoc
			mustUnmarshal(t, doc, &o)
			set.prometheusRules = append(set.prometheusRules, o)
		default:
			t.Fatalf("unexpected rendered kind %q", tm.Kind)
		}
	}
	return set
}

func mustUnmarshal(t *testing.T, doc []byte, out interface{}) {
	t.Helper()
	if err := yaml.Unmarshal(doc, out); err != nil {
		t.Fatalf("decode %T: %v\ndoc:\n%s", out, err, doc)
	}
}

// ruleVerbs returns the verb set of the first rule in rules matching group
// and resource, or nil if none matches.
func ruleVerbs(rules []rbacv1.PolicyRule, group, resource string) []string {
	for _, r := range rules {
		if containsStr(r.APIGroups, group) && containsStr(r.Resources, resource) {
			return r.Verbs
		}
	}
	return nil
}

func containsStr(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func sortedCopy(ss []string) []string {
	out := append([]string(nil), ss...)
	sort.Strings(out)
	return out
}

func assertVerbSetEqual(t *testing.T, rules []rbacv1.PolicyRule, group, resource string, want []string) {
	t.Helper()
	got := ruleVerbs(rules, group, resource)
	if got == nil {
		t.Errorf("no rule found for group=%q resource=%q", group, resource)
		return
	}
	gs, ws := sortedCopy(got), sortedCopy(want)
	if strings.Join(gs, ",") != strings.Join(ws, ",") {
		t.Errorf("group=%q resource=%q verbs = %v, want %v", group, resource, gs, ws)
	}
}

// assertVerbSetSuperset asserts every verb in `want` is present in the rule
// matching group/resource -- used for the escalation-check invariant, where
// the controller Role must cover (superset, not exact match) every rule the
// router Role carries.
func assertVerbSetSuperset(t *testing.T, rules []rbacv1.PolicyRule, group, resource string, want []string) {
	t.Helper()
	got := ruleVerbs(rules, group, resource)
	if got == nil {
		t.Errorf("escalation check: controller Role has no rule for group=%q resource=%q (router Role grants %v)", group, resource, want)
		return
	}
	for _, w := range want {
		if !containsStr(got, w) {
			t.Errorf("escalation check: controller Role group=%q resource=%q missing verb %q (router Role grants it); RoleBinding create for the router Role would 403", group, resource, w)
		}
	}
}

func roleInNamespace(t *testing.T, roles []rbacv1.Role, name, ns string) rbacv1.Role {
	t.Helper()
	for _, r := range roles {
		if r.Name == name && r.Namespace == ns {
			return r
		}
	}
	t.Fatalf("no Role named %q in namespace %q found among %d rendered Roles", name, ns, len(roles))
	return rbacv1.Role{}
}

// --- Step 1 assertions -----------------------------------------------------

func TestServiceAccountNameMatchesConstant(t *testing.T) {
	set := decodeRendered(t, helmTemplate(t, defaultOpts()))
	if len(set.serviceAccounts) == 0 {
		t.Fatal("no ServiceAccount rendered")
	}
	var found bool
	for _, sa := range set.serviceAccounts {
		if sa.Name == v1alpha1.OperatorServiceAccountName {
			found = true
		}
	}
	if !found {
		t.Errorf("no ServiceAccount named %q rendered; got names %v", v1alpha1.OperatorServiceAccountName, saNames(set.serviceAccounts))
	}

	if len(set.deployments) == 0 {
		t.Fatal("no Deployment rendered")
	}
	dep := controllerDeployment(t, set.deployments)
	if dep.Spec.Template.Spec.ServiceAccountName != v1alpha1.OperatorServiceAccountName {
		t.Errorf("controller Deployment serviceAccountName = %q, want %q (v1alpha1.OperatorServiceAccountName) -- a PerUserApp setting spec.workspace.serviceAccountName to the release-prefixed chart name would then NOT be rejected by ValidateApp's constant compare", dep.Spec.Template.Spec.ServiceAccountName, v1alpha1.OperatorServiceAccountName)
	}
}

func saNames(sas []corev1.ServiceAccount) []string {
	var out []string
	for _, sa := range sas {
		out = append(out, sa.Name)
	}
	return out
}

// controllerDeployment picks out the controller's own Deployment (there is
// exactly one Deployment in this chart -- the router Deployment is rendered
// at runtime, not by the chart).
func controllerDeployment(t *testing.T, deps []appsv1.Deployment) appsv1.Deployment {
	t.Helper()
	if len(deps) != 1 {
		t.Fatalf("want exactly 1 Deployment rendered by the chart (the controller's; the router Deployment is rendered at runtime), got %d", len(deps))
	}
	return deps[0]
}

func TestClusterRoleBindingBindsServiceAccountToClusterRole(t *testing.T) {
	set := decodeRendered(t, helmTemplate(t, defaultOpts()))
	if len(set.clusterRoleBinding) != 1 {
		t.Fatalf("want exactly 1 ClusterRoleBinding, got %d", len(set.clusterRoleBinding))
	}
	crb := set.clusterRoleBinding[0]
	if crb.RoleRef.Kind != "ClusterRole" {
		t.Errorf("ClusterRoleBinding roleRef.kind = %q, want ClusterRole", crb.RoleRef.Kind)
	}
	var boundSA bool
	for _, s := range crb.Subjects {
		if s.Kind == "ServiceAccount" && s.Name == v1alpha1.OperatorServiceAccountName {
			boundSA = true
		}
	}
	if !boundSA {
		t.Errorf("ClusterRoleBinding does not bind ServiceAccount %q; subjects = %+v", v1alpha1.OperatorServiceAccountName, crb.Subjects)
	}

	if len(set.clusterRoles) != 1 {
		t.Fatalf("want exactly 1 ClusterRole, got %d", len(set.clusterRoles))
	}
	if crb.RoleRef.Name != set.clusterRoles[0].Name {
		t.Errorf("ClusterRoleBinding.roleRef.name = %q, does not match the rendered ClusterRole's name %q -- an unbound ClusterRole means the controller cannot read the CRDs it watches", crb.RoleRef.Name, set.clusterRoles[0].Name)
	}
}

func TestClusterRoleCoversLeaderElectionAndStorageClassesOnly(t *testing.T) {
	set := decodeRendered(t, helmTemplate(t, defaultOpts()))
	if len(set.clusterRoles) != 1 {
		t.Fatalf("want exactly 1 ClusterRole, got %d", len(set.clusterRoles))
	}
	rules := set.clusterRoles[0].Rules

	// The carry-forward defect: ValidateStorageClass needs `get` on
	// storageclasses, which is cluster-scoped. A namespaced Role granting
	// this answers a namespaced SelfSubjectAccessReview Allowed:true while
	// the real client.Get() (never namespaced) is Forbidden -- proven on a
	// real authorizer in Task 11's review. This MUST be here, in the
	// ClusterRole, not in the per-namespace Role tested below.
	scVerbs := ruleVerbs(rules, "storage.k8s.io", "storageclasses")
	if scVerbs == nil {
		t.Fatal("ClusterRole has no rule for storage.k8s.io/storageclasses -- ValidateStorageClass's client.Get() 403s on every reconcile, ConfigValid goes False fleet-wide, and puc_watched_namespace_ready still reports ready because the probe's SAR is deliberately unnamespaced for this exact check")
	}
	for _, v := range []string{"get", "list", "watch"} {
		if !containsStr(scVerbs, v) {
			t.Errorf("ClusterRole storage.k8s.io/storageclasses missing verb %q, got %v", v, scVerbs)
		}
	}

	if ruleVerbs(rules, "coordination.k8s.io", "leases") == nil {
		t.Error("ClusterRole has no rule for coordination.k8s.io/leases (leader election)")
	}

	// apiextensions.k8s.io/customresourcedefinitions is deliberately ABSENT:
	// no Go code path in this operator reads CustomResourceDefinition
	// objects and no +kubebuilder:rbac marker anywhere names that group.
	// Every other grant in this chart traces to a marker or a documented
	// spec defect (storageclasses above); this one would trace to nothing,
	// i.e. unjustified privilege in a chart whose whole point is
	// least-privilege. Asserted as a negative so a future re-addition needs
	// a deliberate edit here, not a silent drift back in.
	if ruleVerbs(rules, "apiextensions.k8s.io", "customresourcedefinitions") != nil {
		t.Error("ClusterRole grants apiextensions.k8s.io/customresourcedefinitions, but nothing in this operator reads CRD objects and no RBAC marker names that group -- remove it, or add a comment naming the concrete code path that needs it")
	}

	// Confirm the storageclasses grant is NOT duplicated onto any
	// per-namespace Role: that would be the natural-looking move the
	// carry-forward note calls out as silently wrong (a namespaced Role can
	// never authorize a cluster-scoped request, but nothing raises an error
	// when you write one anyway).
	for _, r := range set.roles {
		if ruleVerbs(r.Rules, "storage.k8s.io", "storageclasses") != nil {
			t.Errorf("namespaced Role %s/%s grants storage.k8s.io/storageclasses -- this must live ONLY on the ClusterRole; a namespaced grant on a cluster-scoped resource silently fails at authorization time even though SelfSubjectAccessReview says Allowed", r.Namespace, r.Name)
		}
	}
}

func TestControllerRolePerNamespace(t *testing.T) {
	opts := defaultOpts()
	set := decodeRendered(t, helmTemplate(t, opts))

	wantRoleCount := len(opts.watchNamespaces)
	// One controller Role + one router Role per namespace.
	if len(set.roles) != wantRoleCount*2 {
		t.Fatalf("want %d Roles (%d namespaces x controller+router), got %d", wantRoleCount*2, wantRoleCount, len(set.roles))
	}
	if len(set.roleBindings) != wantRoleCount*1 {
		// Only the controller Role gets a chart-rendered RoleBinding here:
		// the router's RoleBinding is created per-app at runtime
		// (RenderRouterRoleBinding, Task 11), naming an app that does not
		// exist at chart-render time.
		t.Fatalf("want %d controller RoleBindings, got %d", wantRoleCount, len(set.roleBindings))
	}

	for _, ns := range opts.watchNamespaces {
		role := roleInNamespace(t, set.roles, "per-user-container-operator", ns)
		rules := role.Rules

		assertVerbSetEqual(t, rules, v1alpha1.GroupVersion.Group, "peruserapps", []string{"get", "list", "watch", "create", "update", "patch", "delete"})
		assertVerbSetEqual(t, rules, v1alpha1.GroupVersion.Group, "workspaces", []string{"get", "list", "watch", "create", "update", "patch", "delete"})
		assertVerbSetEqual(t, rules, v1alpha1.GroupVersion.Group, "peruserapps/status", []string{"get", "update", "patch"})
		assertVerbSetEqual(t, rules, v1alpha1.GroupVersion.Group, "workspaces/status", []string{"get", "update", "patch"})

		for _, resource := range []string{"deployments"} {
			assertVerbSetEqual(t, rules, "apps", resource, []string{"get", "list", "watch", "create", "update", "patch", "delete"})
		}
		for _, resource := range []string{"services", "persistentvolumeclaims", "pods"} {
			assertVerbSetEqual(t, rules, "", resource, []string{"get", "list", "watch", "create", "update", "patch", "delete"})
		}
		assertVerbSetEqual(t, rules, "networking.k8s.io", "networkpolicies", []string{"get", "list", "watch", "create", "update", "patch", "delete"})

		// get/list/watch/create/update/patch on both: matches
		// cmd/controller.go's controllerNamespaceChecks exactly. Found
		// missing (update/patch on serviceaccounts; list/watch/update/patch
		// on rolebindings) while standing up Task 13's E2E harness on a real
		// cluster: envtest's default client bypasses RBAC entirely, so this
		// drift between the chart's rendered Role and the controller's own
		// probe list was invisible to every existing suite. The missing
		// rolebindings list/watch is the one that actually mattered --
		// WorkspaceReconciler/PerUserAppReconciler watch RoleBindings as a
		// child resource, that watch's initial List 403s without it, and
		// the manager's cache never finishes syncing -- so no reconciler
		// ever runs, in any namespace, with no crash and nothing in the
		// logs obviously pointing at RBAC.
		assertVerbSetEqual(t, rules, "", "serviceaccounts", []string{"get", "list", "watch", "create", "update", "patch"})
		assertVerbSetEqual(t, rules, "", "events", []string{"create", "patch"})
		assertVerbSetEqual(t, rules, "discovery.k8s.io", "endpointslices", []string{"get", "list", "watch"})
		assertVerbSetEqual(t, rules, "rbac.authorization.k8s.io", "rolebindings", []string{"get", "list", "watch", "create", "update", "patch"})

		rb := findRoleBinding(t, set.roleBindings, "per-user-container-operator", ns)
		if rb.RoleRef.Name != role.Name || rb.RoleRef.Kind != "Role" {
			t.Errorf("RoleBinding %s/%s roleRef = %+v, want Role %q", ns, rb.Name, rb.RoleRef, role.Name)
		}
		var boundSA bool
		for _, s := range rb.Subjects {
			if s.Kind == "ServiceAccount" && s.Name == v1alpha1.OperatorServiceAccountName && s.Namespace == opts.releaseNS {
				boundSA = true
			}
		}
		if !boundSA {
			t.Errorf("RoleBinding %s/%s does not bind ServiceAccount %s/%s", ns, rb.Name, opts.releaseNS, v1alpha1.OperatorServiceAccountName)
		}
	}
}

func findRoleBinding(t *testing.T, rbs []rbacv1.RoleBinding, name, ns string) rbacv1.RoleBinding {
	t.Helper()
	for _, rb := range rbs {
		if rb.Name == name && rb.Namespace == ns {
			return rb
		}
	}
	t.Fatalf("no RoleBinding named %q in namespace %q", name, ns)
	return rbacv1.RoleBinding{}
}

// TestRouterRoleRulesExact is the "exactly" assertion the brief calls for:
// this IS where the router Role is the artifact under test, unlike the
// controller Role above which is asserted per-resource-group but not
// exhaustively.
func TestRouterRoleRulesExact(t *testing.T) {
	opts := defaultOpts()
	set := decodeRendered(t, helmTemplate(t, opts))

	for _, ns := range opts.watchNamespaces {
		role := roleInNamespace(t, set.roles, v1alpha1.RouterRoleName, ns)
		rules := role.Rules

		assertVerbSetEqual(t, rules, v1alpha1.GroupVersion.Group, "workspaces", []string{"get", "list", "watch", "create"})
		assertVerbSetEqual(t, rules, v1alpha1.GroupVersion.Group, "workspaces/status", []string{"patch"})
		assertVerbSetEqual(t, rules, "", "services", []string{"get", "list", "watch"})
		assertVerbSetEqual(t, rules, "discovery.k8s.io", "endpointslices", []string{"get", "list", "watch"})

		if len(rules) != 4 {
			t.Errorf("router Role %s/%s has %d rules, want exactly 4 (workspaces, workspaces/status, services, endpointslices); got %+v", ns, role.Name, len(rules), rules)
		}
	}
}

// TestControllerRoleCoversEveryRouterRoleRule is the escalation-prevention
// invariant: Kubernetes rejects a RoleBinding create unless the creator
// (the controller) holds every permission in the referenced Role (the
// router Role), or holds `bind` on it. Without this, every <app>-router
// RoleBinding create 403s at the first reconcile of every PerUserApp.
func TestControllerRoleCoversEveryRouterRoleRule(t *testing.T) {
	opts := defaultOpts()
	set := decodeRendered(t, helmTemplate(t, opts))

	for _, ns := range opts.watchNamespaces {
		controllerRole := roleInNamespace(t, set.roles, "per-user-container-operator", ns)
		routerRole := roleInNamespace(t, set.roles, v1alpha1.RouterRoleName, ns)

		for _, r := range routerRole.Rules {
			for _, group := range r.APIGroups {
				for _, resource := range r.Resources {
					assertVerbSetSuperset(t, controllerRole.Rules, group, resource, r.Verbs)
				}
			}
		}
	}
}

func TestServiceMonitors(t *testing.T) {
	opts := defaultOpts()
	set := decodeRendered(t, helmTemplate(t, opts))

	if len(set.serviceMonitors) != 2 {
		t.Fatalf("want exactly 2 ServiceMonitors (router + controller), got %d", len(set.serviceMonitors))
	}

	var routerSM, controllerSM *serviceMonitorDoc
	for i := range set.serviceMonitors {
		sm := &set.serviceMonitors[i]
		switch sm.Spec.Selector.MatchLabels[v1alpha1.LabelComponent] {
		case v1alpha1.ComponentRouter:
			routerSM = sm
		case v1alpha1.ComponentController:
			controllerSM = sm
		}
	}
	if routerSM == nil {
		t.Fatal("no ServiceMonitor selects LabelComponent=router")
	}
	if controllerSM == nil {
		t.Fatal("no ServiceMonitor selects LabelComponent=controller")
	}

	if routerSM.Spec.Selector.MatchLabels[v1alpha1.LabelPartOf] != v1alpha1.PartOfValue {
		t.Errorf("router ServiceMonitor selector missing LabelPartOf=%s, got %+v", v1alpha1.PartOfValue, routerSM.Spec.Selector.MatchLabels)
	}
	gotNS := sortedCopy(routerSM.Spec.NamespaceSelector.MatchNames)
	wantNS := sortedCopy(opts.watchNamespaces)
	if strings.Join(gotNS, ",") != strings.Join(wantNS, ",") {
		t.Errorf("router ServiceMonitor namespaceSelector.matchNames = %v, want %v (the SAME watchNamespaces list that renders the Roles) -- a bare spec.selector with no namespaceSelector defaults to the ServiceMonitor's OWN namespace and matches nothing, since router Services are created at runtime in the watched namespaces", gotNS, wantNS)
	}

	if controllerSM.Spec.Selector.MatchLabels[v1alpha1.LabelPartOf] != v1alpha1.PartOfValue {
		t.Errorf("controller ServiceMonitor selector missing LabelPartOf=%s, got %+v", v1alpha1.PartOfValue, controllerSM.Spec.Selector.MatchLabels)
	}
	// Selecting on LabelPartOf alone would ALSO match every router Service
	// in every watched namespace -- double-scraping the router and giving
	// up{...controller...} a meaning Task 13 assertion 4 does not expect.
	if len(controllerSM.Spec.Selector.MatchLabels) != 2 {
		t.Errorf("controller ServiceMonitor selector = %+v, want exactly the two labels (LabelPartOf, LabelComponent) -- selecting on LabelPartOf alone also matches every router Service", controllerSM.Spec.Selector.MatchLabels)
	}
}

func TestControllerMetricsServiceAndFlagsTrackTheSameValue(t *testing.T) {
	opts := defaultOpts()
	opts.metricsPort = 9999 // deliberately non-default: proves these track the VALUE, not the v1alpha1.MetricsPort constant.
	set := decodeRendered(t, helmTemplate(t, opts))

	var metricsSvc *corev1.Service
	for i := range set.services {
		svc := &set.services[i]
		if svc.Labels[v1alpha1.LabelComponent] == v1alpha1.ComponentController {
			metricsSvc = svc
		}
	}
	if metricsSvc == nil {
		t.Fatal("no Service carries LabelComponent=controller")
	}
	if metricsSvc.Labels[v1alpha1.LabelPartOf] != v1alpha1.PartOfValue {
		t.Errorf("controller metrics Service missing LabelPartOf=%s, got %+v", v1alpha1.PartOfValue, metricsSvc.Labels)
	}
	if len(metricsSvc.Spec.Ports) != 1 {
		t.Fatalf("controller metrics Service has %d ports, want 1", len(metricsSvc.Spec.Ports))
	}
	port := metricsSvc.Spec.Ports[0]
	if port.Port != int32(opts.metricsPort) {
		t.Errorf("controller metrics Service port = %d, want %d (from .Values.metrics.port)", port.Port, opts.metricsPort)
	}
	if port.TargetPort.IntValue() != opts.metricsPort && port.TargetPort.StrVal != "metrics" {
		t.Errorf("controller metrics Service targetPort = %+v, want %d or a named port resolving to it", port.TargetPort, opts.metricsPort)
	}

	dep := controllerDeployment(t, set.deployments)
	container := dep.Spec.Template.Spec.Containers[0]

	var containerMetricsPort int32 = -1
	for _, p := range container.Ports {
		if p.Name == "metrics" {
			containerMetricsPort = p.ContainerPort
		}
	}
	if containerMetricsPort != int32(opts.metricsPort) {
		t.Errorf("controller container 'metrics' port = %d, want %d", containerMetricsPort, opts.metricsPort)
	}

	wantFlag := fmt.Sprintf("--metrics-addr=:%d", opts.metricsPort)
	if !containsStr(container.Args, wantFlag) {
		t.Errorf("controller container args %v does not contain %q", container.Args, wantFlag)
	}
}

func TestServiceMonitorEnabledFalseRendersNoServiceMonitorsButKeepsService(t *testing.T) {
	opts := defaultOpts()
	f := false
	opts.serviceMonitor = &f
	set := decodeRendered(t, helmTemplate(t, opts))

	if len(set.serviceMonitors) != 0 {
		t.Errorf("metrics.serviceMonitor.enabled=false: want 0 ServiceMonitors, got %d", len(set.serviceMonitors))
	}
	var found bool
	for _, svc := range set.services {
		if svc.Labels[v1alpha1.LabelComponent] == v1alpha1.ComponentController {
			found = true
		}
	}
	if !found {
		t.Error("metrics.serviceMonitor.enabled=false: the controller metrics Service must still render")
	}
}

func TestMetricsEnabledFalseRendersNoMetricsObjects(t *testing.T) {
	opts := defaultOpts()
	f := false
	opts.metricsEnabled = &f
	set := decodeRendered(t, helmTemplate(t, opts))

	if len(set.serviceMonitors) != 0 {
		t.Errorf("metrics.enabled=false: want 0 ServiceMonitors, got %d", len(set.serviceMonitors))
	}
	for _, svc := range set.services {
		if svc.Labels[v1alpha1.LabelComponent] == v1alpha1.ComponentController {
			t.Error("metrics.enabled=false: the controller metrics Service must not render")
		}
	}
	if len(set.prometheusRules) != 0 {
		t.Errorf("metrics.enabled=false: want 0 PrometheusRules, got %d", len(set.prometheusRules))
	}
}

func TestControllerDeploymentStartupContract(t *testing.T) {
	opts := defaultOpts()
	set := decodeRendered(t, helmTemplate(t, opts))
	dep := controllerDeployment(t, set.deployments)
	container := dep.Spec.Template.Spec.Containers[0]

	wantImage := "per-user-container-operator:dev"
	if container.Image != wantImage {
		t.Errorf("controller image = %q, want %q (Task 1's IMG split on the colon)", container.Image, wantImage)
	}

	var relatedImageRouter string
	var podName, podNamespace bool
	for _, e := range container.Env {
		switch e.Name {
		case "RELATED_IMAGE_ROUTER":
			relatedImageRouter = e.Value
		case "POD_NAME":
			if e.ValueFrom != nil && e.ValueFrom.FieldRef != nil && e.ValueFrom.FieldRef.FieldPath == "metadata.name" {
				podName = true
			}
		case "POD_NAMESPACE":
			if e.ValueFrom != nil && e.ValueFrom.FieldRef != nil && e.ValueFrom.FieldRef.FieldPath == "metadata.namespace" {
				podNamespace = true
			}
		}
	}
	if relatedImageRouter != wantImage {
		t.Errorf("RELATED_IMAGE_ROUTER = %q, want %q (the SAME rendered reference as the controller image -- both binaries are one image selected by subcommand); an unset value is not a startup nicety, it is how the airgap extractor finds the router image at all", relatedImageRouter, wantImage)
	}
	if !podName {
		t.Error("container env has no POD_NAME sourced from the downward API (metadata.name)")
	}
	if !podNamespace {
		t.Error("container env has no POD_NAMESPACE sourced from the downward API (metadata.namespace)")
	}

	wantArgs := map[string]bool{
		"--pod-cidr=" + opts.podCIDR:   false,
		"--node-cidr=" + opts.nodeCIDR: false,
	}
	for _, a := range container.Args {
		if _, ok := wantArgs[a]; ok {
			wantArgs[a] = true
		}
	}
	for a, found := range wantArgs {
		if !found {
			t.Errorf("controller container args %v missing %q -- left unwired, workspace/router egress NetworkPolicies render with empty CIDRs and Calico drops everything the positive E2E expects to pass", container.Args, a)
		}
	}

	watchNSFlag := findArgWithPrefix(container.Args, "--watch-namespaces=")
	if watchNSFlag == "" {
		t.Fatal("controller container args missing --watch-namespaces")
	}
	gotNS := sortedCopy(strings.Split(strings.TrimPrefix(watchNSFlag, "--watch-namespaces="), ","))
	wantNS := sortedCopy(opts.watchNamespaces)
	if strings.Join(gotNS, ",") != strings.Join(wantNS, ",") {
		t.Errorf("--watch-namespaces = %v, want %v (the SAME list that renders the per-namespace Roles) -- a mismatch leaves the manager's cache cluster-scoped against namespace-scoped Roles and the controller CrashLoops on every install", gotNS, wantNS)
	}

	// The flag's namespace set must be identical to the namespaces the
	// Roles were actually rendered for -- the four consumers of one list
	// (Roles, RoleBindings, both ServiceMonitors, this flag) cannot drift.
	roleNamespaces := map[string]bool{}
	for _, r := range set.roles {
		if r.Name == "per-user-container-operator" {
			roleNamespaces[r.Namespace] = true
		}
	}
	for _, ns := range gotNS {
		if !roleNamespaces[ns] {
			t.Errorf("--watch-namespaces names %q, but no controller Role was rendered in that namespace", ns)
		}
	}
	for ns := range roleNamespaces {
		if !containsStr(gotNS, ns) {
			t.Errorf("controller Role rendered in namespace %q, but --watch-namespaces does not name it", ns)
		}
	}
}

func findArgWithPrefix(args []string, prefix string) string {
	for _, a := range args {
		if strings.HasPrefix(a, prefix) {
			return a
		}
	}
	return ""
}

// TestPrometheusRuleShipsTheFivePinnedExpressions is relocated from Task 7,
// which could not `helm template` a chart that did not exist yet. Task 7
// owns the expressions and the series they name; this task owns that they
// actually ship. The five strings below are pinned as data in Task 7's own
// test (internal/metrics/metrics_test.go) and reproduced verbatim here.
func TestPrometheusRuleShipsTheFivePinnedExpressions(t *testing.T) {
	set := decodeRendered(t, helmTemplate(t, defaultOpts()))
	if len(set.prometheusRules) != 1 {
		t.Fatalf("want exactly 1 PrometheusRule, got %d", len(set.prometheusRules))
	}

	wantExprs := []string{
		`sum(puc_controller_leader) != 1`,
		`time() - puc_reaper_last_completion_timestamp_seconds > 900`,
		`puc_watched_namespace_ready == 0`,
		`sum(rate(puc_router_identity_rejected_total[5m])) by (namespace, app) > 0`,
		`sum(puc_workspaces{phase="Failed"}) / sum(puc_workspaces) > 0.25`,
	}

	var gotExprs []string
	for _, g := range set.prometheusRules[0].Spec.Groups {
		for _, r := range g.Rules {
			gotExprs = append(gotExprs, r.Expr)
		}
	}

	for _, want := range wantExprs {
		if !containsStr(gotExprs, want) {
			t.Errorf("PrometheusRule missing expression %q; got %v", want, gotExprs)
		}
	}
}
