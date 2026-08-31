//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	autoscalingv1 "k8s.io/api/autoscaling/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	"github.com/kettleofketchup/per-user-container-operator/api/v1alpha1"
)

// identityHeader is the header every fixture PerUserApp in this harness
// names as spec.identity.header (test/e2e/testdata/e2e-app*.yaml).
const identityHeader = "X-User-Id"

// operatorNamespace/operatorDeployment name the controller Deployment
// kind-up.sh installs and waits Ready (charts/per-user-container-operator's
// puc.name template): the fixed target the preamble's quiesce procedure
// scales to 0 and back.
const (
	operatorNamespace  = "puc-system"
	operatorDeployment = "per-user-container-operator"
)

// pollUntil polls cond every interval until it returns true or ctx is done,
// returning the last error cond reported (nil if it never errored). It never
// backgrounds work: every assertion in this package drives its own bounded
// wait in the foreground, one poll at a time, so a stuck condition surfaces
// as this call returning a context-deadline error rather than a test hanging
// forever with nothing to interrupt it.
func pollUntil(ctx context.Context, interval time.Duration, cond func() (bool, error)) error {
	var lastErr error
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		ok, err := cond()
		if err != nil {
			lastErr = err
		}
		if ok {
			return nil
		}
		select {
		case <-ctx.Done():
			if lastErr != nil {
				return fmt.Errorf("%w (last condition error: %v)", ctx.Err(), lastErr)
			}
			return ctx.Err()
		case <-t.C:
		}
	}
}

// runInPod is kubectlExec (checks_test.go) under a name that reads better
// alongside this file's other pod-exec helpers.
func runInPod(kubeconfig, ns, pod string, args ...string) (string, error) {
	return kubectlExec(kubeconfig, ns, pod, args...)
}

// writeMarker writes content to path inside pod via an exec redirect.
// content is deliberately restricted to this package's own marker values
// (fixed identifiers, never arbitrary/attacker-controlled text), so no
// argument-quoting concern arises from building the command with fmt.Sprintf.
func writeMarker(kubeconfig, ns, pod, path, content string) error {
	out, err := runInPod(kubeconfig, ns, pod, "sh", "-c", fmt.Sprintf("printf '%%s' '%s' > '%s'", content, path))
	if err != nil {
		return fmt.Errorf("write marker %s in %s/%s: %w (output: %s)", path, ns, pod, err, out)
	}
	return nil
}

// readMarker cats path inside pod. A missing file surfaces as a non-nil
// error (cat's own "No such file or directory"), which callers use directly
// as the "absent" signal -- never as "the read succeeded and returned
// empty", which a shared-volume bug could also produce.
func readMarker(kubeconfig, ns, pod, path string) (string, error) {
	out, err := runInPod(kubeconfig, ns, pod, "cat", path)
	if err != nil {
		return "", fmt.Errorf("read marker %s in %s/%s: %w (output: %s)", path, ns, pod, err, out)
	}
	return out, nil
}

// findWorkspacePod returns the (single) running workspace pod for
// appName/userKey in ns, using the same WorkspacePodLabels selector the
// operator itself renders the Deployment/Service from.
func findWorkspacePod(ctx context.Context, cs kubernetes.Interface, ns, appName, userKey string) (*corev1.Pod, error) {
	labels := workspacePodLabelSelector(appName, userKey)
	pods, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: labels})
	if err != nil {
		return nil, err
	}
	if len(pods.Items) == 0 {
		return nil, fmt.Errorf("no workspace pod found for app=%s user-key=%s in %s (selector %q)", appName, userKey, ns, labels)
	}
	return &pods.Items[0], nil
}

// workspacePodLabelSelector mirrors internal/controller.WorkspacePodLabels
// as a label-selector string, without importing that package's Kubernetes
// object types into this test binary's dependency graph just for a List
// call.
func workspacePodLabelSelector(appName, userKey string) string {
	return fmt.Sprintf("%s=%s,%s=%s,%s=%s",
		v1alpha1.LabelApp, appName,
		v1alpha1.LabelUserKey, userKey,
		v1alpha1.LabelComponent, v1alpha1.ComponentWorkspace)
}

// routerURL builds the in-cluster URL for appName's router Service in ns.
func routerURL(appName, ns string) string {
	return fmt.Sprintf("http://%s-router.%s.svc.cluster.local:%d/", appName, ns, v1alpha1.RouterPort)
}

// coldStart issues one authenticated request to appName's router in ns from
// clientPod, carrying identityHeader: identityValue, and returns the HTTP
// status code observed. A non-empty markerPath is appended to the URL so the
// request also doubles as a marker-file read once the fixture's nginx
// workspace is up (unused by the plain cold-start callers in this file, but
// shared with any caller that wants both in one round trip).
func coldStart(env e2eEnv, ns, clientPod, appName, identityValue string) (string, error) {
	url := routerURL(appName, ns)
	out, err := runInPod(env.Kubeconfig, ns, clientPod,
		"curl", "-sS", "--max-time", "120", "-o", "/dev/null", "-w", "%{http_code}",
		"-H", "Authorization: Bearer "+env.CallerToken,
		"-H", identityHeader+": "+identityValue,
		url,
	)
	if err != nil {
		return "", fmt.Errorf("cold start request for identity %q via %s failed to run: %w (output: %s)", identityValue, url, err, out)
	}
	return strings.TrimSpace(out), nil
}

