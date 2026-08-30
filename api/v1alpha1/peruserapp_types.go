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
// +kubebuilder:validation:XValidation:rule="self.workspaceEgress.all(r, r.to.size() > 0 && r.to.all(p, has(p.ipBlock)))",message="every workspaceEgress rule needs a non-empty to, and every peer must be ipBlock: an absent to means allow-to-anywhere, and Calico evaluates egress pre-DNAT so a selector-based peer renders, applies and silently drops"
type NetworkSpec struct {
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

// RouterSpec configures the shared router Deployment for this app.
type RouterSpec struct {
	// +kubebuilder:default=2
	Replicas  int32                       `json:"replicas,omitempty"`
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
	// Defaults to RELATED_IMAGE_ROUTER; set only for development.
	Image string `json:"image,omitempty"`
	// +kubebuilder:validation:Minimum=1
	ColdStartHoldSeconds int32 `json:"coldStartHoldSeconds"`
}

// PerUserAppSpec defines the desired state of a PerUserApp.
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
