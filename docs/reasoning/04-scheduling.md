# 04 · Scheduling — leases, fencing, replay and the per-lease step budget

> Decision: **workers poll for leases (30 s TTL, renewed every 10 s), replay
> the log from zero on every step, take at most four steps per lease, and
> yield explicitly; the reaper is the only failure detector.** No heartbeat
> service, no leader election, no per-run process, no checkpointed snapshots
> yet.
>
> Diagrams: [02-scheduling-runtime](../architecture/02-scheduling-runtime.md) · Related: [01-runtime-model](01-runtime-model.md), [03-durability-substrate](03-durability-substrate.md), [07-fairness](07-fairness.md)

## Problem

Given runs as rows and workers as interchangeable processes, four scheduling
questions remain, each with a physical constraint behind it:

| Question | Constraint |
|---|---|
| How does a worker find work? | hundreds of workers must not thunder on one table, and no tenant may be starved by another's burst |
| How does the system know a worker is dead? | any liveness signal is a lie under a pause; the only safe answer is a claim with an expiry judged by a third party's clock |
| How does a replacement worker know where to resume? | it must reconstruct the run's context from durable state only |
| How long may one worker hold a run? | too long and the pool cannot rebalance; too short and replay dominates |

## Options for each question

### Finding work

| Option | Case for | Case against |
|---|---|---|
| **poll a table with `SKIP LOCKED`** (chosen) | no broker; N pollers scale linearly; fairness and priority are `WHERE`/`ORDER BY`; 100 ms poll is invisible to a seconds-long step | ≤100 ms latency to first step; idle polling is a (tiny) constant load |
| push via LISTEN/NOTIFY | near-zero latency | NOTIFY is lossy under reconnects and does not carry a lease; you poll anyway as the fallback — designed as an *addition*, not a replacement |
| a broker with consumer groups | familiar | the two-system problem ([03](03-durability-substrate.md)); fairness across tenants is not a broker feature |
| a central dispatcher assigning runs to workers | global view enables smarter placement | a single point of failure and of state; workers must be addressable; the dispatcher's own liveness is a new problem |

### Detecting death

| Option | Case for | Case against |
|---|---|---|
| **lease with TTL, renewed by the holder, judged by the DB clock** (chosen) | one row, one predicate, one reaper `UPDATE`; no membership; works across pauses because the DB decides | recovery ≥ TTL; heartbeat traffic (1 `UPDATE` per run per 10 s) |
| a heartbeat/membership service (Consul, Serf, k8s Lease objects) | detects *process* death fast | detects the wrong thing: a process can be alive and paused; per-run ownership still needs the lease |
| leader election (one scheduler) | simple mental model | the leader is a serial point; per-run fencing is still needed when the leader changes |
| supervisor trees (OTP) | excellent inside one node | not durable across nodes without the same lease mechanism |

### Resuming

| Option | Case for | Case against |
|---|---|---|
| **replay the full event log** (chosen for the PoC) | trivially correct; the log is the truth; no snapshot format to version | O(events) read per step; the model context is rebuilt from scratch each step (which the model API needs anyway) |
| snapshot per step (LangGraph-style checkpoint) | O(1) read per step | snapshots grow with the conversation (they contain it); a snapshot format is a schema to migrate; a bug in snapshotting silently corrupts every run after it |
| replay + periodic checkpoint event (designed) | bounded replay; the checkpoint is just an event | must define what a checkpoint summarises; the model's context after summarisation is not what it was (a product decision) |
| Temporal-style deterministic replay of *code* | the engine replays your function against history | requires deterministic agent code; the model call must be an activity; you rent the mechanism |

### Holding a run

| Option | Case for | Case against |
|---|---|---|
| **N steps per lease, then explicit yield** (chosen, N = 4) | a long run cannot pin a worker; the pool rebalances; a yield is a commit, so it is fenced | 1 extra commit per N steps; a run pays a re-lease every N steps |
| hold until the run parks or finishes | fewest transactions | one 400-step run monopolises a worker; HPA cannot help; a slow tenant degrades everyone's tail |
| one step per lease | maximal rebalancing | 2× the lease transactions; replay per step already pays the big cost |

## Deep dive

### Why the lease is judged by the database's clock

A worker that is paused (GC, `SIGSTOP`, a VM live-migration, a network
partition) cannot know it was paused. When it resumes it believes the lease
it holds is valid. The only safe arbiter is a party that was *not* paused,
whose clock advanced, and who is consulted **in the same transaction as the
write**. That is `WHERE lease_owner = me AND lease_expires_at > now()` under
`FOR UPDATE`. This is the fencing-token argument; the token here is the
`(owner, expiry, next_seq)` triple rather than a monotonic integer, and it is
checked by the storage, not by the client — the property RedLock lacks.

### Why the heartbeat is a separate goroutine

A step can take longer than the TTL (a slow model, a 20 s tool call).
If renewal were done between steps, a slow step would lose its lease while
still doing legitimate work, and a replacement would start duplicating it
(safely, thanks to the journal — but wastefully). Renewal in its own goroutine
every TTL/3 keeps the lease alive during a long step; if renewal fails, the
goroutine cancels the worker's context so it stops making side effects it can
no longer journal.

### Why `MaxStepsPerLease` yields with a *commit*, not a release

