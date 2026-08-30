ENVTEST_K8S_VERSION ?= 1.31.0
IMG ?= per-user-container-operator:dev

.PHONY: build docker-build test lint manifests envtest envtest-run e2e

build:     ; CGO_ENABLED=0 go build ./...
docker-build: ; docker build -t $(IMG) .
test:      ; go test ./...
lint:      ; { test -z "$$(gofmt -l .)" || { gofmt -l .; exit 1; }; } && go vet ./... && golangci-lint run
manifests: ; go run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.16.4 crd object paths=./api/... output:crd:dir=config/crd
envtest:   ; @go run sigs.k8s.io/controller-runtime/tools/setup-envtest@latest use $(ENVTEST_K8S_VERSION) --bin-dir bin -p path
envtest-run: ; KUBEBUILDER_ASSETS=$$(make envtest) go test -tags envtest ./test/envtest/...
e2e:       ; ./test/e2e/kind-up.sh && go test -tags e2e ./test/e2e/... -timeout 45m
