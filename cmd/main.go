// Command per-user-container-operator dispatches to the controller, router,
// and userkey subcommands.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kettleofketchup/per-user-container-operator/api/v1alpha1"
	"github.com/kettleofketchup/per-user-container-operator/internal/identity"
	"github.com/kettleofketchup/per-user-container-operator/internal/metrics"
	"github.com/kettleofketchup/per-user-container-operator/internal/router"
)

// shutdownTimeout bounds how long runRouter waits, on SIGTERM/SIGINT, for
// http.Server.Shutdown to drain idle connections and in-flight NON-hijacked
// requests before returning regardless. See runRouter's doc comment for
// what this deliberately does NOT wait for.
const shutdownTimeout = 15 * time.Second

func run(argv []string) error {
	if len(argv) < 2 {
		return errors.New("usage: per-user-container-operator <controller|router|userkey>")
	}
	switch argv[1] {
	// controller wired in Task 11.
	case "controller":
		return nil
	case "router":
		return runRouter(argv[2:])
	case "userkey":
		if len(argv) != 5 {
			return errors.New("usage: per-user-container-operator userkey <namespace> <appName> <rawIdentity>")
		}
		fmt.Println(identity.UserKey(argv[2], argv[3], argv[4]))
		return nil
	default:
		return fmt.Errorf("unknown subcommand %q", argv[1])
	}
}

// parseRouterFlags parses the router's ENTIRE startup contract (see
// task-10-brief.md): the router never reads its own PerUserApp, so every
// value it needs arrives as a flag rendered onto the Deployment by the
// controller. --listen-addr and --metrics-addr default to the two shared
// port constants (v1alpha1.RouterPort / MetricsPort) that Task 5's
// NetworkPolicy and Task 11's Deployment/Service rendering also use, so all
// three consumers agree on one number without any of them inventing it.
// The three --upstream-auth-* flags are conditional: they are simply absent
// from argv when spec.workspace.upstreamAuth is unset on the PerUserApp,
// and this function must accept that absence rather than requiring them.
func parseRouterFlags(args []string) (router.Config, error) {
	fs := flag.NewFlagSet("router", flag.ContinueOnError)

	app := fs.String("app", "", "PerUserApp name this router serves")
	namespace := fs.String("namespace", "", "namespace this router serves")
	identityHeader := fs.String("identity-header", "", "header carrying the caller's raw identity")
	identityMaxLength := fs.Int("identity-max-length", 256, "maximum accepted length of the identity header value")
	callerAuthHeader := fs.String("caller-auth-header", "", "header carrying the caller's credential")
	callerAuthScheme := fs.String("caller-auth-scheme", "", "scheme prefix for the caller-auth header value (e.g. Bearer)")
	callerAuthSecretFile := fs.String("caller-auth-secret-file", "", "path to the mounted caller-auth secret value")
	upstreamAuthHeader := fs.String("upstream-auth-header", "", "header the router presents the workspace, if any")
	upstreamAuthScheme := fs.String("upstream-auth-scheme", "", "scheme prefix for the upstream-auth header value")
	upstreamAuthSecretFile := fs.String("upstream-auth-secret-file", "", "path to the mounted upstream-auth secret value, if any")
	workspacePort := fs.Int("workspace-port", 0, "port the workspace container listens on")
	coldStartHold := fs.Duration("cold-start-hold", 0, "how long to hold a request for a workspace to become servable")
	connectionHeartbeat := fs.Duration("connection-heartbeat", 0, "how often this replica refreshes its status.connections heartbeat")
	maxWorkspaces := fs.Int("max-workspaces", 0, "spec.limits.maxWorkspaces, mirrored as a flag since the router holds no peruserapps grant")
	listenAddr := fs.String("listen-addr", fmt.Sprintf(":%d", v1alpha1.RouterPort), "address the proxy listens on")
	metricsAddr := fs.String("metrics-addr", fmt.Sprintf(":%d", v1alpha1.MetricsPort), "address metrics are served on")

	if err := fs.Parse(args); err != nil {
		return router.Config{}, err
	}

	if *app == "" || *namespace == "" {
		return router.Config{}, errors.New("--app and --namespace are required")
	}
	if *identityHeader == "" {
		return router.Config{}, errors.New("--identity-header is required")
	}
	if *callerAuthHeader == "" || *callerAuthSecretFile == "" {
		return router.Config{}, errors.New("--caller-auth-header and --caller-auth-secret-file are required: callerAuth is mandatory, never optional")
	}

	callerSecret, err := router.ReadSecretFile(*callerAuthSecretFile)
	if err != nil {
		return router.Config{}, fmt.Errorf("read caller-auth secret file: %w", err)
	}

	cfg := router.Config{
		App:                         *app,
		Namespace:                   *namespace,
		IdentityHeader:              *identityHeader,
		IdentityMaxLength:           *identityMaxLength,
		CallerAuthHeader:            *callerAuthHeader,
		CallerAuthScheme:            *callerAuthScheme,
		CallerAuthSecret:            callerSecret,
		WorkspacePort:               int32(*workspacePort),
		ColdStartHold:               *coldStartHold,
		ConnectionHeartbeatInterval: *connectionHeartbeat,
		MaxWorkspaces:               int32(*maxWorkspaces),
		PodName:                     os.Getenv("POD_NAME"),
		ListenAddr:                  *listenAddr,
		MetricsAddr:                 *metricsAddr,
	}

	// upstreamAuth is optional and the absent case is a branch, not an
	// oversight (task-10-brief.md): Task 11 renders neither the secret
	// volume nor these three flags when spec.workspace.upstreamAuth is
	// unset, and the router must set no upstream credential while still
	// performing the strip.
	if *upstreamAuthHeader != "" {
		if *upstreamAuthSecretFile == "" {
			return router.Config{}, errors.New("--upstream-auth-header given without --upstream-auth-secret-file")
		}
		upstreamSecret, err := router.ReadSecretFile(*upstreamAuthSecretFile)
		if err != nil {
			return router.Config{}, fmt.Errorf("read upstream-auth secret file: %w", err)
		}
		cfg.UpstreamAuthHeader = *upstreamAuthHeader
		cfg.UpstreamAuthScheme = *upstreamAuthScheme
		cfg.UpstreamAuthSecret = upstreamSecret
	}

	return cfg, nil
}

