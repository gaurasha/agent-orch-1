# Durable execution from first principles

> **Prerequisite:** [What an agent is](../00-problem/01-what-is-an-agent.md)
> **Read next:** [Multi-tenancy](03-multi-tenancy.md)
> **Code:** [`internal/runtime/`](../../backend/internal/runtime/) · [`internal/store/`](../../backend/internal/store/)

The requirement is one sentence: *"an agent mid-task survives a pod restart, a
node drain and a deploy."* Satisfying it honestly turns out to require solving
four separate problems, each of which has a well-known wrong answer.

---

## 1. The problem, stated precisely

An agent run is a sequence of steps, each of which may cause a **side effect in
the outside world** — a file written, an HTTP POST sent, a pull request opened.
The process executing those steps can die at **any instruction**, including:

- after the side effect happened but before we recorded that it happened
- after we recorded it but before we told the model
- while we were partitioned from the database and someone else took over

"Survives" therefore has to mean more than "the run resumes". It must mean:

> **The run resumes, and no side effect happens twice.**

That is the hard part, and it is the part most systems get wrong by not noticing
it is a requirement.

---

## 2. Where state can live, and why only one answer works

| Option | Survives process death? | Survives node death? | Cost when idle |
|---|---|---|---|
| Process memory | ✗ | ✗ | full process |
| Local disk | ✓ | ✗ | full pod |
| A pod with a PVC | ✓ | ✓ (rescheduled) | **full pod, indefinitely** |
| **A row + an event log in a shared store** | ✓ | ✓ | **a few KB** |

The third row is worth dwelling on because it is the intuitive answer. A pod
with a persistent volume *does* survive. But:

1. The pod is evicted on node drain and must be rescheduled — so a controller
   must recreate it, which means the desired state is externalised anyway.
2. It holds ~100 MiB while parked on a human for two days.
3. `kubelet`'s default ceiling of 110 pods/node makes 1000 agents ≥10 nodes of
   pure overhead.

