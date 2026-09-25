# Reasoning — why it is built this way

The architecture folder says **what** was built. This folder says **why**:
for every decision, the full set of options that were on the table, the case for
each, what the industry does in production today, what has gone wrong for
people who chose otherwise, and the references behind every claim.

The method is the same in every document:

1. **Start from constraints, not from tools.** What does the problem physically
   require? (An agent is idle 99.9 % of the time; a model's output is data, not
   code; a credential that a model can read is a credential that a model can
   leak.)
2. **Enumerate the options exhaustively** — including the ones that were
   obviously wrong, because "obviously" is where mistakes hide.
3. **Steelman what was rejected.** Each rejected option gets the strongest case
   that could be made for it and the exact condition under which it would win.
4. **Cite production usage and documented failures**, not opinions. If a
   vendor's own docs say their system has a limit, that limit is quoted.
5. **State what would reverse the decision.**

---

## Contents

| # | Document | Decision it covers |
|---|---|---|
| 00 | [Method](00-method.md) | How to reason about an orchestration platform from first principles; the decision template |
| 01 | [Runtime model](01-runtime-model.md) | What an agent run *is*: a row plus a log with a stateless worker pool — vs. a process, a pod, an actor, a workflow engine |
| 02 | [Isolation](02-isolation.md) | Namespaces + seccomp in the PoC, gVisor in production — vs. runc, Kata/Firecracker, Wasm, remote sandbox vendors |
| 03 | [Durability substrate](03-durability-substrate.md) | Postgres as queue + lock + log + journal — vs. Kafka, SQS, Redis, Temporal, etcd |
| 04 | [Scheduling](04-scheduling.md) | Leases, fencing, replay, per-lease step budgets — vs. heartbeats, leader election, checkpoints |
| 05 | [Authorization](05-authorization.md) | A gateway on the hot path with deny-by-default and parameter policy — vs. libraries, sidecars, OPA, MCP permissions |
| 06 | [Credentials](06-credentials.md) | Broker-minted short-lived tokens, two sandboxes, a `Secret` type — vs. env vars, files, proxies, vaults |
| 07 | [Fairness](07-fairness.md) | Weighted max-min with a spare pool and an interactive reserve — vs. FCFS, fixed quotas, DRF, priority queues |
| 08 | [Audit](08-audit.md) | A per-tenant hash chain in Postgres — vs. append-only tables, Merkle logs, ledgers, SIEM |
| 09 | [LLM integration](09-llm-integration.md) | One model gateway with reserve/settle, jittered retries and fallback — vs. LLM proxies, SDK retries, streaming |
| 10 | [Prompt injection](10-prompt-injection.md) | Contain, don't detect — the lethal trifecta, CaMeL, benchmarks, and where detection still has a job |
| 11 | [Kubernetes](11-kubernetes.md) | How deep to integrate; PSA, NetworkPolicy, RuntimeClass, RBAC; Argo CD — vs. Compose, Nomad, ECS, Jobs, Flux |
| 12 | [Language and dependencies](12-language-and-dependencies.md) | Go, one binary, one external module — vs. Rust, Python, TypeScript; the supply-chain argument |
| 13 | [Evals and testing](13-evals-and-testing.md) | How the platform is tested, how agents should be evaluated, which benchmarks and tools exist, and how to read the numbers |
| 14 | [Industry learnings](14-industry-learnings.md) | Documented incidents and post-mortems, and the decision each one informs |
| 15 | [State of the art](15-state-of-the-art.md) | What the production systems of 2025–26 converge on: sandboxes, durable execution, gateways, identity |

---

## The three observations everything follows from

Every decision in this repository can be traced to one of three facts about
the problem. They are restated here because the rest of this folder assumes
them.

1. **An agent is a loop that is idle almost all the time.** A step is a model
   call (seconds) followed by a tool call (milliseconds to seconds) followed by
   waiting. If the agent's identity is a process, that process holds memory, a
   pod slot and a socket while doing nothing; if its identity is a row and a
   log, nothing is held. → [01](01-runtime-model.md), [04](04-scheduling.md).
2. **A model's output is data, not control.** Whatever the model emits — a tool
   call, a shell script, a URL — was influenced by every byte it read, and some
   of those bytes were written by an adversary. Nothing the model produces may
   carry authority by itself. → [05](05-authorization.md), [10](10-prompt-injection.md).
3. **A credential the model can read is a credential the model can leak.** Not
   "might": the exfiltration channel only needs to exist once. So the process
   that holds the credential and the process that runs model-authored code must
   be different processes in different namespaces. → [02](02-isolation.md),
   [06](06-credentials.md).

---

## How references are used

Every document ends with a **References** section. Links are to primary
sources where they exist (papers, vendor documentation, post-mortems, source
code) and to widely-cited engineering writing where the primary source is a
blog. Where a claim rests on a number from a vendor's documentation, the number
is quoted with the link so it can be re-verified when the vendor changes it.

**Verification status.** Internal links and anchors across the whole
documentation set are checked by `python3 docs/tools/check-links.py` (0 broken
at the time of writing). The ~270 external URLs could **not** be fetched from
the environment this was written in — its egress policy denies those hosts —
so they are cited from memory of the sources and should be treated as
"probably right, check before quoting". `python3 docs/tools/check-links.py
--external` fetches every one and reports anything unreachable; run it from a
machine with normal internet access and open a PR for any that have moved.
