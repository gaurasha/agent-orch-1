# 01 · Runtime model — what an agent run *is*

> Decision: **a run is a row plus an append-only event log; a pool of
> stateless, interchangeable workers leases runs one step at a time and
> commits under a fencing token.** Not a process, not a pod, not an actor, not a
> workflow-engine workflow — although the last of these is the closest relative
> and is what the design would adopt if it had to stop owning the substrate.
>
> Diagrams: [00-overall](../architecture/00-overall.md), [02-scheduling-runtime](../architecture/02-scheduling-runtime.md) · Related: [03-durability-substrate](03-durability-substrate.md), [04-scheduling](04-scheduling.md)

## Problem

The identity of a running agent has to live somewhere. Wherever it lives
determines what a machine failure costs, what an idle agent costs, and how
much code sits between "the model said X" and "X happened".

Physical facts that constrain the answer:

* An agent's loop is **> 99 % waiting** on a model or a tool. Whatever holds the
  agent's identity is held for the whole wait.
* The loop must survive the death of whatever runs it, and must **not repeat a
  side effect** when it resumes.
* Thousands of agents may exist per tenant; hundreds may start in the same
  minute.

## Options

| # | Option | Strongest case for it | Where it breaks | Who uses it |
|---|---|---|---|---|
| A | **Process / goroutine per agent in memory** | simplest possible; the best debugging story; zero infrastructure | a crash loses every in-flight agent; idle agents hold memory; no fairness across processes; horizontal scaling means sticky routing | most agent frameworks' default executors (LangChain `AgentExecutor`, early AutoGen, CrewAI local) |
| B | **Container / pod per agent** (a Kubernetes Job per run) | isolation for free; the platform's own scheduling, limits and logs; "an agent is a pod" is easy to explain | pod creation is 1–3 s and an API-server write; idle pods cost a node slot; 300 agents at 09:00 = 300 scheduler decisions; the pod's memory *is* the state, so a node loss is a loss; no exactly-once for tool calls without a journal anyway | Argo Workflows-style batch systems; early "agent as a Job" designs; Kubeflow pipelines |
| C | **Actor model** (Erlang/OTP, Akka, Orleans virtual actors, Ray actors, Dapr actors) | one actor per agent with a mailbox; supervision trees; Orleans "virtual actors" are activated on demand and deactivated when idle, which solves the idle-cost problem elegantly | state persistence is still your problem (Orleans grains persist via a storage provider you write); exactly-once side effects need an idempotency journal anyway; another cluster runtime to operate; Ray actors are not restarted with state by default | Orleans (Halo services), Akka, Ray Serve, Dapr |
| D | **Workflow / durable-execution engine** (Temporal, Cadence, Restate, DBOS, Inngest, Hatchet, Azure Durable Functions, Step Functions, Conductor) | this *is* the problem they solve: deterministic replay, activities with retries, timers, signals, heartbeats; battle-tested at Uber/Netflix/Stripe scale; a rich ecosystem | the engine becomes the TCB for authorisation (activities run wherever workers are, so the credential boundary must be built on top); history-size limits force `continue-as-new`; the engine is a second cluster (Temporal: frontend/history/matching/worker services + Cassandra/Postgres + Elasticsearch); replay determinism constraints leak into agent code; per-tenant fairness is not native | Temporal: OpenAI (Codex), Netflix, Stripe, Snap; Restate; DBOS; Inngest |
| E | **Serverless durable objects / functions** (Cloudflare Durable Objects + Workflows, Modal, Vercel) | idle is free; storage is co-located with the object; global by default | vendor-bound; a Durable Object is single-threaded with its own storage — good for one agent, but sandboxing model code needs the vendor's sandbox product too; fairness and audit are yours | Cloudflare's own agents SDK; Modal for batch inference |
| F | **Event-sourced state machine on a relational DB, stateless worker pool** (chosen) | the run is a row and a log; any worker can continue any run; the queue, the lock, the log and the idempotency journal are one transaction; nothing is held while idle; ~1 500 lines instead of a second cluster | you own the substrate: leases, fencing, replay, reapers are your code and your bugs (nine were found; see evidence); one primary's write throughput; replay cost grows with the log until you add checkpoints | Oban/River/Que-style job systems in production at many companies; Dagster/Prefect internals; this repository |
| G | **Agent graph framework with a checkpointer** (LangGraph, CrewAI Flows, AutoGen, OpenAI Agents SDK, Anthropic Agent SDK) | the agent's control flow is explicit; checkpointing per super-step; human-in-the-loop primitives | these are *application-level*; each needs a substrate from A–F underneath (LangGraph Platform runs on its own task queue + Postgres); the checkpointer stores the whole state each step, which grows with the conversation | LangGraph Platform, CrewAI Enterprise |

