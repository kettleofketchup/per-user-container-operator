// Package controller renders the Kubernetes objects for a per-user
// workspace and validates a PerUserApp spec against the invariants that keep
// one user's container and volume from reaching another's. Every function in
// this file is pure: CR in, objects out, no client — so it is table-testable
// without an API server (Tasks 6 and 11 depend on that).
package controller

import (
	"fmt"
	"reflect"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/kettleofketchup/per-user-container-operator/api/v1alpha1"
	"github.com/kettleofketchup/per-user-container-operator/internal/identity"
)

const (
	// defaultTerminationGracePeriodSeconds matches WorkspaceTemplateSpec's own
	// +kubebuilder:default=10 — duplicated here because this package renders
	// from the Go struct directly and cannot rely on API-server defaulting.
	defaultTerminationGracePeriodSeconds int64 = 10
	// defaultMemoryEmptyDirSizeLimit bounds a memory-medium emptyDir: its
	// pages count against the pod memory limit, so an unbounded one is an
	// unbounded claim against that limit.
	defaultMemoryEmptyDirSizeLimit = "16Mi"
	// workspaceContainerName is the rendered workspace container and its
	// declared port name; probe port resolution below depends on this name
	// being the one and only container port this package ever declares.
	workspaceContainerName = "workspace"
	workspacePortName      = "http"
	seedContainerName      = "seed"
)

// RouterPodLabels is the single source of the labels a router pod carries
// and the labels the workspace ingress NetworkPolicy selects on. All three
// are required on the selector: the two app-agnostic ones alone would make
// the policy per-namespace instead of per-app.
func RouterPodLabels(appName string) map[string]string {
	return map[string]string{
		v1alpha1.LabelApp:       appName,
		v1alpha1.LabelComponent: v1alpha1.ComponentRouter,
		v1alpha1.LabelPartOf:    v1alpha1.PartOfValue,
	}
}

// WorkspacePodLabels is the single source of the labels a workspace pod
// carries, used both to stamp the pod template and as the Deployment/Service
// selector.
func WorkspacePodLabels(appName, userKey string) map[string]string {
	return map[string]string{
		v1alpha1.LabelApp:       appName,
		v1alpha1.LabelUserKey:   userKey,
		v1alpha1.LabelComponent: v1alpha1.ComponentWorkspace,
		v1alpha1.LabelPartOf:    v1alpha1.PartOfValue,
	}
}

func workspaceOwnerRef(ws *v1alpha1.Workspace) metav1.OwnerReference {
	return *metav1.NewControllerRef(ws, v1alpha1.GroupVersion.WithKind("Workspace"))
}

func appOwnerRef(app *v1alpha1.PerUserApp) metav1.OwnerReference {
	return *metav1.NewControllerRef(app, v1alpha1.GroupVersion.WithKind("PerUserApp"))
}

// childName is the shared name for a user's per-app child resources
// (Deployment, Service, PVC): identity.ChildName(app, userKey).
func childName(app *v1alpha1.PerUserApp, ws *v1alpha1.Workspace) string {
	return identity.ChildName(app.Name, ws.Spec.UserKey)
}

// withDefaultedMemoryEmptyDir returns vols with a default sizeLimit applied
// to every memory-medium emptyDir that does not declare one: tmpfs pages
// count against the pod memory limit, so an unbounded one is effectively an
// unbounded claim against it.
func withDefaultedMemoryEmptyDir(vols []corev1.Volume) []corev1.Volume {
	out := make([]corev1.Volume, len(vols))
	for i, v := range vols {
		if v.EmptyDir != nil && v.EmptyDir.Medium == corev1.StorageMediumMemory && v.EmptyDir.SizeLimit == nil {
			ed := *v.EmptyDir
			limit := resource.MustParse(defaultMemoryEmptyDirSizeLimit)
			ed.SizeLimit = &limit
			v.EmptyDir = &ed
		}
		out[i] = v
	}
	return out
}

