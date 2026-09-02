package main

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kettleofketchup/per-user-container-operator/api/v1alpha1"
)

func TestSubcommandRequired(t *testing.T) {
	if err := run([]string{"per-user-container-operator"}); err == nil {
		t.Fatal("expected error when no subcommand is given")
	}
}

func TestUnknownSubcommandRejected(t *testing.T) {
	if err := run([]string{"per-user-container-operator", "nope"}); err == nil {
		t.Fatal("expected error for unknown subcommand")
	}
}

func writeSecretFile(t *testing.T, value string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "value")
	if err := os.WriteFile(p, []byte(value+"\n"), 0o600); err != nil {
		t.Fatalf("write secret file: %v", err)
	}
	return p
}

// TestParseRouterFlagsDefaultsListenAndMetricsAddr is the "one number, one
// declaration, three consumers" property from task-10-brief.md:
// --listen-addr and --metrics-addr must default to v1alpha1.RouterPort and
// MetricsPort exactly, the same constants Task 5's NetworkPolicy admits and
// Task 11 renders as the Deployment's containerPort / Service targetPort.
func TestParseRouterFlagsDefaultsListenAndMetricsAddr(t *testing.T) {
	callerSecret := writeSecretFile(t, "caller-secret")
	cfg, err := parseRouterFlags([]string{
		"--app", "workspace-app",
		"--namespace", "ns1",
		"--identity-header", "X-User-Id",
		"--caller-auth-header", "Authorization",
		"--caller-auth-scheme", "Bearer",
		"--caller-auth-secret-file", callerSecret,
		"--workspace-port", "8000",
		"--cold-start-hold", "5m",
		"--connection-heartbeat", "30s",
		"--max-workspaces", "200",
	})
	if err != nil {
		t.Fatalf("parseRouterFlags: %v", err)
	}
	wantListen := fmt.Sprintf(":%d", v1alpha1.RouterPort)
	wantMetrics := fmt.Sprintf(":%d", v1alpha1.MetricsPort)
	if cfg.ListenAddr != wantListen {
		t.Fatalf("ListenAddr = %q, want %q", cfg.ListenAddr, wantListen)
	}
	if cfg.MetricsAddr != wantMetrics {
		t.Fatalf("MetricsAddr = %q, want %q", cfg.MetricsAddr, wantMetrics)
	}
	if cfg.App != "workspace-app" || cfg.Namespace != "ns1" {
		t.Fatalf("App/Namespace = %q/%q, want workspace-app/ns1", cfg.App, cfg.Namespace)
	}
	if string(cfg.CallerAuthSecret) != "caller-secret" {
		t.Fatalf("CallerAuthSecret = %q, want %q (trailing newline must be trimmed)", cfg.CallerAuthSecret, "caller-secret")
	}
	if cfg.UpstreamAuthHeader != "" || len(cfg.UpstreamAuthSecret) != 0 {
		t.Fatalf("upstream-auth must be absent when its flags are not given: header=%q secret=%q", cfg.UpstreamAuthHeader, cfg.UpstreamAuthSecret)
	}
	if cfg.ColdStartHold != 5*time.Minute {
		t.Fatalf("ColdStartHold = %v, want 5m", cfg.ColdStartHold)
	}
}

// TestParseRouterFlagsUpstreamAuthConditional exercises the branch, not
// oversight, note in task-10-brief.md: when the three --upstream-auth-*
// flags ARE given (spec.workspace.upstreamAuth was set), the secret file is
// read and both header/scheme are threaded through.
func TestParseRouterFlagsUpstreamAuthConditional(t *testing.T) {
	callerSecret := writeSecretFile(t, "caller-secret")
	upstreamSecret := writeSecretFile(t, "upstream-secret")
	cfg, err := parseRouterFlags([]string{
		"--app", "workspace-app",
		"--namespace", "ns1",
		"--identity-header", "X-User-Id",
		"--caller-auth-header", "Authorization",
		"--caller-auth-secret-file", callerSecret,
		"--upstream-auth-header", "Authorization",
		"--upstream-auth-scheme", "Bearer",
		"--upstream-auth-secret-file", upstreamSecret,
	})
	if err != nil {
		t.Fatalf("parseRouterFlags: %v", err)
	}
	if cfg.UpstreamAuthHeader != "Authorization" || string(cfg.UpstreamAuthSecret) != "upstream-secret" {
		t.Fatalf("upstream auth not threaded through: header=%q secret=%q", cfg.UpstreamAuthHeader, cfg.UpstreamAuthSecret)
	}
}

func TestParseRouterFlagsRequiresCallerAuth(t *testing.T) {
	_, err := parseRouterFlags([]string{"--app", "a", "--namespace", "ns1", "--identity-header", "X-User-Id"})
	if err == nil {
		t.Fatal("callerAuth is mandatory; missing --caller-auth-header/--caller-auth-secret-file must error")
	}
}

func TestParseRouterFlagsRequiresAppAndNamespace(t *testing.T) {
	callerSecret := writeSecretFile(t, "caller-secret")
	_, err := parseRouterFlags([]string{
		"--identity-header", "X-User-Id",
		"--caller-auth-header", "Authorization",
		"--caller-auth-secret-file", callerSecret,
	})
	if err == nil {
		t.Fatal("missing --app/--namespace must error")
	}
}

