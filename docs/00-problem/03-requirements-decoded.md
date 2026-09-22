# Requirements decoded

> **Prerequisite:** [Use cases](02-use-cases.md)
> **Read next:** [Threat model](04-threat-model.md)

Each requirement from the brief, restated as what it actually demands of an
implementation, with the mechanism that satisfies it and the evidence.

---

## R1 — Scale

> *"~1000 concurrent agents, ~10k tool calls/min at peak, with a plan for 10x"*

### What it really demands

| Naive reading | Actual demand |
|---|---|
| "Run 1000 things" | Hold 1000 *obligations*, most of which are idle, while executing ~100 turns/s |
| "10k tool calls/min" | 167 calls/s sustained through a single authorization choke point without it becoming the bottleneck |
| "Plan for 10x" | Identify which axis breaks **first**, and be honest that it is not the same axis as today's bottleneck |

### Decomposed

10k tool calls/min = **167/s**. Split by kind, roughly:

| Kind | Share | Per-call cost to us | Aggregate |
|---|---|---|---|
| `fs.*` | ~40% (67/s) | ~0.1 ms CPU | 7 ms/s |
| `http.*` | ~35% (58/s) | ~0.2 ms CPU + I/O wait | 12 ms/s |
| `exec.*` | ~25% (42/s) | 8.4 ms cold start + payload | **the real cost** |

The first two are nothing. The third is 42 concurrent sandbox creations per
second, which is why sandbox cold start was measured before the lifecycle policy
was chosen rather than after.

### Mechanism

- Stateless worker pool, horizontally scaled — [Durable execution](../01-concepts/02-durable-execution.md)
- `FOR UPDATE SKIP LOCKED` queue: N workers poll without blocking each other
- Sandboxes on a dedicated, tainted, autoscaled node pool

### Evidence

| Measurement | Value |
|---|---|
| 500 agents / 16 workers | 500 completed, 0 failed, **p99 1.0 s** |
| Throughput scaling, 2 → 24 workers | **55.8×** |
| Mixed-tenant burst, 300 agents / 12 workers | 300 completed, p99 469 ms |

The 55.8× figure is the load-bearing one: it confirms the work is I/O-bound and
that nothing in the shared path (the lease query, a mutex) is serialising. If
throughput had flattened, the "stateless pool" story would be false.

Full numbers: [Benchmarks](../04-evidence/01-benchmarks.md).

### The 10× answer

| Component | Breaks at 10×? | Fix |
|---|---|---|
| Workers, gateways | No — stateless | More replicas |
| Lease query | Maybe | Shard by tenant hash |
| **Event log** | **Yes, first** | 100k calls/min × ~2 KB ≈ **288 GB/day**. Partition by month, tier cold partitions to object storage as Parquet |
| Sandbox fleet | Cost, not correctness | Bigger pools + Karpenter; Firecracker snapshot-restore (~10 ms) for density |
| LLM provider | **Cannot be fixed technically** | Multi-provider key pools, caching, smaller models. Ultimately a contract negotiation |

---

## R2 — Safe tool calls

> *"An agent cannot call a tool it was not granted; credentials never enter the
> model context; every call is attributable to agent, tenant and triggering user"*

Three separate requirements wearing one coat.

### R2a — "cannot call a tool it was not granted"

**"Cannot", not "should not".** The agent must be structurally incapable, not
merely instructed. Filtering the model's tool list is necessary but insufficient:
a model can emit any tool name, and a prompt-injected one actively will.

| Mechanism | Layer | Is it security? |
|---|---|---|
| Only granted schemas are sent to the model | Context | **No** — cost and noise reduction |
| Worker pre-checks the grant | Worker | Defence in depth |
| **Gateway checks against the run's pinned digest** | Gateway | **Yes — authoritative** |
| Provider-side token scoping | Third party | Blast-radius bound |

The gateway cannot be bypassed because NetworkPolicy `agentd-egress` permits
workers to reach exactly Postgres and the gateway.

