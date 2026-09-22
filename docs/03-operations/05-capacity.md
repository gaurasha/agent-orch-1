# Capacity planning

> **Prerequisite:** [Runbook](04-runbook.md)
> **Read next:** [Benchmarks](../04-evidence/01-benchmarks.md)

The arithmetic, with the measured inputs stated so you can redo it with your own
numbers.

---

## 1. Measured inputs

| Quantity | Value | Source |
|---|---|---|
| Sandbox cold start (namespace driver) | **8.4 ms** mean, 19.4 ms worst | `TestSandbox_ColdStartIsMeasured`, 10 runs |
| Orchestration CPU per step | **~1–5 ms** | `agentorch_step_duration_seconds` |
| 500 agents / 16 workers | **p99 1.0 s**, 0 failed | `TestLoad_500ConcurrentAgents` |
| Worker scaling, 2 → 24 | **52.9–55.8×** throughput | `TestLoad_ScalesWithWorkers` |
| Event size | ~1–2 KB | `events.payload` |
| Audit records per tool call | **2** (pre + post) | by design |

Everything below derives from these. Substitute your own and the shape holds.

---

## 2. The central insight

> **"1000 concurrent agents" does not mean 1000 of anything.**

"Concurrent" means *in flight*, not *executing*. An agent between start and
finish is, almost all the time, waiting — for the model, for a tool, for a
human.

If the average agent takes one turn every ~10 s:

```
1000 agents ÷ 10 s = 100 turns/second
100 turns/s × 3 ms of our CPU = 0.3 cores of actual orchestration work
```

**The orchestrator is not the scaling problem.** Treating it as one leads
directly to pod-per-agent and a ten-node bill for doing nothing.

---

## 3. Worker sizing

| Concurrent agents | Turns/s | Our CPU | Workers (goroutines) | Pods |
|---|---|---|---|---|
| 100 | 10 | 0.03 cores | 8 | 1–2 |
| **1,000** | **100** | **0.3 cores** | **24–32** | **3–4** |
| 10,000 | 1,000 | 3 cores | 200+ | 25+ |

Goroutines, not cores, is the right unit: each is blocked on I/O almost always.
The measured 52.9× improvement from 2 → 24 workers confirms nothing in the shared
path serialises.

**Headroom:** the table assumes steady state. For the 09:00 burst, size for peak
turns/s and let the HPA scale down afterwards. Scale up fast, down slowly.

---

## 4. Sandbox fleet — the real cost

This dominates everything else.

| Input | Assumption |
|---|---|
| Fraction of agents executing code at any instant | 20% |
| Sandbox request | 0.5 CPU / 512 MiB (Guaranteed QoS) |

```
1000 agents × 20%              = 200 concurrent sandboxes
200 × 0.5 CPU                  = 100 cores
200 × 512 MiB                  = 100 GiB
÷ (8 vCPU, 32 GiB) nodes       ≈ 13 nodes   (CPU-bound)
```

### Sensitivity — this is the number to get right

| Exec fraction | Sandboxes | Cores | Nodes |
|---|---|---|---|
| 5% | 50 | 25 | 4 |
| 10% | 100 | 50 | 7 |
| **20%** | **200** | **100** | **13** |
| 50% | 500 | 250 | 32 |

**Measure your exec fraction before sizing anything.** A 4× error here is a 4×
error in the largest line of the infrastructure bill, and it is the one
assumption most likely to be wrong.

### Why cold start decided the lifecycle policy

At **8.4 ms**, per-call ephemeral sandboxes cost:

```
167 tool calls/s × 25% exec × 8.4 ms = 0.35 cores of setup overhead
```

Negligible — so per-call ephemeral is affordable, and it gives the strongest
isolation because nothing survives between calls.

Under gVisor on Kubernetes, cold start is ~1–3 s:

```
42 exec calls/s × 2 s = 84 concurrent sandboxes JUST STARTING UP
```

That is why the Kubernetes design keeps **warm per-run sandboxes** for
interactive agents and falls back to per-call for batch. The measurement drove
the architecture, not the reverse.

---

## 5. Database

### Storage

| Table | Per 1000 runs | At 10k tool calls/min |
|---|---|---|
| `runs` | ~2 MB | bounded by run rate |
| **`events`** | **~20 MB** | **~29 GB/day** |
| `tool_calls` | ~2 MB | ~3 GB/day |
| `audit_log` | ~10 MB | ~14 GB/day |

At the stated peak (10k tool calls/min), roughly **46 GB/day**, of which `events`
is about two thirds.

**At 10× (100k tool calls/min): ~288 GB/day of events alone.** This is the first
thing that breaks.

### The plan

1. `PARTITION BY RANGE (created_at)` on `events`, monthly
2. Tier partitions older than the operational window to object storage as Parquet
3. Keep `runs` in Postgres indefinitely — it is small and it is what the console
   reads
4. Retain `audit_log` per the compliance requirement, which is usually **longer**
   than the operational need for `events`

