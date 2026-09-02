package router

import (
	"bytes"
	"os"
	"time"
)

// Config is the router's entire startup contract. The router never reads
// its own PerUserApp — it holds no peruserapps grant — so every value here
// arrives as a flag rendered onto the Deployment by the controller (see
// task-10-brief.md's "Startup contract").
type Config struct {
	App       string
	Namespace string

	IdentityHeader    string
	IdentityMaxLength int

	// SharedPaths mirrors spec.router.sharedPaths: the exact paths on which
	// a GET or HEAD arriving with no identity header is served as
	// identity.Shared instead of rejected. Empty (the default) means every
	// path requires an identity, which is the behaviour every app had before
	// this field existed.
	SharedPaths []string

	CallerAuthHeader string
	CallerAuthScheme string
	CallerAuthSecret []byte

	// UpstreamAuthHeader is empty when spec.workspace.upstreamAuth is unset
	// on the PerUserApp: an app with no upstream credential is legal, and
	// the router still performs the strip but sets no credential.
	UpstreamAuthHeader string
	UpstreamAuthScheme string
	UpstreamAuthSecret []byte

	WorkspacePort               int32
	ColdStartHold               time.Duration
	ConnectionHeartbeatInterval time.Duration
	MaxWorkspaces               int32

	// PodName is the status.connections key: the router's own replica pod
	// name, from the downward API's POD_NAME env var.
	PodName string

	ListenAddr  string
	MetricsAddr string
}

// ReadSecretFile reads the credential value mounted at path, trimming a
// single trailing newline: the fixed mount projects the Secret key's value
// verbatim, and most tooling that writes such files appends one.
func ReadSecretFile(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return bytes.TrimRight(b, "\n"), nil
}