The subtle part is *which* grant set is checked: the one in the **content digest
the run pinned at creation**, not the current definition. Editing an agent
cannot retroactively widen a run already in flight.

**Evidence:** Use case 3 — three denials, with the reason returned to the model.

### R2b — "credentials never enter the model context"

Stronger than it sounds. The credential must not reach:

| Location | Why it matters |
|---|---|
| The model's context | It would be echoed back, logged, and re-sent on every subsequent turn |
| The event log | Durable. Replayed into context forever |
| The agent's sandbox environment | `env` and `/proc/self/environ` are readable by the agent's own code |
| Any tool result | Attacker-influenced output is the natural exfiltration channel |
| A log line or error message | The most common accidental leak |

Five mechanisms, described in [Secrets](../01-concepts/05-secrets.md):

1. Architectural — credentialed CLIs run in a **separate sandbox** in a different
   pid/user namespace
2. Type-level — `creds.Secret` redacts through `fmt`, `json.Marshal`, `%#v`,
   `%x` and error wrapping; extraction requires `.Reveal()`, used in **one**
   place, guarded by a test
3. Policy — parameter patterns block credential-disclosing subcommands
4. Tool-level — the CLI refuses to print its own token
5. Output scrubbing — the gateway removes the minted value from results

**Evidence:** Use case 2 — asserted from both sides simultaneously.

### R2c — "attributable to agent, tenant and triggering user"

Every audit record carries all three, plus the definition digest. The triggering
user propagates from the API key at run creation, through the run row, into
every audit record.

**"The agent did it" is not an acceptable answer to "who did this".** A human
authorised the agent's existence and a human triggered the run; both must be
recoverable months later.

**Evidence:**

```
11:00:49  run_01a0c8c68  report-writer  exec.bash   {"script":"echo '--- files ---'; ls -la /work…
```
→ agent, tenant (chain), triggering user, tool, arguments, time.

---

## R3 — Kubernetes-native

> *"Scheduling, isolation, scaling and failure handling use Kubernetes
> primitives or justify why not"*

The "or justify why not" is the real instruction. Honest accounting:

| Concern | Kubernetes primitive? | Rationale |
|---|---|---|
| Sandbox isolation | **Yes** — Pod + RuntimeClass `gvisor` + securityContext | Exactly what it is for |
| Sandbox placement | **Yes** — nodeSelector + taints/tolerations | Untrusted workloads get their own node pool |
| Network isolation | **Yes** — default-deny NetworkPolicy | CNI enforces it regardless of our code |
| Resource limits | **Yes** — requests/limits + ResourceQuota + LimitRange | Cluster-level ceiling above our own accounting |
| Worker scaling | **Yes** — Deployment + HPA | Stateless, so trivially horizontal |
| Disruption safety | **Yes** — PodDisruptionBudget | Keeps a gateway replica during drains |
| Least privilege | **Yes** — per-component ServiceAccount + narrow Role | The gateway can create pods and *cannot* exec into them |
| Config/secrets | **Yes** — ConfigMap + Secret (→ External Secrets in prod) | |
| **Agent run scheduling** | **No — application-level** | See below |
| **Agent run state** | **No — Postgres** | See below |

### Why agent runs are not CRDs

etcd is not a database for this workload:

| | |
|---|---|
| Per-object limit | 1.5 MB — a long agent's event history exceeds it |
| Recommended total DB size | a few GB |
| Update frequency | status writes at agent-step rate generate cluster-wide watch traffic |
| Query capability | no SQL; "every command agent X ran last Tuesday" is not expressible |
| Fair scheduling | no per-tenant weighted fairness |

