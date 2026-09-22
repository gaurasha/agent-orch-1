# FAQ

Questions a reviewer is likely to ask, answered directly.

---

## Architecture

**Why isn't each agent a pod? That's the obvious Kubernetes answer.**

Because a pod does not survive a node drain. Once you have externalised the state
so a controller can recreate the work, the pod has stopped providing durability —
it is an expensive way to hold a place in a queue. Add the arithmetic: 110
pods/node means 1000 agents is ≥10 nodes of pure overhead, and an agent waiting
two days for a human would hold one for two days.
→ [DEEP_DIVE D1](../../DEEP_DIVE.md#d1--agent-runtime-model)

**Why not Temporal? This is exactly what it's for.**

It is, and if the team already ran Temporal I would use it. I did not here
because this system needs the event log, run state, idempotency journal and audit
chain to commit in **one transaction**, and because per-tenant weighted fair
scheduling is not something task queues give you. I would end up putting a
scheduler in front and duplicating the history for the audit requirement.
→ [D4](../../DEEP_DIVE.md#d4--durable-execution-substrate)

**Why not a CRD and an operator?**

etcd's per-object limit is 1.5 MB and a long agent's event history exceeds it;
status writes at agent-step rate generate cluster-wide watch traffic; and
"every command agent X ran last Tuesday" is not expressible without SQL. The
middle ground I *would* build in production is a **thin** `AgentRun` CRD carrying
identity and terminal status only, reconciled from Postgres.
→ [D9](../../DEEP_DIVE.md#d9--kubernetes-integration-depth)

**Why Go and not Rust for the sandbox?**

Rust would be a better choice for the sandbox supervisor specifically — stronger
guarantees at exactly the layer where a memory-safety bug is worst. I did not
split the system across two languages for one component's preference, and the
rest (HTTP, SQL, orchestration) is faster to write correctly in Go. If the
sandbox grew much more complex, I would revisit it.

**One external dependency seems extreme.**

It is a real trade with a real cost: `client_golang` handles exposition edge
cases I have not thought about, and `libseccomp` has a better BPF compiler. The
argument is that the component executing untrusted code is where supply-chain
risk is least acceptable, and `client-go` alone pulls in ~200 modules. It flips
the moment we need informers, leader election, CRD codegen or native histograms.
→ [D16](../../DEEP_DIVE.md#d16--dependency-policy)

---

## Security

**Isn't "the model decides what to call" inherently unsafe?**

Yes, and that is the premise. The model is treated as an untrusted source of
*requests*. Every request is authorized against a capability set fixed at run
creation, and the exfiltration channels are removed so a successful injection has
nowhere to send anything.
→ [Prompt injection](../01-concepts/08-prompt-injection.md)

**Why not detect prompt injection?**

Because you cannot, reliably. There is no in-band way to distinguish "instruction
from my operator" from "text that looks like one". Every published delimiter
scheme has been bypassed. A boundary that depends on classifying natural language
correctly is not a boundary. Detection still has a job — alerting — but not a
load-bearing one.

**What actually stops an agent reading `GH_TOKEN`?**

It is not in its process. The CLI runs in a **second sandbox** in a different pid
and user namespace, so `/proc/<pid>/environ` is not merely permission-protected —
that PID does not exist in the agent's namespace. Plus policy blocks
`gh auth token`, the CLI refuses to print it, and the gateway scrubs the value
from output. → [Secrets](../01-concepts/05-secrets.md)

**Couldn't the agent just call the unix socket itself?**

That is exactly why the credential-helper-socket design was rejected. A socket
inside the sandbox is reachable by everything in that sandbox. Secrecy of the
socket is not a control.

**Is the `Secret` type real security or theatre?**

It is a mitigation for accidents, not for an attacker. An attacker with code
execution in the gateway calls `.Reveal()`. What it prevents is the realistic
failure: someone logs a request struct, or wraps an error, and a token lands in a
durable log. Discipline does not scale across a codebase; types do.

**Why is 404 better than 403 for cross-tenant reads?**

403 confirms the resource exists, which is an oracle for enumerating IDs. 404
says nothing.

**What is the weakest part of the security model?**

No mTLS between platform components. The gateway authenticates the *run token*,
not the *caller* — so a compromised pod with the signing secret can mint a token
for any run of any tenant. SPIFFE/SPIRE is the fix, and it is first in what I
cut. → [Secrets §7](../01-concepts/05-secrets.md#7-workload-identity--the-gap)

---

## Scale

**Have you actually run 1000 agents?**

No. 500 on a 4-core VM, with a stubbed LLM and a no-op tool caller. The claim I
make from that is not a throughput number — it is that **nothing in the shared
path serialises**, evidenced by throughput scaling 52.9× for a 12× increase in
workers. → [Benchmarks](../04-evidence/01-benchmarks.md)

**What breaks first at 10×?**

The event log. 100k tool calls/min ≈ 288 GB/day. Partitioning and tiering to
object storage is designed and not built.

**Why is p99 1.0 s in one place and 1.6 s in another?**

Run-to-run variance of ~60% on a 4-core VM that is simultaneously running
Postgres and the load generator. Both numbers are quoted rather than the better
one.

**Is the fairness limiter production-ready?**

No. It is **per-process**, so with N control-plane replicas the effective quota
is N× the configured value. The algorithm is right; the buckets need to live in
Redis. This is stated in the component doc and the capacity doc because it is a
real limitation, not a rough edge.

---

## The proof of concept

**Why is the LLM faked?**

Because the properties being demonstrated — isolation, the credential boundary,
durable resume, fairness under contention — must be **reproducible**. "A poison
agent is stopped by its budget" should be a deterministic assertion, not a hope
about a model's mood. `llm.Provider` is what a real client implements.

**Isn't a fake GitHub API meaningless?**

It would be if it accepted anything. It **verifies** the credential — rejecting
missing, malformed, expired, revoked and wrong-tenant tokens — and records each
presentation with a fingerprint. Without that, "the platform injected a
credential" would be unfalsifiable: the demo would look identical with an empty
token.

**Why implement namespaces by hand instead of shelling out to Docker?**

Two reasons. It runs where there is no Docker daemon, including CI and this
environment. More importantly, `docker run` would make the security properties
*invisible* — they would be Docker's defaults rather than decisions in the diff.
Writing each explicitly makes every property separately reviewable and separately
testable.

**Is the sandbox in the PoC as strong as the production design?**

No, and `Result.Driver` records which boundary ran so nothing can pretend
otherwise. The namespace driver shares the host kernel. Production adds gVisor,
which is the layer that turns "isolated from other tenants" into "isolated from
the host kernel".

**Why does the local kind overlay disable gVisor?**

kind nodes have no runsc shim, so the alternative is every sandbox pod stuck
`Pending`. The downgrade is commented **in the file that causes it**, because a
local overlay that silently weakens the model is how a weaker model ships.

---

## Testing

**Why run every store test against two implementations?**

Because a test that passes on a fake while the real store is broken is worse than
no test. It caught two Postgres-only bugs — a `NULL` predicate that made the
lease query match nothing, and a timestamp-precision mismatch that made the audit
chain never verify. Both would have shipped.
→ [Bugs found](../04-evidence/03-bugs-found.md)

**Why does the network test skip if the host has no network?**

Because otherwise it proves only that the machine is offline. A safety test needs
a negative control. The first version of that test passed for the wrong reason —
it was failing on a dash/bash incompatibility, not on isolation, and would have
passed with full network access.

**Why assert the exact number of forked children?**

"The fork bomb did not hang" is compatible with the fork bomb never forking —
which is what the first version did, in 27 ms, with exit 0. Asserting the
refusal happened at ≈`pids.max` proves the **cgroup** bound it.

---

## Operations

**How do I stop everything immediately?**

```bash
kubectl -n agentorch scale deployment/agentd --replicas=0
```

Runs stay `QUEUED` and resume when workers return. Nothing is lost — which is
only true because state does not live in the workers.

**Is a rolling deploy safe with runs in flight?**

Yes, with no draining. Workers yield back to the queue; a SIGKILLed worker's
lease simply lapses. Verified: 12 runs survived a hard generation swap with zero
duplicated side effects.

**What happens if Postgres goes down?**

Workers stop leasing and fail closed. Runs pause. This is deliberate: platform
availability matters less than not double-executing side effects.

**How do I know which isolation boundary a call actually got?**

`agentorch_sandbox_runs_total{driver=…}` and `result_meta.driver` in the audit
log. Alerting on `driver != "kubernetes"` in production catches a
misconfiguration that silently downgrades isolation.

---

## Process

**What would you build next?**

In order: SPIFFE/SPIRE workload identity; the egress proxy with per-run domain
allowlists; workspace checkpointing to object storage; a thin `AgentRun` CRD.
→ [README](../../README.md)

**What was hardest?**

Not the sandbox — that is well-trodden. It was deciding **where to draw the
credential boundary** such that a real CLI still works. Every obvious option has
a hole: env vars are readable, helper sockets are callable, TLS interception
breaks pinning, and API-only does not generalise.

**Where did AI help most, and least?**

Most: identifying the Postgres/Go timestamp-precision bug immediately from the
symptom, and suggesting one conformance suite across both stores — which is what
found it. Least: the judgement calls. I wrote the three hardest decisions, the
failure-mode table and every reversal condition myself, before asking for any
implementation. → [AI_LOG](../../AI_LOG.md)
