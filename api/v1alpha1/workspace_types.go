package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// WorkspaceSpec defines the desired state of a Workspace: the app it belongs
// to and the user it belongs to.
type WorkspaceSpec struct {
	AppRef corev1.LocalObjectReference `json:"appRef"`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="userKey is create-only: changing it re-points this workspace at another user's PVC"
	UserKey string `json:"userKey"`
}

// ConnectionEntry is one router replica's view of live connections for a
// Workspace, self-expiring via HeartbeatAt.
//
// One entry per router replica, self-expiring. An absolute count written by two
// replicas is last-writer-wins on a number only correct per replica: the replica
// holding none of a user's connections would zero the one that holds them and
// the reaper would kill a live session.
type ConnectionEntry struct {
	Count       int32       `json:"count"`
	HeartbeatAt metav1.Time `json:"heartbeatAt"`
}

// WorkspaceStatus defines the observed state of a Workspace.
type WorkspaceStatus struct {
	ObservedGeneration int64        `json:"observedGeneration,omitempty"`
	Phase              Phase        `json:"phase,omitempty"`
	ScaledDown         bool         `json:"scaledDown,omitempty"`
	LastActivity       *metav1.Time `json:"lastActivity,omitempty"`
	// Keyed by router replica pod name.
	Connections map[string]ConnectionEntry `json:"connections,omitempty"`
	// Set by the router the moment a request arrives for a scaled-down
	// workspace, so a wake interleaved with a reap loses the race instead of
	// the user losing their pod.
	WakeRequestedAt *metav1.Time `json:"wakeRequestedAt,omitempty"`
	// FIFO ordering key for maxConcurrentStarts. creationTimestamp is
	// second-granular, so a burst of simultaneous first requests would have no
	// total order; this is written by the router with nanosecond precision on
	// the Pending write and never rewritten.
	EnqueuedAt          *metav1.Time `json:"enqueuedAt,omitempty"`
	ConsecutiveFailures int32        `json:"consecutiveFailures,omitempty"`
	BackoffUntil        *metav1.Time `json:"backoffUntil,omitempty"`
	// Absolute, so it survives leader failover.
	StartDeadline *metav1.Time `json:"startDeadline,omitempty"`
	// Copied verbatim from the pod's waiting.reason — the only channel by which
	// an airgap ErrImageNeverPull becomes visible per user.
	WaitingReason string `json:"waitingReason,omitempty"`
	// The largest size this claim has ever been observed Bound at, written by
	// the workspace controller when it first sees the PVC Bound and thereafter
	// only raised. This is the state spec 388-391's controller-side shrink
	// refusal compares against: CEL transition rules cover a straight update,
	// but prune-and-recreate bypasses them entirely and there is nothing else
	// on the object recording what the volume actually is.
	LargestObservedSize *resource.Quantity `json:"largestObservedSize,omitempty"`
	Conditions          []metav1.Condition `json:"conditions,omitempty"`
	PodRef              string             `json:"podRef,omitempty"`
	ServiceRef          string             `json:"serviceRef,omitempty"`
	PVCRef              string             `json:"pvcRef,omitempty"`
}

// Workspace is the Schema for the workspaces API. It represents one
// authenticated user's container and volume for a given PerUserApp.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
type Workspace struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              WorkspaceSpec   `json:"spec,omitempty"`
	Status            WorkspaceStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// WorkspaceList contains a list of Workspace.
type WorkspaceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Workspace `json:"items"`
}
