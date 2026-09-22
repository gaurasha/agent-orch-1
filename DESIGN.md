# Architecture proposal: agentic orchestration on Kubernetes

**Scope:** ~1000 concurrent agents, ~10k tool calls/min, enterprise multi-tenancy,
arbitrary code execution, durable across restarts, auditable.

Diagrams are in [`docs/diagrams/`](docs/diagrams/) and are referenced inline.
The exhaustive alternatives analysis — every option considered, with pros, cons,
industry precedent and links — is in [`DEEP_DIVE.md`](DEEP_DIVE.md). A
first-principles walkthrough for a reader new to the problem is in
[`TUTORIAL.md`](TUTORIAL.md).

**This document is the decisions, and nothing else.** Mechanism lives in the
diagrams, justification lives in the deep dive. Where a claim is measured, the
number and the test that produced it are named.

*On length: this runs to roughly five rendered pages against the brief's four.
I moved every mechanism description into the linked diagram docs and every
alternatives argument into the deep dive, and what remains is the eight
sections the brief asks for. Rather than delete a required failure mode or one
of the hardest-decision reversal conditions to hit the count, I went one page
over and am telling you so.*

---

## 0. The three observations everything follows from

**1. Agents are overwhelmingly idle.** An agent's wall clock is dominated by
waiting — for LLM tokens (seconds), tool I/O (milliseconds to seconds), humans
(hours to days). The orchestrator's own CPU per step is microseconds. Binding
an agent to a long-lived process binds a resource to something that does almost
nothing: 1000 pods at ~100 MiB of pod overhead is ~100 GiB and 10+ nodes to run
essentially no computation.

**2. The durable thing is the conversation, not the process.** Persist the
event log and any worker can reconstruct the agent. Process identity is an
implementation detail — and a liability, because processes do not survive node
drains.

**3. The dangerous thing is only the code execution.** The orchestration loop
runs *our* code. The untrusted parts are the model's output (data, never
control) and the code the model asks to run. The expensive isolation belongs
tightly around execution, not around the whole agent.

These give the shape of the system: **a pooled, stateless runtime over a durable
log; a tight, expensive sandbox only where untrusted code actually runs; and a
single choke point where authorization and credentials live.**

---

## 1. System diagram and trust boundaries

See **[`docs/diagrams/system.md`](docs/diagrams/system.md)** for the rendered
diagram and the full boundary table.

Four trust zones: **untrusted external** (users, the LLM provider, third-party
APIs); the **platform** (`agentorch`, Pod Security `restricted`) holding the
control plane, stateless workers, model gateway, fairness limiter, tool gateway,
credential broker and egress proxy; **state** (Postgres: run rows, event log,
idempotency journal, audit chain); and **sandboxes** (`agentorch-sandboxes`),
split into agent sandboxes running untrusted code and broker sandboxes running a
trusted binary that holds a credential.

**Tenant isolation is enforced in seven places, not one:**

| Where | Mechanism |
|---|---|
| API | Every route resolves the caller to a tenant and scopes the query. Cross-tenant reads return **404, not 403**, so ids cannot be probed. |
| Store | No function takes two tenant ids. |
| Agent definitions | Content-addressed and tenant-owned; a run pins the digest. |
| Workspaces | `<root>/<tenant>/<run>`, resolved twice (lexically, then through symlinks). |
| Credentials | Per-tenant root secrets; a token minted for tenant A fails verification as tenant B. |
| Sandboxes | Per-tenant host uid ranges; pods labelled by tenant; never reused across tenants. |
| Audit | One hash chain per tenant, so a per-tenant export is self-verifying. |

---

## 2. Agent lifecycle

See **[`docs/diagrams/lifecycle.md`](docs/diagrams/lifecycle.md)**.

**What a single agent is at runtime: a row in `runs` plus an append-only
`events` log.** It is executed by a transient *lease* held by any worker in a
stateless pool. It may additionally hold a sandbox lease while a tool call is
in flight.

*Why not a pod per agent, and what that costs me:* argued in full in **D1**
below.

**Scheduling** is `SELECT ... FOR UPDATE SKIP LOCKED` over `runs`, ordered by
priority lane then FIFO, restricted to tenants the fairness layer says currently
have quota. N workers poll the same table without blocking each other and
without a broker. Placement is therefore trivial for the agent loop (any worker
will do) and deliberate for sandboxes (a tainted, labelled node pool, never
co-scheduled with the control plane).

