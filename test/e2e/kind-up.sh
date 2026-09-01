#!/usr/bin/env bash
# kind-up.sh stands up the E2E harness this plan's Task 13 assertions run
# against: a kind cluster running Calico (never kindnet -- see the CNI guard
# below), Traefik, kube-prometheus-stack, the operator's CRDs and chart, two
# namespaces' worth of credential Secrets, and two fixture PerUserApps.
#
# SKIPPING THIS SCRIPT: set PUC_E2E_SKIP_CLUSTER=1 to short-circuit it
# entirely. Tasks 15, 16 and 17 run `go test -tags e2e -run <OneTest>
# ./test/e2e/...` against the live edge cluster this way, reading the
# PUC_E2E_* environment contract (see below) from real cluster values instead
# of the ones this script would create. `make e2e` (this script followed by
# `go test -tags e2e ./test/e2e/... -timeout 45m`, unselected) must never be
# run that way: it is a package-wide run that also executes this task's
# deliberately destructive assertions -- deleting and recreating
# PerUserApps, deleting NetworkPolicies, releasing and rebinding a PV --
# which would run against a live cluster on reclaimPolicy:Retain and
# single-node Ceph.
#
# TEARDOWN: this script only brings the environment UP; nothing in `make e2e`
# tears it down afterwards, and this script does not either. To remove the
# cluster:
#
#   kind delete cluster --name puc-e2e
#
# That leaves the pinned Docker network (`puc-e2e`, 172.30.0.0/16) in place
# on purpose -- see the network section below for why it must never be
# recreated with an auto-picked subnet, and delete it explicitly only once
# no other invocation of this script is using it:
#
#   docker network rm puc-e2e
#
# ENVIRONMENT CONTRACT: this script exports the following into
# test/e2e/.generated-env (KEY=VALUE lines), because it and the `go test`
# step that follows it in the Makefile's `e2e` target are separate processes
# -- a plain `export` here does not survive into the next command. The Go
# suite's TestMain loads that file for any of these variables not already
# present in its own process environment, so a non-kind invocation that
# exports real values directly is never overridden by a stale file.
#
#   PUC_E2E_STORAGECLASS         puc-e2e-retain
#   PUC_E2E_NAMESPACES           the two namespaces this script creates
#   PUC_E2E_CALLER_TOKEN_FILE    path to the caller bearer this script wrote
#   PUC_E2E_WORKSPACE_IMAGE      the fixture workspace image built below
#   PUC_E2E_WORKSPACE_PORT       the port that fixture image listens on
#   PUC_E2E_COLD_START_IDENTITIES 1 (see task-13a-brief.md Step 0's table)
#   PUC_E2E_KUBECONFIG           kubeconfig this script wrote for the cluster
#
# PUC_E2E_MIGRATION_IMAGE is deliberately NOT set here: Task 16's
# migration/Dockerfile does not exist at this point in the plan, and a
# `docker build migration/` line added now would fail this script -- the
# first thing `make e2e` runs -- on a missing file. Task 16 adds its own
# build-and-load line and export when that image exists.
set -euo pipefail

if [[ "${PUC_E2E_SKIP_CLUSTER:-}" == "1" ]]; then
  echo "kind-up.sh: PUC_E2E_SKIP_CLUSTER=1, not touching any cluster."
  exit 0
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

CLUSTER_NAME=puc-e2e
NS1=puc-e2e-1
NS2=puc-e2e-2
TRAEFIK_NS=traefik
MONITORING_NS=monitoring
OPERATOR_NS=puc-system
STORAGECLASS=puc-e2e-retain
POD_SUBNET=10.244.0.0/16
CALICO_CHART_VERSION=v3.29.3
IMG="${IMG:-per-user-container-operator:dev}"
WORKSPACE_IMG=puc-e2e-workspace:e2e
# The port WORKSPACE_IMG listens on (nginx-unprivileged's own default).
# Must stay equal to spec.workspace.port in test/e2e/testdata/e2e-app*.yaml;
# it is exported so a test substituting WORKSPACE_IMG into a consumer CR
# written against that consumer's own port can substitute the port too.
WORKSPACE_PORT=8080
CALLER_TOKEN_VALUE=puc-e2e-caller-token-fixed
WORKSPACE_TOKEN_VALUE=puc-e2e-workspace-token-fixed
# Task 14: workspace-app's real render-config init container mounts a
# `litellm-secret` Secret with `optional: true` and blocks forever on
# `until [ -s /secret/master-key ]` until it exists AND is non-empty. This
# harness has no real LiteLLM to mirror a key from, so a fixed, non-empty
# value is created directly below -- see this script's "Credential Secrets"
# section.
LITELLM_MASTER_KEY_VALUE=puc-e2e-litellm-master-key-fixed