## Deep dive: why F, from first principles

**1. The idle argument.** If an agent is idle 99.9 % of the time, the cost of a
*held* representation is 1 000× the cost of the work. A row and a log are the
only representation with zero holding cost: nothing runs, nothing is
allocated, nothing is routed to. Options A, B and C all hold something
(memory, a pod, an actor activation). Orleans' virtual actors and Cloudflare's
Durable Objects solve this by *deactivating* idle actors and reloading state —
which is F with a framework around it.

**2. The failure argument.** Whatever holds the identity can die. If the
identity is in memory (A, C without persistence), death is loss. If it is in
a pod (B), death is loss unless the pod's work is journaled elsewhere — at
which point the journal is the identity and the pod is a worker. So every
option converges on "state in a durable store, work in replaceable workers".
D and F are that; D rents the store, F owns it.

**3. The exactly-once argument.** A replayed agent will emit the same tool call
again. Nothing in A–E prevents the second execution *unless* there is a journal
keyed by something deterministic. Temporal gives this for activities
(activity IDs are deterministic in the workflow); F gets it from
`idem_key = run:step:i`, which a replacement worker recomputes from the log
with no coordination. The mechanism is identical; F's is 30 lines.

**4. The fencing argument.** Two workers can believe they own the same run
(the first paused, the second promoted). Only a check *in the same
transaction as the write* against a row-level lock prevents the first from
writing stale history. That is Kleppmann's fencing token. Temporal implements
it internally (workflow task tokens); F implements it as
`WHERE lease_owner = me AND next_seq = expected`. The property is the same;
owning it means understanding it.

**5. The TCB argument — the one that actually decides it.** Authorisation and
credential injection must be un-bypassable by the agent. In D, activities run
on workers that the tenant's agent code shares; putting a credential in an
activity means the worker process holds it. The gateway boundary would have to
be built *on top of* Temporal anyway, at which point Temporal is providing
replay and timers — valuable, but not the hard part. F puts the gateway and
the store in the same trust argument.

**6. The operational-surface argument.** Temporal self-hosted is four services
plus a database plus (optionally) Elasticsearch; Temporal Cloud is a vendor
dependency on the hot path of every step. F is Postgres, which was needed
anyway for tenants, definitions and audit.

### What F costs, honestly

* **Replay is O(events) per step.** The PoC bounds the damage with four steps
  per lease and a small event payload, but a 400-step run replays 400 events
  per step: quadratic total reads. The fix is a `CHECKPOINT` event carrying a
  summarised context (the same thing Temporal calls continue-as-new and
  LangGraph calls a checkpoint). Designed, not built.
* **One primary.** Every step is one transaction on one Postgres. Measured on
  a 4-core VM with a stubbed model: 102 → 5 394 runs/s from 2 → 24 workers,
  which says the shared path does not serialise — not that one primary is
  enough forever. Partitioning by tenant and tiering cold history are the
  designed escapes.
* **You own the bugs.** Nine were found building it, including two in exactly
  this layer: a run left `RUNNING` with no owner (B6) and a `cardinality(NULL)`
  edge case in the lease query (B4). Both now have regression tests; both are
  the kind of bug a mature engine has already fixed.

## State of the art

* **OpenAI's Codex** cloud agent runs on **Temporal**; OpenAI has described
  using Temporal for long-running agent workflows in public talks
  (Temporal Replay 2025). This is the strongest data point for option D.
* **Anthropic's Claude Code** is a local process (option A) with the
  filesystem as its state; its cloud variants and Managed Agents run sessions
  on a server-side runtime with sandboxes — the vendor has not published the
  substrate.
* **LangGraph Platform** persists checkpoints in Postgres and runs a task
  queue — option F with option G on top.
* **Cloudflare Agents SDK** builds on Durable Objects (E): one object per
  agent, SQLite-backed state, hibernation while idle.
* **AWS Bedrock AgentCore Runtime** offers session isolation in microVMs with
  up to 8-hour sessions — closer to B/E with a managed substrate.
* **DBOS** ("durable execution as a Postgres library") is the closest
  published design to F: workflow steps are checkpointed in Postgres tables
  and re-executed on failure; the pitch is exactly "no second cluster".
* **Restate** and **Inngest** are D with a lighter operational footprint
  (single binary / managed), and both market to agent builders.

## Documented issues that informed the choice

* Temporal's **event-history limits**: workflows are capped by history size
  and length (the documented defaults are 50 K events / 50 MB, warnings earlier)
  and must use `continue-as-new` — the same replay-growth problem F has, with a
  hard ceiling rather than a slowdown.
