// Package metrics defines every Prometheus series this operator produces
// and registers each one exactly once, at package initialization, into
// controller-runtime's own registry
// (sigs.k8s.io/controller-runtime/pkg/metrics.Registry). There is no
// separate Register() call to invoke: the fifteen collectors below are
// package-level vars built with promauto, and Go initializes a package's
// var block exactly once per process no matter how many callers import it
// — so there is no ambiguous or double-invokable registration path.
//
// This is the ONLY package in the operator that constructs a Prometheus
// collector. Tasks that record metrics (the workspace reconcile loop, the
// admission webhook, the reaper, the router, the controller entrypoint) call
// the free functions below; they never build their own CounterVec or
// GaugeVec, which is what this task's early ordering in the plan exists to
// guarantee — a second collector for, say, puc_workspace_reaped_total would
// panic every process that imports both packages with
// prometheus.AlreadyRegisteredError at controller startup.
//
// EVERY per-app series below carries both namespace and app: PerUserApp is
// namespaced and the controller serves several namespaces, so app alone
// would collapse two same-named CRs in different namespaces into one series
// that reads as their sum. The two named exceptions are puc_controller_leader
// (pod alone — fanning leadership across namespace/app labels breaks
// sum(puc_controller_leader) != 1) and puc_watched_namespace_ready
// (namespace alone — readiness is a property of the namespace grant, not of
// any one PerUserApp in it).
//
// The two rejection counters carry disjoint closed reason sets.
// puc_router_identity_rejected_total owns identity derivation only
// (missing, empty, too_long, duplicate, invalid, from internal/identity) —
// a rise there means a caller stopped sending a trustworthy identity header,
// the silent-degradation failure this project exists to detect.
// puc_router_request_rejected_total owns every other 503 (hold_expired,
// backoff, rwop_conflict, workspace_limit, terminating, from api/v1alpha1).
// No string may appear in both sets; see TestRejectionReasonSetsAreDisjoint.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/kettleofketchup/per-user-container-operator/internal/identity"
)

var (
	// Workspace reconcile loop (Task 6).

	workspacesByPhase = promauto.With(ctrlmetrics.Registry).NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "puc_workspaces",
			Help: "Current number of Workspace objects, by phase.",
		},
		[]string{"namespace", "app", "phase"},
	)

	workspacePVCsTotal = promauto.With(ctrlmetrics.Registry).NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "puc_workspace_pvcs_total",
			Help: "Current number of PersistentVolumeClaims owned by workspaces.",
		},
		[]string{"namespace", "app"},
	)

	workspaceUserInfo = promauto.With(ctrlmetrics.Registry).NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "puc_workspace_user_info",
			Help: "Join metric mapping a workspace's user key to its display name. 1 while the mapping is present, 0 once it is not.",
		},
		[]string{"namespace", "app", "user_key", "user_display"},
	)

	workspaceStartsTotal = promauto.With(ctrlmetrics.Registry).NewCounterVec(
		prometheus.CounterOpts{
			Name: "puc_workspace_starts_total",
			Help: "Total workspace start attempts, by result.",
		},
		[]string{"namespace", "app", "result"},
	)

	workspaceStartSeconds = promauto.With(ctrlmetrics.Registry).NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "puc_workspace_start_seconds",
			Help:    "Time from Workspace creation to Ready, in seconds.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"namespace", "app"},
	)

	reconcileErrorsTotal = promauto.With(ctrlmetrics.Registry).NewCounterVec(
		prometheus.CounterOpts{
			Name: "puc_reconcile_errors_total",
			Help: "Total reconcile errors, by kind.",
		},
		[]string{"namespace", "app", "kind"},
	)

	// Admission (Task 8).

	startFailuresTotal = promauto.With(ctrlmetrics.Registry).NewCounterVec(
		prometheus.CounterOpts{
			Name: "puc_workspace_start_failures_total",
			Help: "Total workspace start failures, by reason.",
		},
		[]string{"namespace", "app", "reason"},
	)

	// Reaper (Task 9).

	workspaceReapedTotal = promauto.With(ctrlmetrics.Registry).NewCounterVec(
		prometheus.CounterOpts{
			Name: "puc_workspace_reaped_total",
			Help: "Total workspaces reaped, by reason.",
		},
		[]string{"namespace", "app", "reason"},
	)

	reaperLastCompletion = promauto.With(ctrlmetrics.Registry).NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "puc_reaper_last_completion_timestamp_seconds",
			Help: "Unix timestamp of the reaper's last completed pass. A wedged reaper looks like a busy Monday in every other metric; this one catches it.",
		},
		[]string{"namespace", "app"},
	)

	// Router (Task 10).

	routerRequestsTotal = promauto.With(ctrlmetrics.Registry).NewCounterVec(
		prometheus.CounterOpts{
			Name: "puc_router_requests_total",
			Help: "Total requests handled by the router, by response code.",
		},
		[]string{"namespace", "app", "code"},
	)

	routerIdentityRejectedTotal = promauto.With(ctrlmetrics.Registry).NewCounterVec(
		prometheus.CounterOpts{
			Name: "puc_router_identity_rejected_total",
			Help: "Total requests rejected during identity header derivation, by reason (missing, empty, too_long, duplicate, invalid). A sustained rate means a caller stopped sending a trustworthy identity header.",
		},
		[]string{"namespace", "app", "reason"},
	)

	routerRequestRejectedTotal = promauto.With(ctrlmetrics.Registry).NewCounterVec(
		prometheus.CounterOpts{
			Name: "puc_router_request_rejected_total",
			Help: "Total requests rejected for reasons other than identity derivation, by reason (hold_expired, backoff, rwop_conflict, workspace_limit, terminating).",
		},
		[]string{"namespace", "app", "reason"},
	)

	routerOpenUpgradedConnections = promauto.With(ctrlmetrics.Registry).NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "puc_router_open_upgraded_connections",
			Help: "Current number of open upgraded (e.g. WebSocket) connections, by router pod.",
		},
		[]string{"namespace", "app", "pod"},
	)

	// Controller entrypoint (Task 11).

	watchedNamespaceReady = promauto.With(ctrlmetrics.Registry).NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "puc_watched_namespace_ready",
			Help: "1 if the controller's startup RBAC self-check succeeded for this watched namespace, 0 otherwise.",
		},
		[]string{"namespace"},
	)

	controllerLeader = promauto.With(ctrlmetrics.Registry).NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "puc_controller_leader",
			Help: "1 if this controller pod currently holds leadership, 0 otherwise.",
		},
		[]string{"pod"},
	)
)