// waitWorkspaceReady polls the Workspace named identity.ChildName(appName,
// userKey) in ns until its status.phase is Ready, or ctx expires.
func waitWorkspaceReady(ctx context.Context, ns, name string) (*v1alpha1.Workspace, error) {
	var ws v1alpha1.Workspace
	err := pollUntil(ctx, 3*time.Second, func() (bool, error) {
		if err := globalRuntimeClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &ws); err != nil {
			return false, err
		}
		return ws.Status.Phase == v1alpha1.PhaseReady, nil
	})
	if err != nil {
		return nil, fmt.Errorf("workspace %s/%s did not reach Ready: %w (last phase %q)", ns, name, err, ws.Status.Phase)
	}
	return &ws, nil
}

// probeResult is a single connectivity probe's outcome: Reached is true iff
// curl completed the request and got an HTTP response at all (any status
// code) -- the network-layer question every isolation probe in this package
// asks. A NetworkPolicy DROP (Calico's default deny action) never sends a
// TCP RST, so a blocked probe times out rather than being refused
// instantly; MaxTimeSecs bounds that wait.
type probeResult struct {
	Reached bool
	Code    string
	Raw     string
}

// probeHTTP runs curl from inside srcPod against url with optional headers,
// bounded by maxTimeSecs. Reached=false covers both a hard connection
// refusal and a NetworkPolicy-induced timeout; Reached=true means an HTTP
// response of some status code came back, which is this package's
// reachability signal (see probeResult).
func probeHTTP(kubeconfig, ns, srcPod, url string, headers map[string]string, maxTimeSecs int) probeResult {
	args := []string{"curl", "-sS", "--max-time", strconv.Itoa(maxTimeSecs), "-o", "/dev/null", "-w", "%{http_code}"}
	for k, v := range headers {
		args = append(args, "-H", k+": "+v)
	}
	args = append(args, url)
	out, err := runInPod(kubeconfig, ns, srcPod, args...)
	if err != nil {
		return probeResult{Reached: false, Raw: out}
	}
	return probeResult{Reached: true, Code: strings.TrimSpace(out), Raw: out}
}

// scaleDeployment patches name's replica count via the Scale subresource.
func scaleDeployment(ctx context.Context, cs kubernetes.Interface, ns, name string, replicas int32) error {
	scale, err := cs.AppsV1().Deployments(ns).GetScale(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get scale for %s/%s: %w", ns, name, err)
	}
	scale.Spec.Replicas = replicas
	_, err = cs.AppsV1().Deployments(ns).UpdateScale(ctx, name, &autoscalingv1.Scale{
		ObjectMeta: scale.ObjectMeta,
		Spec:       autoscalingv1.ScaleSpec{Replicas: replicas},
	}, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("scale %s/%s to %d: %w", ns, name, replicas, err)
	}
	return nil
}

// waitNoPods waits until no pod matching labelSelector remains in ns.
func waitNoPods(ctx context.Context, cs kubernetes.Interface, ns, labelSelector string) error {
	return pollUntil(ctx, 2*time.Second, func() (bool, error) {
		pods, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: labelSelector})
		if err != nil {
			return false, err
		}
		return len(pods.Items) == 0, nil
	})
}

// waitDeploymentReady waits until name in ns has at least one ready replica.
func waitDeploymentReady(ctx context.Context, cs kubernetes.Interface, ns, name string) error {
	return pollUntil(ctx, 2*time.Second, func() (bool, error) {
		dep, err := cs.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		return dep.Status.ReadyReplicas >= 1, nil
	})
}

// quiesceController implements the Step 1 preamble's mandatory procedure for
// assertions 2 and 4: scale the operator controller Deployment to 0 and wait
// for its pods to terminate BEFORE the caller deletes or patches any
// reconciled object, then hand back a restore func that scales it back to 1
// and waits for it to become Ready again. Callers still need to wait for
// their OWN specific object to be reconciled back after calling restore();
// this only guarantees the controller process is running again.
func quiesceController(t *testing.T, ctx context.Context) (restore func()) {
	t.Helper()
	if err := scaleDeployment(ctx, globalClient, operatorNamespace, operatorDeployment, 0); err != nil {
		t.Fatalf("quiesce controller: %v", err)
	}
	sel := fmt.Sprintf("%s=%s,%s=%s", "app.kubernetes.io/part-of", v1alpha1.PartOfValue, v1alpha1.LabelComponent, v1alpha1.ComponentController)
	if err := waitNoPods(ctx, globalClient, operatorNamespace, sel); err != nil {
		t.Fatalf("quiesce controller: waiting for controller pods to terminate: %v", err)
	}
	return func() {
		if err := scaleDeployment(ctx, globalClient, operatorNamespace, operatorDeployment, 1); err != nil {
			t.Fatalf("restore controller: %v", err)
		}
		if err := waitDeploymentReady(ctx, globalClient, operatorNamespace, operatorDeployment); err != nil {
			t.Fatalf("restore controller: waiting Ready: %v", err)
		}
	}
}