Not implemented. Listed in
[what I cut](../../DESIGN.md#8-what-i-cut-and-why-it-was-right-to-cut-it).

### IOPS

| Operation | Rate at peak | Notes |
|---|---|---|
| Lease acquire | ~100/s | indexed; `SKIP LOCKED` does not block |
| Commit (events + run update) | ~100/s | one transaction each |
| Tool-call journal | ~167/s | insert + update |
| Audit append | ~334/s | 2 per call, **serialised per tenant** |
| Console reads | low | consider a read replica |

The audit chain is the most write-serialised path. At 10× with a **skewed**
tenant distribution — one tenant generating most of the traffic — a single
chain becomes the bottleneck and needs per-tenant-per-shard chains.

### Connections

```
(2 controlplane + 2 gateway + 3 agentd) × 25 = 175 connections
```

Postgres defaults to `max_connections = 100`. **Add PgBouncer** in transaction
mode before you scale the platform, or raise `max_connections` and accept the
per-connection memory cost.

---

## 6. LLM quota — the binding constraint

This is the one you cannot engineer around.

```
1000 agents × 1 turn/10 s          = 100 model calls/s = 6,000/min
6,000 calls/min × ~5,000 tokens    = 30,000,000 tokens/min
```

Compare that to a typical enterprise tier of a few hundred thousand TPM and the
conclusion is immediate:

> **You will be quota-bound long before you are compute-bound.**

Which means the design's job is not to go faster — it is to make the rationing
**fair** and **visible**. That is exactly what
[the fairness layer](../01-concepts/06-fairness.md) does, and why it is a
first-class component rather than a rate-limiting middleware.

### Levers, in order of effect

| Lever | Effect |
|---|---|
| **Negotiate more quota** | Direct, and usually the answer |
| Smaller models for cheap steps | 3–10× more calls for the same tokens |
| Prompt caching | Large win — context is re-sent every turn |
| **Cap tool result sizes** | Already done (256 KiB sandbox, 64 KiB gateway) — a large result costs on *every* subsequent turn |
| Shorter conversations | Context growth is quadratic |
| Multi-provider key pools | Sum of several contracts |

### Why the result cap matters more than it looks

A tool returning 100 KB adds ~25k tokens to **every subsequent turn**. Over a
20-step run that is 500k tokens — roughly $1.50 at Sonnet-class input pricing —
from a single unbounded `cat`.

---

## 7. Worked sizing: 1000 agents

| Component | Sizing | Why |
|---|---|---|
| `controlplane` | 2 pods, 0.5 CPU / 512 MiB | Request-driven; 2 for availability |
| `toolgateway` | 4 pods, 1 CPU / 1 GiB | 167 calls/s, mostly I/O; PDB |
| `agentd` | 4 pods × 8 workers, 1 CPU / 1 GiB | 100 turns/s ≈ 0.3 cores + headroom |
| Postgres | 8 vCPU / 32 GiB, 500 GB SSD, PgBouncer | ~700 IOPS, 46 GB/day |
| **Sandbox pool** | **13 × (8 vCPU, 32 GiB), autoscaled** | **200 concurrent sandboxes** |
| Platform pool | 3 × (4 vCPU, 16 GiB) | Everything above except sandboxes |

Sandboxes are ~80% of the compute. Everything else is rounding.

---

## 8. Scaling to 10×

| Component | Breaks? | Fix |
|---|---|---|
| Workers, gateways | No | More replicas |
| Lease query | Maybe | Shard by `hash(tenant_id) % N`, one dispatcher per shard |
| **`events` table** | **Yes, first** | Partition + tier to object storage |
| Audit chain (skewed tenant) | Yes | Per-tenant-per-shard chains |
| Connections | Yes | PgBouncer becomes mandatory |
| Sandbox fleet | Cost only | Karpenter; Firecracker snapshot-restore (~10 ms) for density |
| **LLM quota** | **Cannot be fixed technically** | Contract, multi-provider, caching, smaller models |
| Fairness limiter | **Already broken above 1 replica** | Shared counter in Redis — see below |

### The limiter caveat, stated plainly

The limiter is **per-process**. With 3 control-plane replicas each configured for
400k TPM, the fleet can emit **1.2M TPM**. The algorithm does not change — only
where the buckets live — but the current code over-admits in any multi-replica
deployment. Fix: a shared counter in Redis with a local lease/batch so there is
not a round trip per call.

---

## 9. Cost model

Per 1000 agent runs, each ~5 steps and ~3 tool calls:

| Line | Quantity | Unit | Total |
|---|---|---|---|
| LLM tokens | 5000 calls × ~5k tok | ~$3/Mtok in | **~$75** |
| Sandbox compute | 3000 × 8.4 ms + payload | negligible | **~$0.10** |
| Storage | ~35 MB | ~$0.02/GB-mo | **~$0.001** |
| Orchestration CPU | 5000 × 3 ms | — | **~$0.01** |

> **The LLM is ~99.8% of the marginal cost.**

Which is the entire justification for where this design spends its complexity:
budgets, fair-share quota, reserve-then-settle accounting, per-tenant cost
attribution, and capped tool results. Optimising the orchestrator would be
optimising the rounding error.

---

**Next:** [Benchmarks](../04-evidence/01-benchmarks.md).