// resolveProbePort resolves the readiness probe's HTTPGet.Port against the
// container's declared ports, falling back to spec.workspace.port when the
// probe is absent or the name does not resolve. HTTPGet.Port is an
// IntOrString and a named port reads as the numeric zero if read directly
// instead of resolved — rendering a NetworkPolicy rule for port 0 admits
// nothing, so every kubelet probe would be blocked and every cold start
// would die at startupTimeout with no error naming the cause.
func resolveProbePort(app *v1alpha1.PerUserApp) int32 {
	probe := app.Spec.Workspace.ReadinessProbe
	if probe == nil || probe.HTTPGet == nil {
		return app.Spec.Workspace.Port
	}
	port := probe.HTTPGet.Port
	if port.Type == intstr.Int {
		if port.IntVal > 0 {
			return port.IntVal
		}
		return app.Spec.Workspace.Port
	}
	if port.StrVal == workspacePortName {
		return app.Spec.Workspace.Port
	}
	return app.Spec.Workspace.Port
}

// RenderWorkspaceDeployment renders the per-user workspace Deployment: one
// replica (0 when scaled down), Recreate strategy (RWOP would reject a
// second pod under RollingUpdate), the per-user PVC mounted at
// storage.mountPath, and — when storage.seed is set — a seed init container
// that runs before any user-declared initContainers.
func RenderWorkspaceDeployment(app *v1alpha1.PerUserApp, ws *v1alpha1.Workspace) *appsv1.Deployment {
	wt := app.Spec.Workspace
	name := childName(app, ws)
	podLabels := WorkspacePodLabels(app.Name, ws.Spec.UserKey)

	var replicas int32 = 1
	if ws.Status.ScaledDown {
		replicas = 0
	}

	termGrace := wt.TerminationGracePeriodSeconds
	if termGrace == nil {
		g := defaultTerminationGracePeriodSeconds
		termGrace = &g
	}

	pvcVolume := corev1.Volume{
		Name: v1alpha1.PVCVolumeName,
		VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: name},
		},
	}
	volumes := append([]corev1.Volume{pvcVolume}, withDefaultedMemoryEmptyDir(wt.Volumes)...)

	volumeMounts := append([]corev1.VolumeMount{{Name: v1alpha1.PVCVolumeName, MountPath: app.Spec.Storage.MountPath}}, wt.VolumeMounts...)

	psc := wt.PodSecurityContext
	sc := wt.SecurityContext

	container := corev1.Container{
		Name:            workspaceContainerName,
		Image:           wt.Image,
		ImagePullPolicy: wt.ImagePullPolicy,
		Ports:           []corev1.ContainerPort{{Name: workspacePortName, ContainerPort: wt.Port}},
		Env:             wt.Env,
		EnvFrom:         wt.EnvFrom,
		Resources:       wt.Resources,
		SecurityContext: &sc,
		Command:         wt.Command,
		Args:            wt.Args,
		ReadinessProbe:  wt.ReadinessProbe,
		LivenessProbe:   wt.LivenessProbe,
		VolumeMounts:    volumeMounts,
	}

	var initContainers []corev1.Container
	if seed := app.Spec.Storage.Seed; seed != nil {
		initContainers = append(initContainers, corev1.Container{
			// The seeder's image and command are not the implementer's
			// choice: the corpus lives inside the workspace image, so a
			// generic utility image would cp an empty source and silently
			// hand every user an empty workspace.
			Name:            seedContainerName,
			Image:           wt.Image,
			ImagePullPolicy: wt.ImagePullPolicy,
			Command:         []string{"sh", "-c", fmt.Sprintf("cp -an %s %s/", seed.From, seed.StagingMountPath)},
			VolumeMounts:    []corev1.VolumeMount{{Name: v1alpha1.PVCVolumeName, MountPath: seed.StagingMountPath}},
		})
	}
	initContainers = append(initContainers, wt.InitContainers...)

	serviceAccountName := wt.ServiceAccountName
	if serviceAccountName == "" {
		serviceAccountName = app.Name + "-workspace"
	}

	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       ws.Namespace,
			Labels:          podLabels,
			OwnerReferences: []metav1.OwnerReference{workspaceOwnerRef(ws)},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: podLabels},
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: podLabels},
				Spec: corev1.PodSpec{
					ServiceAccountName: serviceAccountName,
					// Workspace pods never carry the SA token: they have no
					// need for the Kubernetes API and one leaking into a
					// user's workspace would be a privilege escalation path.
					AutomountServiceAccountToken:  boolPtr(false),
					SecurityContext:               &psc,
					InitContainers:                initContainers,
					Containers:                    []corev1.Container{container},
					Volumes:                       volumes,
					NodeSelector:                  wt.NodeSelector,
					Tolerations:                   wt.Tolerations,
					TerminationGracePeriodSeconds: termGrace,
				},
			},
		},
	}
	return d
}