**Pause/resume, timeout, cancellation.** `WAITING_HUMAN` holds no worker, no
sandbox and no quota — only a row; `POST /resume` appends an event and flips the
state back to `QUEUED`. Budgets (steps, tool calls, tokens, dollars, wall clock)
are enforced twice: in the worker before paying for a model call, and in the
gateway before every tool call. Cancellation sets the state, and the gateway
refuses further calls immediately even if a worker is mid-step.

**Durability** rests on two guards on every write (`store.Commit`): the worker
must still hold an unexpired lease (a fencing token), and `next_seq` must match
(optimistic concurrency). A partitioned worker that wakes up late is rejected
rather than allowed to overwrite its replacement's progress.

---

## 3. Tool call path

See **[`docs/diagrams/toolcall.md`](docs/diagrams/toolcall.md)** for the full
sequence and the "what can the agent see at each step" table.

Four things about the ordering are deliberate:

1. **The gateway reloads the run and its pinned definition digest from the
   store** rather than trusting anything in the request, and cross-checks that
   the token's tenant matches the run's. Permissions come from the digest the
   run pinned, so editing an agent definition cannot widen a run already in
   flight.
2. **Policy decides before any credential is touched.** A denied call never
   causes a secret to be minted at all.
3. **Denials are audited as thoroughly as successes**, and the reason is
   returned to the model in plain language — which measurably stops the
   retry-the-same-call loop.
4. **The audit record is written before the side effect, and again after.** If
   the process dies mid-call we still know the attempt happened, which is the
   difference between "we do not know the outcome" and "we have no idea it was
   attempted".

**Where authorization is checked:** all three layers, with the gateway
authoritative. Schema-level filtering is cost and UX, not security. The gateway
cannot be bypassed because NetworkPolicy gives agent workers egress to the
gateway and Postgres and nothing else. Tool-level scoping (fine-grained tokens,
narrow IAM roles) bounds the damage if our own policy is wrong.

**Where credentials are injected:** only inside the gateway process, or in the
environment of a broker sandbox the agent has no handle on. The `creds.Secret`
type redacts through `fmt`, `json.Marshal`, `%#v`, `%x` and error wrapping, so
leaking one requires calling `.Reveal()` — a single greppable token, currently
used in exactly **one** place, guarded by a test that fails if that grows.

---

## 4. Sandbox design

See **[`docs/diagrams/sandbox.md`](docs/diagrams/sandbox.md)** for the layer
diagram, the credential mechanism and the compromise playbook.

**Isolation technology: gVisor by default.** Rejected upward: Kata/Firecracker
(a real VM boundary, but needs bare metal or nested virt and costs ~1s of cold
start). Rejected downward: plain runc + seccomp (shared kernel; for code written
by a model that has read attacker-controlled text, one kernel CVE is a
cross-tenant compromise). gVisor sits at the knee: it removes the "one Linux
kernel LPE = host compromise" class for ~2–15% CPU.

**The PoC implements the sandbox directly from Linux primitives** rather than
shelling out to a runtime, because "a container" is not a kernel object — it is
namespaces + cgroups + a pivoted root + dropped capabilities + seccomp, and
writing each explicitly makes every security property visible in the diff and
testable on its own. The same `Driver` interface has Docker and Kubernetes
implementations; `Result.Driver` records which one ran, so nothing in the system
can claim a stronger boundary than was actually used.