KUBECONFIG_PATH="${SCRIPT_DIR}/.kubeconfig"
ENV_FILE="${SCRIPT_DIR}/.generated-env"
CALLER_TOKEN_FILE="${SCRIPT_DIR}/.caller-token"

log() { echo "kind-up.sh: $*" >&2; }

require_tool() {
  local bin="$1" hint="$2"
  if ! command -v "$bin" >/dev/null 2>&1; then
    echo "kind-up.sh: required tool '$bin' not found. $hint" >&2
    exit 1
  fi
}

require_tool kind "Install it: 'go install sigs.k8s.io/kind@latest' (needs \$GOPATH/bin or \$GOBIN on PATH), or download a release binary from https://kind.sigs.k8s.io/#installation-and-usage"
require_tool docker "Install Docker: https://docs.docker.com/engine/install/"
require_tool kubectl "Install kubectl: https://kubernetes.io/docs/tasks/tools/#kubectl"
require_tool helm "Install Helm: https://helm.sh/docs/intro/install/"

# --- Docker network: PINNED, not optional -------------------------------
#
# A stock `kind create cluster` lets Docker auto-pick the network's subnet
# from its default address pool. On THIS class of machine that is not a
# theoretical risk: 172.17-172.19 and 172.21-172.24/16 are already taken by
# other Docker networks, making 172.20.0.0/16 the next free /16 -- and
# 172.20.0.0/16 is this operator's own production LAN, routed over
# a VPN tunnel (`172.20.0.0/16 via 172.16.100.1 dev tun0`). A cluster created
# without an explicit network plants a local bridge for exactly that range
# and black-holes the entire production LAN -- the PiKVM, the edge nodes,
# GitLab, the artifact store -- for as long as the cluster exists, and it
# presents as unrelated VPN flakiness, never as an e2e error.
#
# So: an explicit Docker network on 172.30.0.0/16 (free, unrouted, clear of
# the VPN's 172.16.0.0/16, the production LAN's 172.20.0.0/16, and this cluster's own
# pod subnet 10.244.0.0/16), created here and pointed at via
# KIND_EXPERIMENTAL_DOCKER_NETWORK below. Do not delete this comment or the
# explicit --subnet: removing it silently re-enables the auto-pick above.
NETWORK_NAME=puc-e2e
NETWORK_SUBNET=172.30.0.0/16

ensure_network() {
  if docker network inspect "$NETWORK_NAME" >/dev/null 2>&1; then
    local existing
    existing="$(docker network inspect "$NETWORK_NAME" --format '{{(index .IPAM.Config 0).Subnet}}')"
    if [[ "$existing" != "$NETWORK_SUBNET" ]]; then
      echo "kind-up.sh: docker network '$NETWORK_NAME' already exists with subnet $existing, expected $NETWORK_SUBNET. Refusing to reuse it -- remove it or rename it and re-run." >&2
      exit 1
    fi
    log "reusing existing docker network $NETWORK_NAME ($existing)"
  else
    log "creating docker network $NETWORK_NAME ($NETWORK_SUBNET)"
    docker network create --subnet="$NETWORK_SUBNET" "$NETWORK_NAME" >/dev/null
  fi
}

ensure_network
export KIND_EXPERIMENTAL_DOCKER_NETWORK="$NETWORK_NAME"

# --- Cluster --------------------------------------------------------------
KIND_CONFIG="$(mktemp)"
trap 'rm -f "$KIND_CONFIG"' EXIT
cat >"$KIND_CONFIG" <<EOF
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
networking:
  disableDefaultCNI: true
  podSubnet: ${POD_SUBNET}
EOF

if kind get clusters 2>/dev/null | grep -qx "$CLUSTER_NAME"; then
  log "reusing existing kind cluster $CLUSTER_NAME"
else
  log "creating kind cluster $CLUSTER_NAME on docker network $NETWORK_NAME"
  kind create cluster --name "$CLUSTER_NAME" --config "$KIND_CONFIG"
