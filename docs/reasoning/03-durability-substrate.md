# 03 · Durability substrate — where the queue, the lock, the log and the journal live

> Decision: **Postgres is the queue (`FOR UPDATE SKIP LOCKED`), the lock service
> (lease columns + a row lock), the event store (append-only `events`), the
> idempotency journal (`tool_calls`) and the audit log (`audit_log`).** One
> transaction commits a step's log append, state change, usage update and lease
> release. No Kafka, no Redis, no etcd, no workflow engine.
>
> Diagrams: [07-state-failover](../architecture/07-state-failover.md), [02-scheduling-runtime](../architecture/02-scheduling-runtime.md) · Concept: [durable execution](../01-concepts/02-durable-execution.md) · Related: [01-runtime-model](01-runtime-model.md), [04-scheduling](04-scheduling.md)

## Problem

Four pieces of state must survive any process death and must agree with each
other at every instant:

1. **which runs are runnable** (a queue),
2. **which worker owns which run** (a lock with an expiry),
3. **what has happened in each run** (an ordered log),
4. **which side effects have already been performed** (an idempotency journal).

The failure that matters is not "the store is down" — that is availability,
and every option has it. The failure that matters is **two of the four
disagreeing**: a run re-queued while its log is half-written; a lock released
while a side effect is in flight; a journal that says "done" for a step the log
says never happened. Every such disagreement is a lost or duplicated side
effect.

## Options