Bug B6: the original code released the lease after four steps without
changing `state`. The row was `RUNNING` with `lease_owner = NULL`. The
dispatcher selects `QUEUED`; the reaper selects `RUNNING AND expired` — an
expired lease that is `NULL` is not `< now()`. The run was invisible to both,
forever. The fix is two-fold: the yield is a fenced `Commit` that sets
`state = QUEUED` and writes a `NOTE`, and the reaper gained a belt-and-braces
clause for `RUNNING AND lease_owner IS NULL AND updated_at < now() − 60 s`.
The general lesson: **every code path that stops working on a run must
leave the row in a state some other actor selects for.**

### Why replay is acceptable now and not forever

Per step, replay reads the run's events (one indexed range scan) and
rebuilds the messages. For a 20-step run that is 20 × ~20 events — trivial.
For a 400-step run it is 400 × 400 = 160 000 event reads over the run's life
— still fine for Postgres, but the *model context* also grows with the
conversation, and that is the real cost: tokens. The designed checkpoint
event summarises older context; the replay then starts from the last
checkpoint. This is the same trade Temporal makes with `continue-as-new` and
LangGraph makes with checkpoints; the difference is only that here the
checkpoint is an event in the same log.

### Backpressure as *not scheduling*

The fairness limiter's `Eligible()` runs once per tick and returns the
tenants that can afford a typical call. The lease query filters
`tenant_id = ANY($1)`. A tenant that is out of quota is therefore never
leased: no worker blocks, no run "waits inside a worker", and the pool serves
whoever has budget. When a leased run finds at `Reserve` time that quota is
gone (the estimate was larger than typical), it is re-queued with
`wake_at = now + 2 s`. Either way the worker is free in microseconds.

### Priority

`ORDER BY priority DESC, created_at` gives three lanes — interactive, normal,
batch — with FIFO inside a lane. There is no ageing: a batch run can be
starved indefinitely by sustained interactive load. That is a deliberate
policy for a platform where a human is waiting on interactive work; the
capacity doc's answer to "batch never runs" is to size the pool, not to age
priorities.

## State of the art

* **Temporal**: workers poll task queues (long-poll RPC), tasks carry a
  timeout, workflow tasks are fenced by task tokens, activities heartbeat with
  a timeout; the model is the same shape as here with the engine as the
  arbiter.
* **Kubernetes controllers**: `Lease` objects in `coordination.k8s.io` with
  `holderIdentity` and `renewTime`, judged by the API server — the same
  pattern for leader election.
* **Celery/Sidekiq/BullMQ**: visibility timeouts or heartbeats on a broker
  message; the well-known failure is a task re-delivered while the first
  execution is still running, which is why every serious deployment adds
  idempotency keys.
* **Oban/River/Solid Queue**: `SKIP LOCKED` polling with a lease-like
  `attempted_by` and a rescuer/stager that re-queues stale jobs — the same
  three parts as here (poll, lease, reaper).
* **Uber's Cadence** and **Netflix's Conductor** both use polling workers
  with task timeouts rather than push.

## Documented issues

* Kleppmann on RedLock: a lease without storage-side fencing is unsafe under
  GC pauses — the exact scenario `TestDurability_PartitionedWorkerIsFencedOut`
  reproduces.
* Sidekiq's documentation on "jobs run twice" and Celery's `acks_late` +
  visibility-timeout interactions — the canonical at-least-once traps.
* Kubernetes' leader election has had bugs where two leaders coexisted under
  clock skew, fixed by judging renewal by the API server's time — the same
  reason this design uses the DB's `now()`.
* Temporal's guidance that activities must be idempotent because retries and
  worker crashes deliver them more than once — the reason the tool-call
  journal is mandatory in any design.
* This repository's own B4 (`cardinality(NULL)`), B6 (invisible `RUNNING` row)
  and B7 (a clamped limit) — all scheduling-adjacent, all found by the
  conformance and regression tests.

## Evidence in this repository

* `TestLease_OnlyOneWorkerWins`, `TestLease_PriorityBeatsFIFO`,
  `TestLease_RespectsEligibleTenants`, `TestCommit_RejectedAfterLeaseLost`,
  `TestDurability_*`, `TestLoad_ScalesWithWorkers` (52.9× for 12× workers —
  the poll path does not serialise).
* [benchmarks §2–3, §6](../04-evidence/01-benchmarks.md).

## Would reverse if

* median step time drops below ~100 ms (a cheap model, no tools) → the poll
  interval and lease transactions become significant; add LISTEN/NOTIFY and
  raise `MaxStepsPerLease`;
* runs routinely exceed a few hundred steps → implement the checkpoint event
  before anything else;
* a strict FIFO SLA for batch work is required → add ageing (promote after T
  seconds) or a separate batch pool.

## References

* Kleppmann, *How to do distributed locking* — https://martin.kleppmann.com/2016/02/08/how-to-do-distributed-locking.html
* Kubernetes, *Leases* — https://kubernetes.io/docs/concepts/architecture/leases/
* Temporal, *Workers and task queues* — https://docs.temporal.io/workers · *Activity heartbeats and timeouts* — https://docs.temporal.io/encyclopedia/detecting-activity-failures
* Sidekiq wiki, *Best practices (idempotent jobs)* — https://github.com/sidekiq/sidekiq/wiki/Best-Practices
* Celery, *Visibility timeout* (Redis transport) — https://docs.celeryq.dev/en/stable/getting-started/backends-and-brokers/redis.html#visibility-timeout
* River, *How it works (leasing and rescuing jobs)* — https://riverqueue.com/docs
* PostgreSQL, `LISTEN` / `NOTIFY` — https://www.postgresql.org/docs/current/sql-notify.html
* Erlang/OTP, *Supervisor behaviour* — https://www.erlang.org/doc/system/sup_princ.html
