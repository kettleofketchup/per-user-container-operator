// Package rbacspec is the single source of truth for the (group, resource,
// verb) tuples the controller's own RBAC needs, in every served namespace.
// cmd/controller.go's startup verb probe (probeNamespace) and the Helm
// chart's rendered Role/ClusterRole (asserted against this same list in
// test/chart) both read from here.
//
// Before this package existed, cmd/controller.go's probe list and
// test/chart's expected verb sets were two hand-maintained copies of the
// same requirement, and they drifted: the chart's rendered Role was missing
// update/patch on serviceaccounts and list/watch/update/patch on
// rolebindings, and every existing suite passed anyway. envtest's default
// client bypasses RBAC entirely, so the drift was invisible there; the
// missing rolebindings list/watch is what actually mattered in production
// shape -- WorkspaceReconciler/PerUserAppReconciler watch RoleBindings as a
// child resource, that watch's initial List 403s under the narrower grant,
// and controller-runtime's manager cache never finishes syncing, so no
// reconciler ever runs, in any namespace, with no crash and nothing in the
// logs pointing at RBAC. Task 13a found this only by standing up the chart
// on a real cluster. Moving the list here, and testing the chart's rendered
// Role/ClusterRole against it directly (rather than a second hand-written
// copy), is what makes that class of drift fail a test instead of a real
// cluster.
package rbacspec

import "github.com/kettleofketchup/per-user-container-operator/api/v1alpha1"

// NamespaceCheck is one (group, resource, verb) the controller's startup
// probe attempts in a served namespace, and the chart's rendered Role (or
// ClusterRole, for ClusterScoped entries) must grant.
//
// ClusterScoped marks a resource that is NOT namespaced (storageclasses is
// the only one today). This field exists specifically so a cluster-scoped
// addition is a deliberate, visible choice at the call site, not a string
// some future reader has to recognize by name: a SelfSubjectAccessReview
// evaluates exactly the hypothesis it is handed, and a NAMESPACED Role
// granting a verb on a cluster-scoped resource answers Allowed:true for a
// namespaced SAR even though the real client.Get() against that
// cluster-scoped object is Forbidden -- passing a namespace asks an easier
// question than the request the controller actually makes. See
// cmd/controller.go's probeNamespace doc comment for how this field
// changes the SAR sent.
type NamespaceCheck struct {
	Group, Resource, Verb string
	ClusterScoped         bool
}