// SetWorkspacesByPhase sets the current count of Workspace objects in ns/app
// that are in phase to n.
func SetWorkspacesByPhase(ns, app, phase string, n float64) {
	workspacesByPhase.WithLabelValues(ns, app, phase).Set(n)
}

// SetWorkspacePVCsTotal sets the current count of PersistentVolumeClaims
// owned by workspaces in ns/app to n.
func SetWorkspacePVCsTotal(ns, app string, n float64) {
	workspacePVCsTotal.WithLabelValues(ns, app).Set(n)
}

// SetWorkspaceUserInfo records whether userKey currently maps to
// userDisplay in ns/app: 1 when present, 0 when not.
func SetWorkspaceUserInfo(ns, app, userKey, userDisplay string, present bool) {
	workspaceUserInfo.WithLabelValues(ns, app, userKey, userDisplay).Set(boolToFloat(present))
}

// DeleteWorkspaceUserInfo removes the join-metric row for (ns, app, userKey,
// userDisplay) entirely, rather than leaving it behind at 0. This is the
// ABSENT path: a workspace/app slug later reused by a different user would
// otherwise leave the old row at 0 beside the new row at 1, and a Grafana
// group_left join keyed on (ns, app) would see two right-hand matches — the
// join errors or resolves ambiguously, and any panel without an explicit
// ==1 filter lists departed users forever. Call this when a Workspace is
// deleted; call SetWorkspaceUserInfo(..., true) while it exists.
func DeleteWorkspaceUserInfo(ns, app, userKey, userDisplay string) {
	workspaceUserInfo.DeleteLabelValues(ns, app, userKey, userDisplay)
}

// RecordWorkspaceStart increments the workspace start attempt counter for
// ns/app with the given result.
func RecordWorkspaceStart(ns, app, result string) {
	workspaceStartsTotal.WithLabelValues(ns, app, result).Inc()
}

// ObserveWorkspaceStartSeconds records a workspace start latency sample, in
// seconds, for ns/app.
func ObserveWorkspaceStartSeconds(ns, app string, seconds float64) {
	workspaceStartSeconds.WithLabelValues(ns, app).Observe(seconds)
}

// RecordReconcileError increments the reconcile error counter for ns/app
// with the given error kind.
func RecordReconcileError(ns, app, kind string) {
	reconcileErrorsTotal.WithLabelValues(ns, app, kind).Inc()
}

// RecordStartFailure increments the workspace start failure counter for
// ns/app with the given reason. reason must be one of the closed set in
// api/v1alpha1 (StartFailureOrphaned, StartFailureTimeout, StartFailureCrash).
func RecordStartFailure(ns, app, reason string) {
	startFailuresTotal.WithLabelValues(ns, app, reason).Inc()
}