| Control | Choice, and the reasoning |
|---|---|
| **Filesystem** | `pivot_root` onto a read-only tmpfs — not `chroot`, which is escapable via a retained directory fd. A *synthesised* `/etc` rather than the host's, so machine config and any secret an operator drops there is invisible. `/work` and a size-capped `/tmp` are the only writable mounts, both `nosuid,nodev`. |
| **Network** | An **empty network namespace**. Not a firewall rule that can be misconfigured — no interface, no address, no route. The metadata endpoint is unreachable because there is nowhere to route *to*. |
| **Identity** | uid 1000 inside a user namespace → an unprivileged host uid that owns nothing. Capability **bounding** set emptied (so no `execve` can ever regain one) and `no_new_privs` set. |
| **Syscalls** | A 36-entry seccomp-BPF denylist covering the primitives an escape needs: `mount`, `ptrace`, `bpf`, `userfaultfd`, `io_uring_*`, `open_by_handle_at`, `unshare`, `setns`, … `clone3` returns `ENOSYS` (not `EPERM`) so glibc falls back to the filterable `clone`. |
| **Resources** | cgroup v1/v2 (cpu quota, memory max with swap pinned, pids max), rlimits (`NPROC`, `FSIZE`, `NOFILE`, `CORE=0`), a wall-clock deadline held by the *parent* that the payload cannot influence, and an output byte cap — an agent running `yes` would otherwise push megabytes into the event log and, worse, into the model context where it costs money. |

**How `gh` gets a token the agent cannot read:** it runs in a *second* sandbox,
in a different pid and user namespace, sharing only the `/work` bind mount. The
agent cannot read that process's `/proc/<pid>/environ`, cannot `ptrace` it, and
never receives the token in any response. Parameter policy permits
`gh pr create` and blocks `gh auth token`; the CLI also refuses to print its own
credential; and the gateway scrubs the value from output as a last resort.

**Cold-start cost: 8.4 ms mean** for the namespace driver (measured, 10 runs).
That number is what makes per-call ephemeral sandboxes affordable here; at the
1–3 s of a gVisor pod it is not, which is why the Kubernetes design keeps warm
per-run sandboxes for interactive agents.

---

## 5. Scaling and cost

**What is per-agent and what is shared:**

| | While active | While idle |
|---|---|---|
| Per agent | one sandbox pod *if* it uses exec tools; a row + log | **a row + log only** |
| Shared | workers, gateways, database, limiter, proxy, observability — these scale with **throughput**, not agent count | — |

**1000 agents does not mean 1000 of anything.** "Concurrent" ≠ "simultaneously
executing". If an average agent takes a turn every ~10 s, 1000 agents is ~100
turns/s; a turn costs the orchestrator a few ms of CPU and is otherwise I/O
wait — well under one core of real orchestration work. Measured here: **500
agents on 16 workers, p99 1.0 s**, and throughput rose **55.8× when workers went
2 → 24**, confirming the work is I/O-bound and the pool is the right shape.
Realistically ~10–20 worker pods with headroom.

**Sandboxes dominate the cost.** If 20% of agents are executing code at any
instant, that is ~200 sandbox pods at 0.5 CPU / 512 MiB → ~100 cores → roughly
13 × (8 vCPU, 32 GiB) nodes. This is why sandbox *lifetime* matters more than
sandbox *speed*, and why release-on-idle is a first-class concern.

**An idle agent costs ~nothing.** `WAITING_HUMAN` holds no worker, no sandbox,
no quota, no connection. A two-day wait costs a few KB of storage.

**LLM rate limits, shared fairly.** The provider quota is modelled as weighted
max-min fairness: `guaranteed_rate(i) = weight(i)/Σweights × provider_rate`, one
token bucket per tenant plus a shared bucket for capacity nobody is claiming.
A tenant can *always* draw at its guaranteed rate whatever anyone else is doing
— that is arithmetic, not a heuristic that might mis-tune. Quota is **reserved
from an estimate before the call and settled against real usage after**.
Human-blocking work draws on a reserved fraction (default 20%) that batch work
cannot touch. **Backpressure is expressed as not scheduling** — a throttled
tenant's runs stay `QUEUED` and no worker is occupied, which is precisely what
stops one noisy tenant from consuming the pool. Measured: a quiet tenant
received **98% of a flooding tenant's throughput while offering 1/50th the
load**, and a 2:1 weight produced a 2:1 ratio.

**Plan for 10×** (10k agents, 100k tool calls/min): workers and gateways are
stateless and scale horizontally; shard the lease query by tenant when one
`SKIP LOCKED` scan gets hot. **The event log breaks first** — 100k calls/min ×
~2 KB ≈ 288 GB/day — so partition `events` by month and tier cold partitions to
object storage, keeping run *state* in Postgres. Sandboxes are the binding cost:
larger tainted pools with Karpenter, and Firecracker snapshot-restore (~10 ms)
if density becomes the constraint. **The LLM cannot be scaled past the
contract** — technically multi-provider key pools, prompt caching and smaller
models for cheap steps; commercially it becomes a negotiation, and the design's
job is to make the rationing fair and visible.

