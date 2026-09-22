# Deep dive: every decision, the alternatives, and why

`DESIGN.md` states the decisions. This document shows the work: for each
decision, what else was on the table, the case *for* the option I rejected,
what my choice actually costs, who else has made this call, and the condition
under which I would reverse it.

A note on how to read the references: I have linked primary sources
(specifications, kernel docs, vendor engineering posts, papers) rather than
blog summaries wherever one exists, because the point of a reference is that
you can check the claim yourself.

**Contents**

| | Decision |
|---|---|
| [D1](#d1--agent-runtime-model) | Agent runtime model: stateless pool + event log vs. pod-per-agent |
| [D2](#d2--sandbox-isolation-technology) | Sandbox isolation technology |
| [D3](#d3--sandbox-lifecycle) | Sandbox lifecycle: per call, per agent, or shared pool |
| [D4](#d4--durable-execution-substrate) | Durable execution substrate: Postgres vs. Temporal vs. Kafka |
| [D5](#d5--where-authorization-lives) | Where authorization lives |
| [D6](#d6--the-credential-boundary) | The credential boundary, and how a CLI gets a token |
| [D7](#d7--workload-identity) | Workload identity |
| [D8](#d8--llm-quota-and-fairness) | LLM quota and fairness |
| [D9](#d9--kubernetes-integration-depth) | Kubernetes integration depth: CRD + operator vs. application scheduler |
| [D10](#d10--language-and-runtime) | Language and runtime |
| [D11](#d11--agent-definition-and-versioning) | Agent definition and versioning |
| [D12](#d12--audit-log-design) | Audit log design |
| [D13](#d13--exactly-once-vs-at-least-once) | Exactly-once vs. at-least-once tool calls |
| [D14](#d14--prompt-injection-posture) | Prompt injection: detect or contain |
| [D15](#d15--observability) | Observability |
| [D16](#d16--dependency-policy) | Dependency policy |
| [D17](#d17--gitops-and-argo-cd) | GitOps and Argo CD |

---

## D1 — Agent runtime model

**The question.** At runtime, what *is* one agent? The brief asks this directly,
and every other decision falls out of the answer.

### Options

**(a) One pod per agent.**
*For:* maximally Kubernetes-native. Free resource isolation, `kubectl logs`,
`kubectl exec` for debugging, per-agent `securityContext`, the scheduler does
placement for you, and the mental model is one sentence long. This is what
[Kubernetes Jobs](https://kubernetes.io/docs/concepts/workloads/controllers/job/)
are for and it is the first thing any Kubernetes engineer reaches for.
*Against:* the kubelet's default ceiling is
[110 pods per node](https://kubernetes.io/docs/setup/best-practices/cluster-large/),
so 1000 agents is ≥10 nodes *before* a single sandbox exists; pod overhead is
~100 MiB each; pods do not survive node drains, so durable state must be
externalised anyway; an agent waiting two days for a human holds a pod for two
days; and 300 agents starting at 09:00 is a burst of pod creation, CNI IPAM and
image pulls that pressures the API server and etcd.

**(b) One long-lived process (goroutine/actor) per agent in a shared pool.**
*For:* far cheaper than a pod; a natural fit for an actor framework
([Orleans virtual actors](https://learn.microsoft.com/en-us/dotnet/orleans/overview),
[Akka Cluster](https://doc.akka.io/docs/akka/current/typed/cluster.html),
[Cloudflare Durable Objects](https://developers.cloudflare.com/durable-objects/)).
*Against:* the process still dies with its host, taking in-memory state with it,
so you *still* need externalised state; and you inherit a placement problem
(which node owns agent X?) that needs a membership protocol — exactly the
component most likely to be subtly wrong.

**(c) A durable record executed by a stateless worker pool replaying an event
log. ← chosen**
*For:* idle costs a row; recovery is replay; the audit trail is a free
by-product; workers are interchangeable so deploys and drains are uneventful.
*Against:* replay re-reaches side effects, so idempotency must be engineered
explicitly; and "the current state" is a fold over the log rather than something
you can `kubectl get`.

### What it costs me

The honest cost is item (c)'s *Against*. Deterministic idempotency keys, a
journal with an `IN_FLIGHT` state, pre- and post-execution audit records, and a
reaper are all machinery that pod-per-agent would not need. This is the part of
the system most likely to contain a subtle bug, which is precisely why the PoC
tests it by SIGKILLing a worker mid-tool-call rather than by asserting it works.

### Precedent

This is the standard **durable execution** shape, arrived at independently by
several teams: [Temporal](https://docs.temporal.io/workflows) (event-sourced
workflow histories, replayed by stateless workers),
[AWS Step Functions](https://docs.aws.amazon.com/step-functions/latest/dg/welcome.html),
[Azure Durable Functions](https://learn.microsoft.com/en-us/azure/azure-functions/durable/durable-functions-overview)
(replay-based orchestrations),
[Restate](https://docs.restate.dev/concepts/durable_building_blocks), and
[DBOS](https://docs.dbos.dev/) (which, like this design, puts the log in
Postgres). The underlying pattern is
[event sourcing](https://martinfowler.com/eaaDev/EventSourcing.html).

### I would reverse it if

- Agents became **CPU-bound** rather than I/O-bound — local inference, heavy
  in-process data transformation. Then a worker *is* the agent and pooling buys
  nothing.
- A tenant contractually required **hard kernel isolation for the agent loop
  itself**, not just for exec.
- **Event-log write amplification** became the bottleneck before pod overhead
  did. At 100k tool calls/min the log is ~288 GB/day; if tiering it proves
  harder than running more nodes, the calculus flips.
- Runs became **short and uniform** (seconds, no human waits), at which point
  Kubernetes Jobs are genuinely simpler and simpler wins.

---

## D2 — Sandbox isolation technology

**The question.** A language model that has read attacker-controlled text is
going to write code and ask us to run it. What do we run it in?

### Options

| Option | Boundary | Cold start | Runs `pandoc`/`gh`/pip? | Verdict |
|---|---|---|---|---|
| runc + seccomp + AppArmor | Shared kernel | ~50–100 ms | Yes | Rejected as the *only* layer |
| **gVisor (runsc)** | User-space kernel | ~150–300 ms | Yes | **Chosen** |
| Kata / Firecracker | Real VM, separate kernel | ~0.5–1 s pod-level | Yes | Premium tier |
| WebAssembly | Capability-based, no syscalls | microseconds | **No** | Fails the requirement |
| Remote executor pool | Depends on the pool | ~0 (warm) | Yes | Moves the problem |

**runc + seccomp.** *For:* cheapest, ubiquitous, and the
[Docker default seccomp profile](https://github.com/moby/moby/blob/master/profiles/seccomp/default.json)
already blocks ~44 syscalls. *Against:* one shared kernel. The history of
container escapes is a history of kernel bugs —
[CVE-2022-0185](https://nvd.nist.gov/vuln/detail/CVE-2022-0185) (fs context heap
overflow → container escape),
[CVE-2022-0492](https://nvd.nist.gov/vuln/detail/CVE-2022-0492) (cgroup v1
`release_agent`), [Dirty Pipe](https://dirtypipe.cm4all.com/). For multi-tenant
untrusted code, "we hope there is no current kernel LPE" is not a security
posture.

**gVisor.** *For:* intercepts syscalls and services them in a user-space kernel
(the Sentry), so the host kernel surface an attacker reaches is drastically
smaller. Used by [Google Cloud Run and App Engine](https://gvisor.dev/docs/),
and available as a
[GKE Sandbox](https://cloud.google.com/kubernetes-engine/docs/concepts/sandbox-pods)
RuntimeClass. *Against:* real overhead — gVisor's own
[performance guide](https://gvisor.dev/docs/architecture_guide/performance/)
documents that syscall-heavy and I/O-heavy workloads suffer most, which document
conversion genuinely is. Some syscalls are unimplemented, so exotic software
breaks. And it is another runtime to install and keep patched.

**Kata / Firecracker.** *For:* the strongest boundary short of separate
hardware — a real VM with its own kernel.
[Firecracker](https://www.usenix.org/conference/nsdi20/presentation/agache) boots
a microVM in ~125 ms and underpins AWS Lambda and Fargate;
[Kata Containers](https://katacontainers.io/) packages this as a Kubernetes
RuntimeClass. *Against:* needs bare metal or nested virtualisation (which many
managed Kubernetes offerings do not give you), more memory per sandbox, and
pod-level cold start closer to a second.

**WebAssembly.** *For:* the best isolation-per-millisecond in existence and a
genuinely capability-based model —
[WASI](https://github.com/WebAssembly/WASI) grants filesystem and network access
explicitly rather than by default. Microsecond instantiation.
*Against:* **it cannot run the workload.** The brief names `pandoc`, `gh`, `jq`,
`curl`, `git` and arbitrary Python. Compiling that ecosystem to WASI is not a
2026 reality. It is a compelling *fast path* for pure-computation tools later,
not a general answer.

### What my choice costs me

2–15% CPU on typical workloads and worse on syscall-heavy ones; an extra
runtime on the sandbox node pool; and a class of "works on my laptop, fails
under gVisor" bugs from unimplemented syscalls.

### Why the PoC implements namespaces directly

Not as a replacement for gVisor — as a way to make the properties *visible*. A
container is not a kernel object; it is
[namespaces](https://man7.org/linux/man-pages/man7/namespaces.7.html) +
[cgroups](https://docs.kernel.org/admin-guide/cgroup-v2.html) +
[`pivot_root`](https://man7.org/linux/man-pages/man2/pivot_root.2.html) +
[capabilities](https://man7.org/linux/man-pages/man7/capabilities.7.html) +
[seccomp](https://www.kernel.org/doc/html/latest/userspace-api/seccomp_filter.html).
Writing each one explicitly means each is reviewable in the diff and testable on
its own, and it means the demo runs on a machine with no container runtime at
all. The `Driver` interface keeps Docker and Kubernetes implementations
honest, and `Result.Driver` records which boundary actually ran, so no part of
the system can overstate the isolation it got.

### I would reverse it if

- We had to run **tenant-supplied images** or nested containers → Kata.
- A regulated tenant demanded a **hardware boundary** → a Kata node pool as a
  premium tier, selected by RuntimeClass per agent definition.
- Profiling showed **gVisor's syscall overhead dominating** real workloads →
  measure first, then consider runc on per-tenant dedicated nodes, trading
  kernel isolation for node isolation.
- We moved to a platform with **cheap microVMs** → Firecracker with
  [snapshot restore](https://github.com/firecracker-microvm/firecracker/blob/main/docs/snapshotting/snapshot-support.md)
  (~10 ms), which would make D3 reconsider per-call sandboxes at scale.

---

## D3 — Sandbox lifecycle

**The question.** Per-call ephemeral, per-agent warm, or a shared executor pool?

**Per-call ephemeral.** Strongest isolation — nothing survives between calls, so
a compromised call cannot plant anything for the next one. Cost is cold start ×
call rate.
**Per-agent warm.** One sandbox per run, reused across its calls. Amortises cold
start; the workspace stays hot; but state persists between calls, so a
compromise in call *n* is present in call *n+1*.
**Shared executor pool.** A fleet of warm sandboxes, leased per call. Best
utilisation, worst isolation — reuse across *tenants* is a cross-tenant channel
unless the sandbox is destroyed between leases, which removes the benefit.

**Chosen: per-call by default, warm per-run for interactive agents, never shared
across tenants.**

The deciding input is a **measurement, not a preference**: cold start is
**8.4 ms mean** for the namespace driver (`TestSandbox_ColdStartIsMeasured`, 10
runs, worst 19.4 ms). At 8 ms, per-call ephemeral is affordable at 10k
calls/min. At the ~1–3 s of a gVisor pod it is not — which is exactly why the
Kubernetes driver's design keeps a warm per-run sandbox for interactive agents
and falls back to per-call for batch.

This is the one place where a number changed the architecture rather than the
architecture choosing a number. If cold start had measured 500 ms I would have
made warm-per-run the default and accepted the weaker isolation.

---

## D4 — Durable execution substrate

**The question.** What actually stores the log and drives the queue?

**Temporal.** *For:* this is a solved problem and Temporal has solved it.
Deterministic replay, timers, signals, retries, visibility. If I were told "you
will run this in production next quarter and you already operate Temporal", I
would use it and D1 would be nearly free.
*Against:* it is a large operational dependency (its own cluster, its own
datastore, its own upgrade cadence). Its task queues do not directly give
per-tenant weighted fair scheduling — I would end up putting a scheduler in
front anyway. And agent runs need a fine-grained, *queryable, auditable* event
history that I would end up duplicating outside Temporal's history for the
audit requirement.

**Kafka + a compacted state view.** *For:* the event log is genuinely a log, and
this scales to any throughput. *Against:* no transactions across the log, the
run state and the audit chain, which is the property that makes crash recovery
tractable here. Exactly-once across topics is possible but subtle.

**Postgres. ← chosen** *For:* the event log, run state, idempotency journal and
audit chain all commit **in one transaction**, which removes an entire class of
split-brain bug.
[`SELECT ... FOR UPDATE SKIP LOCKED`](https://www.postgresql.org/docs/current/sql-select.html#SQL-FOR-UPDATE-SHARE)
gives a work queue with exactly the semantics I want, and every operator on
earth knows how to run, back up and inspect Postgres. (This is also
[DBOS's](https://docs.dbos.dev/) core bet, and the pattern behind
[Que](https://github.com/que-rb/que), [River](https://riverqueue.com/) and
[Oban](https://hexdocs.pm/oban/Oban.html).)
*Against:* it is a single scaling axis. At 10× the event log is the first thing
to break (~288 GB/day), and the answer is partitioning plus tiering to object
storage — real work I have not done.

**I would reverse it if:** the team already operates Temporal (then the
integration cost is near zero and its maturity beats my hand-rolled lease
logic); or if hand-rolled durable execution accumulates bugs — which it will,
because this is a deep problem and Temporal has years of them fixed.

---

## D5 — Where authorization lives

**Options:** compiled into the tool schema the model sees; at a central gateway;
at the tool itself.

**In the schema.** *For:* the model never sees tools it cannot call, so it
mostly does not try — fewer tokens, less audit noise. *Against:* **not
security.** A model can emit any tool name, and an injected prompt actively
will. Anything that relies on the model not trying is not a control.

**At the gateway. ← authoritative** *For:* one place that sees every call; it
cannot be bypassed because NetworkPolicy gives agent workers egress to the
gateway and Postgres and nothing else; and it is small enough to audit as a
single component. *Against:* a network hop (~1–3 ms in-cluster) on every call,
a hot-path dependency, and tool authors constrained to a declarative model.

**At the tool.** *For:* bounds the blast radius when the gateway's policy is
itself wrong — a
[GitHub fine-grained token](https://docs.github.com/en/authentication/keeping-your-account-and-data-secure/managing-your-personal-access-tokens)
scoped to one repository, an IAM role with one action. *Against:* no central
audit trail, and correctness is delegated to N third parties configured by N
different people.

**Chosen: all three, gateway authoritative.** This is defence in depth done
properly — each layer catches a different failure. The failure mode to avoid is
authorization *only* at the tool, which is where most agent frameworks land by
default and which gives you no answer to "show me every command agent X ran".

**Industry shape:** this is the
[Zanzibar](https://research.google/pubs/pub48190/) / policy-decision-point model,
and the same argument as an API gateway or a service mesh
[authorization policy](https://istio.io/latest/docs/reference/config/security/authorization-policy/)
— one enforcement point, many defence layers. Related: the
[OWASP Top 10 for LLM Applications](https://owasp.org/www-project-top-10-for-large-language-model-applications/),
specifically LLM06 Excessive Agency.

**I would reverse it if:** latency became critical for high-frequency
micro-tools. The escape hatch is a signed, short-TTL *capability* pushed to the
worker for a small allowlist of read-only, low-risk tools, with the gateway
still in the audit path asynchronously — trading synchronous enforcement for
synchronous *recording*, which is a real weakening and should be a deliberate
per-tool decision.

---

## D6 — The credential boundary

**The question the brief asks directly: how does `gh` get a token without the
agent being able to read it?**

### Options

**(a) Environment variable in the agent's sandbox.** Trivial, and wrong: the
agent's own code can `env`, read `/proc/self/environ`, or just print it. This is
what nearly every agent framework does today.

**(b) Egress-proxy header injection.** The sandbox's traffic goes through a
proxy that terminates TLS and injects `Authorization`. *For:* fully general, the
token never enters the sandbox. *Against:* requires TLS interception with a CA
in the image, breaks certificate pinning, and the proxy becomes a very
attractive target.

**(c) A credential-helper socket.** `git config credential.helper` pointing at a
unix socket. *Against:* if the socket returns the token, the agent's code can
call the socket directly and read it. So the helper must *proxy* rather than
return — which collapses into (b) or (d).

**(d) Run the credentialed CLI in a separate sandbox. ← chosen** The agent's
sandbox gets no credential and no network. A *second* sandbox, in a different
pid and user namespace, runs a trusted binary from our own image with the token
in its environment and egress only to the allowlisted API. The two share exactly
one thing: the `/work` bind mount. argv goes in (after parameter policy),
stdout comes out.

**(e) Do not allow CLIs at all** — expose `github.create_pr` as an API tool.
Strongest, and it is what I would do for the top few high-value integrations.
But the brief explicitly requires arbitrary CLIs, and enumerating every
subcommand of every CLI as an API tool does not scale.

### Why (d) works

Different pid namespace → the agent cannot see the broker process in `/proc` or
`ps`. Different user namespace → different host uid. `ptrace` is blocked by
seccomp *and* unreachable across the namespace. The token never appears in any
response, and the gateway scrubs its value from output as a last resort.

Four independent things must all fail for the credential to leak, and the test
asserts all four hold: policy blocks `gh auth token`; the CLI refuses to print
its own token; the namespaces prevent observation; the gateway scrubs output.

### What it costs

A second sandbox per credentialed call (~8 ms locally, ~1–3 s under gVisor on
Kubernetes — which argues for warm broker sandboxes per run), and a shared
workspace mount that is a genuine, if narrow, channel between the two.

### The `Secret` type

Beyond the architecture, one language-level control: `creds.Secret` implements
`String`, `GoString`, `Format` and `MarshalJSON` to redact. Printing it is safe
by default; extracting it requires `.Reveal()`, a single greppable token used in
exactly **one** place, guarded by a test that fails if that count grows. This
converts "remember not to log the token" from a discipline problem into a type
problem. Compare Rust's
[`secrecy`](https://docs.rs/secrecy/latest/secrecy/) crate, which makes the same
argument.

---

## D7 — Workload identity

**What the PoC does:** HMAC-signed run-scoped JWTs with a shared secret,
audience-bound to the gateway, 5-minute TTL.

**Why that is not sufficient for production:** the token proves *which run* the
caller claims to be acting for. It does not prove *which service* is calling. A
compromised component inside the platform namespace with access to the signing
secret can mint a token for any run of any tenant.

**What production needs:** [SPIFFE/SPIRE](https://spiffe.io/docs/latest/spiffe-about/overview/)
workload identity with mTLS, so the gateway authenticates the *caller* (a real
agent worker, attested by node and pod selectors) independently of the run token
it presents. Then compromise of the signing key is not sufficient; you also need
a valid workload identity. Combined with asymmetric run tokens (RS256/EdDSA)
issued by the control plane, the gateway needs no shared secret at all.

**This is the single most important gap between the PoC and something I would
run.** It is listed first in "What I cut" for that reason. It was right to cut
because it changes no architectural decision — the token shape and the choke
point are identical either way — but it is not optional.

---

## D8 — LLM quota and fairness

**The problem.** The provider is rate-limited and is the largest cost line. A
tenant that spins 300 agents at 09:00 would, under FIFO, consume the whole quota
and every other tenant's agents would stop.

### Options

**Global token bucket.** Simple, and exactly the failure above.

**Per-tenant fixed quotas.** No starvation, but idle tenants waste capacity —
and at these prices, wasting provider capacity is wasting money.

**Weighted max-min fairness + a shared spare pool. ← chosen**
`guaranteed_rate(i) = weight(i)/Σweights × provider_rate`. One bucket per
tenant, plus a shared bucket fed by capacity nobody's guarantee is claiming. A
tenant can *always* draw at its guaranteed rate whatever anyone else is doing —
that is arithmetic, not a heuristic that might mis-tune. Unused capacity is
still burstable, so guarantees do not cost utilisation.
([Max-min fairness](https://en.wikipedia.org/wiki/Max-min_fairness); the same
shape as [DRR](https://en.wikipedia.org/wiki/Deficit_round_robin), Linux
[CFS](https://docs.kernel.org/scheduler/sched-design-CFS.html), and
[Envoy's rate limit service](https://www.envoyproxy.io/docs/envoy/latest/intro/arch_overview/other_features/global_rate_limiting).)

**Priority lanes.** Human-blocking work draws on a reserved fraction (default
20%) that batch work cannot touch, so a person watching a spinner does not queue
behind a nightly backlog. Same idea as Kubernetes
[PriorityClass](https://kubernetes.io/docs/concepts/scheduling-eviction/pod-priority-preemption/)
and API server
[flow control](https://kubernetes.io/docs/concepts/cluster-administration/flow-control/).

### Two details that matter more than the algorithm

**Reserve then settle.** Quota is reserved from an estimate *before* the call and
reconciled against the provider's reported usage *after*. Charging only
afterwards would let a tenant blow past its share with one enormous request;
charging only the estimate would drift. The estimate is deliberately crude
(~4 chars per token) because it only has to be approximately right.

**Backpressure is expressed as *not scheduling*.** A throttled tenant's runs sit
in `QUEUED` and no worker is occupied. Blocking a worker on a full bucket is
precisely how one noisy tenant takes down the pool for everyone — the failure
this whole layer exists to prevent.

**Measured:** a quiet tenant received **98% of a flooding tenant's throughput
while offering 1/50th the load** (50 spinning goroutines vs. one 5 ms ticker),
and a 2:1 weight produced a 2:1 throughput ratio.

**I would reverse it if:** provider quota stopped being scarce (unlikely), or if
tenants demanded *reserved capacity* rather than fair shares — a different
product decision that maps to per-tenant provider keys.

---

## D9 — Kubernetes integration depth

**The question.** A CRD `AgentRun` plus an operator, or an application-level
scheduler on top of Kubernetes?

**CRD + operator.** *For:* genuinely Kubernetes-native. `kubectl get agentruns`,
declarative, GitOps-able, and it fits every operator's existing mental model.
The [operator pattern](https://kubernetes.io/docs/concepts/extend-kubernetes/operator/)
is the idiomatic answer to "I have a custom workload".
*Against:* **etcd is not a database for this.** 1000 concurrent runs each with
a growing event history would mean large, frequently-updated custom resources;
etcd's
[recommended total database size is a few GB](https://etcd.io/docs/v3.5/dev-guide/limit/)
and a 1.5 MB per-object limit. Status updates at agent-step frequency would
generate watch traffic across the whole cluster. And etcd gives no per-tenant
weighted fair scheduling and no SQL for "every command agent X ran last
Tuesday".

**Application scheduler over Postgres. ← chosen** with Kubernetes doing what it
is genuinely best at: running the stateless worker pool, scheduling sandbox
pods onto a tainted node pool, enforcing NetworkPolicy and RuntimeClass,
autoscaling, PDBs and resource quotas.

**The honest middle ground I did not build:** a *thin* CRD that carries only the
run's identity and terminal status — no event history — reconciled from
Postgres, purely so `kubectl get agentruns` works and GitOps can express "these
agents should exist". That gets the ergonomics without putting the hot path in
etcd. I would build that in production; it was not worth the PoC budget.

**I would reverse it if:** runs became short and uniform enough that the history
fits comfortably in a custom resource, or if the operator experience of
`kubectl`-native runs proved more valuable in practice than I am weighting it.

---

## D10 — Language and runtime

**Go. ← chosen** *For:* goroutines make an I/O-bound worker that holds thousands
of in-flight steps natural rather than clever; static binaries with
`CGO_ENABLED=0` can be bind-mounted into a sandbox with no shared libraries at
all (which is how the `gh` and `aoconvert` personalities work); direct, readable
access to `clone`, `pivot_root`, `setns` and `seccomp` through the standard
library; a distroless final image with no shell.
*Against:* more verbose than the alternatives; no async cancellation of
CPU-bound work; the LLM ecosystem's centre of gravity is Python.

**Rust** would be a better choice for the sandbox supervisor specifically —
stronger guarantees at exactly the layer where a memory-safety bug is worst, and
crates like `secrecy` and `caps` exist. I did not choose it because the rest of
the system (HTTP, SQL, orchestration) is faster to write correctly in Go, and a
two-language system needs a better reason than one component's preference.

**Python** would put us closest to the model ecosystem and is what most agent
frameworks use. Rejected for the control plane: the GIL makes a
high-concurrency I/O worker awkward, and the deployment story (interpreter +
dependencies in the image that runs untrusted code) is a materially larger
attack surface. **Python is the right language for the *agents*, not for the
*platform*.**

---

## D11 — Agent definition and versioning

**Content-addressed, immutable specs; a run pins a digest.** Exactly the
container-image model applied to agents:
`digest = sha256(canonical_json(spec))`, registering the same spec twice is a
no-op, and `runs.def_digest` is the permission set the gateway consults.

The property this buys is the important one: **editing an agent definition
cannot widen the permissions of a run already in flight**, and an auditor can
always reconstruct exactly what an agent was allowed to do at the moment it ran.
Without content addressing, "agent X ran this command" is unanswerable if
someone edited agent X yesterday.

Canonicalisation matters and is easy to get wrong — `internal/canon` sorts keys,
normalises number formatting and round-trips through `json.Number`, because
`{"a":1,"b":2}` and `{"b":2,"a":1}` must hash identically or the same agent
gets two digests. Compare
[JCS, RFC 8785](https://www.rfc-editor.org/rfc/rfc8785).

---

## D12 — Audit log design

**Requirement:** "show me every command agent X ran last Tuesday."

**Design:** an append-only table, indexed on `(tenant_id, ts)` so that question
is a range scan, with a **per-tenant hash chain**:
`hash = sha256(canonical(record) || prev_hash)`.

**Why chain at all.** Any store can hold rows. The question an auditor actually
asks is "can an operator with database access quietly rewrite what an agent
did?" With the chain, editing record *N* invalidates *N* and everything after
it, so tampering requires rewriting the entire tail. If the head hash is
periodically published somewhere the operator does not control — a
[transparency log](https://transparency.dev/), a different account's object
store, a receipt held by the customer — it requires forging that too.

**Why per tenant, not global.** Appends serialise behind the chain head, so a
global chain would serialise the entire fleet. Per tenant, each chain sees a
fraction of the traffic *and* a per-tenant export is self-verifying.

**Denials are audited as thoroughly as successes.** A security log that records
only what worked answers the wrong question.

**A bug the conformance suite caught:** Go's `time.Time` carries nanoseconds;
Postgres `timestamptz` stores microseconds. Hashing the un-truncated value made
every chain fail to re-verify after a round trip. A tamper-evident log that
always reports tampering is worse than none — it trains operators to ignore the
warning. Fixed by truncating to microseconds before hashing *and* storing.

---

## D13 — Exactly-once vs. at-least-once

**There is no exactly-once across a network boundary**, and pretending otherwise
is the most common distributed-systems error. What is achievable is
*effectively-once* via idempotency, and the honest scope is:

| Case | Guarantee |
|---|---|
| Tool supports idempotency keys (Stripe, many modern APIs) | **Effectively once.** Our key is deterministic (`run:step:index`), so replay is safe end-to-end. |
| Tool is naturally idempotent (GET, PUT to a known path) | **Effectively once.** |
| Tool is neither, and the gateway completed the call before dying | **Effectively once** — our journal has the result. |
| Tool is neither, and the gateway died *during* the call | **Unknown.** We do not know whether it happened. |

For the last case the design does the only honest thing: the journal entry stays
`IN_FLIGHT`, a replaying worker gets `409`, the reaper eventually marks it
`FAILED` with a message that says verbatim that the call **may or may not** have
taken effect, and tools with this property are tagged `UnsafeRetry` so the model
is told rather than allowed to blindly retry.

Telling the model the truth is the design decision here. The alternative —
presenting an ambiguous failure as a clean one — invites a duplicate payment.

See [Kleppmann on distributed locking and fencing tokens](https://martin.kleppmann.com/2016/02/08/how-to-do-distributed-locking.html),
which is the argument behind `store.Commit`'s lease check.

---

## D14 — Prompt injection: detect or contain

**Position: contain. Detection is not a control.**

Prompt injection is not reliably detectable. Classifiers and heuristics help at
the margin, but a system whose security depends on classifying natural language
correctly has no security boundary — it has a filter, and filters get bypassed.

So the design assumes injection *will* succeed at the model layer and makes it
not matter:

| Mechanism | What it removes |
|---|---|
| Capability-scoped grants | The agent cannot call a tool it was not granted, however convincingly it is asked |
| Parameter-level policy | "May call `http.get`" does not mean "may GET anything" |
| Empty network namespace | No exfiltration channel from executed code |
| Egress allowlist (credentialed path) | No exfiltration channel from credentialed calls |
| Short-TTL scoped credentials | A stolen credential is near-worthless |
| Human approval for high-blast-radius tools | A person in the loop where it matters |

This is deliberately an attack on Simon Willison's
["lethal trifecta"](https://simonwillison.net/2025/Jun/16/the-lethal-trifecta/)
— private data + untrusted content + an exfiltration channel. We cannot remove
the first two; we remove the third.

*Detection still has a job*, just not a load-bearing one: egress-proxy denials,
a rise in `decision="DENY"`, and high-risk calls immediately after ingesting
untrusted content are all good **alerting** signals. They tell you an attack is
happening. They are not what stops it.

References: [OWASP Top 10 for LLM Applications](https://owasp.org/www-project-top-10-for-large-language-model-applications/)
(LLM01 Prompt Injection, LLM06 Excessive Agency),
[NIST AI 100-2 on adversarial ML](https://csrc.nist.gov/pubs/ai/100/2/e2023/final),
[Anthropic on agent security](https://www.anthropic.com/news/claude-code-security).

---

## D15 — Observability

**Requirement:** "what is agent X doing right now, and what has it cost?"

**Both questions are answered from the event log**, not from a parallel
telemetry stream. This matters: the UI timeline shows literally what the agent
saw, because it is rebuilt from the same events that rebuild the model context.
A separate telemetry path can drift from reality; this one cannot.

On top of that: a small set of Prometheus metrics (tool calls by
tenant/tool/decision, tokens and cost by tenant, run states, quota denials,
audit-write failures, step latency), structured JSON logs tagged with
`run_id`/`tenant_id`, and SSE for live updates.

**Hand-rolled exposition rather than `client_golang`** — see D16.

**What I cut:** distributed tracing. [OpenTelemetry](https://opentelemetry.io/)
spans across worker → gateway → sandbox would be genuinely valuable for latency
attribution, and the trace-id plumbing is already in `internal/obs`. It did not
make the budget.

**SSE rather than WebSocket** for live updates: the data flows one way, it is
text, browsers reconnect automatically, and it needs no handshake, framing
library or extra dependency. The current implementation is a poll-and-push
bridge; at 10× it should become Postgres `LISTEN/NOTIFY`, because one polling
goroutine per open browser tab does not scale.

---

## D16 — Dependency policy

**The whole backend has one external dependency: `github.com/lib/pq`.**

Everything else is the standard library: the HTTP router (Go 1.22+ `ServeMux`
has method and wildcard patterns), JWT (~60 lines of HMAC-SHA256), Prometheus
exposition (~150 lines of text formatting), the Kubernetes client (~150 lines of
REST over the ServiceAccount), UUIDs, and the entire sandbox (`syscall` plus
hand-written seccomp BPF).

**This is a deliberate trade, and it is not free.** I have written code that
mature libraries already provide, and mine has fewer eyes on it. `client_golang`
handles exposition edge cases I have not thought about; `libseccomp` has a
better BPF compiler; `client-go` has informers and retry logic I would need to
write.

**Why I took it anyway, for *this* system specifically:** the component that
executes untrusted code is the one where supply-chain risk is least acceptable.
`client-go` alone pulls in ~200 modules. A dependency tree that large is an
attack surface on a system whose entire purpose is containment, and it is one
that no reviewer will actually read. One dependency means `go.sum` is
reviewable in a minute and `go build` works with no network.

**When this flips:** the moment we need informers, leader election, CRD codegen,
native histograms with exemplars, or a real policy language. Then the
libraries' maturity beats my hand-rolled versions and I should use them. This is
a PoC-scale decision, stated as one.

---

## D17 — GitOps and Argo CD

**Why GitOps is a *security* decision here, not just a deployment one.** This
platform's security rests on policy being reviewable: NetworkPolicies, RBAC,
RuntimeClass, resource quotas. If an operator can `kubectl edit` a NetworkPolicy
at 3am and nobody notices, the policy is advisory. Argo CD's `selfHeal` reverts
that within seconds, which makes "what is in git" and "what is running" the same
statement. `argocd-test.sh` verifies exactly this by making a manual change and
asserting the cluster puts it back.

**The `AppProject` is the part people skip.** Without one, any Application in
the `argocd` namespace can deploy anything anywhere — which makes Argo the
widest privilege in the cluster. Ours restricts destinations to two namespaces,
whitelists exactly two cluster-scoped kinds (`Namespace`, `RuntimeClass`), and
explicitly blacklists `ClusterRole` and `ClusterRoleBinding` so a compromised
repository cannot escalate cluster-wide.

**Alternative considered: Flux.** Equivalent for this purpose; Argo's UI is
better for demonstrating reconciliation to a reviewer, which is why it is here.
Nothing in the design depends on the choice.

**One honest note about the local overlay.** kind nodes run containerd without
the gVisor shim, so the local overlay sets `AGENTORCH_RUNTIME_CLASS=""`. That is
a real security downgrade — sandboxes there share the host kernel. It is
commented as such in the overlay itself, because a local overlay that silently
weakens the model is how a weaker model ships.

Relatedly, `deploy/kind/kind-config.yaml` **disables kind's default CNI** and
`kind-up.sh` installs Calico, because kindnet does not implement NetworkPolicy.
Applying isolation policies to a cluster whose CNI ignores them would give false
assurance — strictly worse than having none.
