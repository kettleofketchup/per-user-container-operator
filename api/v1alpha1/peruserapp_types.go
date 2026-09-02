package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SecretHeaderRef points at a secret value that is presented as an HTTP
// header, optionally with a scheme prefix (e.g. "Bearer").
type SecretHeaderRef struct {
	SecretRef corev1.SecretKeySelector `json:"secretRef"`
	Header    string                   `json:"header"`
	Scheme    string                   `json:"scheme,omitempty"`
}

// IdentitySpec configures how the router extracts a caller's raw identity
// from an inbound HTTP header.
type IdentitySpec struct {
	// Exactly one header. A first-present-wins list is a silent re-keying
	// machine: the day entry 1 stops being sent every user falls through to
	// entry 2 and lands in a fresh empty volume, with no request failing.
	// +kubebuilder:validation:MinLength=1
	Header string `json:"header"`
	// +kubebuilder:default=256
	// +kubebuilder:validation:Minimum=1
	MaxLength int32 `json:"maxLength,omitempty"`
}

// There is deliberately no Required field on IdentitySpec.

// WorkspaceTemplateSpec is the pod template used to render each user's
// workspace Deployment.
type WorkspaceTemplateSpec struct {
	// +kubebuilder:validation:MinLength=1
	Image           string            `json:"image"`
	ImagePullPolicy corev1.PullPolicy `json:"imagePullPolicy,omitempty"`
	// +kubebuilder:validation:Minimum=1
	Port      int32                       `json:"port"`
	Env       []corev1.EnvVar             `json:"env,omitempty"`
	EnvFrom   []corev1.EnvFromSource      `json:"envFrom,omitempty"`
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
	// What the router presents upstream after stripping the caller's credential.
	UpstreamAuth *SecretHeaderRef `json:"upstreamAuth,omitempty"`
	// Value, not pointer: ValidateApp reads FSGroup unconditionally and a nil
	// pointer would make the fsGroup negative test panic instead of failing.
	PodSecurityContext           corev1.PodSecurityContext `json:"podSecurityContext,omitempty"`
	SecurityContext              corev1.SecurityContext    `json:"securityContext,omitempty"`
	ServiceAccountName           string                    `json:"serviceAccountName,omitempty"`
	AutomountServiceAccountToken *bool                     `json:"automountServiceAccountToken,omitempty"`
	// configMap | secret | emptyDir | projected only — allowlist, see ValidateApp.
	Volumes        []corev1.Volume      `json:"volumes,omitempty"`
	VolumeMounts   []corev1.VolumeMount `json:"volumeMounts,omitempty"`
	InitContainers []corev1.Container   `json:"initContainers,omitempty"`
	Command        []string             `json:"command,omitempty"`
	Args           []string             `json:"args,omitempty"`
	ReadinessProbe *corev1.Probe        `json:"readinessProbe,omitempty"`
	LivenessProbe  *corev1.Probe        `json:"livenessProbe,omitempty"`
	NodeSelector   map[string]string    `json:"nodeSelector,omitempty"`
	Tolerations    []corev1.Toleration  `json:"tolerations,omitempty"`
	// +kubebuilder:default=10
	TerminationGracePeriodSeconds *int64 `json:"terminationGracePeriodSeconds,omitempty"`
}

// RouterIngressSpec controls which peers may reach the router.
type RouterIngressSpec struct {
	FromTraefik bool                             `json:"fromTraefik,omitempty"`
	From        []networkingv1.NetworkPolicyPeer `json:"from,omitempty"`
}

