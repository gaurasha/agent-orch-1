# 00 · Method — reasoning about an orchestration platform from first principles

## The question that generates all the others

*A tenant hands us a model, some tools, some credentials and a goal. We run the
loop. What must be true for that to be safe, cheap, fair and recoverable at
10 000 tool calls per minute across many tenants?*

Everything else is a consequence. The method is to decompose that question into
physical constraints, and only then ask which mechanism satisfies each one.

## Step 1 — Find the physical constraints

A physical constraint is something that would be true no matter which
framework, vendor or language were chosen. The ones that matter here:

| Constraint | Why it is physical | Consequence |
|---|---|---|
| a model call takes 1–20 s; a tool call takes 10 ms–20 s; between them the agent does nothing | the latency is the provider's, not ours | idle time dominates; holding a process per agent wastes almost everything it holds |
| the model's output is a function of everything in its context, including attacker-written text | that is what a language model *is* | nothing the model emits can be trusted as an instruction; authority must come from elsewhere |
| a process can read its own environment, its own files and — absent isolation — the files and environment of its neighbours | Unix | model-authored code and credentials cannot share a process, a PID namespace or a mount namespace |
| any component can die between any two instructions | hardware, deploys, OOM | every durable transition must be atomic, and every side effect must be idempotent or reported as ambiguous |
| a shared provider has one rate limit for all tenants | the contract with the provider | fairness must be decided *before* the call, by us |
| the same run must be reconstructable by a different machine | otherwise a machine failure is a data loss | the run's state must live outside any machine |

## Step 2 — Enumerate the options without pruning

For each constraint, list every mechanism that could satisfy it, including the
naive ones. The naive options are kept because two things happen when they
are listed explicitly: their real advantages become visible (a process per
agent has the simplest debugging story in the industry), and their failure
mode becomes precise instead of hand-waved (a process per agent fails at *idle
memory × concurrency*, not at "scale").

The enumerations in this folder are as complete as the author could make
them; each document's options table is the place to challenge that.

## Step 3 — Steelman, then decide on a stated criterion

Each option gets the best case that could be made for it, and the decision is
made on a criterion stated in advance. The criteria used here, in priority
order:

1. **Safety** — can the mechanism be bypassed by anything the model emits?
2. **Correctness under failure** — what happens when a component dies at the
   worst moment? Lost work is acceptable; duplicated side effects are not.
3. **Operational surface** — how many systems must be run, upgraded and
   understood? Each one is a consistency seam and an on-call rotation.
4. **Cost at idle** — what does a waiting agent consume?
5. **Reversibility** — if the choice is wrong, what does changing it cost?

Performance is deliberately not on the list as a first-class criterion. It is a
constraint (the 10k calls/min target), checked by measurement, not a thing to
optimise past the requirement.

## Step 4 — Look for documented failure

Before committing to an option, look for the post-mortem. Most mechanisms in
this space have been in production somewhere long enough for their failure
modes to be written down: Temporal documents its history-size limits;
Kubernetes documents which CNIs ignore NetworkPolicy; the runc project has CVEs
with write-ups; the agent-security incidents of 2025 have public analyses.
[14-industry-learnings](14-industry-learnings.md) collects the ones that
changed a decision here.

## Step 5 — State what would reverse the decision

A decision that cannot be reversed by evidence is a belief. Every document
ends with the condition under which its decision would flip. Two examples:
per-call sandboxes would become per-run sandboxes if the cold start were 500 ms
rather than 8 ms; the in-process fairness limiter would move to Redis the day
there is a second worker replica in production.

## The decision template

Each document in this folder follows the same shape so that they can be
compared:

```
Problem            — what physical constraint is being satisfied
Options            — the exhaustive table, with the strongest case for each
Deep dive          — the chosen mechanism, explained from first principles
State of the art   — who runs what in production, with links
Documented issues  — what has gone wrong, with links
Evidence here      — the tests / measurements in this repository
Would reverse if   — the condition under which the decision flips
References
```

## A note on evidence quality

Three grades of evidence appear in this folder, and each claim tries to say
which it rests on:

* **measured here** — a number produced by a test in this repository
  (e.g. 8.4 ms cold start). Reproducible with one command.
* **documented by the vendor / author** — a limit, behaviour or incident stated
  in a primary source that is linked.
* **reasoned** — follows from the constraints above without a measurement.
  These are the claims most worth challenging.