| # | Substrate | Strongest case | Where it breaks | Production users |
|---|---|---|---|---|
| A | **Postgres only** (chosen) | one transaction spans all four; `SKIP LOCKED` is a correct multi-consumer queue; row locks are the fence; already needed for tenants/definitions/audit; operational maturity | one primary's write throughput (~thousands of small txns/s per core); queue-table bloat needs vacuum discipline; no cross-region without logical replication; replay reads grow with the log | GitLab (sidekiq → PG-backed queues), Oban (Elixir), River (Go), graphile-worker, pg-boss, Que, Dagster/Prefect internals, DBOS |
| B | **Kafka / Redpanda** as the log + queue | the canonical durable log; partitions scale writes linearly; replay by offset is native | Kafka is not a work queue: no per-message ack or visibility timeout (share groups/KIP-932 are recent), no per-run ordering across partitions without keying, no fencing for *your* state (you still need a DB for leases and journal → two systems that must agree); consumer-group rebalances are a classic source of duplicate processing | LinkedIn, Uber (with a DB beside it); Temporal's own history is *not* Kafka |
| C | **SQS / Pub/Sub / RabbitMQ** as the queue | managed; visibility timeouts are leases; dead-letter queues | the log and the journal still need a DB; a message's visibility timeout and the DB's state are two clocks that can disagree (the exact split-brain the fence exists to prevent); ordering needs FIFO queues with their throughput limits; at-least-once means the journal is mandatory anyway | very common in web backends; AWS Step Functions internally |
| D | **Redis** (streams / lists / RedLock) | sub-millisecond; streams have consumer groups with pending-entry lists (a lease); simple | durability is configurable, not default (AOF `everysec` loses up to a second; replication is async); RedLock's fencing is exactly what Kleppmann's essay showed to be unsafe without a token the storage checks; the log outgrows memory | queues at many companies (Sidekiq, BullMQ); rate limiting; caches |
| E | **etcd / Consul / ZooKeeper** for leases + something else for the log | linearizable leases with lease IDs (etcd) or ephemeral znodes; the reference for coordination | designed for small config, not 10 k events/min/tenant; the log and journal go elsewhere, so the fence must be carried into that system anyway; operating a quorum service | Kubernetes itself (etcd), Kafka pre-KRaft (ZooKeeper) |
| F | **A workflow engine** (Temporal, Restate, DBOS, Inngest) | all four properties are the engine's product; deterministic replay, activity idempotency, timers and signals are solved | a second cluster (or vendor) on the hot path of every step; the credential/authorisation boundary must still be built beside it; Temporal's own persistence is Cassandra or Postgres/MySQL — a DB is still there, just not yours | Uber, Netflix, Stripe, Snap, OpenAI Codex (Temporal); Restate/DBOS users |
| G | **Object storage as the log** (S3 + a DB index) | infinitely cheap history; the eventual home of cold events anyway | latency (tens of ms per PUT) on the hot path; no transactions with the row state; conditional writes are new (S3 If-None-Match, 2024) and single-key | as a *tier*, widely (Snowflake, WarpStream, this design's tiering) |
| H | **A distributed SQL DB** (CockroachDB, Spanner, Yugabyte) | the Postgres model with horizontal writes and multi-region | higher per-transaction latency (consensus per commit); `SKIP LOCKED` semantics differ or are unsupported (CockroachDB added it; contention behaviour differs); operational and cost step-up | when A's single primary is genuinely exhausted |

## Deep dive: why a single transaction is the whole argument

Consider the step commit as four writes: append events, bump `next_seq`,
update `usage`, maybe release the lease. Now consider a crash between any two
of them, for each option:

* **A (one transaction):** either all four are visible or none. A replacement
  worker sees a consistent (row, log) pair and replays from it. No special
  case.
* **B/C/D + a DB:** the message is acked in the queue and the DB write fails,
  or vice versa. The standard mitigation is the *transactional outbox* (write
  to the DB, publish from a table by a relay) — which puts the queue back in
  the DB and adds a relay process. The outbox pattern is the industry's
  admission that the queue must be transactional with the state.
* **F:** the engine gives you (A) internally — this is its value — but the side
  effect (the tool call) is still an external system, so the journal is still
  needed, and the engine's journal (activity results) must be the one the
  gateway consults. Feasible; it makes the engine part of the TCB.

The idempotency journal has the same shape: `INSERT … ON CONFLICT DO NOTHING`
then `SELECT` is atomic under Postgres's unique index, so two workers racing on
the same `run:step:i` get exactly one `fresh = true`. In Redis the equivalent
is `SET NX` — fine — but the row must then agree with the log in Postgres,
which is the two-system problem again.

### `FOR UPDATE SKIP LOCKED` as the queue

```sql
SELECT id FROM runs WHERE state='QUEUED' AND … ORDER BY priority DESC, created_at
FOR UPDATE SKIP LOCKED LIMIT 1;
```

Semantics: lock the first row that no other transaction has locked, skipping
locked rows instead of waiting. N workers polling concurrently each get a
different row with no coordination and no broker. The partial index
`WHERE state='QUEUED'` keeps the scan proportional to live work. The known
subtleties, all handled:

* **bloat** — a high-churn queue table accumulates dead tuples; the mitigation
  is that the table is `runs` (one row per run, updated a few times) rather
  than a row per job, plus normal autovacuum; the history is in `events`,
  which is insert-only.
* **`ORDER BY` + `SKIP LOCKED` is not strictly FIFO** under contention (a
  worker may skip a locked older row and take a newer one). Acceptable: the
  order is priority-then-age as a *policy*, not a correctness property.
* **long transactions hold locks** — the lease transaction is two statements
  and commits immediately; the *work* happens outside any transaction, under
  the lease.

### Leases and the fence

A lease is `(lease_owner, lease_expires_at)` on the row. Renewal is
`UPDATE … WHERE lease_owner = me AND lease_expires_at > now()` — zero rows
means the lease is gone. Expiry is judged by the database's `now()`, so worker
clock skew is irrelevant, and the reaper's `UPDATE … WHERE lease_expires_at <
now()` is the entire failure detector. The commit checks the owner *inside*
the transaction under `FOR UPDATE`, which is the fencing token: a stale worker
cannot write because the row it would write is locked and says someone else
owns it. This is the property RedLock lacks and etcd provides with lease IDs;
Postgres provides it with a row lock and a predicate.

### The audit chain

`AppendAudit` locks the chain head (`ORDER BY seq DESC LIMIT 1 FOR UPDATE`),
computes `hash = sha256(prev ‖ canonical(rec))`, inserts. The lock
serialises one tenant's chain and nothing else; tamper-evidence is the
property (see [08-audit](08-audit.md)). Bug B5 was the µs/ns mismatch between
the hashed and the stored timestamp — the kind of bug that only a verify-after-
round-trip test finds.

## State of the art

* **Postgres as the queue** is now mainstream advice with mature libraries:
  Oban (Elixir), River (Go, transactional enqueue), graphile-worker (Node),
  pg-boss (Node), Que (Ruby), Solid Queue (Rails 8's default), DBOS
  (Python/TS). Rails 8 shipping **Solid Queue on the database by default**
  (2024) is the clearest signal that "no Redis" is a respectable default.
* **The transactional outbox** is the standard pattern wherever a broker is
  kept (Debezium's outbox event router; microservices.io).
* **Temporal** persists history in Cassandra, MySQL or Postgres and is the
  production reference for durable execution; its docs on history limits and
  `continue-as-new` are the best public statement of the replay-growth
  trade-off.
* **Kafka's KIP-932 (share groups)** — queue semantics on Kafka, in preview in
  Kafka 4.0 (2025); an acknowledgement that Kafka was not a queue.
* **S3 conditional writes** (2024) enabled log-on-object-storage designs
  (WarpStream, SlateDB, "S3 as a database" experiments) — the direction this
  design's cold tier takes.

## Documented issues

* Kleppmann, *How to do distributed locking* — the RedLock analysis: a lock
  without a fencing token checked by the *storage* is unsafe under pauses.
  This repository's fence is checked by Postgres, not by the worker.
* Brandur, *Postgres job queues & failure by MVCC* — how a job table bloats
  under `SKIP LOCKED` churn and why long-running transactions make it worse.
  Mitigated here by keeping the queue on the low-churn `runs` table.
* Uber's *Cadence/Temporal* history-size guidance; LangGraph checkpoint growth
  issues; both are the replay-cost problem, which this design shares and
  bounds with 4 steps per lease and (designed) checkpoint events.
* Consumer-group rebalances in Kafka causing duplicate processing — the
  standard reason at-least-once systems need an idempotency journal; the
  journal here is therefore not optional even if a broker were added.
* Redis persistence trade-offs (AOF fsync policy, async replication) —
  documented in Redis's own persistence page; the reason it is not the
  journal.

## Evidence in this repository

* Store conformance suite runs the *same* tests against the in-memory store
  and Postgres ([benchmarks §7](../04-evidence/01-benchmarks.md)); it caught
  B4 (`cardinality(NULL)`) and B5 (audit ns vs µs).
* `TestLease_OnlyOneWorkerWins`, `TestCommit_RejectedAfterLeaseLost`,
  `TestCommit_SeqMismatchIsConflict`, `TestToolCall_IdempotencyPreventsSecondSideEffect`,
  `TestToolCall_StuckCallsAreReapedAsAmbiguous`, `TestAudit_ChainIsTamperEvident`,
  `TestEvents_AppendOnlyOrdering`.
* B6 (a run stuck `RUNNING` forever) — the reaper's second clause exists
  because of it.
* Throughput shape: 2 → 24 workers gives 52.9× — the shared path does not
  serialise.

## Would reverse if

* sustained write load exceeds one primary (tens of thousands of
  transactions/s) — partition by tenant first, then H (distributed SQL);
* the organisation standardises on Temporal/Restate — keep Postgres for
  tenants/audit, let the engine own the log and leases, and build the gateway
  journal against the engine's activity results;
* multi-region active-active is required — the lease-on-a-row model needs a
  single writer per run; H or a per-region partition scheme.

## References

* Kleppmann, *How to do distributed locking* — https://martin.kleppmann.com/2016/02/08/how-to-do-distributed-locking.html
* PostgreSQL docs, *SELECT … FOR UPDATE / SKIP LOCKED* — https://www.postgresql.org/docs/current/sql-select.html#SQL-FOR-UPDATE-SHARE · *Routine vacuuming* — https://www.postgresql.org/docs/current/routine-vacuuming.html · *Partial indexes* — https://www.postgresql.org/docs/current/indexes-partial.html
* 2ndQuadrant, *What is SELECT SKIP LOCKED for?* — https://www.2ndquadrant.com/en/blog/what-is-select-skip-locked-for-in-postgresql-9-5/
* Brandur, *Postgres job queues & failure by MVCC* — https://brandur.org/postgres-queues · *Transactionally-staged job drains* — https://brandur.org/job-drain · *Implementing Stripe-like idempotency keys in Postgres* — https://brandur.org/idempotency-keys
* microservices.io, *Transactional outbox* — https://microservices.io/patterns/data/transactional-outbox.html
* Rails, *Solid Queue* — https://github.com/rails/solid_queue · River — https://riverqueue.com/ · Oban — https://github.com/oban-bg/oban · DBOS — https://docs.dbos.dev/
* Temporal, *Workflow limits and continue-as-new* — https://docs.temporal.io/workflows#continue-as-new
* Apache Kafka, *KIP-932: Queues for Kafka* — https://cwiki.apache.org/confluence/display/KAFKA/KIP-932%3A+Queues+for+Kafka
* Redis, *Persistence* — https://redis.io/docs/latest/operate/oss_and_stack/management/persistence/ · *Distributed locks (RedLock)* — https://redis.io/docs/latest/develop/use/patterns/distributed-locks/
* etcd, *Lease API* — https://etcd.io/docs/latest/learning/api/#lease-api
* AWS, *S3 conditional writes* — https://docs.aws.amazon.com/AmazonS3/latest/userguide/conditional-writes.html
* Stripe, *Idempotent requests* — https://docs.stripe.com/api/idempotent_requests
* Burrows, *The Chubby lock service* — https://research.google/pubs/the-chubby-lock-service-for-loosely-coupled-distributed-systems/
