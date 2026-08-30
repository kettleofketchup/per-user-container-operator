package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	authorizationv1client "k8s.io/client-go/kubernetes/typed/authorization/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/kettleofketchup/per-user-container-operator/api/v1alpha1"
	"github.com/kettleofketchup/per-user-container-operator/internal/controller"
	"github.com/kettleofketchup/per-user-container-operator/internal/metrics"
)

// leaderElectionID names both the Lease controller-runtime's leader elector
// contends for and the Lease snapshotLease reads before contending -- they
// MUST be the same name, or the snapshot observes a different object than
// the one leadership is decided on.
const leaderElectionID = "per-user-container-operator-leader"

// controllerConfig is the controller subcommand's ENTIRE startup contract
// (see task-11-brief.md): exactly these flags and env vars, and Task 12's
// chart renders exactly these.
type controllerConfig struct {
	WatchNamespaces []string
	PodCIDR         string
	NodeCIDR        string
	MetricsAddr     string
	// RouterImage is RELATED_IMAGE_ROUTER, fail-fast-checked here: the
	// controller creates the router Deployment at runtime, so an unset
	// image is not a startup nicety -- it is a Deployment that never
	// becomes ready, diagnosed at 3am.
	RouterImage string
	// PodName and PodNamespace are the downward API's POD_NAME/POD_NAMESPACE
	// -- the puc_controller_leader{pod} label and the namespace of the
	// leader-election Lease snapshot taken before mgr.Start(), respectively.
	PodName      string
	PodNamespace string
}

// parseControllerFlags parses the controller's entire startup contract.
// --watch-namespaces is rendered from the SAME chart watchNamespaces list
// that renders the per-namespace Roles and both ServiceMonitors (Task 12):
// a namespace missing here gets no reconciler watch AND is never included in
// the startup verb probe, so a mismatch is silent by construction --
// nothing here can detect a namespace the chart forgot to list.
func parseControllerFlags(args []string) (controllerConfig, error) {
	fs := flag.NewFlagSet("controller", flag.ContinueOnError)

	watchNamespaces := fs.String("watch-namespaces", "", "comma-separated namespaces this controller serves")
	podCIDR := fs.String("pod-cidr", "", "cluster pod CIDR, threaded into rendered NetworkPolicies")
	nodeCIDR := fs.String("node-cidr", "", "cluster node CIDR, threaded into rendered NetworkPolicies")
	metricsAddr := fs.String("metrics-addr", fmt.Sprintf(":%d", v1alpha1.MetricsPort), "address the manager serves /metrics on")

	if err := fs.Parse(args); err != nil {
		return controllerConfig{}, err
	}

	if strings.TrimSpace(*watchNamespaces) == "" {
		return controllerConfig{}, errors.New("--watch-namespaces is required")
	}
	if *podCIDR == "" || *nodeCIDR == "" {
		return controllerConfig{}, errors.New("--pod-cidr and --node-cidr are required")
	}

	routerImage := os.Getenv("RELATED_IMAGE_ROUTER")
	if routerImage == "" {
		return controllerConfig{}, errors.New("RELATED_IMAGE_ROUTER must be set: the controller creates the router Deployment at runtime, and an unset image is a Deployment that never becomes ready")
	}

	podName := os.Getenv("POD_NAME")
	podNamespace := os.Getenv("POD_NAMESPACE")
	if podName == "" || podNamespace == "" {
		return controllerConfig{}, errors.New("POD_NAME and POD_NAMESPACE (downward API) must be set")
	}

	var namespaces []string
	for _, ns := range strings.Split(*watchNamespaces, ",") {
		ns = strings.TrimSpace(ns)
		if ns != "" {
			namespaces = append(namespaces, ns)
		}
	}
	if len(namespaces) == 0 {
		return controllerConfig{}, errors.New("--watch-namespaces resolved to no namespaces")
	}

	return controllerConfig{
		WatchNamespaces: namespaces,
		PodCIDR:         *podCIDR,
		NodeCIDR:        *nodeCIDR,
		MetricsAddr:     *metricsAddr,
		RouterImage:     routerImage,
		PodName:         podName,
		PodNamespace:    podNamespace,
	}, nil
}

// leaseSnapshot is the Lease state read once, before contending for
// leadership -- see snapshotLease and computeLeaderless.
type leaseSnapshot struct {
	found          bool
	holderIdentity string
	renewTime      time.Time
}