// RenderWorkspaceService renders the ClusterIP Service fronting one user's
// workspace pod.
func RenderWorkspaceService(app *v1alpha1.PerUserApp, ws *v1alpha1.Workspace) *corev1.Service {
	name := childName(app, ws)
	podLabels := WorkspacePodLabels(app.Name, ws.Spec.UserKey)
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       ws.Namespace,
			Labels:          podLabels,
			OwnerReferences: []metav1.OwnerReference{workspaceOwnerRef(ws)},
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: podLabels,
			Ports: []corev1.ServicePort{{
				Name:       workspacePortName,
				Port:       app.Spec.Workspace.Port,
				TargetPort: intstr.FromString(workspacePortName),
			}},
		},
	}
}

// RenderWorkspacePVC renders the per-user PersistentVolumeClaim.
//
// Deliberately no ownerReference: an ownerReference means a cascade delete —
// including the prune-and-recreate a GitOps controller performs routinely —
// destroys a user's data. Prune=false and resource-policy:keep say the same
// thing to ArgoCD and Helm respectively. ReadWriteOncePod, not the plain
// ReadWriteOnce: RWO is enforced per-node, so on a single-node cluster two
// pods (exactly what a rollout produces) can hold the same volume at once;
// RWOP is enforced per-pod.
func RenderWorkspacePVC(app *v1alpha1.PerUserApp, ws *v1alpha1.Workspace) *corev1.PersistentVolumeClaim {
	name := childName(app, ws)
	storageClass := app.Spec.Storage.StorageClassName
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ws.Namespace,
			Labels: map[string]string{
				v1alpha1.LabelApp:     app.Name,
				v1alpha1.LabelUserKey: ws.Spec.UserKey,
			},
			Annotations: map[string]string{
				"argocd.argoproj.io/sync-options": "Prune=false",
				"helm.sh/resource-policy":         "keep",
				v1alpha1.AnnUserDisplay:           ws.Annotations[v1alpha1.AnnUserDisplay],
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOncePod},
			StorageClassName: &storageClass,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: app.Spec.Storage.Size},
			},
		},
	}
}

// dnsEgressRule allows DNS lookups to any destination: CoreDNS is a
// clusterIP Service, not a fixed address in podCIDR or nodeCIDR, so the rule
// is scoped by port only.
func dnsEgressRule() networkingv1.NetworkPolicyEgressRule {
	udp := corev1.ProtocolUDP
	tcp := corev1.ProtocolTCP
	port := intstr.FromInt32(53)
	return networkingv1.NetworkPolicyEgressRule{
		Ports: []networkingv1.NetworkPolicyPort{
			{Protocol: &udp, Port: &port},
			{Protocol: &tcp, Port: &port},
		},
	}
}

