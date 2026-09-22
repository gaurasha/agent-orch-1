# Documentation

Everything about this system, from first principles.

```bash
make demo        # Linux + root — 21 assertions, exits non-zero if any fail
make run         # then open http://localhost:8080
```

---

## Reading paths

Pick the one that matches why you are here.

### "I have 20 minutes and I want to understand the design"

1. [What an agent actually is](00-problem/01-what-is-an-agent.md) — the three
   observations everything follows from
2. [DESIGN.md](../DESIGN.md) — the decisions
3. [Bugs found](04-evidence/03-bugs-found.md) — what running it actually taught me

### "I am new to this problem space"

Read in order — each builds on the last:

1. [What an agent actually is](00-problem/01-what-is-an-agent.md)
2. [Use cases](00-problem/02-use-cases.md)
3. [TUTORIAL.md](../TUTORIAL.md) — containers, durability and credentials from scratch
4. [Linux isolation](01-concepts/01-linux-isolation.md)
5. [Durable execution](01-concepts/02-durable-execution.md)

### "I am reviewing the security model"

1. [Threat model](00-problem/04-threat-model.md) — adversaries, assets, attack trees
2. [Linux isolation](01-concepts/01-linux-isolation.md)
3. [Authorization](01-concepts/04-authorization.md)
4. [Secrets](01-concepts/05-secrets.md)
5. [Prompt injection](01-concepts/08-prompt-injection.md)
6. [Safety proofs](04-evidence/02-safety-proofs.md) — what is actually asserted
7. [Syscall denylist](05-reference/02-syscalls.md)

### "I am going to operate this"

1. [Running locally](03-operations/01-running.md)
2. [Kubernetes deployment](03-operations/02-deployment.md)
3. [Observability](03-operations/03-observability.md)
4. [Runbook](03-operations/04-runbook.md)
5. [Capacity planning](03-operations/05-capacity.md)

### "I am going to change the code"

1. [Code map](02-architecture/05-code-map.md)
2. [Components](02-architecture/01-components.md)
3. [Data model](02-architecture/02-data-model.md)
4. [API reference](02-architecture/03-api.md)
5. [Configuration](02-architecture/04-configuration.md)

### "I want to challenge a decision"

1. [DEEP_DIVE.md](../DEEP_DIVE.md) — 17 decisions with the case *for* what I rejected
2. [FAQ](05-reference/04-faq.md)
3. [Benchmarks §10](04-evidence/01-benchmarks.md) — what the numbers do **not** support

---

## Full contents

### [00 — The problem](00-problem/)

| | |
|---|---|
| [What an agent actually is](00-problem/01-what-is-an-agent.md) | The loop, why agents are 99.9% idle, why context growth is quadratic, why model output is data not control |
| [Use cases](00-problem/02-use-cases.md) | Five scenarios end to end, with the mechanism that fires at each step |
| [Requirements decoded](00-problem/03-requirements-decoded.md) | Each stated requirement, what it actually demands, and the evidence it is met |
| [Threat model](00-problem/04-threat-model.md) | Seven adversaries, seven assets, six attack trees, explicit non-goals |

### [01 — Concepts](01-concepts/)

| | |
|---|---|
| [Linux isolation](01-concepts/01-linux-isolation.md) | Namespaces, cgroups, capabilities, `no_new_privs`, seccomp-BPF, `pivot_root` — each built from the syscall up |
| [Durable execution](01-concepts/02-durable-execution.md) | Event sourcing, leases, **fencing**, idempotency, and the honest limits of exactly-once |
| [Multi-tenancy](01-concepts/03-multi-tenancy.md) | Seven enforcement points; making the bug unwritable |
| [Authorization](01-concepts/04-authorization.md) | The confused deputy, capability security, content addressing, parameter policy |
| [Secrets](01-concepts/05-secrets.md) | How `gh` gets a token the agent cannot read; five options and four holes |
| [Fairness](01-concepts/06-fairness.md) | Token buckets, weighted max-min, reserve-then-settle, backpressure as *not scheduling* |
| [Audit](01-concepts/07-audit.md) | Hash chaining, and what makes a log into evidence |
| [Prompt injection](01-concepts/08-prompt-injection.md) | The lethal trifecta; why containment beats detection |
| [Kubernetes primitives](01-concepts/09-kubernetes.md) | What we use, what we deliberately do not, and the kind/NetworkPolicy trap |

### [02 — Architecture](02-architecture/)