// runRouter builds a client from the mounted <app>-router ServiceAccount
// token (rest.InClusterConfig — this binary runs no manager, so it does not
// go through controller-runtime's manager/cache setup) and serves the proxy
// on --listen-addr and metrics on --metrics-addr, per Task 7's shared
// registry (promhttp.HandlerFor(metrics.Gatherer(), ...)) rather than
// standing up its own manager or registry.
func runRouter(args []string) error {
	cfg, err := parseRouterFlags(args)
	if err != nil {
		return err
	}

	restCfg, err := rest.InClusterConfig()
	if err != nil {
		return fmt.Errorf("in-cluster config: %w", err)
	}

	scheme := runtime.NewScheme()
	utilruntime.Must(corev1.AddToScheme(scheme))
	utilruntime.Must(discoveryv1.AddToScheme(scheme))
	utilruntime.Must(v1alpha1.AddToScheme(scheme))

	c, err := client.NewWithWatch(restCfg, client.Options{Scheme: scheme})
	if err != nil {
		return fmt.Errorf("new client: %w", err)
	}

	srv := router.NewServer(cfg, c)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go srv.Conns.Run(ctx)
	go func() { _ = srv.Resolver.WatchServiceDeletes(ctx, c, cfg.Namespace) }()
	go func() { _ = srv.Resolver.WatchWorkspaceDeletes(ctx, c, cfg.Namespace) }()

	metricsMux := http.NewServeMux()
	metricsMux.Handle("/metrics", promhttp.HandlerFor(metrics.Gatherer(), promhttp.HandlerOpts{}))
	metricsServer := &http.Server{Handler: metricsMux, ReadHeaderTimeout: 5 * time.Second}
	proxyServer := &http.Server{Handler: srv, ReadHeaderTimeout: 5 * time.Second}

	metricsLn, err := net.Listen("tcp", cfg.MetricsAddr)
	if err != nil {
		return fmt.Errorf("listen metrics: %w", err)
	}
	proxyLn, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("listen proxy: %w", err)
	}

	sigCtx, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stopSignals()

	return serveAndAwaitShutdown(metricsLn, proxyLn, metricsServer, proxyServer, sigCtx.Done(), shutdownTimeout)
}

// serveAndAwaitShutdown starts metricsServer and proxyServer in the
// background, blocks until either one fails to bind/serve or stop fires,
// and on stop performs a graceful shutdown of both. stop is the signal
// source (SIGTERM/SIGINT in production, an arbitrary channel in tests).
//
// Graceful shutdown here means exactly what http.Server.Shutdown documents
// and no more: it stops accepting new connections, then waits (up to
// shutdownTimeout) for idle connections to close and in-flight, NON-hijacked
// requests to finish. It explicitly does NOT track or wait for hijacked
// connections — this proxy's WebSocket upgrades and any still-streaming SSE
// response are hijacked or long-lived and are NOT drained here: they keep
// running, using the listener that just stopped accepting new connections,
// until they finish on their own or the process is killed outright after
// this function returns. This bounds the blast radius of a rolling update
// to "new requests are refused promptly and ordinary requests finish
// cleanly," not "every open session drains gracefully."
func serveAndAwaitShutdown(metricsLn, proxyLn net.Listener, metricsServer, proxyServer *http.Server, stop <-chan struct{}, shutdownTimeout time.Duration) error {
	// Both servers run in the background against listeners the caller
	// already bound (production binds cfg.MetricsAddr/ListenAddr directly;
	// tests bind ephemeral loopback ports so they can talk to a real
	// server). A Serve failure on either is reported on serveErrCh so it
	// aborts instead of running half-wired forever.
	serveErrCh := make(chan error, 2)
	go func() {
		if err := metricsServer.Serve(metricsLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErrCh <- fmt.Errorf("metrics server: %w", err)
		}
	}()
	go func() {
		if err := proxyServer.Serve(proxyLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErrCh <- fmt.Errorf("proxy server: %w", err)
		}
	}()

	select {
	case err := <-serveErrCh:
		return err
	case <-stop:
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer shutdownCancel()
	_ = proxyServer.Shutdown(shutdownCtx)
	_ = metricsServer.Shutdown(shutdownCtx)
	return nil
}

func main() {
	if err := run(os.Args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
