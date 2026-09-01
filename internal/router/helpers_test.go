package router

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/kettleofketchup/per-user-container-operator/api/v1alpha1"
	"github.com/kettleofketchup/per-user-container-operator/internal/metrics"
)

func newRouterScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add corev1: %v", err)
	}
	if err := discoveryv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add discoveryv1: %v", err)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add v1alpha1: %v", err)
	}
	return scheme
}

func newFakeClient(t *testing.T, objs ...client.Object) client.WithWatch {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(newRouterScheme(t)).
		WithStatusSubresource(&v1alpha1.Workspace{}).
		WithObjects(objs...).
		Build()
}

// testConfig returns a Config with every field filled in with a sane test
// default; callers override the fields their scenario cares about.
func testConfig(ns, app string) Config {
	return Config{
		App:                         app,
		Namespace:                   ns,
		IdentityHeader:              "X-User-Id",
		IdentityMaxLength:           256,
		CallerAuthHeader:            "Authorization",
		CallerAuthScheme:            "Bearer",
		CallerAuthSecret:            []byte("caller-secret"),
		WorkspacePort:               8000,
		ColdStartHold:               300 * time.Millisecond,
		ConnectionHeartbeatInterval: 50 * time.Millisecond,
		MaxWorkspaces:               100,
		PodName:                     "router-pod-1",
	}
}

func workspaceLabels(app, userKey string) map[string]string {
	return map[string]string{v1alpha1.LabelApp: app, v1alpha1.LabelUserKey: userKey}
}

func newWorkspace(ns, app, userKey, name string, status v1alpha1.WorkspaceStatus) *v1alpha1.Workspace {
	return &v1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: workspaceLabels(app, userKey)},
		Spec:       v1alpha1.WorkspaceSpec{AppRef: corev1.LocalObjectReference{Name: app}, UserKey: userKey},
		Status:     status,
	}
}

func readyWorkspace(ns, app, userKey, name string) *v1alpha1.Workspace {
	return newWorkspace(ns, app, userKey, name, v1alpha1.WorkspaceStatus{Phase: v1alpha1.PhaseReady})
}

func serviceFor(ns, name, host string, port int) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: corev1.ServiceSpec{
			ClusterIP: host,
			Ports:     []corev1.ServicePort{{Port: int32(port)}},
		},
	}
}

func endpointSliceFor(ns, svcName string, addrs ...string) *discoveryv1.EndpointSlice {
	ready := true
	endpoints := make([]discoveryv1.Endpoint, 0, len(addrs))
	for _, a := range addrs {
		endpoints = append(endpoints, discoveryv1.Endpoint{Addresses: []string{a}, Conditions: discoveryv1.EndpointConditions{Ready: &ready}})
	}
	return &discoveryv1.EndpointSlice{
		ObjectMeta:  metav1.ObjectMeta{Name: svcName + "-eps", Namespace: ns, Labels: map[string]string{discoveryv1.LabelServiceName: svcName}},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints:   endpoints,
	}
}

// emptyEndpointSlice models "EndpointSlice present but empty": a slice
// object exists for the Service but carries zero endpoints.
func emptyEndpointSlice(ns, svcName string) *discoveryv1.EndpointSlice {
	return &discoveryv1.EndpointSlice{
		ObjectMeta:  metav1.ObjectMeta{Name: svcName + "-eps", Namespace: ns, Labels: map[string]string{discoveryv1.LabelServiceName: svcName}},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints:   nil,
	}
}

func doAuthedRequest(t *testing.T, srv *Server, cfg Config, rawIdentity string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(cfg.CallerAuthHeader, cfg.CallerAuthScheme+" "+string(cfg.CallerAuthSecret))
	req.Header.Set(cfg.IdentityHeader, rawIdentity)
	rw := httptest.NewRecorder()
	srv.ServeHTTP(rw, req)
	return rw.Result()
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}

func gatherCounter(t *testing.T, name string, want map[string]string) (float64, bool) {
	t.Helper()
	mfs, err := metrics.Gatherer().Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			if !matchLabels(m, want) {
				continue
			}
			switch {
			case m.Counter != nil:
				return m.Counter.GetValue(), true
			case m.Gauge != nil:
				return m.Gauge.GetValue(), true
			}
		}
	}
	return 0, false
}

func matchLabels(m *dto.Metric, want map[string]string) bool {
	got := map[string]string{}
	for _, lp := range m.GetLabel() {
		got[lp.GetName()] = lp.GetValue()
	}
	for k, v := range want {
		if got[k] != v {
			return false
		}
	}
	return true
}

// fakeClock is a settable clock so tests drive time explicitly instead of
// sleeping on the wall clock.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClockAt(t time.Time) *fakeClock { return &fakeClock{t: t} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func keysOf(m map[string]v1alpha1.ConnectionEntry) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func waitUntil(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("condition not met within %s", timeout)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