// NetworkSpec configures the NetworkPolicies rendered for this app's
// workspaces and router.
//
// The workspaceEgress CEL rule below nests two unbounded arrays
// (workspaceEgress itself, and each rule's to peers). Without a maxItems
// bound on both, the API server's CEL cost estimator refuses to install
// this CRD at all ("estimated rule cost exceeds budget"): the estimator
// prices the nested self.workspaceEgress.all(r, r.to.all(...)) as
// N (outer length) * M (inner length), and an unbounded array's assumed
// worst-case cardinality is large enough that the product blows the
// budget by orders of magnitude. The maxItems:20 below bounds N;
// controller-gen cannot bound M (r.to) because that field is declared on
// the upstream networkingv1.NetworkPolicyEgressRule type, not here — so
// config/crd/apps.kettleofketchup_peruserapps.yaml carries a matching
// maxItems:20 hand-patched onto network.workspaceEgress.items.properties.to.
// `make manifests` will NOT reproduce that patch (controller-gen has no
// marker to attach there); until Task 4/5's API design gives egress peers
// a bounded first-party type, `make manifests` output for this field must
// be re-patched by hand and TestSelectorEgressPeerIsAcceptedByTheAPIServer
// (test/envtest/cel_test.go) re-run to confirm the CRD still installs.
//
// +kubebuilder:validation:XValidation:rule="self.workspaceEgress.all(r, r.to.size() > 0 && r.to.all(p, has(p.ipBlock) || has(p.podSelector) || has(p.namespaceSelector)))",message="every workspaceEgress rule needs a non-empty to, and every peer must set ipBlock, podSelector or namespaceSelector: an absent to means allow-to-anywhere, reaching every other workspace and the router"
type NetworkSpec struct {
	// +kubebuilder:validation:MaxItems=20
	//
	// WorkspaceEgress declares the peers a workspace pod may initiate
	// traffic to, beyond the DNS-anywhere and per-app router rules this
	// operator always renders. All three NetworkPolicyPeer kinds are
	// accepted (ipBlock, podSelector, namespaceSelector) -- but not
	// interchangeably for reaching a Kubernetes Service:
	//
	// A Service's ClusterIP is a virtual address that kube-proxy DNAT-rewrites
	// to a real backend pod IP before ordinary NetworkPolicy egress rules are
	// evaluated (this is standard Kubernetes NetworkPolicy semantics, true of
	// every CNI enforcing it via iptables/ipvs, not a Calico- or
	// implementation-specific quirk). An ipBlock peer naming that ClusterIP's
	// /32 therefore never matches the packet policy actually evaluates, and
	// the rule renders, applies, and silently drops every connection to that
	// Service -- this operator's own router NetworkPolicy avoids the exact
	// same trap for its apiserver access by using the node's CIDR rather
	// than the apiserver Service's ClusterIP (see RenderRouterNetworkPolicy).
	//
	// To let a workspace reach a Service, select its BACKING PODS instead --
	// a podSelector (with a namespaceSelector when the Service lives in
	// another namespace) resolves to those pods' real, post-DNAT IPs and is
	// evaluated correctly. Reserve ipBlock for a genuinely IP-addressed peer
	// (an external endpoint, a raw pod or node CIDR).
	WorkspaceEgress []networkingv1.NetworkPolicyEgressRule `json:"workspaceEgress"`
	RouterIngress   RouterIngressSpec                      `json:"routerIngress"`
}

// SeedSpec configures how a fresh per-user volume is populated on first use.
type SeedSpec struct {
	// The claim is mounted HERE in the seeder, never at mountPath — mounting at
	// mountPath shadows the copy source and every user gets an empty workspace.
	StagingMountPath string `json:"stagingMountPath"`
	From             string `json:"from"`
}

// StorageSpec configures the per-user PersistentVolumeClaim.
//
// +kubebuilder:validation:XValidation:rule="quantity(self.size).compareTo(quantity(oldSelf.size)) >= 0",message="storage.size may only grow; a shrink would strand bound volumes"
type StorageSpec struct {
	Size resource.Quantity `json:"size"`
	// +kubebuilder:validation:MinLength=1
	StorageClassName string    `json:"storageClassName"`
	MountPath        string    `json:"mountPath"`
	Seed             *SeedSpec `json:"seed,omitempty"`
}

// LimitsSpec bounds how many workspaces this app may run and how many may
// start concurrently.
type LimitsSpec struct {
	// +kubebuilder:validation:Minimum=1
	MaxWorkspaces int32 `json:"maxWorkspaces"`
	// +kubebuilder:validation:Minimum=1
	MaxConcurrentStarts int32 `json:"maxConcurrentStarts"`
}