fi
kind export kubeconfig --name "$CLUSTER_NAME" --kubeconfig "$KUBECONFIG_PATH"
export KUBECONFIG="$KUBECONFIG_PATH"

KCTL=(kubectl --kubeconfig "$KUBECONFIG_PATH")
HLM=(helm --kubeconfig "$KUBECONFIG_PATH")

# nodeCIDR: kind node IPs are addresses on the pinned Docker network above,
# not an internal Kubernetes concept -- so the controller's --node-cidr
# (which the workspace ingress policy uses to admit kubelet probes, and the
# router egress policy uses to reach the node) is the SAME subnet as the
# Docker network, never a guess.
NODE_CIDR="$NETWORK_SUBNET"

# --- Calico -----------------------------------------------------------
log "installing Calico (tigera-operator $CALICO_CHART_VERSION)"
helm repo add projectcalico https://docs.tigera.io/calico/charts >/dev/null 2>&1 || true
helm repo update projectcalico >/dev/null
CALICO_VALUES="$(mktemp)"
trap 'rm -f "$KIND_CONFIG" "$CALICO_VALUES"' EXIT
cat >"$CALICO_VALUES" <<EOF
installation:
  calicoNetwork:
    ipPools:
      - cidr: ${POD_SUBNET}
        encapsulation: VXLAN
        natOutgoing: Enabled
        nodeSelector: all()
EOF
"${HLM[@]}" upgrade --install calico projectcalico/tigera-operator \
  --version "$CALICO_CHART_VERSION" \
  --namespace tigera-operator --create-namespace \
  --force-conflicts \
  -f "$CALICO_VALUES"

log "waiting for calico-node DaemonSet to exist"
for _ in $(seq 1 60); do
  if "${KCTL[@]}" -n calico-system get daemonset calico-node >/dev/null 2>&1; then
    break
  fi
  sleep 5
done
"${KCTL[@]}" -n calico-system rollout status daemonset/calico-node --timeout=300s

# --- Namespaces ---------------------------------------------------------
for ns in "$NS1" "$NS2" "$TRAEFIK_NS" "$MONITORING_NS" "$OPERATOR_NS"; do
  "${KCTL[@]}" get namespace "$ns" >/dev/null 2>&1 || "${KCTL[@]}" create namespace "$ns"
done

# --- Traefik --------------------------------------------------------------
log "installing Traefik"
helm repo add traefik https://traefik.github.io/charts >/dev/null 2>&1 || true
helm repo update traefik >/dev/null
"${HLM[@]}" upgrade --install traefik traefik/traefik --namespace "$TRAEFIK_NS"
"${KCTL[@]}" -n "$TRAEFIK_NS" rollout status deployment/traefik --timeout=300s

# --- kube-prometheus-stack ------------------------------------------------
# serviceMonitorSelectorNilUsesHelmValues/podMonitorSelectorNilUsesHelmValues
# are set false, with both selectors set to {} (match everything): the
# chart's default (nil-uses-helm-values true) auto-restricts discovery to
# ServiceMonitors carrying a `release: <helm-release>` label, which ours
# does not; and the namespace selectors must be {} (match every namespace)
# because the operator's ServiceMonitors live in whatever namespace the
# `helm install` below targets, never this stack's own namespace -- an
# unset namespaceSelector defaults a Prometheus CR to its own namespace
# only. grafana/kube-state-metrics/node-exporter are disabled: this harness
# only needs the Prometheus and Alertmanager Services (and the ServiceMonitor
# CRD), and installing the rest only spends the 45-minute e2e budget.
log "installing kube-prometheus-stack"
helm repo add prometheus-community https://prometheus-community.github.io/helm-charts >/dev/null 2>&1 || true
helm repo update prometheus-community >/dev/null
KPS_VALUES="$(mktemp)"
trap 'rm -f "$KIND_CONFIG" "$CALICO_VALUES" "$KPS_VALUES"' EXIT
cat >"$KPS_VALUES" <<'EOF'
grafana:
  enabled: false
kubeStateMetrics:
  enabled: false
nodeExporter:
  enabled: false
prometheus:
  prometheusSpec:
    serviceMonitorSelectorNilUsesHelmValues: false
    serviceMonitorSelector: {}
    serviceMonitorNamespaceSelector: {}
    podMonitorSelectorNilUsesHelmValues: false
    podMonitorSelector: {}
    podMonitorNamespaceSelector: {}
