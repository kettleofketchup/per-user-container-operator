// Package testfixtures holds the shared PerUserApp/Workspace fixtures used by
// Task 5's renderer tests and, via this exported non-test package, by Task 6
// and Task 11's test suites as well. It must stay free of *testing.T so every
// consumer package can import it regardless of its own test layout.
package testfixtures

import (
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kettleofketchup/per-user-container-operator/api/v1alpha1"
	"github.com/kettleofketchup/per-user-container-operator/internal/identity"
)

// Ptr returns a pointer to v. Generic helper for building literal pointer
// fields (bool, int64, ...) inline in fixtures.
func Ptr[T any](v T) *T { return &v }

// Dur parses s as a Go duration and wraps it as a metav1.Duration. It panics
// on a malformed literal: fixtures are compiled-in constants, not user input.
func Dur(s string) metav1.Duration {
	d, err := time.ParseDuration(s)
	if err != nil {
		panic("testfixtures.Dur: " + err.Error())
	}
	return metav1.Duration{Duration: d}
}

// ValidApp returns a PerUserApp that passes ValidateApp and every CEL rule:
// a real storage block (so the fsGroup negative test is non-vacuous), a
// mandatory callerAuth, a numeric runAsUser under runAsNonRoot, an
// automountServiceAccountToken:false, and declared network policy peers.
func ValidApp() *v1alpha1.PerUserApp {
	one := int64(1000)
	f := int64(1000)
	no := false
	return &v1alpha1.PerUserApp{
		ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "ns"},
		Spec: v1alpha1.PerUserAppSpec{
			Identity: v1alpha1.IdentitySpec{Header: "X-User-Id", MaxLength: 256},
			CallerAuth: v1alpha1.SecretHeaderRef{
				SecretRef: corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "app-router"},
					Key:                  "api-key",
				},
				Header: "Authorization",
				Scheme: "Bearer",
			},
			Workspace: v1alpha1.WorkspaceTemplateSpec{
				Image: "example/app:1", Port: 8000,
				PodSecurityContext:           corev1.PodSecurityContext{RunAsNonRoot: Ptr(true), RunAsUser: &one, RunAsGroup: &one, FSGroup: &f},
				AutomountServiceAccountToken: &no,
				Env:                          []corev1.EnvVar{{Name: "HOME", Value: "/home/user"}},
			},
			Network: v1alpha1.NetworkSpec{
				WorkspaceEgress: []networkingv1.NetworkPolicyEgressRule{{To: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: "10.0.0.0/8"}}}}},
				RouterIngress:   v1alpha1.RouterIngressSpec{From: []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": "caller"}}}}},
			},
			Storage:   v1alpha1.StorageSpec{Size: resource.MustParse("10Gi"), StorageClassName: "ceph-block-static", MountPath: "/home/user"},
			Limits:    v1alpha1.LimitsSpec{MaxWorkspaces: 200, MaxConcurrentStarts: 5},
			Lifecycle: v1alpha1.LifecycleSpec{IdleTimeout: Dur("30m"), StartupTimeout: Dur("180s"), BackoffMax: Dur("30m"), ReapInterval: Dur("60s"), ConnectionHeartbeatInterval: Dur("450s")},
			Router:    v1alpha1.RouterSpec{Replicas: 2, ColdStartHoldSeconds: 300},
		},
	}
}

// ValidWorkspace returns a Workspace for user "alice" belonging to the
// ValidApp fixture, with the labels and annotations the renderers and
// NetworkPolicy tests expect.
func ValidWorkspace() *v1alpha1.Workspace {
	k := identity.UserKey("ns", "app", "alice")
	return &v1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{
			Name: identity.ChildName("app", k), Namespace: "ns",
			Labels:      map[string]string{v1alpha1.LabelApp: "app", v1alpha1.LabelUserKey: k},
			Annotations: map[string]string{v1alpha1.AnnUserDisplay: "alice@corp.example"},
		},
		Spec: v1alpha1.WorkspaceSpec{AppRef: corev1.LocalObjectReference{Name: "app"}, UserKey: k},
	}
}