// LifecycleSpec configures scale-to-zero and reaping timers.
//
// +kubebuilder:validation:XValidation:rule="duration(self.reapInterval) < duration(self.idleTimeout)",message="reapInterval must be well under idleTimeout or nothing is ever reaped"
type LifecycleSpec struct {
	IdleTimeout                 metav1.Duration `json:"idleTimeout"`
	StartupTimeout              metav1.Duration `json:"startupTimeout"`
	BackoffMax                  metav1.Duration `json:"backoffMax"`
	ReapInterval                metav1.Duration `json:"reapInterval"`
	ConnectionHeartbeatInterval metav1.Duration `json:"connectionHeartbeatInterval"`
}

// ReclaimSpec configures least-recently-used reclamation of per-user
// storage. It is the ONLY part of this API that destroys a user's data, so
// it is absent by default and every clause below is a floor rather than a
// target: a workspace is reclaimed only when the app is over
// targetWorkspaces AND that workspace has been idle longer than minIdleAge
// AND it is already scaled down. Widening any one clause alone reclaims
// nothing extra.
//
// The two count knobs are deliberately different things. limits.maxWorkspaces
// is a HARD refusal — the router answers 503 workspace_limit at it, and the
// admission gate never exceeds it. targetWorkspaces is the level reclamation
// restores the app to, and must sit strictly below the hard cap: equal values
// would mean the first reclaim pass only runs once the app is already
// refusing users, which is the outage this exists to prevent.
//
// +kubebuilder:validation:XValidation:rule="duration(self.minIdleAge) > duration(self.interval)",message="minIdleAge must exceed interval, or a single sweep can reclaim a workspace that went idle between two ticks"
type ReclaimSpec struct {
	// Off unless explicitly turned on. An operator adopting this API must
	// opt into data deletion in the same commit that sets the thresholds,
	// rather than inheriting it from a default.
	//
	// +kubebuilder:default=false
	Enabled bool `json:"enabled"`

	// The workspace count a sweep reclaims down to. Must be strictly below
	// limits.maxWorkspaces (enforced by CEL on PerUserAppSpec).
	//
	// +kubebuilder:validation:Minimum=1
	TargetWorkspaces int32 `json:"targetWorkspaces"`

	// The absolute floor: a workspace whose status.lastActivity is more
	// recent than this is never a candidate, however far over target the app
	// is. If every workspace is fresher than minIdleAge the sweep reclaims
	// NOTHING and the app stays over target — deliberately, because the
	// alternative is deleting the files of somebody who used the app this
	// morning to make room for somebody who wants it now.
	MinIdleAge metav1.Duration `json:"minIdleAge"`

	// How often a sweep runs, gated per app exactly as lifecycle.reapInterval
	// gates the Reaper.
	Interval metav1.Duration `json:"interval"`

	// What reclamation actually frees.
	//
	// false (the default): the Workspace and its PVC are deleted and nothing
	// else. Because the operator only accepts Retain-reclaim StorageClasses,
	// the bound PersistentVolume survives in phase Released with the backing
	// volume intact — the user's files are still recoverable by hand, and NO
	// disk space is freed. This reclaims the limits.maxWorkspaces slot, not
	// the storage.
	//
	// true: the bound PersistentVolume's persistentVolumeReclaimPolicy is
	// patched from Retain to Delete BEFORE the PVC is deleted, so releasing
	// it makes the CSI driver destroy the backing volume. This is the only
	// setting that frees disk, and it is IRREVERSIBLE: once the driver has
	// deleted the image there is nothing left to rebind. It also requires the
	// controller to hold patch on the cluster-scoped persistentvolumes
	// resource; without that grant a sweep fails closed and deletes neither
	// the claim nor the volume, rather than deleting the claim and silently
	// freeing nothing.
	//
	// +kubebuilder:default=false
	// +optional
	DeleteVolumeData bool `json:"deleteVolumeData,omitempty"`
}

