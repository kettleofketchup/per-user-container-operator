package metrics_test

import (
	"regexp"
	"sort"
	"testing"

	dto "github.com/prometheus/client_model/go"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/kettleofketchup/per-user-container-operator/api/v1alpha1"
	"github.com/kettleofketchup/per-user-container-operator/internal/identity"
	"github.com/kettleofketchup/per-user-container-operator/internal/metrics"
)

// gather returns every metric family currently on metrics.Gatherer(), keyed
// by name. A family that has never had a WithLabelValues() call made against
// it emits no samples and therefore never appears here — tests must record
// at least one sample per series before asserting on it, exactly as any real
// caller (Task 6, 8, 9, 10, 11) would.
func gather(t *testing.T) map[string]*dto.MetricFamily {
	t.Helper()
	families, err := metrics.Gatherer().Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	out := make(map[string]*dto.MetricFamily, len(families))
	for _, f := range families {
		out[f.GetName()] = f
	}
	return out
}

// labelNames returns the sorted set of label names on the first Metric entry
// of a family. All samples in a family share the same label schema, so the
// first entry is representative.
func labelNames(f *dto.MetricFamily) []string {
	if len(f.GetMetric()) == 0 {
		return nil
	}
	var names []string
	for _, lp := range f.GetMetric()[0].GetLabel() {
		names = append(names, lp.GetName())
	}
	sort.Strings(names)
	return names
}

