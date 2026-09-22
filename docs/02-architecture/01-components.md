# Component reference

> **Prerequisite:** [Kubernetes primitives](../01-concepts/09-kubernetes.md)
> **Read next:** [Data model](02-data-model.md)
> **Diagram:** [System + trust boundaries](../diagrams/system.md)

Every component: what it does, what it may touch, what it may not, how it fails,
and how it scales.

---

## The deployment shape

One binary, several personalities, selected by `--role` or by `argv[0]`. Three
Deployments in Kubernetes, one process in `make demo`.

```
agentorch serve --role=controlplane   → Deployment controlplane  (2 replicas)
agentorch serve --role=gateway        → Deployment toolgateway   (2 replicas)
agentorch serve --role=worker         → Deployment agentd        (3 replicas, HPA 2-40)
agentorch serve --role=all            → everything in one process (demo)
argv[0] == "gh"                       → the GitHub CLI personality (in a sandbox)
argv[0] == "aoconvert"                → the document converter    (in a sandbox)
AGENTORCH_SANDBOX_INIT=1              → the jail init (re-exec of /proc/self/exe)
```

**Why one binary** ([DEEP_DIVE D16](../../DEEP_DIVE.md#d16--dependency-policy)):
one image to build, scan and sign, for a system whose purpose is containment;
the sandbox helpers can be bind-mounted as a single static file with no image
and no shared libraries; and `make demo` is one process while production is
three Deployments with separate identities and blast radii.

---

## 1. Control plane — `internal/api`

**Job:** the only way runs are created, inspected, paused, resumed, approved or
cancelled.

| | |
|---|---|
| **Listens** | `:8080` |
| **Talks to** | Postgres only |
| **Holds** | no credentials; `automountServiceAccountToken: false` |
| **Identity** | `Principal{TenantID, User, Operator}` resolved from the API key |

### Responsibilities

| Function | Detail |
|---|---|
| AuthN/AuthZ | Resolve caller → principal; scope **every** query by tenant |
| Admission control | Per-tenant `MaxConcurrentRuns`; returns `429` + `retry_after_seconds` |
| Agent registration | Validates the spec, **rejects grants for tools that do not exist** |
| Run creation | Resolves the agent, **pins the digest**, writes the first event |
| Human input | `/resume`, `/approve`, `/cancel` — each an audited event |
| Live view | SSE at `/runs/{id}/stream` |
| Aggregation | `/overview`, `/quota`, `/audit` (with chain verification) |
| Static assets | Serves the built console when `--ui-dir` is set |

### Two decisions worth noting

**Unknown tools are rejected at registration, not at first call.** A typo in a
tool name should fail while a human is looking at it, not at 3am inside a run.

**Cross-tenant reads return 404, not 403.** A 403 confirms the resource exists.
See [Multi-tenancy §3.1](../01-concepts/03-multi-tenancy.md).

### Scaling and failure

Stateless, so horizontal. If it dies, **in-flight runs are unaffected** — workers
lease directly from the database and never talk to the control plane. Only the
human-facing surface is lost.

---

## 2. Agent worker (`agentd`) — `internal/runtime`

**Job:** advance runs. Completely stateless and interchangeable.

| | |
|---|---|
| **Talks to** | Postgres, the tool gateway, the model gateway |
| **Holds** | a run-scoped JWT (≤5 min). **No tenant credentials** |
| **Egress** | NetworkPolicy permits Postgres and the gateway. Nothing else |

### The loop

```go
for {
    eligible := limiter.Eligible(typicalTokens)     // who has quota?
    if len(eligible) == 0 { sleep; continue }       // backpressure = not scheduling
    run, err := store.AcquireLease(ctx, workerID, ttl, eligible)
    if errors.Is(err, types.ErrNotFound) { sleep; continue }
    executeLease(ctx, run)                          // up to MaxStepsPerLease steps
}
```

### One step

1. **Replay** — `ListEvents(run, 0)` → `Rebuild(events)`. Nothing from the
   previous step is trusted.
2. **Budget** — stop before paying for another model call.
3. **Model** — through the gateway, which reserves quota first.
4. **Tools** — each with a deterministic idempotency key `run:step:index`.
5. **Commit** — fenced by the lease and by `next_seq`.

### Lifecycle guarantees

| Situation | Behaviour |
|---|---|
| Heartbeat fails | Cancel own context **immediately** — stop making side effects we cannot journal |
| Step budget spent | `YieldRun` → `QUEUED`, fenced. **Not** a bare `ReleaseLease` (that stranded runs) |
| SIGTERM | Same yield path; `terminationGracePeriodSeconds: 30` |
| SIGKILL | Nothing runs; the lease lapses; reaper requeues within one TTL |

### Scaling

`replicas × --workers` goroutines. Default `3 × 8 = 24`. Measured: **55.8×
throughput going from 2 to 24 workers** — the work is I/O-bound, so this is a
pure throughput dial and is unrelated to the number of agents.

---

## 3. Tool gateway — `internal/gateway`

**Job:** the single choke point. Every tool call in the system passes through
`process()`.

| | |
|---|---|
| **Listens** | `:8081` |
| **Talks to** | Postgres, third-party APIs, the Kubernetes API (to create sandboxes) |
| **Holds** | **tenant credentials** — the only component that does |
| **Identity** | `agentorch-toolgateway`, the only SA with an API token |

> This is the **TCB**: the trusted computing base. If it is wrong, nothing else
> matters. It is deliberately small, separately deployed, and separately
> reviewable.

### The sequence

```
verify JWT (sig, exp, audience=tool-gateway)
  → reload run + PINNED definition digest from the store
  → cross-check token tenant == run tenant
  → POLICY DECIDES                       ← no credential touched yet
  → validate args against the JSON schema
  → reserve the idempotency key          ← before anything observable
  → audit(pre)                           ← before the side effect
  → mint credential (≤60s, tenant+ref scoped)
  → execute  ┬ API tool  → gateway makes the call, injects a header
             ├ exec tool → sandbox, NO network
             └ CLI tool  → BROKER sandbox, holds the credential
  → cap size, scrub the credential value
  → journal the outcome, audit(post), revoke the credential
```

Each ordering choice is explained in [Authorization §5](../01-concepts/04-authorization.md)
and [Secrets](../01-concepts/05-secrets.md).

### Failure behaviour

| Failure | Response |
|---|---|
| Cannot journal the key | **Refuse to execute.** `503`. Never do something we cannot record |
| Audit write fails | Log loudly, increment `agentorch_audit_write_failures_total`, **do not fail the call that already happened** — hiding a completed side effect is worse than a gap we shouted about |
| Invocation fails | `FAILED` + a message that says the outcome is unknown if the tool is `UnsafeRetry` |
| Key already `IN_FLIGHT` | `409` — the caller waits rather than duplicating |

### Scaling

Stateless and horizontal; on the hot path for every tool call, so it has a PDB.
At 10× it shards by tenant.

---

## 4. Model gateway — `internal/llm/gateway.go`

**Job:** the single choke point for the LLM. Everything expensive or
externally-constrained lives here.

```go
est := EstimateTokens(req)
res, ok := limiter.Reserve(tenantID, priority, est)
if !ok { return ErrQuotaUnavailable }          // caller requeues; never blocks
defer settle(est)                               // exactly once, every path
// … bounded retries with full jitter …
settle(resp.InputTokens + resp.OutputTokens)
```

### Retry policy

**Three attempts, never unbounded.** An unbounded retry loop against a
struggling provider is how a partial outage becomes total — every client
hammering hardest exactly when the provider copes least.

Backoff uses **full jitter** (`rand(0, base·2ⁿ)`), because synchronised retries
from hundreds of workers are themselves an outage. See
[AWS, *Exponential Backoff And Jitter*](https://aws.amazon.com/blogs/architecture/exponential-backoff-and-jitter/).

A **permanent** error settles `0` (refund; the call never happened); exhausted
retries also settle `0`. A run that cannot proceed is **requeued, not failed** —
durability means waiting is free.

---

## 5. Fairness limiter — `internal/fairness`

**Job:** share the provider quota. In-process, called by the model gateway and
by every worker's dispatch loop.

| Method | Used by | Purpose |
|---|---|---|
| `SetTenants` | background refresher (5 s) | Recompute all guaranteed rates |
| `Eligible` | worker dispatch | Which tenants may be scheduled **right now** |
| `Reserve` / `Settle` | model gateway | Reserve-then-reconcile |
| `Snapshot` | API `/v1/quota` | Operator view |

Full treatment: [Fairness](../01-concepts/06-fairness.md).

> **⚠ Known limitation:** the limiter is **per-process**. With N control-plane
> replicas the effective quota is N× the configured value. Production needs a
> shared counter (Redis or a sharded service). The algorithm does not change —
> only where the buckets live.

---

## 6. Sandbox executor — `internal/sandbox`

**Job:** run untrusted code. Three drivers behind one interface.

```go
type Driver interface {
    Name() string
    Run(ctx context.Context, spec Spec) (Result, error)
    Close() error
}
```

| Driver | Boundary | Cold start | Use |
|---|---|---|---|
| `namespace` | shared kernel + namespaces | **8.4 ms** | local demo, CI, the safety tests |
| `docker` | shared kernel (or `--runtime=runsc`) | ~150–400 ms | macOS/Windows |
| `kubernetes` | **gVisor** + NetworkPolicy + tainted nodes | ~1–3 s | production |

`Result.Driver` records which one ran, so nothing in the system can claim a
stronger boundary than it got. Parity table:
[Linux isolation §12](../01-concepts/01-linux-isolation.md#12-driver-parity-table).

### Two sandbox profiles

| | Agent sandbox | Broker sandbox |
|---|---|---|
| Runs | model-authored code | a trusted binary from our image |
| Network | **`NetworkNone`** — empty netns | `NetworkProxy` |
| Credential | **none** | `GH_TOKEN` in its environment |
| Workspace | `/work` | the **same** `/work` |

---

## 7. Credential broker — `internal/creds`

**Job:** mint short-lived, tenant-and-reference-scoped credentials; make leaking
one a type error.

```go
type Broker interface {
    Mint(ctx, tenantID, ref string, ttl time.Duration) (Credential, error)
    Revoke(ctx, credID string) error
    Refs(ctx, tenantID string) []string
}
```

TTL is capped **at the broker** (10 min hard, 60 s in practice), not trusted from
the caller. Per-tenant HMAC roots mean a token minted for `acme` fails
verification as `globex`.

`DerivedBroker` is a stand-in; production wants Vault-style dynamic secrets. See
[Secrets §6](../01-concepts/05-secrets.md).

---

## 8. Store — `internal/store`

**Job:** the durability boundary. Two implementations, **one conformance suite**.

```go
type Store interface {
    // tenants, definitions, runs …
    AcquireLease(ctx, worker string, ttl time.Duration, eligibleTenants []string) (Run, error)
    RenewLease(ctx, runID, worker string, ttl time.Duration) error
    YieldRun(ctx, runID, worker, reason string) error
    ReapExpiredLeases(ctx) (int, error)
    Commit(ctx, runID, worker string, expectedNextSeq int, evs []Event, up RunUpdate) error
    BeginToolCall(ctx, rec ToolCallRecord) (existing ToolCallRecord, fresh bool, err error)
    AppendAudit(ctx, rec AuditRecord) (AuditRecord, error)
    // …
}
```

**The conformance suite is the point.** Every test runs against both Postgres and
the in-memory store. Testing them separately would let the fake stay green while
the real one is broken — which is exactly what happened twice:

| Bug | Symptom |
|---|---|
| `pq.Array(nil)` → SQL `NULL`; `cardinality(NULL)=0` is `NULL` not `true` | Lease query matched **nothing** |
| ns vs µs timestamp precision | Audit chain **never** verified after a round trip |

---

## 9. Reaper — `internal/runtime/reaper.go`

**Job:** the failure detector, in its entirety. Two idempotent SQL statements on
a ticker.

```sql
-- 1. reclaim runs whose worker stopped renewing
UPDATE runs SET state='QUEUED', lease_owner=NULL, lease_expires_at=NULL
WHERE state='RUNNING' AND (lease_expires_at < now()
                           OR (lease_owner IS NULL AND updated_at < now() - interval '60 seconds'));

-- 2. resolve tool calls that never reported an outcome
UPDATE tool_calls SET state='FAILED', is_error=true,
  result='tool call abandoned: … This call MAY OR MAY NOT have taken effect.'
WHERE state='IN_FLIGHT' AND started_at < now() - $1;
```

No membership protocol, no leader election, no gossip. Safe to run many
concurrently. The second clause of the first statement is a belt-and-braces
guard: no correct code path produces an ownerless `RUNNING` row, but one did
once, and such a row is invisible to **both** the dispatcher and the reaper.

`stuckAfter` must comfortably exceed the longest tool timeout, or live calls get
declared abandoned.

---

## 10. Operator console — `ui/`

React 19 + TypeScript + Vite. ~890 lines, no component library.

| View | Answers |
|---|---|
| Overview | Fleet state and spend, by tenant |
| Runs | Every run; note the empty `worker` column on parked runs |
| **Run detail** | *"What is agent X doing right now"* — the event timeline, from the same log that rebuilds the model context, with budget meters |
| Quota & fairness | Per-tenant buckets, in-flight, throttled badge, shared spare pool |
| Audit log | Records with a **chain-verified banner** |

Switching the "Acting as" key **remounts the whole tree** (`key={apiKey}`) rather
than invalidating caches, so no other tenant's data can survive the switch on
screen.

> **⚠ Known gap:** SSE takes the API key as a query parameter because
> `EventSource` cannot set headers, so keys land in access logs. Production needs
> a short-lived signed stream token.

---

## 11. Fake GitHub API — `internal/fakegithub`

**Job:** be a third party that **actually verifies** the credential.

Without verification, "the platform injected a credential" would be
unfalsifiable — the demo would look identical if the token were empty. This
server rejects missing, malformed, expired, revoked and wrong-tenant tokens, and
records every presentation with a fingerprint so a human can see each call used a
*different*, freshly minted token.

---

## Dependency graph

```
        ┌──────────────┐
        │ control plane│───────────────┐
        └──────────────┘               │
                                       ▼
   ┌─────────┐   lease/commit    ┌──────────┐
   │ agentd  │──────────────────►│ Postgres │◄──────┐
   └────┬────┘                   └──────────┘       │
        │ run-scoped JWT                            │ audit + journal
        ▼                                           │
   ┌──────────────┐    mint    ┌──────────────┐     │
   │ tool gateway │───────────►│ cred broker  │     │
   └──┬────────┬──┘            └──────────────┘     │
      │        └────────────────────────────────────┘
      │ argv in / stdout out
      ▼
   ┌───────────────────┐   ┌────────────────────┐
   │  agent sandbox    │   │  broker sandbox    │
   │  no network       │   │  holds the token   │
   └─────────┬─────────┘   └─────────┬──────────┘
             └────── /work ──────────┘
```

**Note the absence:** no arrow from `agentd` to a third-party API, and none from
the agent sandbox to anywhere. Both are enforced by NetworkPolicy and by the
empty network namespace respectively.

---

**Next:** [Data model](02-data-model.md) — every table and every column, with
the reason it exists.
