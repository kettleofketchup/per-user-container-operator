//go:build envtest

// Package envtest holds the shared envtest harness (a real kube-apiserver +
// etcd, no controller-manager) that this task's suite and Tasks 8-11 reuse.
// See task-6-brief.md Step 0 for what this environment can and cannot
// observe: no garbage collector, no PV binder, no Deployment/ReplicaSet
// controller, no kubelet. Every test below either asserts something this
// environment genuinely produces, or forges the one input it needs per that
// step's table.
package envtest

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlenvtest "sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/kettleofketchup/per-user-container-operator/api/v1alpha1"
	"github.com/kettleofketchup/per-user-container-operator/internal/controller"
	"github.com/kettleofketchup/per-user-container-operator/internal/identity"
	"github.com/kettleofketchup/per-user-container-operator/internal/testfixtures"
)

var (
	testEnv   *ctrlenvtest.Environment
	cfg       *rest.Config
	k8sClient client.Client
	scheme    = runtime.NewScheme()
)

func TestMain(m *testing.M) {
	logf.SetLogger(zap.New(zap.WriteTo(io.Discard)))

	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(v1alpha1.AddToScheme(scheme))

	testEnv = &ctrlenvtest.Environment{
		CRDDirectoryPaths:     []string{"../../config/crd"},
		ErrorIfCRDPathMissing: true,
	}

	var err error
	cfg, err = testEnv.Start()
	if err != nil {
		fmt.Fprintln(os.Stderr, "envtest start:", err)
		os.Exit(1)
	}

	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		fmt.Fprintln(os.Stderr, "envtest client:", err)
		os.Exit(1)
	}

	// The real API server refuses to resize a PVC unless its StorageClass
	// exists and allows expansion ("only dynamically provisioned pvc can be
	// resized and the storageclass ... must support resize"). testfixtures
	// hardcodes "ceph-block-static" as the storage class name, so it must
	// exist here for TestStorageGrowthPatchesAndMismatchSetsDrift to
	// exercise a real growth patch rather than a self-fulfilling mock.
	//
	// reclaimPolicy is explicit Retain, matching the production
	// ceph-block-static class this name refers to (CLAUDE.md): left
	// unspecified, the API server's own defaulting sets it to Delete, which
	// would make ValidateStorageClass (Task 11) refuse every fixture app in
	// this suite.
	allowExpansion := true
	retain := corev1.PersistentVolumeReclaimRetain
	sc := &storagev1.StorageClass{
		ObjectMeta:           metav1.ObjectMeta{Name: "ceph-block-static"},
		Provisioner:          "rook-ceph.rbd.csi.ceph.com",
		AllowVolumeExpansion: &allowExpansion,
		ReclaimPolicy:        &retain,
	}
	if err := k8sClient.Create(context.Background(), sc); err != nil {
		fmt.Fprintln(os.Stderr, "envtest storageclass:", err)
		os.Exit(1)
	}

	code := m.Run()

	if err := testEnv.Stop(); err != nil {
		fmt.Fprintln(os.Stderr, "envtest stop:", err)
	}
	os.Exit(code)
}

// newNamespace creates a uniquely-named Namespace for one test and arranges
// for its deletion, so fixtures sharing app/user names never collide across
// tests run against one shared apiserver.
func newNamespace(t *testing.T) string {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "ws-test-"}}
	if err := k8sClient.Create(context.Background(), ns); err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), ns)
	})
	return ns.Name
}

// newFixtures returns testfixtures.ValidApp/ValidWorkspace re-namespaced to
// ns, with the Workspace's userKey and name recomputed accordingly (both are
// derived from namespace+appName+rawIdentity and would otherwise still
// point at the fixture's hardcoded "ns").
func newFixtures(ns string) (*v1alpha1.PerUserApp, *v1alpha1.Workspace) {
	app := testfixtures.ValidApp()
	app.Namespace = ns

	ws := testfixtures.ValidWorkspace()
	ws.Namespace = ns
	userKey := identity.UserKey(ns, app.Name, "alice")
	ws.Spec.UserKey = userKey
	ws.Name = identity.ChildName(app.Name, userKey)
	ws.Labels[v1alpha1.LabelUserKey] = userKey

	return app, ws
}

// newTestManager builds a Manager bound to the shared envtest apiserver,
// with its metrics and health servers disabled (many managers run
// concurrently across this suite's test binary).
func newTestManager() (ctrl.Manager, error) {
	return ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
	})
}

// newTestReconciler builds a WorkspaceReconciler wired to mgr's client, with
// fast poll/requeue intervals so tests don't wait on production-scale
// timers. configure, if non-nil, runs before SetupWithManager so callers can
// substitute e.g. a recording Admitter without racing the controller's
// worker goroutines.
var reconcilerNameSeq int64

func newTestReconciler(mgr ctrl.Manager, configure func(*controller.WorkspaceReconciler)) *controller.WorkspaceReconciler {
	rec := &controller.WorkspaceReconciler{
		Client:               mgr.GetClient(),
		Scheme:               scheme,
		Name:                 fmt.Sprintf("workspace-test-%d", atomic.AddInt64(&reconcilerNameSeq, 1)),
		PodCIDR:              "10.0.0.0/8",
		NodeCIDR:             "10.1.0.0/16",
		PendingRetryInterval: 150 * time.Millisecond,
		StartingPollInterval: 150 * time.Millisecond,
		SteadyStateInterval:  250 * time.Millisecond,
	}
	if configure != nil {
		configure(rec)
	}
	return rec
}

// runManager starts mgr in the background and blocks until its cache has
// synced, returning a function that stops it and waits for shutdown.
func runManager(t *testing.T, mgr ctrl.Manager) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = mgr.Start(ctx)
	}()
	if !mgr.GetCache().WaitForCacheSync(ctx) {
		cancel()
		<-done
		t.Fatal("manager cache did not sync")
	}
	return func() {
		cancel()
		<-done
	}
}

// startReconciler is the common case: a fresh Manager and WorkspaceReconciler,
// started immediately and stopped via t.Cleanup.
func startReconciler(t *testing.T, configure func(*controller.WorkspaceReconciler)) *controller.WorkspaceReconciler {
	t.Helper()
	mgr, err := newTestManager()
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	rec := newTestReconciler(mgr, configure)
	if err := rec.SetupWithManager(mgr); err != nil {
		t.Fatalf("setup reconciler: %v", err)
	}
	t.Cleanup(runManager(t, mgr))
	return rec
}

// stoppableReconciler is used by tests that need to explicitly stop a
// manager mid-test (e.g. to simulate a controller restart) rather than
// waiting for t.Cleanup.
type stoppableReconciler struct {
	rec  *controller.WorkspaceReconciler
	stop func()
}

func startStoppableReconciler(t *testing.T, configure func(*controller.WorkspaceReconciler)) *stoppableReconciler {
	t.Helper()
	mgr, err := newTestManager()
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	rec := newTestReconciler(mgr, configure)
	if err := rec.SetupWithManager(mgr); err != nil {
		t.Fatalf("setup reconciler: %v", err)
	}
	return &stoppableReconciler{rec: rec, stop: runManager(t, mgr)}
}

func (s *stoppableReconciler) Stop() { s.stop() }
