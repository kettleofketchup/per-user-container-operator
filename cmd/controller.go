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
type namespaceCheck struct{ group, resource, verb string }

// controllerNamespaceChecks mirrors every namespaced +kubebuilder:rbac
// marker across WorkspaceReconciler and PerUserAppReconciler: the verbs
// each actually uses, per resource. Nothing here is cluster-scoped (that is
// spec 556's "nothing else is cluster-scoped" -- storageclasses is the one
// exception both reconcilers read, and it is probed too, since a missing
// grant there fails ConfigValid for every app in a served namespace exactly
// like any other missing grant would).
var controllerNamespaceChecks = []namespaceCheck{
	{v1alpha1.GroupVersion.Group, "peruserapps", "get"},
	{v1alpha1.GroupVersion.Group, "peruserapps", "list"},
	{v1alpha1.GroupVersion.Group, "peruserapps", "watch"},
	{v1alpha1.GroupVersion.Group, "peruserapps/status", "get"},
	{v1alpha1.GroupVersion.Group, "peruserapps/status", "update"},
	{v1alpha1.GroupVersion.Group, "peruserapps/status", "patch"},
	{v1alpha1.GroupVersion.Group, "workspaces", "get"},
	{v1alpha1.GroupVersion.Group, "workspaces", "list"},
	{v1alpha1.GroupVersion.Group, "workspaces", "watch"},
	{v1alpha1.GroupVersion.Group, "workspaces", "create"},
	{v1alpha1.GroupVersion.Group, "workspaces", "update"},
	{v1alpha1.GroupVersion.Group, "workspaces", "patch"},
	{v1alpha1.GroupVersion.Group, "workspaces", "delete"},
	{v1alpha1.GroupVersion.Group, "workspaces/status", "get"},
	{v1alpha1.GroupVersion.Group, "workspaces/status", "update"},
	{v1alpha1.GroupVersion.Group, "workspaces/status", "patch"},
	{v1alpha1.GroupVersion.Group, "workspaces/finalizers", "update"},
	{"apps", "deployments", "get"},
	{"apps", "deployments", "list"},
	{"apps", "deployments", "watch"},
	{"apps", "deployments", "create"},
	{"apps", "deployments", "update"},
	{"apps", "deployments", "patch"},
	{"", "services", "get"},
	{"", "services", "list"},
	{"", "services", "watch"},
	{"", "services", "create"},
	{"", "services", "update"},
	{"", "services", "patch"},
	{"", "persistentvolumeclaims", "get"},
	{"", "persistentvolumeclaims", "list"},
	{"", "persistentvolumeclaims", "watch"},
	{"", "persistentvolumeclaims", "create"},
	{"", "persistentvolumeclaims", "update"},
	{"", "persistentvolumeclaims", "patch"},
	{"", "pods", "get"},
	{"", "pods", "list"},
	{"", "pods", "watch"},
	{"", "serviceaccounts", "get"},
	{"", "serviceaccounts", "list"},
	{"", "serviceaccounts", "watch"},
	{"", "serviceaccounts", "create"},
	{"", "serviceaccounts", "update"},
	{"", "serviceaccounts", "patch"},
	{"", "events", "create"},
	{"", "events", "patch"},
	{"networking.k8s.io", "networkpolicies", "get"},
	{"networking.k8s.io", "networkpolicies", "list"},
	{"networking.k8s.io", "networkpolicies", "watch"},
	{"networking.k8s.io", "networkpolicies", "create"},
	{"networking.k8s.io", "networkpolicies", "update"},
	{"networking.k8s.io", "networkpolicies", "patch"},
	{"rbac.authorization.k8s.io", "rolebindings", "get"},
	{"rbac.authorization.k8s.io", "rolebindings", "list"},
	{"rbac.authorization.k8s.io", "rolebindings", "watch"},
	{"rbac.authorization.k8s.io", "rolebindings", "create"},
	{"rbac.authorization.k8s.io", "rolebindings", "update"},
	{"rbac.authorization.k8s.io", "rolebindings", "patch"},
	{"storage.k8s.io", "storageclasses", "get"},
	{"storage.k8s.io", "storageclasses", "list"},
	{"storage.k8s.io", "storageclasses", "watch"},
}

// probeNamespace attempts every verb this controller uses in ns via a
// SelfSubjectAccessReview per (group, resource, verb), so a missing grant is
// discovered at startup, in puc_watched_namespace_ready, rather than per
// user at 03:00. It reports ready only if every single check is allowed --
// a probe that stopped at the first Allowed check would report ready with,
// say, no grant to create Deployments at all.
func probeNamespace(ctx context.Context, authz authorizationv1client.AuthorizationV1Interface, ns string) (bool, []string) {
	ok := true
	var failed []string
	for _, c := range controllerNamespaceChecks {
		review := &authorizationv1.SelfSubjectAccessReview{
			Spec: authorizationv1.SelfSubjectAccessReviewSpec{
				ResourceAttributes: &authorizationv1.ResourceAttributes{
					Namespace: ns,
					Group:     c.group,
					Resource:  c.resource,
					Verb:      c.verb,
				},
			},
		}
		result, err := authz.SelfSubjectAccessReviews().Create(ctx, review, metav1.CreateOptions{})
		if err != nil || !result.Status.Allowed {
			ok = false
			failed = append(failed, fmt.Sprintf("%s/%s %s (namespace %s)", c.group, c.resource, c.verb, ns))
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
