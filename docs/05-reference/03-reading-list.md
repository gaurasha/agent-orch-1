# Reading list

Primary sources behind the design decisions, grouped by the decision they
inform. Specifications and engineering write-ups are preferred over summaries,
because the point of a reference is that you can check the claim yourself.

---

## Durable execution and distributed systems

| Source | Why it matters here |
|---|---|
| Kleppmann, [*How to do distributed locking*](https://martin.kleppmann.com/2016/02/08/how-to-do-distributed-locking.html) | **The fencing-token argument.** Directly behind `store.Commit` |
| Fowler, [*Event Sourcing*](https://martinfowler.com/eaaDev/EventSourcing.html) | The log-as-truth pattern |
| [Temporal: workflows](https://docs.temporal.io/workflows) | Deterministic replay by stateless workers — the shape this system independently reaches |
| [Azure Durable Functions](https://learn.microsoft.com/en-us/azure/azure-functions/durable/durable-functions-overview) | Replay-based orchestration |
| [Restate](https://docs.restate.dev/concepts/durable_building_blocks) | Durable RPC with journaled side effects |
| [DBOS](https://docs.dbos.dev/) | The same bet: the log lives in Postgres |
| [Postgres `FOR UPDATE SKIP LOCKED`](https://www.postgresql.org/docs/current/sql-select.html#SQL-FOR-UPDATE-SHARE) | The entire scheduling mechanism |
| [River](https://riverqueue.com/) · [Oban](https://hexdocs.pm/oban/Oban.html) · [Que](https://github.com/que-rb/que) | Prior art for `SKIP LOCKED` queues |

---

## Linux isolation

| Source | Why |
|---|---|
| [`namespaces(7)`](https://man7.org/linux/man-pages/man7/namespaces.7.html) | The six namespaces used |
| [`capabilities(7)`](https://man7.org/linux/man-pages/man7/capabilities.7.html) | Why the **bounding** set is the strong claim |
| [`seccomp_filter.rst`](https://www.kernel.org/doc/html/latest/userspace-api/seccomp_filter.html) | The BPF program format and return values |
| [`pivot_root(2)`](https://man7.org/linux/man-pages/man2/pivot_root.2.html) | Why not `chroot` |
| [`no_new_privs`](https://docs.kernel.org/userspace-api/no_new_privs.html) | setuid neutralisation; seccomp precondition |
| [cgroup v2](https://docs.kernel.org/admin-guide/cgroup-v2.html) | Resource bounds on a process tree |
| [Docker's default seccomp profile](https://github.com/moby/moby/blob/master/profiles/seccomp/default.json) | The allowlist comparison, and the `clone3`/`ENOSYS` precedent |
| [Linux syscall table](https://filippo.io/linux-syscall-table/) | Verifying the numbers in the denylist |

### Sandboxing technologies

| Source | Why |
|---|---|
| [gVisor](https://gvisor.dev/docs/) | The production isolation layer |
| [gVisor performance guide](https://gvisor.dev/docs/architecture_guide/performance/) | The honest cost: syscall- and I/O-heavy workloads suffer most |
| [Firecracker (NSDI '20)](https://www.usenix.org/conference/nsdi20/presentation/agache) | The microVM alternative and its ~125 ms boot |
| [Kata Containers](https://katacontainers.io/) | VM-per-pod as a RuntimeClass |
| [GKE Sandbox](https://cloud.google.com/kubernetes-engine/docs/concepts/sandbox-pods) | gVisor as a managed offering |

### Why a shared kernel is not enough

| CVE | What it was |
|---|---|
| [CVE-2022-0185](https://nvd.nist.gov/vuln/detail/CVE-2022-0185) | fs context heap overflow → container escape |
| [CVE-2022-0492](https://nvd.nist.gov/vuln/detail/CVE-2022-0492) | cgroup v1 `release_agent` → escape |
| [Dirty Pipe](https://dirtypipe.cm4all.com/) | Arbitrary file overwrite |

---

## AI agent security

| Source | Why |
|---|---|
| Willison, [*The lethal trifecta*](https://simonwillison.net/2025/Jun/16/the-lethal-trifecta/) | **The framing this design is built around** |
| Willison, [*Prompt injection* series](https://simonwillison.net/series/prompt-injection/) | Named the problem; documents why filtering fails |
| Greshake et al., [*Not what you've signed up for*](https://arxiv.org/abs/2302.12173) | Indirect injection through retrieved content |
| [OWASP Top 10 for LLM Applications](https://owasp.org/www-project-top-10-for-large-language-model-applications/) | LLM01 Prompt Injection, LLM06 Excessive Agency |
| [NIST AI 100-2](https://csrc.nist.gov/pubs/ai/100/2/e2023/final) | Adversarial ML taxonomy |

---

## Authorization and capabilities

| Source | Why |
|---|---|
| Hardy, [*The Confused Deputy*](https://cap-lore.com/CapTheory/ConfusedDeputy.html) | The 1988 paper describing exactly this problem |
| [Capability-based security](https://en.wikipedia.org/wiki/Capability-based_security) | Why ACLs ask the wrong question here |
| [Google Zanzibar](https://research.google/pubs/pub48190/) | Centralised authorization at scale |
| [JCS, RFC 8785](https://www.rfc-editor.org/rfc/rfc8785) | Canonical JSON — without it, hashing is unstable |

---

## Secrets

| Source | Why |
|---|---|
| [SPIFFE/SPIRE](https://spiffe.io/docs/latest/spiffe-about/overview/) | **The top production gap**: workload identity |
| [Vault dynamic secrets](https://developer.hashicorp.com/vault/docs/secrets) | What a real broker looks like |
| [GitHub fine-grained tokens](https://docs.github.com/en/authentication/keeping-your-account-and-data-secure/managing-your-personal-access-tokens) | Provider-side scoping as defence in depth |
| Rust's [`secrecy`](https://docs.rs/secrecy/latest/secrecy/) | The same type-level argument as `creds.Secret` |

---

## Fairness and rate limiting

| Source | Why |
|---|---|
| [Max-min fairness](https://en.wikipedia.org/wiki/Max-min_fairness) | The allocation model |
| [Deficit round robin](https://en.wikipedia.org/wiki/Deficit_round_robin) | The packet-scheduling analogue |
| [Linux CFS](https://docs.kernel.org/scheduler/sched-design-CFS.html) | Weighted fair sharing of CPU |
| [Envoy global rate limiting](https://www.envoyproxy.io/docs/envoy/latest/intro/arch_overview/other_features/global_rate_limiting) | Distributed token buckets — what a multi-replica limiter needs |
| [Kubernetes API Priority and Fairness](https://kubernetes.io/docs/concepts/cluster-administration/flow-control/) | Priority lanes with reserved capacity |
| AWS, [*Exponential Backoff And Jitter*](https://aws.amazon.com/blogs/architecture/exponential-backoff-and-jitter/) | Why **full** jitter, not a fixed multiplier |

---

## Audit and transparency

| Source | Why |
|---|---|
| [Certificate Transparency, RFC 6962](https://www.rfc-editor.org/rfc/rfc6962) | Append-only logs with cryptographic proofs |
| [transparency.dev](https://transparency.dev/) | Where to anchor a chain head so it cannot be rewritten |
| [Merkle trees](https://en.wikipedia.org/wiki/Merkle_tree) | The upgrade from a chain when inclusion proofs are needed |

---

## Kubernetes

| Source | Why |
|---|---|
| [Pod Security Standards](https://kubernetes.io/docs/concepts/security/pod-security-standards/) | The `restricted` profile |
| [RuntimeClass](https://kubernetes.io/docs/concepts/containers/runtime-class/) | Selecting gVisor per workload |
| [NetworkPolicy](https://kubernetes.io/docs/concepts/services-networking/network-policies/) | Default-deny; **requires a CNI that implements it** |
| [QoS classes](https://kubernetes.io/docs/concepts/workloads/pods/pod-qos/) | Why `requests == limits` for sandboxes |
| [etcd limits](https://etcd.io/docs/v3.5/dev-guide/limit/) | Why agent runs are not CRDs |
| [Large cluster guidance](https://kubernetes.io/docs/setup/best-practices/cluster-large/) | The 110-pods-per-node figure |
| [Argo CD projects](https://argo-cd.readthedocs.io/en/stable/user-guide/projects/) | The AppProject guardrail |
| [kubeconform](https://github.com/yannh/kubeconform) | Schema validation in CI |

---

## Go

| Source | Why |
|---|---|
| [`syscall` package](https://pkg.go.dev/syscall) | `SysProcAttr.Cloneflags`, `UidMappings` — how the jail is built |
| [`log/slog`](https://pkg.go.dev/log/slog) | Structured logging in the standard library |
| [`net/http` routing (1.22+)](https://go.dev/blog/routing-enhancements) | Why no router dependency |
| [`context`](https://pkg.go.dev/context) | Cancellation through the worker and sandbox |

---

## Worth reading even though it did not change a decision

| Source | Why |
|---|---|
| [WASI](https://github.com/WebAssembly/WASI) | The best isolation-per-millisecond available — and why it cannot run `pandoc` |
| [Cloudflare Durable Objects](https://developers.cloudflare.com/durable-objects/) | The actor-per-entity alternative to a worker pool |
| [Orleans virtual actors](https://learn.microsoft.com/en-us/dotnet/orleans/overview) | The same, with automatic placement |
| [OPA / Rego](https://www.openpolicyagent.org/docs/latest/) · [Cedar](https://www.cedarpolicy.com/) | What the hand-rolled policy engine should become |