// RenderWorkspaceNetworkPolicies renders the two NetworkPolicies scoped to
// one user's workspace pod: ingress (default-deny except the router and the
// node's kubelet probes) and egress (default-deny except DNS and the
// declared workspaceEgress peers). They are separate objects because they
// declare disjoint policyTypes; Kubernetes unions multiple policies
// targeting the same pod, each governing only the type it declares.
func RenderWorkspaceNetworkPolicies(app *v1alpha1.PerUserApp, ws *v1alpha1.Workspace, podCIDR, nodeCIDR string) []*networkingv1.NetworkPolicy {
	// podCIDR is accepted for interface symmetry with RenderRouterNetworkPolicy
	// but unused here: the workspace policy admits the router by podSelector
	// and the kubelet by nodeCIDR, not by the pod network's address range.
	_ = podCIDR

	name := childName(app, ws)
	podLabels := WorkspacePodLabels(app.Name, ws.Spec.UserKey)
	owner := workspaceOwnerRef(ws)

	routerSel := &metav1.LabelSelector{MatchLabels: RouterPodLabels(app.Name)}
	svcPort := intstr.FromInt32(app.Spec.Workspace.Port)
	probePort := intstr.FromInt32(resolveProbePort(app))

	ingress := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name + "-ingress",
			Namespace:       ws.Namespace,
			OwnerReferences: []metav1.OwnerReference{owner},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: podLabels},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{
				{
					From:  []networkingv1.NetworkPolicyPeer{{PodSelector: routerSel}},
					Ports: []networkingv1.NetworkPolicyPort{{Port: &svcPort}},
				},
				{
					// Kubelet probes originate from the node, not a pod:
					// admitted by CIDR, not a podSelector.
					From:  []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: nodeCIDR}}},
					Ports: []networkingv1.NetworkPolicyPort{{Port: &probePort}},
				},
			},
		},
	}

	egressRules := append([]networkingv1.NetworkPolicyEgressRule{dnsEgressRule()}, app.Spec.Network.WorkspaceEgress...)
	egress := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name + "-egress",
			Namespace:       ws.Namespace,
			OwnerReferences: []metav1.OwnerReference{owner},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: podLabels},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress:      egressRules,
		},
	}
	return []*networkingv1.NetworkPolicy{ingress, egress}
}

// RenderRouterNetworkPolicy renders the single NetworkPolicy scoped to an
// app's router pods. nodeCIDR is mandatory: without egress to the node
// block the router cannot reach the apiserver (Calico evaluates egress
// pre-DNAT, so kubernetes.default.svc is not a valid selector target), and
// every Workspace create and status patch would fail.
func RenderRouterNetworkPolicy(app *v1alpha1.PerUserApp, podCIDR, nodeCIDR string) *networkingv1.NetworkPolicy {
	routerPort := intstr.FromInt32(v1alpha1.RouterPort)
	metricsPort := intstr.FromInt32(v1alpha1.MetricsPort)

	from := append([]networkingv1.NetworkPolicyPeer{}, app.Spec.Network.RouterIngress.From...)
	if app.Spec.Network.RouterIngress.FromTraefik {
		from = append(from, networkingv1.NetworkPolicyPeer{
			NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": "traefik"}},
		})
	}

	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:            app.Name + "-router",
			Namespace:       app.Namespace,
			OwnerReferences: []metav1.OwnerReference{appOwnerRef(app)},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: RouterPodLabels(app.Name)},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{
				{
					From:  from,
					Ports: []networkingv1.NetworkPolicyPort{{Port: &routerPort}},
				},
				{
					// Prometheus reach: admitted by pod CIDR rather than a
					// hardcoded monitoring-namespace selector, since the
					// router's own series carry no per-user identity — only
					// aggregate counts are exposed here.
					From:  []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: podCIDR}}},
					Ports: []networkingv1.NetworkPolicyPort{{Port: &metricsPort}},
				},
			},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				dnsEgressRule(),
				{To: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: nodeCIDR}}}},
				{To: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: podCIDR}}}},
			},
		},
	}
}

// allowedVolumeSource reports whether vs sets exactly one VolumeSource field
// and that field is one of the four allowed kinds. This is an allowlist, not
// a denylist: a denylist would let csi/nfs/iscsi/downwardAPI (and anything
// added to VolumeSource in the future) sail through untouched.
func allowedVolumeSource(vs corev1.VolumeSource) bool {
	const (
		fConfigMap = "ConfigMap"
		fSecret    = "Secret"
		fEmptyDir  = "EmptyDir"
		fProjected = "Projected"
	)
	allowed := map[string]bool{fConfigMap: true, fSecret: true, fEmptyDir: true, fProjected: true}

	rv := reflect.ValueOf(vs)
	rt := rv.Type()
	set := ""
	count := 0
	for i := 0; i < rt.NumField(); i++ {
		f := rv.Field(i)
		if f.Kind() == reflect.Ptr && !f.IsNil() {
			count++
			set = rt.Field(i).Name
		}
	}
	return count == 1 && allowed[set]
}

