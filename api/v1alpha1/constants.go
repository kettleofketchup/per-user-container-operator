package v1alpha1

// Phase is the lifecycle phase of a Workspace.
type Phase string

// Workspace lifecycle phases.
const (
	PhasePending  Phase = "Pending"
	PhaseStarting Phase = "Starting"
	PhaseReady    Phase = "Ready"
	PhaseIdle     Phase = "Idle"
	PhaseFailed   Phase = "Failed"
)

// Labels and annotations.
const (
	LabelApp       = "puc.kettleofketchup/app"
	LabelUserKey   = "puc.kettleofketchup/user-key"
	LabelComponent = "puc.kettleofketchup/component"
	AnnUserDisplay = "puc.kettleofketchup/user-display"

	ComponentRouter    = "router"
	ComponentWorkspace = "workspace"
	// ComponentController identifies the controller's own metrics Service and
	// its ServiceMonitor (Task 12). Without a distinct value the controller
	// ServiceMonitor either selects on LabelPartOf alone — matching every
	// router Service in every watched namespace, double-scraping the router
	// and giving up{...controller...} a meaning Task 13 assertion 4 does not
	// expect — or the chart invents a string with no shared constant, and a
	// Service/selector mismatch selects nothing with no error and no series.
	ComponentController = "controller"

	// LabelPartOf and PartOfValue are the generic label pair Task 12's
	// ServiceMonitor selects on. The VALUE matters as much as the key: Task
	// 5's RouterPodLabels, Task 11's stamping and Task 12's selector are
	// three separate dispatches, and a mismatch makes the ServiceMonitor
	// select nothing — no error, just no series.
	LabelPartOf = "app.kubernetes.io/part-of"
	PartOfValue = "per-user-container-operator"

	// OperatorServiceAccountName is deliberately never used as a workspace
	// SA: it can create pods and read PVCs across served namespaces, which
	// inside a workspace is `kubectl create pod` with claimName:
	// workspace-app-u-<bob>.
	OperatorServiceAccountName = "per-user-container-operator"

	// PVCVolumeName is the pod-spec volume name the per-user PVC is rendered
	// under, and a RESERVED name in spec.workspace.volumes (ValidateApp
	// rejects a user volume that reuses it — Task 5 Step 3 item 6). Prefixed
	// deliberately: BOTH consumer charts declare a volume literally named
	// `workspace` (an emptyDir scratch dir, or a shared claim),
	// and Tasks 14 and 17 transcribe those declarations into
	// spec.workspace.volumes from a golden they are forbidden to hand-edit.
	// An unprefixed name would render two volumes called `workspace` and the
	// API server rejects the Deployment with
	// `spec.template.spec.volumes[1].name: Duplicate value` at Task 14 Step
	// 4 on kind, with nothing in the error naming the operator's own volume
	// as the other half.
	PVCVolumeName = "puc-workspace"

	// RouterRoleName is the per-namespace router Role the chart renders
	// (Task 12) and the controller binds <app>-router to (Task 11). Every
	// other cross-task name is either here or formulaically derived
	// (<app>-router, <app>-workspace); this one is neither, and two
	// dispatches picking different names produce a RoleBinding with a
	// dangling roleRef — every Workspace create 403s and the operator's
	// primary path is dead at first request, invisibly to both tasks' own
	// test suites.
	RouterRoleName = "per-user-container-operator-router"
)

// Ports. Same argument as RouterRoleName: neither number is derivable and
// each is needed by three separately-dispatched tasks. Task 5's
// RenderRouterNetworkPolicy admits ingress on RouterPort and Prometheus on
// MetricsPort; Task 10 binds its listener on RouterPort and its metrics
// handler on MetricsPort; Task 11 renders the router Deployment's
// containerPort and the Service's targetPort from RouterPort. Three
// dispatches each inventing a number produces a policy admitting port X, a
// Service targeting port Y and a listener on port Z — and none of the three
// tasks' own suites can see it, because Task 5 asserts on selectors, Task 11
// on the flag list and Task 10 on httptest. It surfaces at Task 13 as every
// request timing out with the NetworkPolicy blamed, or a Service with no
// reachable endpoint.
const (
	RouterPort  int32 = 8080
	MetricsPort int32 = 9090
)

// Condition types. PerUserApp: ConfigValid, WorkspaceLimitReached,
// MigrationComplete. Workspace: StorageSpecDrift, StorageShrinkRejected,
// AmbiguousVolume.
const (
	CondConfigValid           = "ConfigValid"
	CondWorkspaceLimitReached = "WorkspaceLimitReached"
	CondMigrationComplete     = "MigrationComplete"
	CondStorageSpecDrift      = "StorageSpecDrift"
	CondStorageShrinkRejected = "StorageShrinkRejected"
	CondAmbiguousVolume       = "AmbiguousVolume"

	ReasonConfigInvalid = "ConfigInvalid"
)

// puc_workspace_start_failures_total{reason} — closed set.
const (
	StartFailureOrphaned = "start_orphaned"
	StartFailureTimeout  = "startup_timeout"
	StartFailureCrash    = "crashloop"
)

// ReapReasonIdle is the sole puc_workspace_reaped_total{reason} value — closed set.
const ReapReasonIdle = "idle"

// puc_router_request_rejected_total{reason} — closed set, disjoint from the
// identity reasons in internal/identity. The same string goes in the 503
// body so a screenshot maps onto a series.
const (
	RejectHoldExpired    = "hold_expired"
	RejectBackoff        = "backoff"
	RejectRWOPConflict   = "rwop_conflict"
	RejectWorkspaceLimit = "workspace_limit"
	RejectTerminating    = "terminating"
)

// Workspace.status.waitingReason values copied from the pod.
const (
	WaitingImagePullBackOff  = "ImagePullBackOff"
	WaitingErrImageNeverPull = "ErrImageNeverPull"
	WaitingRWOPConflict      = "RWOPConflict"
)