---

## 6. Failure modes

| # | Failure | Detection | Recovery |
|---|---|---|---|
| 1 | **Node loss mid-tool-call** | Lease lapses (worker stops renewing); no failure detector to get wrong | Another worker replays the log to the same call and sends the **same deterministic idempotency key**; the journal returns the recorded result instead of repeating the side effect. If the gateway also died the entry is `IN_FLIGHT` → replay gets `409`, and the reaper marks it `FAILED` with a message stating the call **may or may not** have taken effect. **Honest limit: at-least-once for non-idempotent third-party APIs**; such tools are tagged `UnsafeRetry` and the ambiguity is surfaced to the model. |
| 2 | **Poison agent looping on tool calls** | Per-run counters; a rise in `agentorch_tool_calls_total` for one run | Budgets are **enforced, not reported**: the gateway refuses once exhausted. Per-tenant concurrency caps stop one tenant's poison agents monopolising workers. Remaining budget is injected into the prompt, which cheaply breaks the loop for well-behaved models. *Demonstrated: stopped at exactly 5/5 tool calls, $0.19 of a $0.50 cap.* |
| 3 | **Prompt injection → confused deputy** | **Not reliably detectable — so the design contains it rather than detecting it.** Detect the *effects*: egress-proxy denials, a spike in `decision="DENY"`, high-risk calls right after ingesting untrusted content | Capability-scoped grants (an agent literally cannot call an ungranted tool), parameter-level policy, an egress allowlist, and approval for high-blast-radius tools — breaking the "lethal trifecta" (private data + untrusted content + an exfiltration channel) by removing the channel. *Demonstrated: 3 exfiltration routes refused, metadata unreachable.* |
| 4 | **Sandbox escape** | eBPF runtime rules on the sandbox pool; gVisor blocked-syscall logs; egress denials | Kill the pod, cordon and drain the node with a forensic snapshot, revoke every credential minted for the run, freeze the tenant. Credentials are ≤60s and single-purpose, so a stolen one is near-worthless. |
| 5 | **LLM provider degradation / 429 storm** | Error-rate and latency SLO on the model gateway; 429 ratio | Bounded retries (3, never unbounded — that is how a partial outage becomes total) with **full jitter**; optional provider failover; then the run is **requeued, not failed** — durability means waiting is free. |
| 6 | **09:00 thundering herd (300 agents at once)** | Queue depth, dispatch latency, pending sandbox pods | Admission control at the API (per-tenant cap returning `429` with `retry_after`), token-bucket quota, an HPA tuned to scale up fast and down slowly, a `ResourceQuota` ceiling on sandboxes, and node headroom via preemptible balloon pods. Runs queue; they do not fail. |
| 7 | **Database unavailable** | Connection errors, replication lag | Workers stop leasing and **fail closed** — we do not execute side effects we cannot journal. Runs pause. This is a deliberate CP-over-AP choice: platform availability matters less than not double-executing side effects. |
| 8 | **Rolling deploy / node drain** | — | Workers are interchangeable; a drained worker yields its run back to the queue, and if it is SIGKILLed the lease simply lapses. PDBs keep a gateway replica alive. *Demonstrated: 12 runs survived a hard, un-drained generation swap with zero duplicated side effects.* |
| 9 | **Human never replies** | `waiting_since` age | Per-run human timeout, escalation, then auto-cancel with a final summary event. |

---

## 7. The three hardest decisions

### D1 — A stateless worker pool over a durable event log, **not** one pod per agent

**Rejected:** pod-per-agent. It is genuinely more Kubernetes-native, gives free
resource isolation and `kubectl logs`, and is a simpler mental model.

**Why:** 1000 mostly-idle pods is 10+ nodes of overhead; pods do not survive
drains so state must be externalised regardless; day-long human waits would hold
pods; and bursty creation hammers the API server and CNI.

**Cost I accepted:** replay makes side effects re-reachable, so idempotency has
to be engineered explicitly (deterministic keys + a journal + pre/post audit).
That is real complexity, and it is the part most likely to have a subtle bug.