// ControllerNamespaceChecks mirrors every +kubebuilder:rbac marker across
// WorkspaceReconciler and PerUserAppReconciler: the verbs each actually
// uses, per resource. Every entry here is namespaced EXCEPT the three
// storageclasses checks, marked ClusterScoped: true -- storage.k8s.io
// StorageClass is spec 556's one stated exception to "nothing else is
// cluster-scoped," and the chart grants it via a ClusterRole rather than
// the per-namespace Role this list otherwise probes.
var ControllerNamespaceChecks = []NamespaceCheck{
	{Group: v1alpha1.GroupVersion.Group, Resource: "peruserapps", Verb: "get"},
	{Group: v1alpha1.GroupVersion.Group, Resource: "peruserapps", Verb: "list"},
	{Group: v1alpha1.GroupVersion.Group, Resource: "peruserapps", Verb: "watch"},
	{Group: v1alpha1.GroupVersion.Group, Resource: "peruserapps/status", Verb: "get"},
	{Group: v1alpha1.GroupVersion.Group, Resource: "peruserapps/status", Verb: "update"},
	{Group: v1alpha1.GroupVersion.Group, Resource: "peruserapps/status", Verb: "patch"},
	{Group: v1alpha1.GroupVersion.Group, Resource: "workspaces", Verb: "get"},
	{Group: v1alpha1.GroupVersion.Group, Resource: "workspaces", Verb: "list"},
	{Group: v1alpha1.GroupVersion.Group, Resource: "workspaces", Verb: "watch"},
	{Group: v1alpha1.GroupVersion.Group, Resource: "workspaces", Verb: "create"},
	{Group: v1alpha1.GroupVersion.Group, Resource: "workspaces", Verb: "update"},
	{Group: v1alpha1.GroupVersion.Group, Resource: "workspaces", Verb: "patch"},
	{Group: v1alpha1.GroupVersion.Group, Resource: "workspaces", Verb: "delete"},
	{Group: v1alpha1.GroupVersion.Group, Resource: "workspaces/status", Verb: "get"},
	{Group: v1alpha1.GroupVersion.Group, Resource: "workspaces/status", Verb: "update"},
	{Group: v1alpha1.GroupVersion.Group, Resource: "workspaces/status", Verb: "patch"},
	{Group: v1alpha1.GroupVersion.Group, Resource: "workspaces/finalizers", Verb: "update"},
	{Group: "apps", Resource: "deployments", Verb: "get"},
	{Group: "apps", Resource: "deployments", Verb: "list"},
	{Group: "apps", Resource: "deployments", Verb: "watch"},
	{Group: "apps", Resource: "deployments", Verb: "create"},
	{Group: "apps", Resource: "deployments", Verb: "update"},
	{Group: "apps", Resource: "deployments", Verb: "patch"},
	{Group: "", Resource: "services", Verb: "get"},
	{Group: "", Resource: "services", Verb: "list"},
	{Group: "", Resource: "services", Verb: "watch"},
	{Group: "", Resource: "services", Verb: "create"},
	{Group: "", Resource: "services", Verb: "update"},
	{Group: "", Resource: "services", Verb: "patch"},
	{Group: "", Resource: "persistentvolumeclaims", Verb: "get"},
	{Group: "", Resource: "persistentvolumeclaims", Verb: "list"},
	{Group: "", Resource: "persistentvolumeclaims", Verb: "watch"},
	{Group: "", Resource: "persistentvolumeclaims", Verb: "create"},
	{Group: "", Resource: "persistentvolumeclaims", Verb: "update"},
	{Group: "", Resource: "persistentvolumeclaims", Verb: "patch"},
	{Group: "", Resource: "pods", Verb: "get"},
	{Group: "", Resource: "pods", Verb: "list"},
	{Group: "", Resource: "pods", Verb: "watch"},
	{Group: "", Resource: "serviceaccounts", Verb: "get"},
	{Group: "", Resource: "serviceaccounts", Verb: "list"},
	{Group: "", Resource: "serviceaccounts", Verb: "watch"},
	{Group: "", Resource: "serviceaccounts", Verb: "create"},
	{Group: "", Resource: "serviceaccounts", Verb: "update"},
	{Group: "", Resource: "serviceaccounts", Verb: "patch"},
	{Group: "", Resource: "events", Verb: "create"},
	{Group: "", Resource: "events", Verb: "patch"},
	{Group: "networking.k8s.io", Resource: "networkpolicies", Verb: "get"},
	{Group: "networking.k8s.io", Resource: "networkpolicies", Verb: "list"},
	{Group: "networking.k8s.io", Resource: "networkpolicies", Verb: "watch"},
	{Group: "networking.k8s.io", Resource: "networkpolicies", Verb: "create"},
	{Group: "networking.k8s.io", Resource: "networkpolicies", Verb: "update"},
	{Group: "networking.k8s.io", Resource: "networkpolicies", Verb: "patch"},
	{Group: "rbac.authorization.k8s.io", Resource: "rolebindings", Verb: "get"},
	{Group: "rbac.authorization.k8s.io", Resource: "rolebindings", Verb: "list"},
	{Group: "rbac.authorization.k8s.io", Resource: "rolebindings", Verb: "watch"},
	{Group: "rbac.authorization.k8s.io", Resource: "rolebindings", Verb: "create"},
	{Group: "rbac.authorization.k8s.io", Resource: "rolebindings", Verb: "update"},
	{Group: "rbac.authorization.k8s.io", Resource: "rolebindings", Verb: "patch"},
	{Group: "storage.k8s.io", Resource: "storageclasses", Verb: "get", ClusterScoped: true},
	{Group: "storage.k8s.io", Resource: "storageclasses", Verb: "list", ClusterScoped: true},
	{Group: "storage.k8s.io", Resource: "storageclasses", Verb: "watch", ClusterScoped: true},
}