EOF
"${HLM[@]}" upgrade --install kps prometheus-community/kube-prometheus-stack \
  --namespace "$MONITORING_NS" \
  -f "$KPS_VALUES" \
  --wait --timeout 10m

# ALERTMANAGER_CLUSTERIP is not substituted into anything below (the Go
# suite resolves it live, by label selector, when it needs it for assertion
# 2's non-fixture-service probe) -- this is diagnostic only. There is no
# equivalent PROM_CLUSTERIP any more: the fixtures below select the
# Prometheus pod by namespace + pod labels (a podSelector+namespaceSelector
# workspaceEgress peer), not by its Service's ClusterIP -- NetworkPolicy
# egress evaluates against the POST-DNAT destination, so an ipBlock naming a
# ClusterIP never matches the packet policy actually evaluates and silently
# drops every connection to it (see NetworkSpec.WorkspaceEgress's doc
# comment, api/v1alpha1/peruserapp_types.go, for the full reasoning).
ALERTMANAGER_CLUSTERIP="$("${KCTL[@]}" -n "$MONITORING_NS" get svc -l app=kube-prometheus-stack-alertmanager -o jsonpath='{.items[0].spec.clusterIP}')"
if [[ -z "$ALERTMANAGER_CLUSTERIP" ]]; then
  echo "kind-up.sh: could not resolve the Alertmanager ClusterIP from kube-prometheus-stack" >&2
  exit 1
fi
log "Alertmanager ClusterIP=$ALERTMANAGER_CLUSTERIP"

# --- StorageClass ---------------------------------------------------------
# reclaimPolicy: Retain, matching production's ceph-block-static: Task 5's
# Delete-class refusal (ValidateStorageClass) rejects kind's own default
# class, so a fixture that reached for it would fail at CR admission for a
# reason no assertion mentions.
#
# volumeBindingMode: DEVIATION from task-13a-brief.md's literal
# "Immediate (matching production's Immediate binding)". rancher.io/
# local-path's dynamic provisioner structurally cannot honor Immediate: it
# creates a hostPath directory on a specific node and has no node to pick
# until a pod referencing the claim is scheduled. Observed directly on this
# cluster -- an Immediate-bound puc-e2e-retain PVC sat Pending forever, and
# local-path-provisioner's own controller logged "configuration error, no
# node was specified" on every retry -- and kind's own bundled default
# StorageClass ("standard"), using this exact same provisioner, is itself
# WaitForFirstConsumer. There is no configuration of this provisioner that
# makes Immediate work; using it here would make every workspace in this
# harness permanently unschedulable. WaitForFirstConsumer is the only value
# that lets this StorageClass's chosen provisioner function at all.
cat <<EOF | "${KCTL[@]}" apply -f -
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: ${STORAGECLASS}
provisioner: rancher.io/local-path
reclaimPolicy: Retain
volumeBindingMode: WaitForFirstConsumer
EOF

# --- Images -----------------------------------------------------------
log "building and loading operator image $IMG"
docker build -t "$IMG" "$REPO_ROOT"
kind load docker-image "$IMG" --name "$CLUSTER_NAME"

log "building and loading fixture workspace image $WORKSPACE_IMG"
docker build -f "${SCRIPT_DIR}/fixture-workspace.Dockerfile" -t "$WORKSPACE_IMG" "$SCRIPT_DIR"
kind load docker-image "$WORKSPACE_IMG" --name "$CLUSTER_NAME"

# --- Credential Secrets ----------------------------------------------------
# ValidateApp rejects an absent callerAuth, and Task 11 renders a read-only,
# optional:false Secret volume for it -- a missing Secret leaves the router
# pod in ContainerCreating forever and every assertion in this task fails
# for a reason unrelated to isolation. Written to every E2E namespace before
# the CRDs/chart/fixtures below.
echo -n "$CALLER_TOKEN_VALUE" >"$CALLER_TOKEN_FILE"
for ns in "$NS1" "$NS2"; do
  "${KCTL[@]}" -n "$ns" create secret generic puc-e2e-router \
    --from-literal=api-key="$CALLER_TOKEN_VALUE" \
    --dry-run=client -o yaml | "${KCTL[@]}" apply -f -
  "${KCTL[@]}" -n "$ns" create secret generic puc-e2e-workspace \
    --from-literal=api-key="$WORKSPACE_TOKEN_VALUE" \
    --dry-run=client -o yaml | "${KCTL[@]}" apply -f -
  # Task 14: see LITELLM_MASTER_KEY_VALUE's own comment above. The key name
  # (master-key) and Secret name (litellm-secret) match what
  # examples/workspace-app.yaml's spec.workspace.volumes names, which is itself
  # transcribed verbatim from the real workspace-app chart's Secret volume.
  "${KCTL[@]}" -n "$ns" create secret generic litellm-secret \
    --from-literal=master-key="$LITELLM_MASTER_KEY_VALUE" \
    --dry-run=client -o yaml | "${KCTL[@]}" apply -f -
