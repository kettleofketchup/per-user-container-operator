package main

import (
	"fmt"
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