// TestServeAndAwaitShutdownDrainsInFlightNonHijackedRequest is Finding 2's
// fix: on a stop signal, serveAndAwaitShutdown must let an in-flight,
// ordinary (non-hijacked) request finish rather than killing it outright,
// and must not return until that drain (bounded by shutdownTimeout)
// completes.
func TestServeAndAwaitShutdownDrainsInFlightNonHijackedRequest(t *testing.T) {
	metricsLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen metrics: %v", err)
	}
	proxyLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen proxy: %v", err)
	}

	handlerStarted := make(chan struct{})
	releaseHandler := make(chan struct{})
	proxyServer := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(handlerStarted)
		<-releaseHandler
		w.WriteHeader(http.StatusOK)
	})}
	metricsServer := &http.Server{Handler: http.NewServeMux()}

	stop := make(chan struct{})
	shutdownDone := make(chan error, 1)
	go func() {
		shutdownDone <- serveAndAwaitShutdown(metricsLn, proxyLn, metricsServer, proxyServer, stop, 5*time.Second)
	}()

	// Issue a request and wait for the handler to actually start before
	// triggering shutdown, so the request is genuinely in-flight.
	reqDone := make(chan int, 1)
	go func() {
		resp, err := http.Get("http://" + proxyLn.Addr().String() + "/")
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = resp.Body.Close() }()
		reqDone <- resp.StatusCode
	}()
	select {
	case <-handlerStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("handler never started")
	}

	close(stop)

	// serveAndAwaitShutdown must NOT return while the in-flight request is
	// still being held open by the handler.
	select {
	case err := <-shutdownDone:
		t.Fatalf("serveAndAwaitShutdown returned (err=%v) before the in-flight request finished -- it must drain non-hijacked requests, not kill them", err)
	case <-time.After(200 * time.Millisecond):
	}

	close(releaseHandler)

	select {
	case status := <-reqDone:
		if status != http.StatusOK {
			t.Fatalf("in-flight request status = %d, want 200 (it must complete successfully during shutdown)", status)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight request never completed")
	}

	select {
	case err := <-shutdownDone:
		if err != nil {
			t.Fatalf("serveAndAwaitShutdown returned error %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("serveAndAwaitShutdown never returned after the in-flight request finished")
	}
}

// TestServeAndAwaitShutdownReturnsPromptlyWithNoInFlightRequests is the
// companion case: with no in-flight work, shutdown must complete almost
// immediately rather than blocking for the full shutdownTimeout.
func TestServeAndAwaitShutdownReturnsPromptlyWithNoInFlightRequests(t *testing.T) {
	metricsLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen metrics: %v", err)
	}
	proxyLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen proxy: %v", err)
	}
	metricsServer := &http.Server{Handler: http.NewServeMux()}
	proxyServer := &http.Server{Handler: http.NewServeMux()}

	stop := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- serveAndAwaitShutdown(metricsLn, proxyLn, metricsServer, proxyServer, stop, 30*time.Second)
	}()

	close(stop)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("serveAndAwaitShutdown took far longer than necessary to shut down with no in-flight work")
	}
}

// TestParseRouterFlagsCollectsRepeatedSharedPath: the router holds no
// peruserapps grant, so spec.router.sharedPaths reaches it only as these
// flags. Each entry must survive as its own value, in order.
func TestParseRouterFlagsCollectsRepeatedSharedPath(t *testing.T) {
	callerSecret := writeSecretFile(t, "caller-secret")
	cfg, err := parseRouterFlags([]string{
		"--app", "workspace-app",
		"--namespace", "ns1",
		"--identity-header", "X-User-Id",
		"--caller-auth-header", "Authorization",
		"--caller-auth-secret-file", callerSecret,
		"--shared-path", "/openapi.json",
		"--shared-path", "/.well-known/schema",
	})
	if err != nil {
		t.Fatalf("parseRouterFlags: %v", err)
	}
	want := []string{"/openapi.json", "/.well-known/schema"}
	if len(cfg.SharedPaths) != len(want) {
		t.Fatalf("SharedPaths = %v, want %v", cfg.SharedPaths, want)
	}
	for i := range want {
		if cfg.SharedPaths[i] != want[i] {
			t.Errorf("SharedPaths[%d] = %q, want %q", i, cfg.SharedPaths[i], want[i])
		}
	}
}

// TestParseRouterFlagsRejectsRelativeSharedPath: a shared path is compared
// verbatim against the request path, so a relative one could never match and
// would silently leave the app undiscoverable. Fail at startup instead.
func TestParseRouterFlagsRejectsRelativeSharedPath(t *testing.T) {
	callerSecret := writeSecretFile(t, "caller-secret")
	_, err := parseRouterFlags([]string{
		"--app", "workspace-app",
		"--namespace", "ns1",
		"--identity-header", "X-User-Id",
		"--caller-auth-header", "Authorization",
		"--caller-auth-secret-file", callerSecret,
		"--shared-path", "openapi.json",
	})
	if err == nil {
		t.Fatal("parseRouterFlags accepted a relative --shared-path")
	}
}

// TestParseRouterFlagsDefaultsToNoSharedPaths: absent flags must leave the
// router requiring an identity on every path.
func TestParseRouterFlagsDefaultsToNoSharedPaths(t *testing.T) {
	callerSecret := writeSecretFile(t, "caller-secret")
	cfg, err := parseRouterFlags([]string{
		"--app", "workspace-app",
		"--namespace", "ns1",
		"--identity-header", "X-User-Id",
		"--caller-auth-header", "Authorization",
		"--caller-auth-secret-file", callerSecret,
	})
	if err != nil {
		t.Fatalf("parseRouterFlags: %v", err)
	}
	if len(cfg.SharedPaths) != 0 {
		t.Fatalf("SharedPaths = %v, want empty", cfg.SharedPaths)
	}
}
