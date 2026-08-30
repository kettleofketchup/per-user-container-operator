package metrics_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// This is the metrics call-site coverage check, relocated here (from where
// it would have been sited in Task 7) to Task 11 Step 1 -- the first point
// in the plan at which it can pass, since it is the last task that wires a
// recorder call. Two things would make it green by construction rather than
// by actual coverage, and both are treated as bugs in the check itself:
// counting a call inside a _test.go file, and counting a call inside
// internal/metrics itself (this package). Neither proves anything a real
// caller does, and a check that let either count would stay green forever
// no matter how many recorders or reasons ship dead.
//
// It answers two separate questions over go/ast, walking every non-test .go
// file under internal/ and cmd/ except internal/metrics itself:
//
//  1. Does every one of the fifteen collectors this package registers have
//     at least one call to a function that writes it?
//  2. Does every constant in the three closed reason sets
//     (internal/identity's five identity rejections, v1alpha1's five
//     request-rejection reasons, and v1alpha1's three StartFailure*
//     reasons) appear as an argument at some call site?
//
// The second question needs two different rules to avoid two different
// false positives, since go/ast cannot distinguish a declaration from a use
// by node shape alone:
//
//   - A QUALIFIED reference (pkg.ReasonXxx from outside the declaring
//     package, e.g. v1alpha1.RejectTerminating read from internal/router)
//     can never be the declaration itself -- the declaration is always an
//     unqualified identifier inside the declaring package -- so any
//     appearance of the selector, in any expression position (a call
//     argument, a return value, ...), counts. This is what actually catches
//     server.go's classify(): its reasons flow out through a `return
//     v1alpha1.RejectX, true` and only reach a metrics call one frame up,
//     through a local variable -- a call-argument-only rule would call that
//     dead and be wrong.
//   - A BARE identifier (the shape a reason takes when consumed inside its
//     OWN declaring package, e.g. internal/identity's reject(ReasonMissing,
//     ...)) is ambiguous with the declaration itself, so only a bare
//     identifier that is a CALL ARGUMENT counts -- the declaration's
//     ValueSpec is never a CallExpr.Args entry.
var reasonPackageAliases = map[string]bool{"v1alpha1": true, "identity": true}

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed")
	}
	// This file lives at <root>/internal/metrics/callsite_coverage_test.go.
	return filepath.Dir(filepath.Dir(filepath.Dir(file)))
}

// scanResult accumulates what callSiteScan found across the whole tree.
type scanResult struct {
	// calledFuncs maps a bare function name (e.g. "RecordStartFailure") to
	// whether some CallExpr's Fun selector ends in that name.
	calledFuncs map[string]bool
	// argIdents maps a bare identifier name (e.g. "StartFailureCrash") to
	// whether it appeared as a call argument, either bare or as the Sel of a
	// qualified selector (pkg.Name).
	argIdents map[string]bool
}

func callSiteScan(t *testing.T, root string) scanResult {
	t.Helper()
	res := scanResult{calledFuncs: map[string]bool{}, argIdents: map[string]bool{}}

	for _, sub := range []string{"internal", "cmd"} {
		dir := filepath.Join(root, sub)
		err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() {
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			// Never count a call site inside internal/metrics itself: that is
			// exactly how this check would become tautological (a "call"
			// that is really just the definition, or a stub added solely to
			// satisfy this check).
			if filepath.Dir(path) == filepath.Join(root, "internal", "metrics") {
				return nil
			}

			fset := token.NewFileSet()
			f, perr := parser.ParseFile(fset, path, nil, 0)
			if perr != nil {
				t.Fatalf("parse %s: %v", path, perr)
			}

			ast.Inspect(f, func(n ast.Node) bool {
				switch expr := n.(type) {
				case *ast.CallExpr:
					if sel, ok := expr.Fun.(*ast.SelectorExpr); ok {
						res.calledFuncs[sel.Sel.Name] = true
					} else if id, ok := expr.Fun.(*ast.Ident); ok {
						res.calledFuncs[id.Name] = true
					}
					for _, arg := range expr.Args {
						if id, ok := arg.(*ast.Ident); ok {
							res.argIdents[id.Name] = true
						}
					}
				case *ast.SelectorExpr:
					// A qualified pkg.Name reference can never be the
					// declaration itself (the declaration is always
					// unqualified, inside pkg's own source), so any
					// appearance -- not just as a call argument -- counts.
					if id, ok := expr.X.(*ast.Ident); ok && reasonPackageAliases[id.Name] {
						res.argIdents[expr.Sel.Name] = true
					}
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}
	return res
}

// recorderFuncs is every exported function in internal/metrics that writes
// one of the fifteen registered collectors -- everything except Gatherer
// and ResetForTest, which read/reset rather than write. SetWorkspaceUserInfo
// and DeleteWorkspaceUserInfo both write the same collector
// (puc_workspace_user_info); both are still required here since each is the
// sole producer of one of that series' two states (present vs. absent).
var recorderFuncs = []string{
	"SetWorkspacesByPhase",
	"SetWorkspacePVCsTotal",
	"SetWorkspaceUserInfo",
	"DeleteWorkspaceUserInfo",
	"RecordWorkspaceStart",
	"ObserveWorkspaceStartSeconds",
	"RecordReconcileError",
	"RecordStartFailure",
	"RecordWorkspaceReaped",
	"SetReaperLastCompletion",
	"RecordRouterRequest",
	"RecordIdentityRejection",
	"RecordRequestRejection",
	"SetOpenUpgradedConnections",
	"SetWatchedNamespaceReady",
	"SetLeader",
}

// identityReasons, requestRejectReasons and startFailureReasons are the
// three closed reason sets the check names verbatim.
var identityReasons = []string{"ReasonMissing", "ReasonEmpty", "ReasonTooLong", "ReasonDuplicate", "ReasonInvalid"}
var requestRejectReasons = []string{"RejectHoldExpired", "RejectBackoff", "RejectRWOPConflict", "RejectWorkspaceLimit", "RejectTerminating"}
var startFailureReasons = []string{"StartFailureOrphaned", "StartFailureTimeout", "StartFailureCrash"}

func TestEveryRecorderHasACallSite(t *testing.T) {
	res := callSiteScan(t, repoRoot(t))
	for _, fn := range recorderFuncs {
		if !res.calledFuncs[fn] {
			t.Errorf("metrics.%s has no call site under internal/ or cmd/ (excluding _test.go files and internal/metrics itself)", fn)
		}
	}
}

func TestEveryClosedReasonConstantHasACallSite(t *testing.T) {
	res := callSiteScan(t, repoRoot(t))
	check := func(setName string, names []string) {
		for _, n := range names {
			if !res.argIdents[n] {
				t.Errorf("%s.%s never appears as a call argument under internal/ or cmd/ (excluding _test.go files and internal/metrics itself)", setName, n)
			}
		}
	}
	check("identity", identityReasons)
	check("v1alpha1", requestRejectReasons)
	check("v1alpha1", startFailureReasons)
}