done

# --- CRDs -------------------------------------------------------------
# --server-side --force-conflicts, deliberately: PerUserAppSpec.Workspace
# embeds several corev1 PodSpec types that controller-gen expands inline,
# and client-side apply would stuff the whole schema into the 262144-byte
# last-applied-configuration annotation and fail with
# `metadata.annotations: Too long` before any test runs. See README.md's
# Install section for the same flag and the same one-line reason -- this is
# also how a consumer installs the operator.
log "applying CRDs (server-side)"
"${KCTL[@]}" apply --server-side --force-conflicts -f "${REPO_ROOT}/config/crd/"

# --- Operator chart ------------------------------------------------------
IMG_REPO="${IMG%%:*}"
IMG_TAG="${IMG##*:}"
log "installing operator chart (image ${IMG_REPO}:${IMG_TAG})"
"${HLM[@]}" upgrade --install puc "${REPO_ROOT}/charts/per-user-container-operator" \
  --namespace "$OPERATOR_NS" \
  --set "watchNamespaces={${NS1},${NS2}}" \
  --set "clusterPodCIDR=${POD_SUBNET}" \
  --set "nodeCIDR=${NODE_CIDR}" \
  --set "image.repository=${IMG_REPO}" \
  --set "image.tag=${IMG_TAG}" \
  --set "image.pullPolicy=IfNotPresent"
"${KCTL[@]}" -n "$OPERATOR_NS" rollout status deployment/per-user-container-operator --timeout=300s

# --- Test-client pods (item 10a) ------------------------------------------
# The dedicated in-cluster client the router ingress policy admits (plus
# Traefik): labelled puc-e2e/role=client, which is NEVER a value
# RouterPodLabels/WorkspacePodLabels produce, so this peer cannot
# accidentally admit workspace pods and turn a real negative probe into one
# that never denied anything.
for ns in "$NS1" "$NS2"; do
  cat <<EOF | "${KCTL[@]}" apply -f -
apiVersion: v1
kind: Pod
metadata:
  name: puc-e2e-client
  namespace: ${ns}
  labels:
    puc-e2e/role: client
spec:
  containers:
    - name: client
      image: curlimages/curl:8.11.0
      command: ["sleep", "infinity"]
EOF
done
"${KCTL[@]}" -n "$NS1" wait --for=condition=Ready pod/puc-e2e-client --timeout=120s
"${KCTL[@]}" -n "$NS2" wait --for=condition=Ready pod/puc-e2e-client --timeout=120s

# A second debug pod in NS1 carrying RouterPodLabels("e2e-app-b") -- the
# labels the workspace ingress policy actually selects on -- for the cross-
# app probe a later dispatch's assertion 2 runs. It is a plain curl image,
# never the operator image: the real router pod runs the operator's
# CGO_ENABLED=0 binary with no /bin/sh and no curl, so a kubectl exec into it
# failing because the binary does not exist would be indistinguishable from
# the NetworkPolicy refusing the connection.
cat <<EOF | "${KCTL[@]}" apply -f -
apiVersion: v1
kind: Pod
metadata:
  name: puc-e2e-app-b-router-probe
  namespace: ${NS1}
  labels:
    puc.kettleofketchup/app: e2e-app-b
    puc.kettleofketchup/component: router
    app.kubernetes.io/part-of: per-user-container-operator
spec:
  containers:
    - name: client
      image: curlimages/curl:8.11.0
      command: ["sleep", "infinity"]
EOF
"${KCTL[@]}" -n "$NS1" wait --for=condition=Ready pod/puc-e2e-app-b-router-probe --timeout=120s