// snapshotLease reads the leader-election Lease exactly once. It must be
// called BEFORE mgr.Start(): controller-runtime's leader elector writes
// renewTime/acquireTime as part of the acquisition itself, before
// mgr.Elected() fires, so reading the Lease at or after acquisition would
// have this process see its own just-written timestamp instead of the
// PREVIOUS holder's -- see computeLeaderless's doc comment for the failure
// that produces. A NotFound Lease (first-ever leader) is not an error: it
// simply means there is no prior holder to compare against.
func snapshotLease(ctx context.Context, cs kubernetes.Interface, namespace string) (leaseSnapshot, error) {
	lease, err := cs.CoordinationV1().Leases(namespace).Get(ctx, leaderElectionID, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return leaseSnapshot{}, nil
	}
	if err != nil {
		return leaseSnapshot{}, err
	}
	snap := leaseSnapshot{found: true}
	if lease.Spec.HolderIdentity != nil {
		snap.holderIdentity = *lease.Spec.HolderIdentity
	}
	if lease.Spec.RenewTime != nil {
		snap.renewTime = lease.Spec.RenewTime.Time
	}
	return snap, nil
}

// leaseIsSameHolder reports whether holderIdentity names podName as the
// pod that held it. controller-runtime's default leader-election identity
// is "<hostname>_<uuid>" (pkg/leaderelection.NewResourceLock), generated
// once per process; a Pod's hostname is its own name by default. So the
// same PHYSICAL pod re-acquiring after a container restart (new process,
// new uuid, same hostname) still compares equal here, which is the
// intended reading of "a re-acquisition by the same holder was never
// leaderless" -- an exact string match would wrongly treat that restart as
// a different holder.
func leaseIsSameHolder(holderIdentity, podName string) bool {
	return strings.HasPrefix(holderIdentity, podName+"_")
}

// computeLeaderless implements the arithmetic task-11-brief.md calls out by
// name: leaderless = now - snapshottedRenewTime when the previous holder
// differs from this pod, and 0 when it does not (including "no prior
// Lease at all"). Task 8's own test cannot catch a version of this that
// reads the Lease again at/after acquisition instead of using a
// pre-acquisition snapshot: it injects a duration directly into
// OnLeaseAcquired, so the bug this guards against -- every startDeadline
// extended by ~0 regardless of how long the fleet was actually leaderless,
// failing every in-flight Starting workspace on a restart that takes longer
// than the remaining startupTimeout -- can only be caught here, at the
// snapshot/comparison site itself.
func computeLeaderless(now time.Time, snap leaseSnapshot, podName string) time.Duration {
	if !snap.found {
		return 0
	}
	if leaseIsSameHolder(snap.holderIdentity, podName) {
		return 0
	}
	return now.Sub(snap.renewTime)
}

// namespaceCheck is one (group, resource, verb) the startup verb probe
// attempts in a served namespace.
//
// clusterScoped marks a resource that is NOT namespaced (storageclasses is
// the only one today). This field exists specifically so a cluster-scoped
// addition is a deliberate, visible choice at the call site, not a string
// some future reader has to recognize by name: a SelfSubjectAccessReview
// evaluates exactly the hypothetical it is handed, and a NAMESPACED Role
// granting a verb on a cluster-scoped resource answers Allowed:true for a
// namespaced SAR even though the real client.Get() against that
// cluster-scoped object is Forbidden -- passing a namespace asks an easier
// question than the request the controller actually makes. See
// probeNamespace's doc comment for how this field changes the SAR sent.
type namespaceCheck struct {
	group, resource, verb string
	clusterScoped         bool
}