* Temporal's **determinism constraint**: workflow code must be deterministic
  on replay; any nondeterminism (map iteration, time, random) is a bug class
  the SDKs detect at runtime. Agent code that reads a model is nondeterministic
  by nature, so the model call must be an activity — fine, but it shapes the
  code.
* **Kubernetes Job per agent** designs hit the API server: creating hundreds of
  pods per minute is a control-plane load, and pod startup is seconds. This is
  why B is used for *sandboxes per tool call* only in the production driver,
  and why the design warms pods per run.
* **Postgres-as-a-queue bloat**: a queue table with high churn needs
  aggressive autovacuum or it bloats; `SKIP LOCKED` with `ORDER BY` and a
  partial index is the standard mitigation (2ndQuadrant's write-up; Brandur's
  *Postgres job queues* series). The partial index `WHERE state='QUEUED'` here
  is that mitigation.
* **Actor frameworks and exactly-once**: Orleans' documentation is explicit
  that message delivery is at-most-once by default and that idempotency is the
  application's responsibility; Ray actors are not fault-tolerant by default
  (`max_restarts` recreates a fresh actor without state).

## Evidence in this repository

* `TestDurability_WorkerKilledMidToolCall_RunCompletesExactlyOnce` — a worker
  is SIGKILLed between the tool call and its commit; the run completes with
  exactly one side effect.
* `TestDurability_PartitionedWorkerIsFencedOut` — the timeline in diagram 02.
* `TestDurability_RollingDeployLosesNoWork` — SIGTERM path.
* `TestLoad_ScalesWithWorkers` — nothing in the shared path serialises.
* Bugs B4 and B6 in [bugs found](../04-evidence/03-bugs-found.md).

## Would reverse if

* the organisation already runs Temporal (or Restate/DBOS) in production and
  is willing to build the gateway boundary on top — the replay/timers/signals
  machinery is then free and better tested than this repository's;
* run histories routinely exceed a few thousand events *and* checkpointing
  proves insufficient — a purpose-built engine's history management would
  then pay for its operational cost;
* per-agent state must be co-located with a global edge (Cloudflare's model).

## References

* Kleppmann, *How to do distributed locking* (fencing tokens) — https://martin.kleppmann.com/2016/02/08/how-to-do-distributed-locking.html
* Fowler, *Event Sourcing* — https://martinfowler.com/eaaDev/EventSourcing.html
* Kreps, *The Log: what every software engineer should know about real-time data's unifying abstraction* — https://engineering.linkedin.com/distributed-systems/log-what-every-software-engineer-should-know-about-real-time-datas-unifying
* Helland, *Life beyond Distributed Transactions* (idempotence, entities) — https://queue.acm.org/detail.cfm?id=3025012
* Burrows, *The Chubby lock service* (leases, sequencers) — https://research.google/pubs/the-chubby-lock-service-for-loosely-coupled-distributed-systems/
* PostgreSQL, `SELECT … FOR UPDATE SKIP LOCKED` — https://www.postgresql.org/docs/current/sql-select.html#SQL-FOR-UPDATE-SHARE
* 2ndQuadrant, *What is SELECT SKIP LOCKED for in PostgreSQL 9.5?* — https://www.2ndquadrant.com/en/blog/what-is-select-skip-locked-for-in-postgresql-9-5/
* Brandur, *Postgres job queues & failure by MVCC* — https://brandur.org/postgres-queues
* Brandur, *Transactionally-staged job drains* — https://brandur.org/job-drain
* Temporal documentation — https://docs.temporal.io/
* Temporal, *Workflow limits / continue-as-new* — https://docs.temporal.io/workflows#continue-as-new
* Restate — https://docs.restate.dev/
* DBOS — https://docs.dbos.dev/
* Inngest — https://www.inngest.com/docs
* Hatchet — https://docs.hatchet.run/
* Microsoft Orleans, *Overview (virtual actors)* — https://learn.microsoft.com/en-us/dotnet/orleans/overview
* Ray, *Actors* and fault tolerance — https://docs.ray.io/en/latest/ray-core/actors.html
* Cloudflare, *Durable Objects* — https://developers.cloudflare.com/durable-objects/
* Cloudflare, *Agents SDK* — https://developers.cloudflare.com/agents/
* LangGraph, *Persistence* — https://langchain-ai.github.io/langgraph/concepts/persistence/
* Anthropic, *Building effective agents* — https://www.anthropic.com/engineering/building-effective-agents
* AWS, *Amazon Bedrock AgentCore* — https://aws.amazon.com/bedrock/agentcore/
* Argo Workflows — https://argoproj.github.io/workflows/
* Oban — https://github.com/oban-bg/oban · River — https://riverqueue.com/ · graphile-worker — https://github.com/graphile/worker · pg-boss — https://github.com/timgit/pg-boss
