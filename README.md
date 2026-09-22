# Agentic orchestration platform

Running ~1000 concurrent agents on Kubernetes, where any agent may execute
arbitrary code and call credentialed APIs, without one agent being able to harm
another agent, the platform, or a tenant's data.

```bash
make demo        # Linux + root — the full end-to-end run with 21 assertions
make demo-docker # macOS / Windows / no root
```

| Document | What it is |
|---|---|
| **[DESIGN.md](DESIGN.md)** | The architecture proposal. Decisions, trade-offs, failure modes, what I cut. |
| **[DEEP_DIVE.md](DEEP_DIVE.md)** | Every decision with its alternatives, the case *for* what I rejected, industry precedent and links. |
| **[TUTORIAL.md](TUTORIAL.md)** | The whole system from first principles: what a container actually is, why durability is hard, why credentials cannot touch the agent. |
| **[AI_LOG.md](AI_LOG.md)** | How I used AI, where I overrode it, and where it was better than me. |
| **[docs/diagrams/](docs/diagrams/)** | System + trust boundaries, agent lifecycle, tool call path, sandbox layers. |

---

## The short version

**An agent is a row in Postgres plus an append-only event log — not a pod, not a
process.** A stateless worker leases a run, replays its log, advances it one
step, and lets go. That is what makes an agent survive a pod restart, a node
drain and a deploy, and what makes an agent waiting two days for a human cost
essentially nothing.

**Every tool call goes through one gateway** that decides authorization, mints a
short-lived credential and writes a hash-chained audit record. Agent workers
have network egress to the gateway and the database and nothing else, so the
choke point cannot be bypassed.

**Untrusted code runs in a jail built directly from Linux primitives** —
namespaces, cgroups, `pivot_root` onto a read-only tmpfs, an emptied capability
bounding set, `no_new_privs` and a seccomp filter blocking 36 escape primitives.
Production adds gVisor. Cold start measures **8.4 ms**.

**Credentials never reach the agent.** A credentialed CLI runs in a *separate*
sandbox in a different pid and user namespace, sharing only the workspace
directory.

---

## Running it

### Prerequisites

| Target | Needs |
|---|---|
| `make demo` | Linux, root (or `CAP_SYS_ADMIN`), Go 1.24+, Postgres |
| `make demo-docker` | Docker + Compose — works on macOS and Windows |
| `make run` | the above, plus Node 20+ for the console |
| `make kind-up` | Docker, `kind`, `kubectl` |

`./scripts/preflight.sh <target>` checks and tells you exactly what is missing.

### 1. The demonstration

```bash
make demo
```

Runs six scenarios against a real Postgres and asserts 21 properties. It exits
non-zero if any fail — it is an acceptance test, not a slideshow.

```
1. A NORMAL AGENT        writes a document, converts it in a sandbox, verifies it
2. THE CREDENTIAL BOUNDARY  the agent dumps its own environment and finds nothing,
                            then opens a PR with a token the API actually verified
3. PROMPT INJECTION      four exfiltration attempts, all refused by policy
4. A POISON AGENT        an infinite tool-call loop, stopped by an enforced budget
5. HUMAN IN THE LOOP     parked with no worker and no quota, then resumed
6. THE AUDIT TRAIL       chain verifies; tampering is detected
```

Without Postgres it falls back to an in-memory store and says so loudly — the
durability guarantee does not hold there.

### 2. The operator console

```bash
make run     # then open http://localhost:8080
```

Switch identity with the "Acting as" selector to see tenant scoping: the
`globex` key cannot see `acme`'s runs at all.

### 3. The tests

```bash
make test            # unit tests, no infrastructure
make test-safety     # the sandbox isolation claims, one at a time
make test-durability # kill a worker mid-tool-call and watch a run recover
make test-load       # 500 concurrent agents
make test-all        # everything, including Postgres conformance
```

### 4. Kubernetes and GitOps

```bash
make kind-up      # kind cluster + Calico + build/load images + deploy
make kind-test    # verify the DEPLOYED cluster enforces what it claims
make argocd-up    # install Argo CD and register this repo
make argocd-test  # prove selfHeal reverts a manual kubectl change
make kind-down
```

---

## What is real and what is faked

Being precise about this is the point.

### Real

| | |
|---|---|
| **The sandbox** | Actual Linux namespaces, cgroups, `pivot_root`, capability dropping, and a hand-written seccomp-BPF filter. Not a wrapper around `docker run`. |
| **Network isolation** | An empty network namespace. `connect()` returns `ENETUNREACH` — verified with a real socket call plus a host negative control. |
| **The credential boundary** | A second sandbox in a different pid/user namespace really does hold the token; the agent's sandbox really has no credential. Verified both ways. |
| **The audit chain** | Real SHA-256 chaining, verified on read, with a test that forges a record and asserts detection. |
| **Durability** | Real leases with fencing, real replay from the event log, real idempotency journal. Tests SIGKILL a worker mid-tool-call. |
| **Fairness** | Real token buckets with weighted max-min shares and reserve-then-settle accounting. |
| **The store** | Real Postgres, with a conformance suite that runs every test against both Postgres and the in-memory store. |
| **The Kubernetes manifests** | Validated against 1.31 schemas with `kubeconform` (`make k8s-validate`). |