// controllerNamespaceChecks mirrors every +kubebuilder:rbac marker across
// WorkspaceReconciler and PerUserAppReconciler: the verbs each actually
// uses, per resource. Every entry here is namespaced EXCEPT the three
// storageclasses checks, marked clusterScoped: true -- storage.k8s.io
// StorageClass is spec 556's one stated exception to "nothing else is
// cluster-scoped," and Task 12's chart grants it via a ClusterRole rather
// than the per-namespace Role this list otherwise probes.
var controllerNamespaceChecks = []namespaceCheck{
	{group: v1alpha1.GroupVersion.Group, resource: "peruserapps", verb: "get"},
	{group: v1alpha1.GroupVersion.Group, resource: "peruserapps", verb: "list"},
	{group: v1alpha1.GroupVersion.Group, resource: "peruserapps", verb: "watch"},
	{group: v1alpha1.GroupVersion.Group, resource: "peruserapps/status", verb: "get"},
	{group: v1alpha1.GroupVersion.Group, resource: "peruserapps/status", verb: "update"},
	{group: v1alpha1.GroupVersion.Group, resource: "peruserapps/status", verb: "patch"},
	{group: v1alpha1.GroupVersion.Group, resource: "workspaces", verb: "get"},
	{group: v1alpha1.GroupVersion.Group, resource: "workspaces", verb: "list"},
	{group: v1alpha1.GroupVersion.Group, resource: "workspaces", verb: "watch"},
	{group: v1alpha1.GroupVersion.Group, resource: "workspaces", verb: "create"},
	{group: v1alpha1.GroupVersion.Group, resource: "workspaces", verb: "update"},
	{group: v1alpha1.GroupVersion.Group, resource: "workspaces", verb: "patch"},
	{group: v1alpha1.GroupVersion.Group, resource: "workspaces", verb: "delete"},
	{group: v1alpha1.GroupVersion.Group, resource: "workspaces/status", verb: "get"},
	{group: v1alpha1.GroupVersion.Group, resource: "workspaces/status", verb: "update"},
	{group: v1alpha1.GroupVersion.Group, resource: "workspaces/status", verb: "patch"},
	{group: v1alpha1.GroupVersion.Group, resource: "workspaces/finalizers", verb: "update"},
	{group: "apps", resource: "deployments", verb: "get"},
	{group: "apps", resource: "deployments", verb: "list"},
	{group: "apps", resource: "deployments", verb: "watch"},
	{group: "apps", resource: "deployments", verb: "create"},
	{group: "apps", resource: "deployments", verb: "update"},
	{group: "apps", resource: "deployments", verb: "patch"},
	{group: "", resource: "services", verb: "get"},
	{group: "", resource: "services", verb: "list"},
	{group: "", resource: "services", verb: "watch"},
	{group: "", resource: "services", verb: "create"},
	{group: "", resource: "services", verb: "update"},
	{group: "", resource: "services", verb: "patch"},
	{group: "", resource: "persistentvolumeclaims", verb: "get"},
	{group: "", resource: "persistentvolumeclaims", verb: "list"},
	{group: "", resource: "persistentvolumeclaims", verb: "watch"},
	{group: "", resource: "persistentvolumeclaims", verb: "create"},
	{group: "", resource: "persistentvolumeclaims", verb: "update"},
	{group: "", resource: "persistentvolumeclaims", verb: "patch"},
	{group: "", resource: "pods", verb: "get"},
	{group: "", resource: "pods", verb: "list"},
	{group: "", resource: "pods", verb: "watch"},
	{group: "", resource: "serviceaccounts", verb: "get"},
	{group: "", resource: "serviceaccounts", verb: "list"},
	{group: "", resource: "serviceaccounts", verb: "watch"},
	{group: "", resource: "serviceaccounts", verb: "create"},
	{group: "", resource: "serviceaccounts", verb: "update"},
	{group: "", resource: "serviceaccounts", verb: "patch"},
	{group: "", resource: "events", verb: "create"},
	{group: "", resource: "events", verb: "patch"},
	{group: "networking.k8s.io", resource: "networkpolicies", verb: "get"},
	{group: "networking.k8s.io", resource: "networkpolicies", verb: "list"},
	{group: "networking.k8s.io", resource: "networkpolicies", verb: "watch"},
	{group: "networking.k8s.io", resource: "networkpolicies", verb: "create"},
	{group: "networking.k8s.io", resource: "networkpolicies", verb: "update"},
	{group: "networking.k8s.io", resource: "networkpolicies", verb: "patch"},
	{group: "rbac.authorization.k8s.io", resource: "rolebindings", verb: "get"},
	{group: "rbac.authorization.k8s.io", resource: "rolebindings", verb: "list"},
	{group: "rbac.authorization.k8s.io", resource: "rolebindings", verb: "watch"},
	{group: "rbac.authorization.k8s.io", resource: "rolebindings", verb: "create"},
	{group: "rbac.authorization.k8s.io", resource: "rolebindings", verb: "update"},
	{group: "rbac.authorization.k8s.io", resource: "rolebindings", verb: "patch"},
	{group: "storage.k8s.io", resource: "storageclasses", verb: "get", clusterScoped: true},
	{group: "storage.k8s.io", resource: "storageclasses", verb: "list", clusterScoped: true},
	{group: "storage.k8s.io", resource: "storageclasses", verb: "watch", clusterScoped: true},
}