func sorted(ss []string) []string {
	out := append([]string(nil), ss...)
	sort.Strings(out)
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// recordOneOfEach exercises every one of the fifteen recorders exactly once,
// with distinct sample values, so every series in the spec's table has at
// least one sample on the registry.
func recordOneOfEach() {
	metrics.SetWorkspacesByPhase("ns1", "app1", "Ready", 3)
	metrics.SetWorkspacePVCsTotal("ns1", "app1", 3)
	metrics.SetWorkspaceUserInfo("ns1", "app1", "u-abc123", "alice", true)
	metrics.RecordWorkspaceStart("ns1", "app1", "success")
	metrics.ObserveWorkspaceStartSeconds("ns1", "app1", 4.2)
	metrics.RecordReconcileError("ns1", "app1", "conflict")
	metrics.RecordStartFailure("ns1", "app1", v1alpha1.StartFailureCrash)
	metrics.RecordWorkspaceReaped("ns1", "app1", v1alpha1.ReapReasonIdle)
	metrics.SetReaperLastCompletion("ns1", "app1", 1700000000)
	metrics.RecordRouterRequest("ns1", "app1", "200")
	metrics.RecordIdentityRejection("ns1", "app1", identity.ReasonMissing)
	metrics.RecordRequestRejection("ns1", "app1", v1alpha1.RejectHoldExpired)
	metrics.SetOpenUpgradedConnections("ns1", "app1", "app1-router-abcde", 2)
	metrics.SetWatchedNamespaceReady("ns1", true)
	metrics.SetLeader("controller-0", true)
}

// wantRow is one row of the spec's metrics table (design doc lines 512-528).
type wantRow struct {
	name   string
	typ    dto.MetricType
	labels []string
}

func wantTable() []wantRow {
	return []wantRow{
		{"puc_workspaces", dto.MetricType_GAUGE, []string{"namespace", "app", "phase"}},
		{"puc_workspace_pvcs_total", dto.MetricType_GAUGE, []string{"namespace", "app"}},
		{"puc_workspace_user_info", dto.MetricType_GAUGE, []string{"namespace", "app", "user_key", "user_display"}},
		{"puc_workspace_starts_total", dto.MetricType_COUNTER, []string{"namespace", "app", "result"}},
		{"puc_workspace_start_seconds", dto.MetricType_HISTOGRAM, []string{"namespace", "app"}},
		{"puc_workspace_start_failures_total", dto.MetricType_COUNTER, []string{"namespace", "app", "reason"}},
		{"puc_workspace_reaped_total", dto.MetricType_COUNTER, []string{"namespace", "app", "reason"}},
		{"puc_reaper_last_completion_timestamp_seconds", dto.MetricType_GAUGE, []string{"namespace", "app"}},
		{"puc_controller_leader", dto.MetricType_GAUGE, []string{"pod"}},
		{"puc_watched_namespace_ready", dto.MetricType_GAUGE, []string{"namespace"}},
		{"puc_reconcile_errors_total", dto.MetricType_COUNTER, []string{"namespace", "app", "kind"}},
		{"puc_router_requests_total", dto.MetricType_COUNTER, []string{"namespace", "app", "code"}},
		{"puc_router_identity_rejected_total", dto.MetricType_COUNTER, []string{"namespace", "app", "reason"}},
		{"puc_router_request_rejected_total", dto.MetricType_COUNTER, []string{"namespace", "app", "reason"}},
		{"puc_router_open_upgraded_connections", dto.MetricType_GAUGE, []string{"namespace", "app", "pod"}},
	}
}

// TestAllFifteenSeriesRegisteredWithSpecLabels asserts every row of the
// spec's table (design doc lines 512-528) is registered exactly once on
// metrics.Gatherer(), with exactly that row's label set — row by row, not by
// a blanket "every series has namespace" rule. puc_controller_leader must
// carry pod ALONE (no namespace, no app: fanning one leader gauge across
// per-namespace series breaks sum(puc_controller_leader) != 1) and
// puc_watched_namespace_ready must carry namespace ALONE (no app).
func TestAllFifteenSeriesRegisteredWithSpecLabels(t *testing.T) {
	metrics.ResetForTest()
	recordOneOfEach()
	families := gather(t)

	want := wantTable()
	if len(want) != 15 {
		t.Fatalf("test table itself must have 15 rows, has %d", len(want))
	}

	for _, row := range want {
		f, ok := families[row.name]
		if !ok {
			t.Errorf("%s: not registered on metrics.Gatherer()", row.name)
			continue
		}
		if f.GetType() != row.typ {
			t.Errorf("%s: type = %v, want %v", row.name, f.GetType(), row.typ)
		}
		if len(f.GetMetric()) != 1 {
			t.Errorf("%s: expected exactly 1 sample from recordOneOfEach, got %d", row.name, len(f.GetMetric()))
			continue
		}
		got := labelNames(f)
		wantLabels := sorted(row.labels)
		if !equalStrings(got, wantLabels) {
			t.Errorf("%s: labels = %v, want %v", row.name, got, wantLabels)
		}
	}

	// No extra puc_* series beyond the fifteen named above.
	for name := range families {
		found := false
		for _, row := range want {
			if row.name == name {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("unexpected registered series %s not present in the spec's table", name)
		}
	}
}

// TestRejectionReasonSetsAreDisjoint is the test the brief calls out
// explicitly: a bare "no panic" test passes even if someone reuses, say,
// "duplicate" as both an identity reason and a request-rejection reason. This
// test fails in that case because it walks the actual closed sets from
// internal/identity and api/v1alpha1 and checks for any overlap.
func TestRejectionReasonSetsAreDisjoint(t *testing.T) {
	identityReasons := []string{
		string(identity.ReasonMissing),
		string(identity.ReasonEmpty),
		string(identity.ReasonTooLong),
		string(identity.ReasonDuplicate),
		string(identity.ReasonInvalid),
	}
	requestReasons := []string{
		v1alpha1.RejectHoldExpired,
		v1alpha1.RejectBackoff,
		v1alpha1.RejectRWOPConflict,
		v1alpha1.RejectWorkspaceLimit,
		v1alpha1.RejectTerminating,
	}

	if len(identityReasons) != 5 || len(requestReasons) != 5 {
		t.Fatalf("closed sets drifted: identity=%d request=%d, want 5 and 5", len(identityReasons), len(requestReasons))
	}

	seen := make(map[string]string, 10)
	for _, r := range identityReasons {
		seen[r] = "identity"
	}
	for _, r := range requestReasons {
		if owner, ok := seen[r]; ok {
			t.Errorf("reason %q appears in both the identity set (%s) and the request-rejection set", r, owner)
		}
	}
}

// TestRejectionCountersDoNotCrossContaminate records every identity reason
// against puc_router_identity_rejected_total and every request reason
// against puc_router_request_rejected_total, then asserts each series has
// +1 for its own reason and that the OTHER counter never picked up a sample
// under a foreign reason label. This is the "+1 for the expected reason and
// +0 for the other four" shape called out in the brief: it fails if the two
// recorders were accidentally wired to share one CounterVec, or if a reason
// string leaked across the two closed sets.
func TestRejectionCountersDoNotCrossContaminate(t *testing.T) {
	metrics.ResetForTest()

	identityReasons := []identity.Reason{
		identity.ReasonMissing,
		identity.ReasonEmpty,
		identity.ReasonTooLong,
		identity.ReasonDuplicate,
		identity.ReasonInvalid,
	}
	for _, r := range identityReasons {
		metrics.RecordIdentityRejection("ns1", "app1", r)
	}

	requestReasons := []string{
		v1alpha1.RejectHoldExpired,
		v1alpha1.RejectBackoff,
		v1alpha1.RejectRWOPConflict,
		v1alpha1.RejectWorkspaceLimit,
		v1alpha1.RejectTerminating,
	}
	for _, r := range requestReasons {
		metrics.RecordRequestRejection("ns1", "app1", r)
	}

	families := gather(t)

	identityFam, ok := families["puc_router_identity_rejected_total"]
	if !ok {
		t.Fatal("puc_router_identity_rejected_total not registered")
	}
	requestFam, ok := families["puc_router_request_rejected_total"]
	if !ok {
		t.Fatal("puc_router_request_rejected_total not registered")
	}

	gotIdentityReasons := reasonValues(identityFam)
	gotRequestReasons := reasonValues(requestFam)

	wantIdentity := []string{"missing", "empty", "too_long", "duplicate", "invalid"}
	wantRequest := []string{"hold_expired", "backoff", "rwop_conflict", "workspace_limit", "terminating"}

	if !equalStrings(sorted(gotIdentityReasons), sorted(wantIdentity)) {
		t.Errorf("puc_router_identity_rejected_total reasons = %v, want %v", sorted(gotIdentityReasons), sorted(wantIdentity))
	}
	if !equalStrings(sorted(gotRequestReasons), sorted(wantRequest)) {
		t.Errorf("puc_router_request_rejected_total reasons = %v, want %v", sorted(gotRequestReasons), sorted(wantRequest))
	}

	for _, m := range identityFam.GetMetric() {
		if m.GetCounter().GetValue() != 1 {
			t.Errorf("puc_router_identity_rejected_total sample %v = %v, want 1", m.GetLabel(), m.GetCounter().GetValue())
		}
	}
	for _, m := range requestFam.GetMetric() {
		if m.GetCounter().GetValue() != 1 {
			t.Errorf("puc_router_request_rejected_total sample %v = %v, want 1", m.GetLabel(), m.GetCounter().GetValue())
		}
	}
}

func reasonValues(f *dto.MetricFamily) []string {
	var out []string
	for _, m := range f.GetMetric() {
		for _, lp := range m.GetLabel() {
			if lp.GetName() == "reason" {
				out = append(out, lp.GetValue())
			}
		}
	}
	return out
}

// pucMetricNameRe matches a Prometheus metric/aggregation identifier that
// starts with puc_, as it appears free-standing in a PromQL expression
// (e.g. inside sum(...), on the left of a binary op, or as a bare selector).
var pucMetricNameRe = regexp.MustCompile(`\bpuc_[a-zA-Z0-9_]*\b`)

// TestAlertExpressionsReferenceRegisteredSeries pins the five alert
// expressions from the design doc (lines 538-541) as data and asserts every
// puc_* series name mentioned in each one is actually registered here. An
// alert naming a series nothing registers evaluates on no data and silently
// never fires; this is the failure Task 6 calls out for the workspace-loop
// series and Task 13 assertion 4 catches five weeks too late otherwise.
//
// The Failed-ratio expression is intentionally the corrected form, not the
// spec's literal text: spec line 541 writes
// `puc_workspaces{phase="Failed"} / sum(puc_workspaces)`, an unlabelled
// scalar-shaped right side joined by PromQL's default full-label-set
// matching against a namespace+app+phase left side, which can never match
// and so can never fire under any condition. Summing both sides fixes it.
func TestAlertExpressionsReferenceRegisteredSeries(t *testing.T) {
	metrics.ResetForTest()
	recordOneOfEach()
	families := gather(t)

	exprs := []string{
		`sum(puc_controller_leader) != 1`,
		`time() - puc_reaper_last_completion_timestamp_seconds > 900`,
		`puc_watched_namespace_ready == 0`,
		`sum(rate(puc_router_identity_rejected_total[5m])) by (namespace, app) > 0`,
		`sum(puc_workspaces{phase="Failed"}) / sum(puc_workspaces) > 0.25`,
	}

	for _, expr := range exprs {
		names := pucMetricNameRe.FindAllString(expr, -1)
		if len(names) == 0 {
			t.Errorf("expression %q named no puc_* series; expected at least one", expr)
		}
		for _, name := range names {
			if _, ok := families[name]; !ok {
				t.Errorf("alert expression %q references %s, which is not registered on metrics.Gatherer()", expr, name)
			}
		}
	}
}

// TestResetForTestClearsAllSeries confirms ResetForTest actually zeroes
// every series rather than only some, so tests in Task 6/8/9/10/11 that call
// it in setup start from a clean registry every time.
func TestResetForTestClearsAllSeries(t *testing.T) {
	metrics.ResetForTest()
	recordOneOfEach()
	if before := gather(t); len(before) == 0 {
		t.Fatal("recordOneOfEach produced no series to reset")
	}

	metrics.ResetForTest()
	families := gather(t)
	for name, f := range families {
		if len(f.GetMetric()) != 0 {
			t.Errorf("%s: ResetForTest left %d samples, want 0", name, len(f.GetMetric()))
		}
	}
}

// TestGathererIsControllerRuntimeRegistry asserts metrics.Gatherer() is
// wired to sigs.k8s.io/controller-runtime/pkg/metrics.Registry itself, not a
// private prometheus.NewRegistry() — the router's promhttp handler and the
// controller manager's /metrics endpoint must observe the same series this
// package's recorders write to.
func TestGathererIsControllerRuntimeRegistry(t *testing.T) {
	if metrics.Gatherer() != ctrlmetrics.Registry {
		t.Fatal("metrics.Gatherer() is not sigs.k8s.io/controller-runtime/pkg/metrics.Registry")
	}
}