# --- Traefik IngressRoute + Middlewares (items 5, 10a) ---------------------
# Bare Traefik is not enough for the plan's assertion 3: a `headers`
# middleware whose `customRequestHeaders` names the identity header is the
# browser-equivalent stand-in for forward-auth's `authResponseHeaders`.
# Traefik's headers middleware applies customRequestHeaders via
# http.Header.Set (verified against traefik v3.6's
# pkg/middlewares/headers/header.go), which replaces every existing value
# under that header name -- so this single entry both deletes any
# client-supplied copy of the identity header and sets the pinned one, with
# no second delete-then-set step needed. The identity is pinned to attacker
# "A"; the victim "V" is provisioned off this path entirely (see below).
cat <<EOF | "${KCTL[@]}" apply -f -
apiVersion: traefik.io/v1alpha1
kind: Middleware
metadata:
  name: puc-e2e-identity-pin
  namespace: ${NS1}
spec:
  headers:
    customRequestHeaders:
      X-User-Id: "A"
---
apiVersion: traefik.io/v1alpha1
kind: Middleware
metadata:
  name: puc-e2e-caller-auth
  namespace: ${NS1}
spec:
  headers:
    customRequestHeaders:
      Authorization: "Bearer ${CALLER_TOKEN_VALUE}"
---
apiVersion: traefik.io/v1alpha1
kind: IngressRoute
metadata:
  name: puc-e2e-app
  namespace: ${NS1}
spec:
  entryPoints:
    - web
  routes:
    - match: Host(\`e2e-app.puc-e2e.local\`)
      kind: Rule
      services:
        - name: e2e-app-router
          port: 8080
      middlewares:
        - name: puc-e2e-identity-pin
        - name: puc-e2e-caller-auth
EOF

# --- Fixture PerUserApps (item 11) -----------------------------------------
# __WORKSPACE_IMAGE__ is substituted here because its value is not knowable
# when the fixture files were written: the workspace image is built above.
render_fixture() {
  sed -e "s|__WORKSPACE_IMAGE__|${WORKSPACE_IMG}|g" \
      "$1"
}

render_fixture "${SCRIPT_DIR}/testdata/e2e-app.yaml" | "${KCTL[@]}" -n "$NS1" apply -f -
render_fixture "${SCRIPT_DIR}/testdata/e2e-app.yaml" | "${KCTL[@]}" -n "$NS2" apply -f -
render_fixture "${SCRIPT_DIR}/testdata/e2e-app-b.yaml" | "${KCTL[@]}" -n "$NS1" apply -f -

log "waiting for router Deployments to become ready"
"${KCTL[@]}" -n "$NS1" rollout status deployment/e2e-app-router --timeout=180s
"${KCTL[@]}" -n "$NS2" rollout status deployment/e2e-app-router --timeout=180s
"${KCTL[@]}" -n "$NS1" rollout status deployment/e2e-app-b-router --timeout=180s

# --- Victim V (item 5) ------------------------------------------------------
# Provisioned off the Traefik path (the pinned middleware above would
# overwrite V's header with A), from the dedicated test-client pod, so V has
# its own workspace and a marker a later dispatch's assertion 3 reads back.
# This just cold-starts V's workspace here; writing/reading V's marker is
# that dispatch's job, not this harness's.
V_CODE="$("${KCTL[@]}" -n "$NS1" exec puc-e2e-client -- curl -sS --max-time 100 -o /dev/null -w '%{http_code}' \
  -H "Authorization: Bearer ${CALLER_TOKEN_VALUE}" \
  -H "X-User-Id: V" \
  "http://e2e-app-router.${NS1}.svc.cluster.local:8080/")"
if [[ "$V_CODE" != "200" ]]; then
  echo "kind-up.sh: provisioning victim V failed: router returned $V_CODE" >&2
  exit 1
fi

# --- Environment file for the Go suite -------------------------------------
cat >"$ENV_FILE" <<EOF
PUC_E2E_STORAGECLASS=${STORAGECLASS}
PUC_E2E_NAMESPACES=${NS1},${NS2}
PUC_E2E_CALLER_TOKEN_FILE=${CALLER_TOKEN_FILE}
PUC_E2E_WORKSPACE_IMAGE=${WORKSPACE_IMG}
PUC_E2E_WORKSPACE_PORT=${WORKSPACE_PORT}
PUC_E2E_COLD_START_IDENTITIES=1
PUC_E2E_KUBECONFIG=${KUBECONFIG_PATH}
EOF

log "harness ready. Generated env: $ENV_FILE"