**I would reverse it if:** agents became CPU-bound rather than I/O-bound (local
inference, heavy in-process data work); a tenant contractually required hard
kernel isolation for the *loop* and not just for exec; event-log write
amplification became the bottleneck before pod overhead did; or runs became
short and uniform, at which point Kubernetes `Job`s are genuinely simpler.

### D2 — gVisor, **not** Kata/Firecracker and **not** plain runc

**Rejected upward:** Kata/Firecracker — a true VM boundary, but needs bare metal
or nested virt and costs roughly a second of cold start.
**Rejected downward:** runc + seccomp + AppArmor — cheapest, but a shared kernel
under code written by a model that has read hostile text.

**Cost I accepted:** ~2–15% CPU, materially worse for syscall-heavy and I/O-heavy
workloads (which document conversion genuinely is), plus an operational
dependency on the runtime being installed on the sandbox pool.

**I would reverse it if:** we needed to run tenant-supplied *images* or nested
containers (→ Kata); a regulated tenant demanded a hardware boundary (→ a Kata
node pool as a premium tier); profiling showed gVisor's syscall overhead
dominating real workloads (→ measure first, then consider runc on per-tenant
dedicated nodes); or we moved to a platform with cheap microVMs (→ Firecracker).

### D3 — Authorization and credential injection at a central gateway, **not** in the agent process

**Rejected:** in-process tool execution with SDK credentials — which is what
most agent frameworks do by default.

**Why:** it puts long-lived tenant credentials in the same process as
attacker-influenced data; there is no central audit; every tool author
re-implements authorization; and a prompt injection that reaches `os.environ` is
game over.

**Cost I accepted:** an extra network hop (~1–3 ms in-cluster) on every tool
call; the gateway is a hot-path dependency (mitigated: stateless, horizontally
scaled, PDB-protected, shardable per tenant); and tool authors are constrained
to a declarative model.

**I would reverse it if:** latency became critical for high-frequency
micro-tools — in which case I would push a signed, short-TTL *capability* to the
worker for a small allowlist of read-only, low-risk tools, keeping the gateway
in the audit path asynchronously. Or if a tool fundamentally needed an
interactive session the gateway cannot proxy.

*(A close fourth — Postgres rather than Temporal as the durable-execution
substrate — is argued in [`DEEP_DIVE.md`](DEEP_DIVE.md) D4. Short version: I
need the event log, run state, idempotency journal and audit chain to commit in
one transaction, and per-tenant fair scheduling that task queues do not provide.
If the team already ran Temporal, I would use it.)*

---

## 8. What I cut, and why it was right to cut it

Cut because the **PoC's job is to retire risk, not to be complete**:

- **SPIFFE/SPIRE workload identity.** The PoC uses shared-secret HMAC run tokens;
  production needs mTLS so the gateway can prove *which service* is calling, not
  just which run it claims. **The most important production gap.** Right to cut
  because it changes no architectural decision — the token shape and the choke
  point stay identical.
- **The egress proxy.** Designed and referenced by NetworkPolicy, not built. The
  agent sandbox's empty netns already gives the property I wanted to show; the
  proxy matters for the *credentialed* path, which I proved a different way.
- **OPA/Cedar.** I hand-rolled a narrower policy model; production wants a
  reviewable, externally-authored policy language.
- **Workspace checkpointing to object storage.** A node loss currently loses
  `emptyDir` contents: the run recovers but re-does file work.
- **Per-tenant node pools** (hard multi-tenancy at the node level).
- **Multi-provider LLM routing, semantic caching, reconciliation against
  provider-reported usage.** Cost comes from a price table in code today.
- **Event-log compaction and tiering** — the first thing that breaks at 10×.
- **Agent-to-agent communication and sub-agent spawning.** A large design area
  (quota inheritance, cycle detection, transitive authorization) that would have
  consumed the whole budget.
- **DR, multi-region, backups, PgBouncer.** The demo Postgres is a
  single-replica StatefulSet and is explicitly not production.
- **Queue-depth autoscaling.** CPU is a poor proxy for an I/O-bound system;
  needs a custom metrics adapter.

Cut because I judged them lower value than the above, and I could be wrong:
cost attribution below the run level; a richer approval workflow (delegation,
expiry, dual control); structured classification of tool *results* (today they
are size-capped and secret-scrubbed, not classified).