// RecordWorkspaceReaped increments the workspace reaped counter for ns/app
// with the given reason. reason must be one of the closed set in
// api/v1alpha1 (currently only ReapReasonIdle).
func RecordWorkspaceReaped(ns, app, reason string) {
	workspaceReapedTotal.WithLabelValues(ns, app, reason).Inc()
}

// SetReaperLastCompletion sets the reaper's last-completion timestamp, as
// Unix seconds, for ns/app.
func SetReaperLastCompletion(ns, app string, unixSeconds float64) {
	reaperLastCompletion.WithLabelValues(ns, app).Set(unixSeconds)
}

// RecordRouterRequest increments the router request counter for ns/app with
// the given response code.
func RecordRouterRequest(ns, app, code string) {
	routerRequestsTotal.WithLabelValues(ns, app, code).Inc()
}

// RecordIdentityRejection increments the identity-rejection counter for
// ns/app with the given reason. r comes from internal/identity's closed set
// (ReasonMissing, ReasonEmpty, ReasonTooLong, ReasonDuplicate, ReasonInvalid)
// and MUST NEVER be a value from api/v1alpha1's request-rejection reasons —
// see the package doc and TestRejectionReasonSetsAreDisjoint.
func RecordIdentityRejection(ns, app string, r identity.Reason) {
	routerIdentityRejectedTotal.WithLabelValues(ns, app, string(r)).Inc()
}

// RecordRequestRejection increments the request-rejection counter for
// ns/app with the given reason. reason comes from api/v1alpha1's closed set
// (RejectHoldExpired, RejectBackoff, RejectRWOPConflict, RejectWorkspaceLimit,
// RejectTerminating) and MUST NEVER be a value from internal/identity's
// rejection reasons — see the package doc and
// TestRejectionReasonSetsAreDisjoint.
func RecordRequestRejection(ns, app, reason string) {
	routerRequestRejectedTotal.WithLabelValues(ns, app, reason).Inc()
}

// SetOpenUpgradedConnections sets the current count of open upgraded
// connections for ns/app on the given router pod.
func SetOpenUpgradedConnections(ns, app, pod string, n float64) {
	routerOpenUpgradedConnections.WithLabelValues(ns, app, pod).Set(n)
}

// SetWatchedNamespaceReady records whether the controller's startup RBAC
// self-check succeeded for ns. Carries namespace ALONE: readiness is a
// property of the namespace grant, not of any one PerUserApp within it.
func SetWatchedNamespaceReady(ns string, ready bool) {
	watchedNamespaceReady.WithLabelValues(ns).Set(boolToFloat(ready))
}

// SetLeader records whether pod currently holds controller leadership.
// Carries pod ALONE — no namespace, no app: fanning a single leadership bit
// out across per-namespace or per-app series would break the shipped alert
// expression sum(puc_controller_leader) != 1.
func SetLeader(pod string, leader bool) {
	controllerLeader.WithLabelValues(pod).Set(boolToFloat(leader))
}

// Gatherer returns the prometheus.Gatherer this package registers into:
// sigs.k8s.io/controller-runtime/pkg/metrics.Registry, and nothing else.
// This is deliberate, not the reflexive "private registry" default — see the
// package doc's explanation and the task-7-report.md Self-Review. The
// controller binary's manager serves that exact registry on /metrics, and
// the router (which runs no manager) serves it directly via
// promhttp.HandlerFor(metrics.Gatherer(), promhttp.HandlerOpts{}), so both
// binaries and every test that calls Gatherer() observe the same series.
func Gatherer() prometheus.Gatherer {
	return ctrlmetrics.Registry
}

// ResetForTest zeroes every series this package registers, by calling
// Reset() on each underlying *Vec. It does NOT swap or replace the
// registry — controller-runtime's Registry is a package-level singleton and
// unregistering from it is neither necessary nor safe to repeat across
// tests — so a test needs only to call ResetForTest() in setup to start from
// a clean slate.
func ResetForTest() {
	workspacesByPhase.Reset()
	workspacePVCsTotal.Reset()
	workspaceUserInfo.Reset()
	workspaceStartsTotal.Reset()
	workspaceStartSeconds.Reset()
	reconcileErrorsTotal.Reset()
	startFailuresTotal.Reset()
	workspaceReapedTotal.Reset()
	reaperLastCompletion.Reset()
	routerRequestsTotal.Reset()
	routerIdentityRejectedTotal.Reset()
	routerRequestRejectedTotal.Reset()
	routerOpenUpgradedConnections.Reset()
	watchedNamespaceReady.Reset()
	controllerLeader.Reset()
}

func boolToFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
