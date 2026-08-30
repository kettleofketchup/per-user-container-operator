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
test:      ; go test ./...
lint:      ; { test -z "$$(gofmt -l .)" || { gofmt -l .; exit 1; }; } && go vet ./... && golangci-lint run
manifests: ; go run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.16.4 crd object paths=./api/... output:crd:dir=config/crd && $(MAKE) patch-crds
patch-crds: ; python3 hack/patch-crds.py
envtest:   ; @go run sigs.k8s.io/controller-runtime/tools/setup-envtest@latest use $(ENVTEST_K8S_VERSION) --bin-dir bin -p path
envtest-run: ; KUBEBUILDER_ASSETS=$$(make envtest) go test -tags envtest ./test/envtest/...
e2e:       ; ./test/e2e/kind-up.sh && go test -tags e2e ./test/e2e/... -timeout 45m
