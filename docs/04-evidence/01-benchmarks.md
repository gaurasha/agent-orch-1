# Benchmarks

> **Prerequisite:** [Capacity planning](../03-operations/05-capacity.md)
> **Read next:** [Safety proofs](02-safety-proofs.md)

Every measurement in this repository, the command that produces it, and what it
does and does **not** support.

---

## Environment

| | |
|---|---|
| Host | Linux 6.18, x86-64, **4 vCPU**, 15 GiB RAM |
| Go | 1.24.7 |
| Postgres | 16.13 |
| Sandbox driver | `namespace` (shared kernel — **not** gVisor) |
| LLM | scripted provider, 2 ms latency |

**This is a 4-core VM, not a cluster.** The absolute numbers are therefore lower
bounds on what dedicated hardware would show, and — more importantly — they were
produced with the **orchestrator, the database and the load generator all
competing for the same 4 cores**. Read the *shapes* (does throughput scale? does
anything serialise?) rather than the absolute figures.

---

## Reproducing everything

```bash
createdb -O agentorch agentorch_test
export AGENTORCH_TEST_DSN="postgres://agentorch:agentorch@127.0.0.1:5432/agentorch_test?sslmode=disable"
cd backend && go test ./... -count=1 -v -timeout 20m
```

Current state: **57 test results, 57 passing, 0 failing.**

---

## 1. Sandbox cold start — the number that shaped the architecture

```
TestSandbox_ColdStartIsMeasured
COLD START over 10 runs: mean=8.67ms worst=15.983ms
```

Observed across runs: **mean 8.4–8.7 ms, worst 15–20 ms.**

What is being measured: the full path — create cgroups, `clone` with six
namespace flags, write config over a pipe, place the child in the cgroups,
release it, build the rootfs, `pivot_root`, drop privileges, install seccomp,
`execve`, run `exit 0`, reap.

### Why this number decided the sandbox lifecycle

| Cold start | Per-call ephemeral sandboxes? |
|---|---|
| **8.4 ms** (namespace) | **Yes.** 42 exec calls/s × 8.4 ms = 0.35 cores of overhead |
| ~200 ms (docker) | Marginal |
| ~1–3 s (gVisor pod) | **No** — 42/s × 2 s = 84 sandboxes permanently in startup |