// probeNamespace attempts every verb this controller uses in ns via a
// SelfSubjectAccessReview per (group, resource, verb), so a missing grant is
// discovered at startup, in puc_watched_namespace_ready, rather than per
// user at 03:00. It reports ready only if every single check is allowed --
// a probe that stopped at the first Allowed check would report ready with,
// say, no grant to create Deployments at all.
//
// A clusterScoped check's SelfSubjectAccessReview carries NO Namespace: the
// real request it stands in for (ValidateStorageClass's client.Get against
// a cluster-scoped StorageClass) is never namespaced either, and setting
// Namespace here would ask the authorizer an easier question than that real
// request makes -- exactly the gap that let a namespaced Role's grant read
// as sufficient while the real Get 403s on every reconcile.
func probeNamespace(ctx context.Context, authz authorizationv1client.AuthorizationV1Interface, ns string) (bool, []string) {
	ok := true
	var failed []string
	for _, c := range controllerNamespaceChecks {
		attrs := &authorizationv1.ResourceAttributes{
			Group:    c.group,
			Resource: c.resource,
			Verb:     c.verb,
		}
		scope := ns
		if !c.clusterScoped {
			attrs.Namespace = ns
		} else {
			scope = "cluster-scoped"
		}
		review := &authorizationv1.SelfSubjectAccessReview{
			Spec: authorizationv1.SelfSubjectAccessReviewSpec{ResourceAttributes: attrs},
		}
		result, err := authz.SelfSubjectAccessReviews().Create(ctx, review, metav1.CreateOptions{})
		if err != nil || !result.Status.Allowed {
			ok = false
			failed = append(failed, fmt.Sprintf("%s/%s %s (%s)", c.group, c.resource, c.verb, scope))
		}
	}
	return ok, failed
}

// runController builds the manager, wires every reconciler and Runnable
// this operator has (WorkspaceReconciler, PerUserAppReconciler, the
// Reaper), runs the startup namespace-readiness probe, and on winning
// leadership calls the Admitter's orphan sweep with the leaderless duration
// computed from a pre-acquisition Lease snapshot. It matches runRouter's
// shape: SIGTERM/SIGINT trigger controller-runtime's own graceful shutdown
// of mgr.Start via the cancelled context.
func runController(args []string) error {
	cfg, err := parseControllerFlags(args)
	if err != nil {
		return err
	}

	restCfg, err := ctrl.GetConfig()
	if err != nil {
		return fmt.Errorf("get kubeconfig: %w", err)
	}

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(v1alpha1.AddToScheme(scheme))

	clientset, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return fmt.Errorf("new clientset: %w", err)
	}

	// Snapshot the Lease BEFORE contending: see snapshotLease's doc comment
	// for why this must happen ahead of mgr.Start(), not inside the
	// mgr.Elected() callback below.
	snap, err := snapshotLease(context.Background(), clientset, cfg.PodNamespace)
	if err != nil {
		return fmt.Errorf("snapshot leader lease: %w", err)
	}

	for _, ns := range cfg.WatchNamespaces {
		ready, failed := probeNamespace(context.Background(), clientset.AuthorizationV1(), ns)
		metrics.SetWatchedNamespaceReady(ns, ready)
		if !ready {
			fmt.Fprintf(os.Stderr, "warning: namespace %s is missing RBAC grants this controller needs: %v\n", ns, failed)
		}
	}

	defaultNamespaces := make(map[string]cache.Config, len(cfg.WatchNamespaces))
	for _, ns := range cfg.WatchNamespaces {
		defaultNamespaces[ns] = cache.Config{}
	}

	mgr, err := ctrl.NewManager(restCfg, ctrl.Options{
		Scheme:                  scheme,
		Cache:                   cache.Options{DefaultNamespaces: defaultNamespaces},
		Metrics:                 metricsserver.Options{BindAddress: cfg.MetricsAddr},
		LeaderElection:          true,
		LeaderElectionID:        leaderElectionID,
		LeaderElectionNamespace: cfg.PodNamespace,
	})
	if err != nil {
		return fmt.Errorf("new manager: %w", err)
	}

	admitter := controller.NewAdmitter(mgr.GetClient())

	wsReconciler := &controller.WorkspaceReconciler{
		Client:   mgr.GetClient(),
		Scheme:   scheme,
		Admitter: admitter,
		PodCIDR:  cfg.PodCIDR,
		NodeCIDR: cfg.NodeCIDR,
	}
	if err := wsReconciler.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setup workspace reconciler: %w", err)
	}

	appReconciler := &controller.PerUserAppReconciler{
		Client:      mgr.GetClient(),
		Scheme:      scheme,
		RouterImage: cfg.RouterImage,
		PodCIDR:     cfg.PodCIDR,
		NodeCIDR:    cfg.NodeCIDR,
	}
	if err := appReconciler.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setup peruserapp reconciler: %w", err)
	}

	if err := controller.NewReaper(mgr.GetClient()).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setup reaper: %w", err)
	}

	go func() {
		<-mgr.Elected()
		leaderless := computeLeaderless(time.Now(), snap, cfg.PodName)
		metrics.SetLeader(cfg.PodName, true)
		if err := admitter.OnLeaseAcquired(context.Background(), leaderless); err != nil {
			fmt.Fprintf(os.Stderr, "warning: OnLeaseAcquired: %v\n", err)
		}
	}()

	sigCtx, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stopSignals()

	return mgr.Start(sigCtx)
}