| | |
|---|---|
| [Components](02-architecture/01-components.md) | Every component: job, boundaries, failure behaviour, scaling |
| [Data model](02-architecture/02-data-model.md) | Six tables, every column justified, every index and its query |
| [API reference](02-architecture/03-api.md) | Both HTTP surfaces, every status code, the error philosophy |
| [Configuration](02-architecture/04-configuration.md) | Every flag; the four that actually matter; tuning |
| [Code map](02-architecture/05-code-map.md) | Guided tour, reading paths, conventions, how to add things |

### [03 — Operations](03-operations/)

| | |
|---|---|
| [Running locally](03-operations/01-running.md) | Four paths, troubleshooting, what good looks like |
| [Kubernetes deployment](03-operations/02-deployment.md) | kind, Argo CD, and verifying the **deployed** cluster |
| [Observability](03-operations/03-observability.md) | Every metric, suggested alerts, dashboard, gaps |
| [Runbook](03-operations/04-runbook.md) | Eight incidents: symptom → diagnosis → action |
| [Capacity planning](03-operations/05-capacity.md) | The arithmetic, with measured inputs |

### [04 — Evidence](04-evidence/)

| | |
|---|---|
| [Benchmarks](04-evidence/01-benchmarks.md) | Every measurement, how to reproduce it, **and what it does not support** |
| [Safety proofs](04-evidence/02-safety-proofs.md) | 17 claims: what is asserted, what would make the test lie |
| [Bugs found](04-evidence/03-bugs-found.md) | Nine real bugs and what each one teaches |

### [05 — Reference](05-reference/)

| | |
|---|---|
| [Glossary](05-reference/01-glossary.md) | Terms as used **in this system** |
| [Syscall denylist](05-reference/02-syscalls.md) | All 36, grouped by attack class — generated from source |
| [Reading list](05-reference/03-reading-list.md) | Primary sources, grouped by the decision they inform |
| [FAQ](05-reference/04-faq.md) | What a reviewer is likely to ask |

### [Diagrams](diagrams/)

| | |
|---|---|
| [System and trust boundaries](diagrams/system.md) | Four zones; what crosses each boundary and what cannot |
| [Agent lifecycle](diagrams/lifecycle.md) | State machine; one step in detail; failover |
| [Tool call path](diagrams/toolcall.md) | The full sequence; what the agent sees at each step |
| [Sandbox design](diagrams/sandbox.md) | Eight layers; the credential mechanism; compromise playbook |

### Top-level

| | |
|---|---|
| [DESIGN.md](../DESIGN.md) | The architecture proposal |
| [DEEP_DIVE.md](../DEEP_DIVE.md) | 17 decisions with alternatives and 53 references |
| [TUTORIAL.md](../TUTORIAL.md) | First principles, end to end |
| [AI_LOG.md](../AI_LOG.md) | How AI was used, overridden, and where it was better |
| [README.md](../README.md) | Run it; what is real; what is faked; known gaps |

---

## The design in five sentences

**An agent is a Postgres row plus an append-only event log — not a pod, not a
process.** A stateless worker leases it, replays the log, advances one step, and
lets go, which is why an agent survives a pod restart, a node drain and a deploy,
and why one parked on a human for two days costs storage and nothing else.

**Every tool call passes through one gateway** that decides authorization, mints
a ≤60-second credential and writes a hash-chained audit record — and agent
workers have network egress to that gateway and the database and nothing else,
so the choke point cannot be bypassed.

**Untrusted code runs in a jail built directly from Linux primitives**:
namespaces, cgroups, `pivot_root` onto a read-only tmpfs, an emptied capability
bounding set, `no_new_privs` and a 36-entry seccomp filter — measured at 8.4 ms
cold start, with gVisor added in production.

**Credentials never reach the agent**, because a credentialed CLI runs in a
*separate* sandbox in a different pid and user namespace, sharing only the
workspace directory.

**The LLM quota is shared by weighted max-min fairness**, and backpressure is
expressed as declining to schedule rather than as a blocked worker — measured: a
quiet tenant kept 98% of a flooding tenant's throughput while offering 1/50th the
load.

---

## Honest status

| | |
|---|---|
| Tests | **57 results, 57 passing** |
| End-to-end | `make demo` — **21 checks, 0 failed** |
| Manifests | `kubeconform` strict — **30/30 valid**, base and local |
| Biggest gap | **No mTLS workload identity.** The gateway authenticates the run token, not the caller |
| Not measured | gVisor overhead, real LLM latency, multi-replica fairness |
| Not built | Egress proxy, workspace checkpointing, event-log tiering, external audit anchoring |

Full list: [README known gaps](../README.md#known-gaps) ·
[Benchmarks §10](04-evidence/01-benchmarks.md).