This is the one place where a measurement drove the design rather than the
reverse. Had it measured 500 ms, warm-per-run would be the default and the
isolation story would be weaker. See
[sandbox lifecycle](../../DEEP_DIVE.md#d3--sandbox-lifecycle).

The test asserts a ceiling of 2 s, so a regression from milliseconds to seconds
fails the build rather than quietly changing the architecture.

---

## 2. Load: 500 concurrent agents

```
TestLoad_500ConcurrentAgents
{ "Agents": 500, "Workers": 16, "Completed": 500, "Failed": 0,
  "ToolCalls": 1000, "P50": 815ms, "P95": 1.568s, "P99": 1.641s }
```

Two observed runs on the same host:

| Run | p50 | p95 | p99 |
|---|---|---|---|
| A | 455 ms | 965 ms | **1.007 s** |
| B | 815 ms | 1.568 s | **1.641 s** |

**Run-to-run variance is ~60%**, which is what you expect from a 4-core VM also
running Postgres and the load generator. Quoting the better number alone would
be dishonest; the useful claim is *"p99 in the 1–2 second range for a 500-agent
burst on 16 workers"*.

### What is and is not being measured

| Measured | Not measured |
|---|---|
| Lease acquisition under contention | Real LLM latency (2 ms stub) |
| Event-log append and replay | Sandbox throughput (no-op tool caller) |
| Commit fencing and OCC | Network I/O to third parties |
| The fairness limiter's hot path | gVisor overhead |

The stubs are deliberate: the question is *"does the orchestrator get in the
way"*, not *"how fast is the model"*. Sandbox cost is measured separately in §1.

### The exactly-once check

`ToolCalls: 1000` for 500 agents × 2 calls each. The test asserts this
**exactly**, so both lost work and duplicated work fail.

---

## 3. Worker scaling — the load-bearing result

```
TestLoad_ScalesWithWorkers
2 workers : 102.1 runs/s (p99 1.938s)
24 workers: 5394.1 runs/s (p99 598ms)
throughput improved 52.9x with 12x the workers
```

Observed: **52.9× and 55.8×** across runs, for a 12× increase in workers.

### Why super-linear, and why that is not suspicious

With 2 workers the run is latency-bound: each worker does one step at a time and
most of that is waiting on the (stubbed) model. Adding workers removes queueing
delay *as well as* adding parallelism, so throughput rises faster than the worker
count until something saturates.

**The claim this supports is not "52× speedup".** It is:

> Nothing in the shared path — the lease query, the limiter, a mutex — is
> serialising.

If the `SKIP LOCKED` query or the limiter's lock were the bottleneck, throughput
would have flattened. It did not. That is the property the "stateless worker
pool" architecture depends on, and this is the test that would falsify it.

The assertion is deliberately weak (`large > small`) because the ratio is
machine-dependent; the *shape* is what matters.

---

## 4. Mixed-tenant burst

```
TestLoad_BurstDoesNotStarveTheOtherTenant
{ "Agents": 300, "Workers": 12, "Completed": 300, "Failed": 0,
  "P50": 241ms, "P95": 443ms, "P99": 469ms }
```

The brief's 09:00 scenario. 300 agents split across two tenants, all complete.

---

## 5. Fairness

### Noisy neighbour

```
grants over 2s: noisy(50 spinning goroutines)=60 quiet(1 polite ticker)=59
quiet tenant received 98% of the noisy tenant's throughput
while offering ~1/50th of the load
```

**50× the offered load produced 1.02× the throughput.** Under a single shared
bucket the ratio would be near zero.

Test parameters: 120k TPM provider budget, two tenants at weight 1, 100 tokens
per call, 2-second window. Entitlement per tenant ≈ 5000 burst / 100 + 2 s ×
1000 tok/s / 100 ≈ 70 calls. Both landed at ~60.

### Weights

```
refilled grants after 600ms: big(weight 2)=12  small(weight 1)=6
```

Exactly 2:1. A "weight" that did not produce proportional throughput would be
decoration.

### Interactive reserve

```
batch drained after 15 grants; interactive still admitted
```

Batch work drains to the 25% reserve and is refused; interactive work is still
granted from it.

### Reserve-then-settle

```
reserved 5000, used 200, refunded 4800
under-estimate charged; tenant now in deficit at 0 tokens
```

Both directions. The second line matters: after a 1000× under-estimate the
tenant is **refused** its next reservation, so it cannot under-report its way
past the quota.

### Guarantee conservation

```
4 equal tenants each guaranteed 15000 TPM of 60000
```

Adding a tenant reduces everyone else's guarantee, so the guarantees never sum to
more than the provider gives.

---

## 6. Durability

### Worker killed mid-tool-call

```
victim holds the lease on run_… at step 0; killing it now with no drain
recovered: 12 events, 4 tool requests, 3 actual side effects, final state SUCCEEDED
workers that contributed to this run: [replacement]
```

**4 requests, 3 side effects.** The fourth was recognised as a replay and served
from the journal. That one line is the exactly-once property.

The kill happens while the worker is *inside* a tool call — the worst moment —
using a blocking tool caller so the timing is deterministic rather than lucky.

### Rolling deploy with no drain

```
all 12 runs survived a hard worker replacement;
36 tool requests, 36 side effects (no duplicates)
```

Generation 1 killed abruptly mid-flight while generation 2 is already running.
12/12 completed, zero duplicates.

### Fencing

```
partitioned worker correctly fenced out:
  commit rejected for worker slow (owner=""): lease lost
```

The test then asserts the stale text never reached the event log.

---

## 7. Store conformance

**19 subtests × 2 implementations = 38 results, all passing.**

| Property | Test |
|---|---|
| Exactly one of 16 concurrent workers wins a lease | `TestLease_OnlyOneWorkerWins` |
| A fenced worker cannot write | `TestCommit_RejectedAfterLeaseLost` |
| Sequence mismatch is a conflict | `TestCommit_SeqMismatchIsConflict` |
| Interactive beats FIFO | `TestLease_PriorityBeatsFIFO` |
| Quota-ineligible tenants are not scheduled | `TestLease_RespectsEligibleTenants` |
| Exactly one of 12 racers owns a side effect | `TestToolCall_IdempotencyPreventsSecondSideEffect` |
| Abandoned calls are reported as **ambiguous** | `TestToolCall_StuckCallsAreReapedAsAmbiguous` |
| Forged audit records are detected | `TestAudit_ChainIsTamperEvident` |
| The log is densely ordered | `TestEvents_AppendOnlyOrdering` |
| Over-max limits clamp to the **maximum** | `TestListRuns_OverMaxLimitReturnsMaximumNotDefault` |

**Running every test against both stores is the point**, and it caught two
Postgres-only bugs that a separate fake-only suite would have shown as green.
See [Bugs found](03-bugs-found.md).

---

## 8. End-to-end

```
$ make demo
21 checks, 0 failed
```

Six scenarios against real Postgres with real namespace sandboxes. Exits non-zero
on any failure — it is an acceptance test, not a demonstration.

---

## 9. Manifests

```
$ make k8s-validate
base:  Summary: 30 resources found in 1 file - Valid: 30, Invalid: 0, Errors: 0
local: Summary: 30 resources found in 1 file - Valid: 30, Invalid: 0, Errors: 0
```

`kubeconform` strict mode against real Kubernetes 1.31 schemas — unknown fields
are errors, not warnings.

---

## 10. What these numbers do **not** support

Stating this matters as much as the numbers:

| Claim I am **not** making | Why |
|---|---|
| "10,000 tool calls/min in production" | Not measured. Tool execution was stubbed in the load test |
| "p99 1 s under real load" | The LLM was a 2 ms stub; real latency is 2–20 s |
| "gVisor cold start is 8.4 ms" | That is the **namespace** driver. gVisor is ~1–3 s |
| "This scales to 10,000 agents" | Extrapolated from the scaling shape, not measured |
| "The fairness limiter is correct in production" | It is **per-process**; multi-replica needs a shared counter |
| "500 agents is the limit" | It is what was tested on a 4-core VM, not a ceiling |

---

## 11. What I would measure next

1. **Sandbox throughput under gVisor** — the real cold start and the syscall
   overhead on a document-conversion workload, which is I/O heavy and is exactly
   where gVisor is worst.
2. **End-to-end with a real model** — p99 including provider latency and
   variance.
3. **Event-log growth against a realistic agent mix** — to size partitioning.
4. **The audit chain under a skewed tenant distribution** — where per-tenant
   serialisation becomes the bottleneck.
5. **Failure injection at scale** — kill 30% of workers during a 500-agent burst
   and confirm zero duplicated side effects, not just at n=12.

---

**Next:** [Safety proofs](02-safety-proofs.md).
