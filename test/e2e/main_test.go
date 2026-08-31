//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes"
)

// globalEnv and globalClient are set once in TestMain and read by every
// Test function in this package (this and the assertions a later dispatch
// adds): one apiserver connection for the whole binary, matching how a real
// e2e run against either kind or the live edge cluster behaves.
var (
	globalEnv    e2eEnv
	globalClient kubernetes.Interface
)

// TestMain enforces, in this exact order, BEFORE m.Run() is ever called --
// not as ordinary Test functions a `-run` selection could skip past --
// task-13a-brief.md Step 0 item 3's two preconditions:
//
//  1. TestCNIIsCalico's check: the calico-node DaemonSet is present and
//     fully ready, and no kindnet DaemonSet exists anywhere in the cluster.
//  2. The smoke assertion: the fixture app's router Deployment is Ready and
//     one authenticated, in-cluster request returns 200.
//
// A failure of either os.Exit(1)s immediately, aborting the entire suite --
// deliberately stronger than an ordinary test failure, so that no assertion
// in this package (including one hand-picked with `-run`) can ever execute
// against a cluster that fails a precondition every one of them depends on.
// Both checks are also exposed as ordinary Test functions (cni_test.go,
// smoke_test.go) purely so they can be run and observed in isolation
// (`go test -tags e2e -run TestCNIIsCalico ./test/e2e/...`); TestMain does
// not rely on that selection to enforce them.
func TestMain(m *testing.M) {
	env, err := loadEnv()
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e TestMain: load environment:", err)
		os.Exit(1)
	}
	globalEnv = env

	cfg, err := restConfig(env.Kubeconfig)
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e TestMain: build kube client config:", err)
		os.Exit(1)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e TestMain: new clientset:", err)
		os.Exit(1)
	}
	globalClient = cs

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	if err := checkCNIIsCalico(ctx, globalClient); err != nil {
		fmt.Fprintln(os.Stderr, "PRECONDITION FAILED (CNI guard, see TestCNIIsCalico):", err)
		os.Exit(1)
	}

	if err := checkRouterSmoke(ctx, globalClient, globalEnv); err != nil {
		fmt.Fprintln(os.Stderr, "PRECONDITION FAILED (smoke assertion, see TestRouterSmoke):", err)
		os.Exit(1)
	}

	os.Exit(m.Run())
}