// RouterSpec configures the shared router Deployment for this app.
type RouterSpec struct {
	// +kubebuilder:default=2
	Replicas  int32                       `json:"replicas,omitempty"`
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
	// Defaults to RELATED_IMAGE_ROUTER; set only for development.
	Image string `json:"image,omitempty"`
	// +kubebuilder:validation:Minimum=1
	ColdStartHoldSeconds int32 `json:"coldStartHoldSeconds"`

	// Exact request paths served without an identity header.
	//
	// A GET or HEAD that passes caller auth, matches one of these paths
	// exactly, and carries NO identity header is served as the reserved user
	// identity.Shared rather than rejected. Nothing downstream is special-
	// cased: that user gets an ordinary Workspace, holds a
	// limits.maxWorkspaces slot, and is reaped and reclaimed like any other.
	//
	// This exists because a client discovering the app's API has no user in
	// scope yet — it fetches the schema before it can know whose session it
	// will open — so that one fetch cannot carry an identity however the
	// client is configured. Without a path listed here such a client sees
	// the app as unreachable and every later request fails, even though the
	// per-user routing behind it works.
	//
	// The match is exact and never a prefix. A listed path is a route into a
	// real container that no user had to identify themselves to reach, so
	// widening one to a subtree would expose the app's whole surface that
	// way. List only user-independent, read-only metadata endpoints. Caller
	// auth remains mandatory here, and a caller that supplies the reserved
	// identity itself is rejected rather than honoured.
	//
	// +kubebuilder:validation:MaxItems=8
	// +kubebuilder:validation:XValidation:rule="self.all(p, p.startsWith('/'))",message="every sharedPaths entry must be an absolute path beginning with /"
	// +optional
	SharedPaths []string `json:"sharedPaths,omitempty"`
}

// PerUserAppSpec defines the desired state of a PerUserApp.
//
// +kubebuilder:validation:XValidation:rule="!has(self.reclaim) || self.reclaim.targetWorkspaces < self.limits.maxWorkspaces",message="reclaim.targetWorkspaces must be strictly below limits.maxWorkspaces: at equal values reclamation only starts once the router is already refusing users with 503 workspace_limit"
// +kubebuilder:validation:XValidation:rule="!has(self.reclaim) || duration(self.reclaim.minIdleAge) > duration(self.lifecycle.idleTimeout)",message="reclaim.minIdleAge must exceed lifecycle.idleTimeout: a floor below it would delete the files of a user the reaper had only just scaled down"
type PerUserAppSpec struct {
	Identity IdentitySpec `json:"identity"`
	// Mandatory, never optional. Anyone who can reach the router directly can
	// set the identity header and become any user — including the workspace
	// pods, since both consumers hand the user an interactive session in the same namespace.
	CallerAuth SecretHeaderRef       `json:"callerAuth"`
	Workspace  WorkspaceTemplateSpec `json:"workspace"`
	Network    NetworkSpec           `json:"network"`
	Storage    StorageSpec           `json:"storage"`
	Limits     LimitsSpec            `json:"limits"`
	Lifecycle  LifecycleSpec         `json:"lifecycle"`
	Router     RouterSpec            `json:"router"`
	// Absent means no workspace is ever reclaimed and no user's data is ever
	// deleted, which is why this is the one spec field with no default value
	// at all rather than a disabled-by-default struct.
	//
	// +optional
	Reclaim *ReclaimSpec `json:"reclaim,omitempty"`
}

// PerUserAppStatus defines the observed state of a PerUserApp.
type PerUserAppStatus struct {
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	RouterReady        bool               `json:"routerReady,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
}

// PerUserApp is the Schema for the peruserapps API. It describes an HTTP
// application that gets one container and one volume per authenticated user.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:validation:XValidation:rule="self.metadata.name.size() <= 27",message="PerUserApp name must be <= 27 chars: pod name budget is app + 1 + userKey(18) + 1 + pod-template-hash(10) + 1 + random(5) <= 63"
type PerUserApp struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              PerUserAppSpec   `json:"spec,omitempty"`
	Status            PerUserAppStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// PerUserAppList contains a list of PerUserApp.
type PerUserAppList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PerUserApp `json:"items"`
}
