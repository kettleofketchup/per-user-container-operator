# Measured values for the consumer PerUserApp CRs

The durable record of Task 15. Every number a consumer CR freezes is
recorded here with the app it was measured on and the method used, because
these are per-`PerUserApp` fields consumed by two different CRs and several
of them can never be lowered once set.

Two consumers are covered:

- **Consumer A — `workspace-app`**, `examples/workspace-app.yaml`. No pre-existing user
  data.
- **Consumer B — the second consumer application** (identified in the design
  spec; referred to generically here). Has existing per-user home
  directories that a migration must preserve.

Status of each step is stated explicitly. An entry marked PENDING is not a
default to copy — it is a measurement nobody has taken yet.

---

## Step 1 — Open WebUI's tool-server client timeout — ANSWERED

**Bounds `router.coldStartHoldSeconds`** (consumer B).

Measured against Open WebUI **v0.11.1** upstream source (method: reading the
tool-server client and its timeout configuration, not a live probe — no live
Open WebUI was reachable; see Step 4's blocker).

There are two distinct timeouts on the tool-server path, and they do not
share a default:

| Phase | Setting | Default |
|---|---|---|
| Fetching the tool server's OpenAPI spec | `AIOHTTP_CLIENT_TIMEOUT_TOOL_SERVER_DATA` | **10 s** |
| Invoking a tool on that server | `AIOHTTP_CLIENT_TIMEOUT_TOOL_SERVER` | falls back to `AIOHTTP_CLIENT_TIMEOUT` |
| — its fallback | `AIOHTTP_CLIENT_TIMEOUT` | **unset → `None` → unbounded** |

So on an Open WebUI deployment that has not configured these, the spec fetch
gives up after **10 s** and the tool invocation itself has **no client-side
deadline at all**. Two unrelated call sites in the same codebase hardcode
`total=5` and `total=3`; neither is on the tool-server path.

**Consequence for `coldStartHoldSeconds`:** the binding constraint is the
10 s spec fetch, not the invocation. A router that holds a request open
longer than 10 s during a cold start will still fail Open WebUI's *discovery*
of the tool server even though the subsequent invocation would have waited
indefinitely. Any value above ~10 s buys nothing on discovery and should be
justified by the invocation path alone.

**Caveat — pin the version before relying on this.** v0.11.1's changelog
includes a fix to session-credential attribution on this exact path
(upstream PR #28630); neighbouring releases differ. Re-check against the
version actually deployed on the edge cluster.

`examples/workspace-app.yaml` currently sets `router.coldStartHoldSeconds: 60`.
That is consumer A's value and is not governed by this measurement.

---

## Step 2 — does Open WebUI's tool-server client forward inbound request headers? — ANSWERED

**Recorded finding, not a CR field.** No field depends on the answer; the
operator already implements both merge shapes and rejects a duplicated
identity header.

**Answer: (a) — only Open WebUI's own headers are sent.** Inbound browser
request headers are **not** merged into the outbound tool-server call.

Evidence, all in Open WebUI v0.11.1:

1. The function that assembles tool-server headers builds its dictionary
   **from scratch** (`headers = {}`) and never reads the inbound
   `request.headers`. There is no merge, update, or copy of the caller's
   headers anywhere on that path.
2. The user-info forwarding feature (`ENABLE_FORWARD_USER_INFO_HEADERS`)
   defaults to **False**, and when enabled it sources its values from the
   **server-side user object**, not from the request.
3. The single hardcoded `X-User-Id` on this path is populated from
   `user.id` — the authenticated session's user — not from any inbound
   header.

**Consequence:** a browser-supplied `X-User-Id` cannot reach the tool
server through Open WebUI. The operator's `duplicate` rejection therefore
guards a case that this particular caller does not produce.

It is still correct to keep it. The rejection is defence against any *other*
caller placed in front of the router — a reverse proxy, a future Open WebUI
release that changes this behaviour, or a direct client — and the cost of
keeping it is one comparison. But it should not be described as closing a
live hole in the Open WebUI path, because it does not.

**Branch this selects for the duplicated-header isolation assertion:** the
duplicated-header variant must be driven by a **direct client**, not through
Open WebUI, since Open WebUI cannot be made to emit two identity headers.

Same version caveat as Step 1.

---

## Step 3 — consumer B image identity and legacy home sizes — PARTIAL

Read off the consumer B image:

- Account: `uid=1000(user)`, and exactly **one** home directory,
  `/home/user`, at **20K**.

**Still needed:** the transformation between `X-User-Id` and the account
name, and — if that transformation is not invertible — a forward mapping
from Open WebUI's own user table to directory names, halting on any
collision. Not yet done.

**Related production finding (already surfaced separately):** consumer B's
production Deployment mounts its `workspace` volume as `emptyDir: {}` at
`/home/user` with no PVC. That data does not survive a pod restart today,
which is both a standing data-loss condition and the reason the 20K figure
above is small — there is effectively nothing accumulated to migrate.

---

## Step 4 — cold-start distribution — PENDING, BLOCKED

**Blocked on production cluster access.** This step requires deploying the
operator to the production edge cluster and creating ten PVCs on production
Ceph. The dev, auto and airgapped edge environments are all unreachable, and
this measurement cannot be taken on kind — a kind cluster's storage is not
the storage whose behaviour is being measured.

Consequently these remain **unmeasured guesses** and are marked `# TASK 15`
in `examples/workspace-app.yaml`:

| Field | App | Current value | Status |
|---|---|---|---|
| `limits.maxConcurrentStarts` | A | `5` | guess |
| `storage.size` | A | `1Gi` | test value — **can never be lowered once set** |
| `limits.maxWorkspaces` | A | `50` | test value |
| `storage.size` | B | — | not set; must come from Step 3's per-user sizes |
| `limits.maxWorkspaces` | B | — | not set |

When the step runs, it must record: p50 and p95 of the ten cold starts, the
derivation from p95 to `maxConcurrentStarts`, and the shared OSD's
`ceph df` MAX AVAIL headroom that `storage.size × maxWorkspaces` is fixed
against — **per app**. Consumer A's p95 is not consumer B's: the two differ
in image size, RBD working set and boot cost.

Two invocation constraints for whoever runs it:

- Run **`-run TestWorkspaceAppColdStart`** only. A bare package run would also
  execute the isolation suite's destructive assertions — deleting and
  recreating `PerUserApp`s, deleting NetworkPolicies, releasing and
  rebinding a PV — against a live `Retain` single-node Ceph cluster.
- The test as written **does not emit p50/p95**. The distribution has to
  come from the operator's own metrics, scraped around the run.

---

## Step 5 — record and commit — IN PROGRESS

Steps 1 and 2 are recorded above and committed. Steps 3 (partly) and 4
remain open; this file is where their answers go.