// ValidateApp checks a PerUserApp against the invariants that keep one
// user's container and volume from reaching another's. Every rejection
// error names the failure mode, not just the offending field, because these
// errors are the only signal an operator gets when the CEL layer (bypassed
// by prune-and-recreate) is not enough on its own. reclaimPolicy:Retain is
// not checked here: that needs a GET on a cluster-scoped StorageClass and
// this package has no client — see ValidateStorageClass (Task 11).
func ValidateApp(app *v1alpha1.PerUserApp) (warnings []string, err error) {
	for _, v := range app.Spec.Workspace.Volumes {
		if v.Name == v1alpha1.PVCVolumeName {
			return nil, fmt.Errorf("spec.workspace.volumes: volume name %q is reserved for the per-user PVC; a user volume with this name would collide with it on the pod spec", v1alpha1.PVCVolumeName)
		}
		if !allowedVolumeSource(v.VolumeSource) {
			return nil, fmt.Errorf("spec.workspace.volumes[%s]: volume source is not on the allowlist (configMap, secret, emptyDir, projected only); it may reach another user's files", v.Name)
		}
		if v.Projected != nil {
			for _, src := range v.Projected.Sources {
				if src.ServiceAccountToken != nil {
					return nil, fmt.Errorf("spec.workspace.volumes[%s]: a projected serviceAccountToken source is forbidden; it ignores automountServiceAccountToken:false and the mounted token can create pods and read PVCs across served namespaces", v.Name)
				}
			}
		}
	}

	if app.Spec.Workspace.ServiceAccountName == v1alpha1.OperatorServiceAccountName {
		return nil, fmt.Errorf("spec.workspace.serviceAccountName: %q is the operator's own service account and must never be assigned to a workspace pod", v1alpha1.OperatorServiceAccountName)
	}

	if app.Spec.Workspace.PodSecurityContext.FSGroup == nil {
		return nil, fmt.Errorf("spec.workspace.podSecurityContext.fsGroup: required; a freshly provisioned volume is root-owned without it and the workspace container cannot write to it")
	}

	psc := app.Spec.Workspace.PodSecurityContext
	if psc.RunAsNonRoot != nil && *psc.RunAsNonRoot && psc.RunAsUser == nil {
		return nil, fmt.Errorf("spec.workspace.podSecurityContext: runAsNonRoot is true but runAsUser is unset; the image's own USER directive is not guaranteed numeric and this becomes a CreateContainerConfigError for every user of this app")
	}

	for i, r := range app.Spec.Network.WorkspaceEgress {
		if len(r.To) == 0 {
			return nil, fmt.Errorf("spec.network.workspaceEgress[%d]: to must be non-empty; an absent to is allow-to-anywhere and reaches every other workspace and the router", i)
		}
		for j, p := range r.To {
			if p.IPBlock == nil {
				return nil, fmt.Errorf("spec.network.workspaceEgress[%d].to[%d]: only ipBlock peers are allowed; Calico evaluates egress pre-DNAT so a selector-based peer renders, applies and silently drops", i, j)
			}
		}
	}

	if len(app.Spec.Network.RouterIngress.From) == 0 && !app.Spec.Network.RouterIngress.FromTraefik {
		return nil, fmt.Errorf("spec.network.routerIngress: from is empty and fromTraefik is false; this is a silent default-allow on the router's ingress")
	}

	if app.Spec.CallerAuth.Header == "" || app.Spec.CallerAuth.SecretRef.Name == "" {
		return nil, fmt.Errorf("spec.callerAuth: mandatory; without it, anyone who can open a socket to the router can set the identity header and become any user")
	}

	if len(app.Spec.Network.WorkspaceEgress) == 0 {
		warnings = append(warnings, "spec.network.workspaceEgress is empty: the workspace pod will have DNS-only egress and cannot reach any declared peer")
	}
	if app.Spec.Storage.MountPath != "" {
		for _, m := range app.Spec.Workspace.VolumeMounts {
			if strings.HasPrefix(m.MountPath, app.Spec.Storage.MountPath+"/") {
				warnings = append(warnings, fmt.Sprintf("volume mount %q is nested under the workspace mount %q: a file written there is on a different, volatile volume and vanishes on the next reap", m.MountPath, app.Spec.Storage.MountPath))
			}
		}
	}

	return warnings, nil
}

func boolPtr(b bool) *bool { return &b }
