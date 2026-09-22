# Glossary

> Terms as used **in this system**. Where a word is overloaded in the wider
> industry, the intended meaning is stated.

---

### Agent
A configured loop: a system prompt, a model, a granted tool set, and a budget.
At runtime an agent is **not** a process or a pod — it is a
[run](#run). → [What an agent is](../00-problem/01-what-is-an-agent.md)

### Agent definition
An immutable, [content-addressed](#content-addressing) specification owned by one
tenant. A run pins its **digest**, so editing a definition cannot change the
permissions of a run already in flight.

### Audit chain
A per-tenant hash chain over audit records:
`hash = sha256(canonical(record) ‖ prev_hash)`. Makes tampering **evident** (not
impossible). → [Audit](../01-concepts/07-audit.md)

### Backpressure
Declining to **schedule** work, never blocking a worker. The distinction is the
whole point: a blocked worker means one noisy tenant occupies the pool.
→ [Fairness §6](../01-concepts/06-fairness.md)

### Bounding set
The capability ceiling. Emptying it means **no `execve` anywhere in this process
tree can ever acquire a capability** — strictly stronger than clearing the
effective set. → [Linux isolation §3](../01-concepts/01-linux-isolation.md)

### Broker sandbox
The second sandbox, which runs a **trusted** binary (e.g. `gh`) with a credential
in its environment, in a different pid and user namespace from the agent. Shares
only `/work`. → [Secrets §3](../01-concepts/05-secrets.md)

### Budget
Per-run limits on steps, tool calls, tokens, dollars and wall-clock. **Enforced**
at two points, not merely reported. Zero never means unlimited.

### Capability (two meanings — be careful)
1. **Linux capability** — one of ~40 slices of root's power (`CAP_SYS_ADMIN`, …).
2. **Capability security** — an unforgeable reference that *is* the authority.
   Here, the granted tool set in a run's pinned digest.

### cgroup
Kernel mechanism bounding CPU, memory and PID count for a whole process
**tree**. Complements rlimits, which bound one process.

### Confused deputy
A program with legitimate authority tricked into using it for someone else. An
agent platform is a confused-deputy generator.
→ [Authorization §1](../01-concepts/04-authorization.md)

### Content addressing
Identifying a value by the hash of its canonical form. `digest =
sha256(canonical_json(spec))`. Registering the same spec twice is a no-op.

### Conformance suite
One set of tests run against **both** store implementations. Caught two
Postgres-only bugs that a fake-only suite would have shown green.

### Durable execution
Persisting an event log such that any worker can replay it and continue. The
foundational decision here. → [Durable execution](../01-concepts/02-durable-execution.md)

### Effectively once
The achievable guarantee: the side effect happens at most once *observably*, via
idempotency. **Exactly-once across a network boundary does not exist.**

### Empty network namespace
A network namespace with no interfaces, no addresses and no routes. `connect()`
returns `ENETUNREACH`. Not a firewall rule — there is nothing to misconfigure.

### Event log
The append-only `(run_id, seq)` history that **is** the agent. Rebuilding the
model context is a pure fold over it.

### Fencing token
A value the resource itself checks, so a stale writer is rejected. Here: the
lease ownership check inside `Commit`.
→ [Durable execution §5](../01-concepts/02-durable-execution.md)

### gVisor / runsc
A user-space kernel that services a container's syscalls, so a Linux kernel LPE
is not automatically a host compromise. The production isolation layer.

### Idempotency key
A **deterministic** name for one side effect: `run:step:index`. A replaying
worker computes the same key, necessarily, so the journal can recognise the
repeat.

### `IN_FLIGHT`
The honest journal state meaning *"we told the outside world to do something and
do not yet know whether it happened"*.

### Lease
`lease_owner` + `lease_expires_at` on a run row. At-most-one-active-worker, and
**the entire failure detector**: a dead worker stops renewing.

### Lethal trifecta
Private data + untrusted content + an exfiltration channel. Remove any one and
prompt injection fails. This design removes the third.
→ [Prompt injection](../01-concepts/08-prompt-injection.md)

### Max-min fairness
Allocation where each tenant is guaranteed `weight/Σweights × capacity` and
unclaimed capacity is redistributed. Starvation is impossible by arithmetic.

### Namespace (Linux)
A private copy of a global kernel resource: mount, pid, net, user, uts, ipc.
Not to be confused with a Kubernetes namespace.

### `no_new_privs`
A one-way prctl after which no `execve` can gain privilege. Also a precondition
for installing an unprivileged seccomp filter.

### `pivot_root`
Replaces the mount namespace's root outright. Used instead of `chroot`, which is
escapable via a retained directory fd.

### Prompt injection
Text in the model's context that redirects its behaviour. **Not reliably
detectable**, so the design contains it rather than detecting it.

### Reserve then settle
Reserve quota from an estimate before a call; reconcile against real usage after.
Over-estimates are refunded, under-estimates charged.

### Run
The durable identity of an executing agent: a row in `runs` plus its event log.
Carries no pod name, node or process id.

### Sandbox
A process with namespaces, cgroups, a pivoted read-only root, an emptied
capability bounding set, `no_new_privs` and a seccomp filter. Not a kernel
object.

### seccomp-BPF
A classic-BPF program inspecting each syscall, returning allow / errno / kill.
36 syscalls denied here. → [Syscall reference](02-syscalls.md)

### `Secret`
A Go type that redacts through `String`, `Format`, `GoString` and `MarshalJSON`.
Extraction requires `.Reveal()`, used in exactly **three** audited places.

### `SKIP LOCKED`
`SELECT … FOR UPDATE SKIP LOCKED` — N workers poll one table concurrently and
each takes a different row, with no broker and no coordination.

### Tenant
The isolation unit. **No function in this system takes two tenant IDs.**

### Tool gateway
The single choke point. Every tool call passes through it, so authorization,
credential minting and audit cannot drift apart.

### TCB (trusted computing base)
The components whose correctness the security model depends on: the tool gateway
and the credential broker. Deliberately small.

### Workload identity
Proving *which service* is calling, not just what token it presents. Currently
**absent** — the top production gap. → [Secrets §7](../01-concepts/05-secrets.md)

### Yield
Returning a still-unfinished run to `QUEUED` in one fenced statement. Replaces a
bare `ReleaseLease`, which stranded runs in ownerless `RUNNING`.
