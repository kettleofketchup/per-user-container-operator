ENVTEST_K8S_VERSION ?= 1.31.0
IMG ?= per-user-container-operator:dev

# NetworkSpec.WorkspaceEgress carries a CEL rule that nests two unbounded
# arrays (workspaceEgress itself, and each rule's `to` peers). The API
# server's CEL cost estimator refuses to install the CRD at all without a
# maxItems bound on BOTH. controller-gen can only bound the outer array from
# a +kubebuilder:validation:MaxItems marker in api/v1alpha1/peruserapp_types.go
# (that field is declared here); the inner `to` field lives on the upstream
# networkingv1.NetworkPolicyEgressRule type, which controller-gen has no
# marker to reach, so `manifests` reapplies that bound by hand (hack/patch-crds.py)
# every time it regenerates the CRD. See api/v1alpha1/crd_generated_test.go
# for the CI-gated assertion that catches this if patch-crds is ever skipped.

.PHONY: build docker-build test lint manifests patch-crds envtest envtest-run e2e

build:     ; CGO_ENABLED=0 go build ./...
docker-build: ; docker build -t $(IMG) .
# -count=1 disables Go's test-result cache: test/chart invokes `helm
# template` and re-reads chart YAML on every run, which is NOT a tracked
# input to Go's cache -- a chart edit with no .go file touched can print
# "ok (cached)" and hide a real regression (e.g. an RBAC grant moved onto
# the wrong Role) behind a stale pass. Verified: primed the cache, moved a
# grant, `go test ./test/chart/...` said cached+ok, `make test` (with
# -count=1) caught it.
test:      ; go test -count=1 ./...
lint:      ; { test -z "$$(gofmt -l .)" || { gofmt -l .; exit 1; }; } && go vet ./... && golangci-lint run
manifests: ; go run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.16.4 crd object paths=./api/... output:crd:dir=config/crd && $(MAKE) patch-crds
patch-crds: ; python3 hack/patch-crds.py
envtest:   ; @go run sigs.k8s.io/controller-runtime/tools/setup-envtest@latest use $(ENVTEST_K8S_VERSION) --bin-dir bin -p path
# envtest itself spawns a real kube-apiserver/etcd subprocess and real
# listeners, which defeats Go's test cache regardless of -count -- this
# target's -count=1 is defence in depth, not a fix for an observed hazard
# here (see the `test` target's comment for where the hazard IS real).
# KUBEBUILDER_ASSETS is resolved to an ABSOLUTE path: `make envtest`'s
# underlying tool prints one relative to the current directory, and a
# consumer that captures it and changes directory (or a CI step boundary
# that doesn't preserve $PWD assumptions) before `go test` runs would get a
# non-existent relative path instead of a clear error.
envtest-run: ; assets=$$(make --no-print-directory envtest) && KUBEBUILDER_ASSETS=$$(cd $$assets && pwd) go test -tags envtest -count=1 ./test/envtest/...
e2e:       ; ./test/e2e/kind-up.sh && go test -tags e2e ./test/e2e/... -timeout 45m