// deleteNetworkPolicy deletes a NetworkPolicy, tolerating NotFound so a
// caller can unconditionally clean up more than one candidate name.
func deleteNetworkPolicy(ctx context.Context, cs kubernetes.Interface, ns, name string) error {
	err := cs.NetworkingV1().NetworkPolicies(ns).Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// waitNetworkPolicyExists polls until a NetworkPolicy named name in ns
// exists again (used after restoring the controller, to confirm it
// re-created a policy this test deleted).
func waitNetworkPolicyExists(ctx context.Context, cs kubernetes.Interface, ns, name string) error {
	return pollUntil(ctx, 2*time.Second, func() (bool, error) {
		_, err := cs.NetworkingV1().NetworkPolicies(ns).Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		return true, nil
	})
}

// getServiceClusterIP resolves the ClusterIP of the single Service matching
// labelSelector in ns.
func getServiceClusterIP(ctx context.Context, cs kubernetes.Interface, ns, labelSelector string) (string, error) {
	svcs, err := cs.CoreV1().Services(ns).List(ctx, metav1.ListOptions{LabelSelector: labelSelector})
	if err != nil {
		return "", err
	}
	if len(svcs.Items) == 0 {
		return "", fmt.Errorf("no Service in %s matching %q", ns, labelSelector)
	}
	ip := svcs.Items[0].Spec.ClusterIP
	if ip == "" || ip == "None" {
		return "", fmt.Errorf("Service %s/%s has no usable ClusterIP (%q)", ns, svcs.Items[0].Name, ip)
	}
	return ip, nil
}

// promQueryResult is the minimal shape of Prometheus's /api/v1/query
// response this package needs: the metric label set and the instant value
// for each returned series.
type promQueryResult struct {
	Metric map[string]string
	Value  float64
}

// promQuery runs an instant query against a Prometheus reachable at
// promClusterIP:9090, executed from inside srcPod (so it is subject to the
// same NetworkPolicies any other in-cluster caller would be), and returns
// every series in the result.
func promQuery(kubeconfig, ns, srcPod, promClusterIP, query string) ([]promQueryResult, error) {
	url := fmt.Sprintf("http://%s:9090/api/v1/query?query=%s", promClusterIP, urlQueryEscape(query))
	out, err := runInPod(kubeconfig, ns, srcPod, "curl", "-sS", "--max-time", "30", url)
	if err != nil {
		return nil, fmt.Errorf("prometheus query %q failed to run: %w (output: %s)", query, err, out)
	}

	var parsed struct {
		Status string `json:"status"`
		Data   struct {
			Result []struct {
				Metric map[string]string `json:"metric"`
				Value  []interface{}     `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		return nil, fmt.Errorf("prometheus query %q: unmarshal response %q: %w", query, out, err)
	}
	if parsed.Status != "success" {
		return nil, fmt.Errorf("prometheus query %q did not succeed: %s", query, out)
	}

	var results []promQueryResult
	for _, r := range parsed.Data.Result {
		if len(r.Value) != 2 {
			continue
		}
		s, ok := r.Value[1].(string)
		if !ok {
			continue
		}
		v, err := strconv.ParseFloat(s, 64)
		if err != nil {
			continue
		}
		results = append(results, promQueryResult{Metric: r.Metric, Value: v})
	}
	return results, nil
}

// urlQueryEscape is url.QueryEscape without a second import alias fight in
// every call site above.
func urlQueryEscape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// kubectlHost runs kubectl on the controller host (NOT inside a pod) against
// kubeconfig -- the primitive assertion 3 and assertion 6 need for mutating
// cluster-scoped or Traefik CRD objects this package has no typed client
// for (PersistentVolume claimRef patches, the fixture IngressRoute's
// middlewares list), matching kubectlExec's own reasoning in checks_test.go
// for reaching for the kubectl binary rather than hand-rolling more client-go
// call sites than this test harness needs.
func kubectlHost(kubeconfig string, args ...string) (string, error) {
	full := append([]string{"--kubeconfig", kubeconfig}, args...)
	out, err := exec.CommandContext(context.Background(), "kubectl", full...).CombinedOutput()
	return string(out), err
}

// kubectlApplyStdin runs `kubectl apply -f -`, piping yaml on stdin.
func kubectlApplyStdin(kubeconfig, yaml string) (string, error) {
	cmd := exec.CommandContext(context.Background(), "kubectl", "--kubeconfig", kubeconfig, "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(yaml)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// deploymentReplicaCount reads the current desired replica count.
func deploymentReplicas(ctx context.Context, cs kubernetes.Interface, ns, name string) (int32, error) {
	dep, err := cs.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return 0, err
	}
	if dep.Spec.Replicas == nil {
		return 1, nil
	}
	return *dep.Spec.Replicas, nil
}