Once the state is durable **and** something external recreates the work, the pod
has stopped providing durability. It is an expensive way to hold a place in a
queue. Full argument: [DEEP_DIVE D1](../../DEEP_DIVE.md#d1--agent-runtime-model).

---

## 3. Event sourcing: the log *is* the agent

Rather than storing "the current conversation", store **every fact that ever
happened**, append-only, and derive the conversation by folding over it.

```
events(run_id, seq, type, payload, created_at)   -- PRIMARY KEY (run_id, seq)
```

| seq | type | payload |
|---|---|---|
| 0 | `RUN_CREATED` | `{"text": "Write the Q3 report."}` |
| 1 | `MODEL_RESPONSE` | `{"text":"I'll draft…","model":"…","cost_usd":0.042}` |
| 2 | `TOOL_CALL` | `{"tool":"fs.write","args":{…},"idem_key":"run_x:0:0"}` |
| 3 | `TOOL_RESULT` | `{"result":"Wrote 120 bytes…","duration_ms":7}` |
| 4 | `MODEL_RESPONSE` | `{"text":"Now I'll convert it.",…}` |
| … | | |

### Why this shape, and not a `conversation` column

| Property | Event log | Mutable column |
|---|---|---|
| Recovery | replay | hope the last write landed |
| Audit | **free** — the log *is* the audit trail | separate, can drift |
| Debugging | the exact sequence, with timings | final state only |
| Concurrency | append-only + a sequence number = optimistic concurrency for free | read-modify-write races |
| Cost | grows unboundedly ⚠ | bounded |

The last row is the honest cost and is the first thing that breaks at 10× scale.
See [Capacity](../03-operations/05-capacity.md).

### Reconstruction is a pure function

[`runtime.Rebuild(events) []llm.Message`](../../backend/internal/runtime/context.go)
has no dependencies, no clock, no randomness. Two workers replaying the same log
produce byte-identical context. That is what makes any worker interchangeable
with any other.

**It is also where we decide what the model is allowed to know.** Deliberately
*not* carried into context:

| Excluded | Why |
|---|---|
| `NOTE` events (quota waits, provider retries, lease churn) | Operational noise costs money on every subsequent turn, and gives a prompt-injected agent material to reason about the platform with |
| Worker IDs, idempotency keys, audit metadata | Same |
| Anything a denial revealed beyond the human-readable reason | Minimises what an attacker learns from probing |

The operator sees all of it in the UI; the model sees a curated subset. The UI
and the model are built from the *same* log, so the operator view cannot drift
from what the agent actually experienced — but they are not the same projection.

---

## 4. Leases: at-most-one worker, without a failure detector

N stateless workers poll one table. Two must never run the same run at once.

### Why not the obvious answers

| Approach | Why not |
|---|---|
| A distributed lock service (etcd, ZooKeeper, Consul) | Another system to operate, and it does not solve the real problem — see fencing below |
| Leader election, one scheduler assigns work | The leader is a bottleneck and a SPOF; you still need to detect worker death |
| A message queue with visibility timeouts | Closer, and SQS-shaped. But the ack and the state change are in different systems, so they can disagree |

### What this system does

A lease is **two columns on the run row**:

```sql
lease_owner      TEXT,
lease_expires_at TIMESTAMPTZ
```

Claiming one is a single statement:

```sql
SELECT id FROM runs
WHERE state='QUEUED'
  AND (wake_at IS NULL OR wake_at <= now())
  AND (lease_expires_at IS NULL OR lease_expires_at < now())
  AND ($1::text[] IS NULL OR cardinality($1::text[])=0 OR tenant_id = ANY($1))
ORDER BY CASE priority WHEN 'interactive' THEN 2 WHEN 'normal' THEN 1 ELSE 0 END DESC,
         created_at ASC
FOR UPDATE SKIP LOCKED
LIMIT 1;
```

[`FOR UPDATE SKIP LOCKED`](https://www.postgresql.org/docs/current/sql-select.html#SQL-FOR-UPDATE-SHARE)
is the whole trick: 16 workers can run this concurrently and each gets a
*different* row. No blocking, no broker, no coordination.

> **A real bug this query had.** `pq.Array(nil)` produces SQL `NULL`, and
> `cardinality(NULL)=0` evaluates to `NULL` — not `true`. The predicate went
> `NULL`, so **no row ever matched** when the tenant filter was empty. The
> in-memory store was unaffected and green. Caught only because one conformance
> suite runs against both. Hence the explicit `IS NULL` guard above.

### The failure detector is the absence of a renewal

There isn't one. A worker heartbeats every `LeaseTTL/3`; if it dies, it simply
stops. After at most one TTL the lease is stale and a reaper requeues it:

```sql
UPDATE runs SET state='QUEUED', lease_owner=NULL, lease_expires_at=NULL
WHERE state='RUNNING' AND lease_expires_at < now()
```

No membership protocol, no gossip, no split-brain. The tuning is a
straightforward trade:

| `LeaseTTL` | Recovery latency | Risk |
|---|---|---|
| Short (5 s) | fast | a merely-slow worker gets fenced out mid-step |
| Long (5 min) | slow | a run is stuck for 5 minutes after a node dies |
| **30 s (default)** | ≤30 s | heartbeat at 10 s gives 3 chances before expiry |

---

## 5. Fencing: the part that is actually subtle

A lease alone is **not** sufficient, and this is the single most common
distributed-systems mistake in this class of system.

### The failure

```
t=0    w1 acquires a 30s lease
t=1    w1 begins a step
t=2    w1 experiences a 40-second GC pause / network partition
t=32   lease expires; reaper requeues
t=33   w2 acquires the lease, replays, does work, commits
t=42   w1 wakes up. It still "believes" it holds the lease.
       It writes its stale result.        ← CORRUPTION
```

`w1` is not malicious and not buggy. It was paused. **No timeout can prevent
this**, because `w1` cannot know it was paused.

This is exactly the scenario in Martin Kleppmann's
[*How to do distributed locking*](https://martin.kleppmann.com/2016/02/08/how-to-do-distributed-locking.html),
and the answer is a **fencing token**: the resource itself rejects a stale
writer.

### The implementation

`store.Commit` checks three conditions inside the transaction, against a locked
row:

```go
// 1. the run still exists
// 2. `worker` still owns an UNEXPIRED lease
// 3. next_seq is exactly what the worker thinks it is
if owner.String != worker || !exp.Valid || exp.Time.Before(time.Now()) {
    return fmt.Errorf("commit rejected for worker %s (owner=%q): %w",
        worker, owner.String, types.ErrLeaseLost)
}
if seq != expectedNextSeq {
    return fmt.Errorf("store: seq mismatch have=%d want=%d: %w", seq, expectedNextSeq, types.ErrConflict)
}
```

Condition 2 is the fence. Condition 3 is optimistic concurrency on the log
itself, catching any interleaving the lease check alone would miss.

**Verified:**

```
partitioned worker correctly fenced out:
  commit rejected for worker slow (owner=""): lease lost
```

and the test then asserts the stale text never appears in the log.

### The corollary: never write without the fence

A worker that stops mid-step must not leave the run in a state nobody will pick
up. Originally the cleanup path just called `ReleaseLease`, which nulls the
lease but leaves `state='RUNNING'` — and:

- the dispatcher selects `state='QUEUED'` → never sees it
- the reaper selects `lease_expires_at < now()` → it is `NULL` → never sees it

**The run was stuck forever.** The fix is a fenced `YieldRun` that returns the
run to `QUEUED` in one statement, plus a belt-and-braces reaper clause for
ownerless `RUNNING` rows:

```sql
UPDATE runs SET state='QUEUED', lease_owner=NULL, lease_expires_at=NULL, status_reason=$3
WHERE id=$1 AND lease_owner=$2 AND state='RUNNING'
```

The `lease_owner=$2` predicate is the fence: if someone else already took over,
this matches nothing and leaves them alone.

---

## 6. Idempotency: making replay safe

Replay is the recovery mechanism, and replay **re-reaches side effects**.

### Why the key must be deterministic

A randomly generated idempotency key is useless here: the replaying worker would
generate a *different* one and the side effect would repeat. The key must be a
function of **position in the log**:

```go
idemKey := fmt.Sprintf("%s:%d:%d", run.ID, run.Step, i)
//                      run      step  call index
```

A worker replaying to the same position computes the same key, necessarily.

### The journal

```sql
CREATE TABLE tool_calls (
    idem_key    TEXT PRIMARY KEY,
    state       TEXT NOT NULL,          -- IN_FLIGHT | DONE | FAILED | DENIED
    result      TEXT NOT NULL DEFAULT '',
    is_error    BOOLEAN NOT NULL DEFAULT false,
    started_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at TIMESTAMPTZ
);
```

Reserving a key is atomic under concurrency:

```sql
INSERT INTO tool_calls (idem_key, …, state) VALUES ($1, …, 'IN_FLIGHT')
ON CONFLICT (idem_key) DO NOTHING;
-- rows affected == 1  ⟹  you own the side effect
-- rows affected == 0  ⟹  re-SELECT and use the recorded outcome
```

Exactly one caller is told to execute. **Verified** with 12 concurrent racers:

```
expected exactly 1 fresh reservation, got 1
```

### The four cases

| Case | Journal state | What replay gets |
|---|---|---|
| Never started | absent | fresh → execute |
| Completed | `DONE` + result | the recorded result; **no re-execution** |
| Failed cleanly | `FAILED` + error | the recorded error |
| **Gateway died mid-call** | **`IN_FLIGHT`** | `409 Conflict` |

### The honest fourth case

If the gateway died between "told GitHub to open a PR" and "wrote down that it
did", **we do not know whether it happened**. There is no way to know.

The design does the only honest thing:

1. The entry stays `IN_FLIGHT`.
2. A replaying worker gets `409` and waits.
3. A reaper eventually fails it with a message that says, verbatim:

```
tool call abandoned: the gateway did not report an outcome.
This call MAY OR MAY NOT have taken effect.
```

4. Tools whose providers offer no idempotency key are tagged `UnsafeRetry`, and
   the agent is told explicitly.

**Presenting an ambiguous failure as a clean one is how you get a duplicate
payment.** Telling the model the truth is the design decision here.

### Guarantee table

| Scenario | Guarantee |
|---|---|
| Tool supports idempotency keys (Stripe, most modern APIs) | **Effectively once**, end to end |
| Tool is naturally idempotent (GET, PUT to a known path) | **Effectively once** |
| Neither, gateway completed before dying | **Effectively once** — journal has the result |
| Neither, gateway died *during* the call | **Unknown, and reported as unknown** |

There is no exactly-once across a network boundary. Claiming otherwise is the
most common distributed-systems error; this system does not claim it.

---

## 7. Budgets: enforced, not reported

A poison agent loops forever. Nothing in the model layer stops it — "try again"
is always a locally plausible next action.

```go
type Budget struct {
    MaxSteps       int
    MaxToolCalls   int
    MaxTokens      int64
    MaxCostUSD     float64
    MaxWallSeconds int
}
```

Checked at **two** points, for two different reasons:

| Where | Why |
|---|---|
| Worker, before the model call | Stops *before* paying for another turn — cheaper |
| Gateway, before every tool call | The choke point a straggler worker cannot route around |

Plus three softer layers:

- **Remaining budget is injected into the system prompt.** Costs a handful of
  tokens and measurably breaks the loop for well-behaved models. Guidance for the
  cooperative case, enforcement for the rest.
- **`MaxConcurrentRuns` per tenant** stops one tenant's poison agents
  monopolising the worker pool.
- **The fairness limiter** drains that tenant's bucket, so its other runs simply
  are not scheduled.

> **Defaults are finite on purpose.** `DefaultBudget()` is 25 steps / 50 tool
> calls / 200k tokens / $5 / 1 hour. Defaulting to "unlimited" would make an
> omitted field a production incident.

**Verified:** stopped at exactly 5/5 tool calls, $0.19 of a $0.50 cap.

---

## 8. Putting it together — the full failure/recovery matrix

| Failure | Detected by | Recovery | Side-effect safety |
|---|---|---|---|
| Worker SIGKILL | lease expiry | reaper → requeue → replay | journal returns recorded result |
| Worker SIGTERM (drain) | graceful | `YieldRun` → `QUEUED` immediately | nothing in flight |
| Worker partitioned | lease expiry | another worker takes over | **fencing** rejects the straggler |
| Gateway dies mid-call | `IN_FLIGHT` + reaper | run is told the outcome is unknown | **honest ambiguity** |
| Database unavailable | connection errors | workers **stop leasing** (fail closed) | no side effect we cannot journal |
| Rolling deploy | n/a | both generations alive; fencing makes the old one harmless | verified with 12 in-flight runs |
| Node drain | eviction | same as SIGTERM, or SIGKILL path | same |

### Measured

| Test | Result |
|---|---|
| Worker SIGKILLed mid-tool-call | Recovered → `SUCCEEDED`. **4 tool requests, 3 side effects** — one replayed |
| Hard generation swap, 12 runs in flight, **no drain** | 12/12 succeeded. 36 requests, 36 side effects, **zero duplicates** |
| Partitioned worker commits late | Rejected `ErrLeaseLost`; stale text absent from the log |

The first row is the whole thesis in one line: **4 requests, 3 effects.**

---

## 9. What this costs

Being honest about the price of the choice:

| Cost | Detail |
|---|---|
| **Idempotency must be engineered** | Deterministic keys, a journal, pre/post audit, a reaper. This is the most subtle machinery in the system and the most likely place for a latent bug. |
| **State is a fold, not a value** | "What is this agent doing" is a query over events, not `kubectl get`. |
| **The log grows unboundedly** | 100k tool calls/min ≈ 288 GB/day. Needs partitioning and tiering. |
| **Replay cost grows with run length** | A 200-step run replays 200 steps of history every step. Mitigation (not built): periodic snapshot events that replay can start from. |

The last one is a genuine scaling limit not mentioned elsewhere: replay is O(n)
per step, so a run is O(n²) overall. At today's step counts (≤50) this is
microseconds. At 1000-step agents it would need snapshotting.

---

## 10. Prior art

This is the standard **durable execution** shape, reached independently by
several teams:

| System | Approach |
|---|---|
| [Temporal](https://docs.temporal.io/workflows) | Event-sourced workflow histories, deterministic replay by stateless workers |
| [AWS Step Functions](https://docs.aws.amazon.com/step-functions/latest/dg/welcome.html) | Managed state machine with durable history |
| [Azure Durable Functions](https://learn.microsoft.com/en-us/azure/azure-functions/durable/durable-functions-overview) | Replay-based orchestrations |
| [Restate](https://docs.restate.dev/concepts/durable_building_blocks) | Durable RPC with journaled side effects |
| [DBOS](https://docs.dbos.dev/) | Same bet as here — the log lives in Postgres |
| [River](https://riverqueue.com/) / [Oban](https://hexdocs.pm/oban/Oban.html) / [Que](https://github.com/que-rb/que) | `SKIP LOCKED` job queues in Postgres |

Underlying pattern: [event sourcing](https://martinfowler.com/eaaDev/EventSourcing.html).

**Why not just use Temporal?** Argued in
[DEEP_DIVE D4](../../DEEP_DIVE.md#d4--durable-execution-substrate). Short version:
this system needs the event log, run state, idempotency journal and audit chain
to commit in **one transaction**, plus per-tenant weighted fair scheduling that
task queues do not provide. If the team already operated Temporal, that trade
would flip.

---

**Next:** [Multi-tenancy](03-multi-tenancy.md) — how "tenant A cannot see tenant
B" becomes something the code cannot express.
