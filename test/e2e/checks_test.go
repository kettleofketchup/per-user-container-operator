//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// smokeApp is the fixture PerUserApp the smoke assertion talks to. It lives
// in env.Namespaces[0] -- kind-up.sh applies test/e2e/testdata/e2e-app.yaml
// there (and into the second namespace, and e2e-app-b.yaml alongside it in
// the first) -- see kind-up.sh item 11.
const smokeApp = "e2e-app"

// restConfig builds a *rest.Config from an explicit kubeconfig path when
// one is given (kind-up.sh always sets PUC_E2E_KUBECONFIG), or from the
// standard client-go loading rules (KUBECONFIG env var, then
// ~/.kube/config) for a non-kind invocation that has no reason to write one.
func restConfig(kubeconfig string) (*rest.Config, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		rules.ExplicitPath = kubeconfig
	}
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{}).ClientConfig()
}

// checkCNIIsCalico is the FIRST of TestMain's two preconditions (see
// main_test.go): the calico-node DaemonSet must be present and fully
// ready, AND no kindnet DaemonSet may exist anywhere in the cluster. A
// Calico install that half-fails or gets skipped leaves kindnet in place;
// every isolation assertion this plan makes would still pass against it,
// and the run would be green while testing nothing -- the precise scenario
// this guard exists to catch, which is why it runs before every other
// check, including the smoke assertion below.
func checkCNIIsCalico(ctx context.Context, cs kubernetes.Interface) error {
	ds, err := cs.AppsV1().DaemonSets("calico-system").Get(ctx, "calico-node", metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get calico-system/calico-node DaemonSet: %w", err)
	}
	if ds.Status.DesiredNumberScheduled == 0 {
		return fmt.Errorf("calico-node DaemonSet has DesiredNumberScheduled=0: no node is running it")
	}
	if ds.Status.NumberReady < ds.Status.DesiredNumberScheduled {
		return fmt.Errorf("calico-node DaemonSet not fully ready: %d/%d nodes", ds.Status.NumberReady, ds.Status.DesiredNumberScheduled)
	}

	dsList, err := cs.AppsV1().DaemonSets("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list DaemonSets across all namespaces: %w", err)
	}
	for _, d := range dsList.Items {
		if d.Name == "kindnet" {
			return fmt.Errorf("found a kindnet DaemonSet in namespace %q: Calico and kindnet must never both be installed, since kindnet enforces no NetworkPolicy at all and every isolation assertion in this plan would pass against it while verifying nothing", d.Namespace)
		}
	}
	return nil
}

// kubectlExec runs `kubectl exec -n ns pod -- args...` and returns combined
// stdout+stderr. It invokes the kubectl binary (already a hard
// dependency of this harness, per kind-up.sh) rather than driving
// client-go's SPDY executor directly: this is test-harness code exercising
// the same primitive kind-up.sh itself uses, not production code, and a
// plain kubectl invocation is far less code to get right.
func kubectlExec(kubeconfig, ns, pod string, args ...string) (string, error) {
	return kubectlExecContainer(kubeconfig, ns, pod, "", args...)
}

// kubectlExecContainer is kubectlExec with an explicit container. Naming the
// container is not cosmetic on a multi-container pod: kubectl picks the
// first one and prints `Defaulted container "x" out of: ...` to stderr,
// which CombinedOutput folds into the returned string and any caller
// comparing that string against a file's contents then fails on the notice
// rather than on the file. Pass "" for a single-container pod.
func kubectlExecContainer(kubeconfig, ns, pod, container string, args ...string) (string, error) {
	cmdArgs := []string{}
	if kubeconfig != "" {
		cmdArgs = append(cmdArgs, "--kubeconfig", kubeconfig)
	}
	cmdArgs = append(cmdArgs, "-n", ns, "exec", pod)
	if container != "" {
		cmdArgs = append(cmdArgs, "-c", container)
	}
	cmdArgs = append(cmdArgs, "--")
	cmdArgs = append(cmdArgs, args...)
	out, err := exec.CommandContext(context.Background(), "kubectl", cmdArgs...).CombinedOutput()
	return string(out), err
}

// checkRouterSmoke is TestMain's SECOND precondition, run only once the CNI
// guard has passed: the fixture app's router Deployment must be Ready, and
// one authenticated, in-cluster request (from the dedicated test-client pod
// kind-up.sh creates -- the only peer the router's NetworkPolicy admits
// besides Traefik) must return 200. This exists so a missing Secret, an
// unmounted credential or an unreachable apiserver is diagnosed exactly
// once, here, instead of surfacing as an apparent isolation failure in four
// unrelated tests at once.
func checkRouterSmoke(ctx context.Context, cs kubernetes.Interface, env e2eEnv) error {
	ns := env.Namespaces[0]
	depName := smokeApp + "-router"

	dep, err := cs.AppsV1().Deployments(ns).Get(ctx, depName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get router Deployment %s/%s: %w", ns, depName, err)
	}
	if dep.Status.ReadyReplicas < 1 {
		return fmt.Errorf("router Deployment %s/%s is not Ready: readyReplicas=%d", ns, depName, dep.Status.ReadyReplicas)
	}

	url := fmt.Sprintf("http://%s.%s.svc.cluster.local:8080/", depName, ns)
	out, err := kubectlExec(env.Kubeconfig, ns, "puc-e2e-client",
		"curl", "-sS", "--max-time", "100", "-o", "/dev/null", "-w", "%{http_code}",
		"-H", "Authorization: Bearer "+env.CallerToken,
		"-H", "X-User-Id: e2e-smoke",
		url,
	)
	if err != nil {
		return fmt.Errorf("authenticated smoke request via puc-e2e-client failed to run: %w (output: %s)", err, out)
	}
	if code := strings.TrimSpace(out); code != "200" {
		return fmt.Errorf("authenticated smoke request to %s returned %q, want 200", url, code)
	}
	return nil
}
