//go:build e2e

// Package e2e is the E2E harness this plan's Task 13 assertions run
// against: a kind cluster (test/e2e/kind-up.sh) running Calico, Traefik and
// kube-prometheus-stack, or -- for Tasks 15-17's single, -run-selected
// invocations -- the live edge cluster instead. Every value the suite needs
// to talk to that environment arrives through the PUC_E2E_* environment
// contract documented in kind-up.sh's header comment, never hardcoded here.
package e2e

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// e2eEnv is the parsed PUC_E2E_* environment contract.
type e2eEnv struct {
	StorageClass        string
	Namespaces          []string
	CallerTokenFile     string
	CallerToken         string
	WorkspaceImage      string
	ColdStartIdentities int
	// WorkspacePort is PUC_E2E_WORKSPACE_PORT: the port the substitute
	// image named by PUC_E2E_WORKSPACE_IMAGE actually listens on. Optional,
	// and zero when unset -- meaning "leave every CR's own
	// spec.workspace.port alone", which is what a live-cluster invocation
	// running a consumer's real image wants. kind-up.sh sets it because its
	// fixture image is nginx-unprivileged on 8080 while
	// examples/workspace-app.yaml declares workspace-app's own 7399: a CR whose image is
	// substituted but whose port is not never passes readiness, and the
	// router answers 503 for the whole cold-start hold with nothing in the
	// failure naming the port.
	WorkspacePort int
	// MigrationImage is Task 16's PUC_E2E_MIGRATION_IMAGE. It is not
	// required here: kind-up.sh does not set it (Task 16's migration image
	// does not exist at this point in the plan), so it is read
	// best-effort and left empty when absent.
	MigrationImage string
	Kubeconfig     string
}

// packageDir returns this source file's own directory, independent of the
// test binary's actual working directory: `go test` conventionally runs
// with cwd set to the package directory, but computing this from
// runtime.Caller means the generated-env lookup below does not silently
// break under a different invocation style.
func packageDir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Dir(file)
}

// loadGeneratedEnvFile reads kind-up.sh's KEY=VALUE output file. A missing
// file is not an error: a non-kind invocation (Tasks 15-17 against the live
// edge cluster) never runs kind-up.sh and exports the real values directly
// instead -- see getenv.
func loadGeneratedEnvFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]string{}, nil
		}
		return nil, err
	}
	defer f.Close()

	out := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		out[k] = v
	}
	return out, sc.Err()
}

// getenv prefers the real process environment: kind-up.sh and this test
// binary are separate processes in the Makefile's `e2e` target (`./test/e2e
// /kind-up.sh && go test ...`), so a plain `export` inside the script never
// reaches here -- kind-up.sh writes the generated-env file instead, and
// this is the ONLY fallback path that reads it. A non-kind invocation that
// exports PUC_E2E_* directly is never shadowed by a stale generated file.
func getenv(fromFile map[string]string, key string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fromFile[key]
}

// loadEnv parses the PUC_E2E_* contract, preferring real environment
// variables over kind-up.sh's generated file (see getenv), and fails loudly
// -- naming every missing value -- rather than let a nil Namespaces slice
// or an empty image string surface three tests later as an unrelated panic
// or a confusing apiserver error.
func loadEnv() (e2eEnv, error) {
	fileVars, err := loadGeneratedEnvFile(filepath.Join(packageDir(), ".generated-env"))
	if err != nil {
		return e2eEnv{}, fmt.Errorf("read generated env file: %w", err)
	}

	env := e2eEnv{
		StorageClass:    getenv(fileVars, "PUC_E2E_STORAGECLASS"),
		CallerTokenFile: getenv(fileVars, "PUC_E2E_CALLER_TOKEN_FILE"),
		WorkspaceImage:  getenv(fileVars, "PUC_E2E_WORKSPACE_IMAGE"),
		MigrationImage:  getenv(fileVars, "PUC_E2E_MIGRATION_IMAGE"),
		Kubeconfig:      getenv(fileVars, "PUC_E2E_KUBECONFIG"),
	}

	if ns := getenv(fileVars, "PUC_E2E_NAMESPACES"); ns != "" {
		for _, n := range strings.Split(ns, ",") {
			n = strings.TrimSpace(n)
			if n != "" {
				env.Namespaces = append(env.Namespaces, n)
			}
		}
	}

	var missing []string
	if env.StorageClass == "" {
		missing = append(missing, "PUC_E2E_STORAGECLASS")
	}
	if len(env.Namespaces) == 0 {
		missing = append(missing, "PUC_E2E_NAMESPACES")
	}
	if env.CallerTokenFile == "" {
		missing = append(missing, "PUC_E2E_CALLER_TOKEN_FILE")
	}
	if env.WorkspaceImage == "" {
		missing = append(missing, "PUC_E2E_WORKSPACE_IMAGE")
	}
	if len(missing) > 0 {
		return e2eEnv{}, fmt.Errorf("missing required environment: %s -- run test/e2e/kind-up.sh (set PUC_E2E_SKIP_CLUSTER=1 and export these directly for a non-kind invocation)", strings.Join(missing, ", "))
	}

	csi := getenv(fileVars, "PUC_E2E_COLD_START_IDENTITIES")
	if csi == "" {
		csi = "1"
	}
	n, err := strconv.Atoi(csi)
	if err != nil {
		return e2eEnv{}, fmt.Errorf("PUC_E2E_COLD_START_IDENTITIES=%q: %w", csi, err)
	}
	env.ColdStartIdentities = n

	if wp := getenv(fileVars, "PUC_E2E_WORKSPACE_PORT"); wp != "" {
		p, err := strconv.Atoi(wp)
		if err != nil {
			return e2eEnv{}, fmt.Errorf("PUC_E2E_WORKSPACE_PORT=%q: %w", wp, err)
		}
		env.WorkspacePort = p
	}

	tokenBytes, err := os.ReadFile(env.CallerTokenFile)
	if err != nil {
		return e2eEnv{}, fmt.Errorf("read caller token file %s: %w", env.CallerTokenFile, err)
	}
	env.CallerToken = strings.TrimRight(string(tokenBytes), "\n")

	return env, nil
}
