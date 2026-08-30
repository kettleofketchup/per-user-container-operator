# per-user-container-operator

A Kubernetes operator that gives each user of an HTTP application their own
container and volume.

## Install

Install the CRDs before installing the operator itself. Use server-side
apply, not client-side apply:

```bash
kubectl apply --server-side --force-conflicts -f config/crd/
```

`PerUserApp` embeds several `corev1` PodSpec types that `controller-gen`
expands inline, so the generated CRD exceeds the 262144-byte ceiling that
client-side apply imposes on its `last-applied-configuration` annotation.
Running `kubectl apply -f config/crd/` (without `--server-side`) fails with
`metadata.annotations: Too long` on the very first install, and the error
names nothing about schema size.

## Usage

The operator binary is a single entry point with subcommands:

```bash
per-user-container-operator controller   # run the controller manager
per-user-container-operator router       # run the per-request router
per-user-container-operator userkey      # derive a per-user storage key
```

## Development

```bash
make build     # compile all packages
make test      # run the unit test suite
make lint      # gofmt + go vet + golangci-lint
make manifests # regenerate CRDs and object code via controller-gen
make envtest   # download the envtest binaries for the pinned Kubernetes version
make e2e       # spin up a kind cluster and run the end-to-end suite
```

## Runbooks

### Recovering a Released PV

Never clear a `Retain` PV's `claimRef` to re-bind it to a new claim.

Every per-user workspace claim in this system matches identically: same
storage class, same `ReadWriteOncePod` access mode, same size. A `Released`
PV that has had its `claimRef` cleared becomes `Available`, and Kubernetes
will bind it to whichever user's workspace claim is created next — with no
regard for whose data is actually on the volume. Clearing `claimRef` on a
released home-directory volume means the next user to cold-start gets
handed the previous occupant's files.

To recover a specific released PV for a specific new user, do not clear
`claimRef`. Instead:

1. Scale the old user's `Deployment` to zero replicas.
2. Delete the old user's `Workspace` object and its children.
3. Patch the `Released` PV's `claimRef` to pre-bind it directly to the new
   claim (matching `namespace`, `name`, and `uid` of the intended
   `PersistentVolumeClaim`), rather than clearing it and letting the
   scheduler pick a claimant.

This guarantees the PV can only bind to the claim you intended, never to
whichever workspace claim happens to be created first.