### Faked, and why

| | |
|---|---|
| **The LLM** | A scripted provider. Deliberate: the properties being demonstrated — isolation, the credential boundary, durable resume, fairness under contention — must be *reproducible*. "A poison agent is stopped by its budget" should be a deterministic assertion, not a hope about a model's mood. The `llm.Provider` interface is what a real client implements. |
| **GitHub** | A local API that **actually verifies the credential** — it rejects missing, malformed, expired, revoked and wrong-tenant tokens. Without that, "the platform injected a credential" would be unverifiable: the demo would look identical if the token were empty. |
| **`gh`** | A minimal implementation of `pr create`, `pr list`, `api`, `auth token`. What is being demonstrated is the *plumbing* — a trusted binary receiving a short-lived token in a process the agent cannot observe. Swapping in the real `gh` binary changes nothing about that. |
| **`pandoc`** | Used when the image has it; otherwise a built-in Markdown converter, so the demo works on a machine without it. Either way it is a real external binary operating on the shared workspace inside a sandbox. |
| **Credentials** | HMAC-derived tokens from a per-tenant root secret. Genuinely short-lived, genuinely scoped, genuinely verifiable. Production needs a real secrets manager issuing dynamic credentials. |

### Local vs. production isolation

The kind overlay sets `AGENTORCH_RUNTIME_CLASS=""` because kind nodes have no
gVisor shim. **That is a real security downgrade** — sandboxes there share the
host kernel — and it is commented as such in the overlay. Everything else still
applies. Similarly, `deploy/kind/kind-config.yaml` disables kind's default CNI
so Calico can enforce NetworkPolicy; kindnet ignores it, and policies that are
silently ignored are worse than no policies.

---

## Known gaps

Ordered by how much they would keep me up at night.

1. **Workload identity.** Run tokens are HMAC with a shared secret. That proves
   which *run* a caller claims, not which *service* is calling. Production needs
   SPIFFE/SPIRE mTLS. **The most important gap.**
2. **The egress proxy is designed, not built.** NetworkPolicy references it. The
   agent sandbox's empty netns already provides the property demonstrated here;
   the proxy matters for the credentialed path.
3. **`NetworkProxy` mode shares the host network namespace** in the local
   driver, instead of joining a prepared netns wired to the proxy. Used only by
   the credential broker sandbox, which runs a trusted binary — but it is
   weaker than the design describes, and it is labelled in the code.
4. **The SSE stream takes the API key as a query parameter**, because
   `EventSource` cannot set headers. Keys land in access logs. Production needs
   a short-lived signed stream token.
5. **No workspace checkpointing.** A node loss loses `emptyDir` contents; the
   run recovers but re-does file work.
6. **Event-log growth is unmanaged.** At 10× this is the first thing to break
   (~288 GB/day). Needs partitioning and tiering.
7. **Cost is computed from a price table in code**, not reconciled against
   provider-reported usage.
8. **Postgres is a single-replica StatefulSet** with no backups or pooler. Fine
   for kind; not a production database.
9. **No distributed tracing.** The trace-id plumbing exists; the spans do not.
10. **The policy engine is hand-rolled**, not OPA or Cedar.

---

## Layout

```
backend/
  cmd/agentorch/        one binary, several personalities (busybox pattern)
  internal/
    sandbox/            namespaces, cgroups, seccomp + docker & k8s drivers
    gateway/            THE choke point: authz + credentials + audit
    authz/              policy engine, fails closed
    creds/              the Secret type that refuses to print itself
    store/              Postgres + in-memory, one conformance suite for both
    runtime/            the stateless worker, replay, the reaper
    fairness/           weighted max-min LLM quota
    llm/                provider interface, scripted provider, model gateway
    tools/              registry (model-facing schema vs. backend config), invoker
    api/                control plane, tenancy-scoped, SSE
  test/
    durability/         kill a worker mid-tool-call
    load/               500 concurrent agents
ui/                     React + TypeScript operator console
deploy/
  docker/  compose/  kind/  k8s/{base,overlays/local}  argocd/
scripts/                preflight, kind up/test, argocd up/test
```

---

## Where I stopped

I built more than one vertical slice, because the interesting property spans
several: the credential boundary is only meaningful if the sandbox is real, the
sandbox is only meaningful if authorization decides what runs in it, and none of
it matters if a node restart loses the run. Those four together are the slice.

What I did **not** do is production-harden any of it. The gaps above are real
and are listed in the order I would fix them.

If you only read one thing in the code, read
[`internal/sandbox/init_linux.go`](backend/internal/sandbox/init_linux.go) —
it is the whole isolation argument in one file, and the comment above the
privilege-drop sequence explains an ordering bug that cost me a confusing
`SIGABRT`.
