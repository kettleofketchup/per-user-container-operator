# Measured values for the consumer PerUserApp CRs

The durable record of Task 15. Every number a consumer CR freezes is
recorded here with the app it was measured on and the method used, because
these are per-`PerUserApp` fields and several of them can never be lowered
once set.

The consumer covered here is `workspace-app`, the example CR at
`examples/workspace-app.yaml`. Two of its facets are measured separately,
because the shape this operator renders and the shape running today diverge:

- **the example CR** — what the operator renders. No pre-existing user data.
- **the deployed workload** — what runs today. Has existing per-user home
  directories that a migration must preserve.

Status of each step is stated explicitly. An entry marked PENDING is not a
default to copy — it is a measurement nobody has taken yet.

---

## Step 1 — Open WebUI's tool-server client timeout — ANSWERED

**Bounds `router.coldStartHoldSeconds`** (the deployed workload).

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

**Version caveat — RESOLVED.** The concern was that v0.11.1's changelog
touches this exact path (upstream PR #28630), so neighbouring releases
differ. The production instance runs
`ghcr.io/open-webui/open-webui:0.11.1` — read off the live Deployment —
which is the same version these answers were read against. They apply
exactly; nothing here is extrapolated across releases.

`examples/workspace-app.yaml` sets `router.coldStartHoldSeconds: 60`, and
since that CR is a transcription of this very workload, this measurement
governs the value directly. 60 is chosen for the invocation path alone: it
is deliberately above the 10 s discovery deadline, because a cold start
during discovery fails it either way and the client retries, while the first
real tool *call* has no deadline and can wait the start out. It is not an
attempt to beat the 10 s fetch.

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

Same version basis as Step 1, and equally exact: production runs the very
version this was read against.

---

## Step 3 — deployed image identity, id transformation and legacy sizes — ANSWERED

All of this is read off the **live production deployment** and its database,
read-only. No production state was modified.

### Container identity

`id` inside the running container: **`uid=1000(user) gid=1000(user)
groups=1000(user)`**. So the CR takes `runAsUser: 1000`,
`runAsGroup: 1000`, `fsGroup: 1000`. These are numeric, which is what the
operator's validation requires — the chart refers to the account by name
only, so the number is written down nowhere else.

### The identity → account-name transformation — **NOT INVERTIBLE**

Read from the image's own `…/utils/user_isolation.py`
(`sanitize_username`). The rule, exactly:

1. lowercase the identity, then delete every character outside `[a-z0-9]`
   (for a UUID this removes the four dashes, leaving 32 hex characters);
2. if at least 4 characters remain, take the **first 8**; otherwise fall
   back to the first 8 hex digits of `sha256(identity)`;
3. prepend the consumer's optional user-prefix env var (**unset in
   production** — the container's only env var is its API key);
4. prepend `u` if the result starts with a digit.

The home is then `/home/<that name>`.

**Step 3's stop-and-record condition is met: 36 characters collapse to 8, so
the mapping cannot be inverted.** Per the plan, the source of truth is
therefore the application's own user table, mapped **forward**.

### The forward map, computed over the real user table

Production Open WebUI has **8 users**. Every identity is a canonical
36-character UUID (`^[0-9a-f-]+$`, all 36 chars, verified by a length and
charset query over the whole table).

Applying the rule above to all 8: **8 distinct account names, zero
collisions.** The migration can proceed for every existing user without
guessing, and its halt-on-collision path is not triggered by today's data.

One incidental: all 8 identities begin with a digit (verified twice by
independent queries), so every derived name takes the `u` prefix and is
9 characters — `u` followed by 8 hex. A migration that assumes an 8-character
name would miss every existing home.

No user identifiers were emitted from the cluster; only the aggregate counts
above.

### Legacy home sizes — **there are none, and this is the important finding**

The live container has exactly one home, `/home/user`, at **20K**, owned by
the single shared account. There are **no per-user homes to size against**,
because multi-user mode has never run in production.

The `storage.size` therefore **cannot** be derived from a per-user
`du`, as the plan's Step 3 assumed. It has to come from the budget in
Step 4 plus a growth assumption, and it must be recorded as such — it can
never be lowered once set.

`limits.maxWorkspaces` is bounded by **8** users today.

## Step 4 — cold-start distribution — HALF ANSWERED, HALF BLOCKED

### The storage budget — ANSWERED

Read from the production Ceph cluster, read-only:

| Quantity | Value |
|---|---|
| RAW capacity | 594 GiB |
| RAW used | 42.25% |
| `replicapool` MAX AVAIL | **313 GiB** |

`replicapool` is the pool behind the `ceph-block-static` StorageClass, so
**313 GiB is the entire budget** that `storage.size × maxWorkspaces` is
fixed against — for both consumers together, plus every other RBD claim on
the cluster. It is a single-node cluster; there is no second failure domain
to grow into.

Working the constraint backwards at 8 users: even a generous 20 GiB per
workspace is 160 GiB, over half the pool, before anything else on the
cluster claims a byte. `maxWorkspaces` should be set from the real user
count with headroom (say 16), not from a round number.

### The cold-start distribution — still PENDING

**Blocked, and the target is fixed: `edge-auto` only.** Production is not an
option for this measurement — that is a standing constraint, not a
preference, so the fact that the production cluster happens to answer does
not make it eligible. `edge-auto` was not responding during this round, so
the step waits on that environment.

The measurement cannot be taken on kind either: a kind cluster's storage is
not the storage whose behaviour is being measured. The budget above was read
from production read-only, which is a different matter from creating PVCs
there.

Consequently these remain **unmeasured guesses** and are marked `# TASK 15`
in `examples/workspace-app.yaml`:

| Field | Current value | Status |
|---|---|---|
| `limits.maxConcurrentStarts` | `5` | guess |
| `storage.size` | `1Gi` | test value — **can never be lowered once set**; **cannot** come from per-user sizes (Step 3: none exist) |
| `limits.maxWorkspaces` | `50` | test value — against a 313 GiB pool, `1Gi x 50` fits; a later `storage.size` raise does not. Bounded by 8 real users today |

When the step runs, it must record: p50 and p95 of the ten cold starts, the
derivation from p95 to `maxConcurrentStarts`, and the MAX AVAIL headroom
above. A p95 measured against one image does not carry to another: image
size, RBD working set and boot cost all differ.

Two invocation constraints for whoever runs it:

- Run **`-run TestWorkspaceAppColdStart`** only. A bare package run would also
  execute the isolation suite's destructive assertions — deleting and
  recreating `PerUserApp`s, deleting NetworkPolicies, releasing and
  rebinding a PV — against a live `Retain` single-node Ceph cluster.
- The test as written **does not emit p50/p95**. The distribution has to
  come from the operator's own metrics, scraped around the run.

---

## Production divergence — what actually runs today

Recorded because Task 16's migration and Task 17's cutover both assume a
source that does not exist yet.

The **deployed** workload is the single-user render: one shared
account, home on an `emptyDir` (not a PVC), multi-user mode not enabled at
all, image tag `0.12.3`, unchanged for over three months.

The **committed chart** — already merged and already inside the pinned
submodule the next release builds from — switches it to multi-user with a
50 GiB `Retain` PVC and a root init container.

Two consequences worth stating plainly:

1. **Today there is a standing data-loss exposure.** The shared home is
   20K on an `emptyDir`; a pod reschedule discards it. Small, but it is
   user data with no persistence behind it.
2. **Task 16's migration has no source data until that chart ships.** The
   per-user homes it copies from are created by the multi-user mode that
   has never run. The migration is still worth building — it is correct if
   the release happens, inert if it does not, and unrecoverable to skip if
   it does — but it cannot be validated against production until then.

---

## Step 5 — record and commit — IN PROGRESS

Steps 1, 2 and 3 are recorded above and committed, as is Step 4's storage
budget. What remains open is Step 4's cold-start distribution, which needs
authorisation to run against production. Task 14 Step 5 — substituting the
measured values into `examples/workspace-app.yaml` and deleting its `# TASK 15`
markers — waits on that one number.