**The middle ground I did not build** and would build in production: a *thin*
`AgentRun` CRD carrying identity and terminal status only — no event history —
reconciled *from* Postgres. That gets `kubectl get agentruns` and GitOps-declared
agents without putting the hot path in etcd. Discussed in
[DEEP_DIVE D9](../../DEEP_DIVE.md#d9--kubernetes-integration-depth).

---

## R4 — Sandboxed execution

> *"Agents run arbitrary code, shell commands and CLIs in an environment where
> escape, lateral movement and data exfiltration are contained"*

Three distinct threats, three distinct controls:

| Threat | Meaning | Control |
|---|---|---|
| **Escape** | Payload gains host code execution | gVisor + seccomp denylist + empty capability bounding set + `no_new_privs` + `pivot_root` |
| **Lateral movement** | Payload reaches another tenant, or the control plane | Separate namespaces, tainted node pool, no ServiceAccount token, default-deny NetworkPolicy, per-tenant host uid ranges |
| **Exfiltration** | Payload sends data out | **Empty network namespace** — not a firewall rule, no interface and no route |

Full mechanism: [Linux isolation](../01-concepts/01-linux-isolation.md).
Per-control evidence: [Safety proofs](../04-evidence/02-safety-proofs.md).

---

## R5 — Durability

> *"An agent mid-task survives a pod restart, a node drain and a deploy"*

Three different failure shapes:

| Event | Grace? | What the system does |
|---|---|---|
| **Pod restart** (OOM, crash) | None — SIGKILL | Lease lapses; reaper requeues within one TTL; another worker replays |
| **Node drain** | SIGTERM, then SIGKILL | Worker yields the run back to `QUEUED` in one fenced write; if killed first, identical to above |
| **Deploy** | Rolling, both generations briefly alive | Fencing makes the old generation harmless — its commits are rejected |

The requirement implies more than "the run resumes". It implies **the run resumes
without repeating side effects**, which is a much harder property. See
[Durable execution](../01-concepts/02-durable-execution.md#6-idempotency-making-replay-safe).

**Evidence:**

| Test | Result |
|---|---|
| Worker SIGKILLed mid-tool-call | Recovered; **4 tool requests, 3 side effects** — one replayed from the journal |
| Hard generation swap, 12 runs in flight, no drain | All 12 succeeded, zero duplicated side effects |
| Partitioned worker wakes up and commits | Rejected with `ErrLeaseLost`; stale history never reached the log |

---

## R6 — Observability

> *"An operator can answer 'what is agent X doing right now and what has it cost
> so far'"*

Two questions with different shapes:

**"What is it doing right now"** — needs the current state *and* how it got
there. Answered from the event log itself, so the operator sees literally what
the agent saw. A separate telemetry pipeline can drift from reality; this one
cannot, because the same events rebuild the model's context.

**"What has it cost"** — needs per-run attribution across three resources:

| Resource | Tracked as | Where |
|---|---|---|
| LLM tokens | `usage.input_tokens`, `usage.output_tokens` | per model response event |
| Money | `usage.cost_usd` from a price table | per model response event |
| Compute | `usage.sandbox_ms` | per sandbox result |
| Tool calls | `usage.tool_calls` | per gateway call |

All four are also exposed as Prometheus series by tenant. Budget meters in the
console show *usage against the limit*, so an operator sees a run approaching its
budget rather than learning about it from the failure.

Full detail: [Observability](../03-operations/03-observability.md).

---

## R7 — Decisions the brief requires us to state

| Decision | Choice | Argued in |
|---|---|---|
| Language and runtime | Go | [DEEP_DIVE D10](../../DEEP_DIVE.md#d10--language-and-runtime) |
| Message bus / workflow engine | Neither — Postgres `SKIP LOCKED` | [D4](../../DEEP_DIVE.md#d4--durable-execution-substrate) |
| Sandbox technology | gVisor (prod) / raw namespaces (PoC) | [D2](../../DEEP_DIVE.md#d2--sandbox-isolation-technology) |
| State store | Postgres + object storage for artifacts | [D4](../../DEEP_DIVE.md#d4--durable-execution-substrate) |
| Agent definition & versioning | Content-addressed immutable specs | [D11](../../DEEP_DIVE.md#d11--agent-definition-and-versioning) |

---

**Next:** [Threat model](04-threat-model.md) — who the adversary is and what
they can actually do.
