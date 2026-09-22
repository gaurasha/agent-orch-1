# Threat model

> **Prerequisite:** [Requirements decoded](03-requirements-decoded.md)
> **Read next:** [Linux isolation](../01-concepts/01-linux-isolation.md)

A design that says "it is secure" is not reviewable. This document names the
adversaries, the assets, the trust boundaries, the attack paths, and — for each
path — the control that stops it and the control that would catch it if the
first one failed.

---

## 1. Assets, ranked by what losing them costs

| # | Asset | Impact if compromised |
|---|---|---|
| A1 | **Tenant credentials** (GitHub tokens, API keys, DB passwords) | Full compromise of the tenant's external systems. Worst outcome in the system. |
| A2 | **Cross-tenant data** | Contractual and regulatory breach. Loses the enterprise business outright. |
| A3 | **Cloud node credentials** (IMDS, ServiceAccount tokens) | Cluster-wide, then account-wide escalation. |
| A4 | **The audit log's integrity** | Not an outage, but it destroys the ability to answer "what happened", which is the one thing an incident needs. |
| A5 | **LLM quota / spend** | Direct financial loss; denial of service to every other tenant. |
| A6 | **Platform availability** | Business impact, no data loss. |
| A7 | **Compute** (cryptomining, using egress as a proxy) | Cost and reputation. |

Note the ordering. Availability is **last**, and that ordering is a real design
input: when the database is unavailable, workers **stop leasing** rather than
executing side effects they cannot journal. We choose unavailability over
double-execution.

---

## 2. Adversaries

| ID | Adversary | Capability | Motivation | In scope? |
|---|---|---|---|---|
| **T1** | **Prompt-injected agent** | Emits any tool call, writes arbitrary code that the platform will run | Whatever the injected text says | **Primary** |
| **T2** | **Malicious tenant** | Authors agent definitions, supplies inputs, runs arbitrary code by design | Reach another tenant; steal compute; escape | **Primary** |
| **T3** | **Compromised platform component** | Whatever that component can do | Lateral movement | **Yes** |
| **T4** | **Malicious insider with DB access** | Read/write the datastore directly | Cover tracks after an action | Partially — see A4 |
| **T5** | **Compromised third-party API** | Returns hostile data in tool results | Second-order injection | **Yes** |
| **T6** | **Network attacker in-cluster** | Sees/modifies pod-to-pod traffic | Credential theft | Partially — mTLS is a gap |
| **T7** | **Supply-chain attacker** | Compromises a dependency | Anything | Partially — see mitigation |

### T1 deserves emphasis

The primary adversary is **not** a human attacker who has broken in. It is the
system working exactly as designed: an agent reads a document, the document
contains hostile text, and the model — behaving normally — emits hostile tool
calls.

This means the adversary is **inside the trust boundary by construction** and has
the agent's full legitimate authority. Every control must assume the agent is
hostile *right now*, not that it might become hostile later.

---

## 3. Trust boundaries

```
┌─ ZONE 0: UNTRUSTED ─────────────────────────────────────────────┐
│  users · LLM provider · third-party APIs · documents the agent  │
│  reads                                                           │
└──────────────────────────────┬──────────────────────────────────┘
                               │ OIDC / TLS / model responses
┌──────────────────────────────▼──────────────────────────────────┐
│  ZONE 1: PLATFORM  (PSA restricted, NetworkPolicy default-deny)  │
│                                                                  │
│   control plane · agent workers · model gateway · fairness       │
│   ┌────────────────────────────────────────────────────────┐    │
│   │ ZONE 1a: THE TCB — tool gateway + credential broker     │    │
│   │ The only component that holds plaintext credentials.    │    │
│   │ Smallest possible, separately deployed, separately      │    │
│   │ reviewed. If this is wrong, nothing else matters.       │    │
│   └────────────────────────────────────────────────────────┘    │
└──────────────┬───────────────────────────────┬──────────────────┘
               │ fenced writes                 │ argv in / stdout out
┌──────────────▼──────────┐   ┌────────────────▼──────────────────┐
│  ZONE 2: STATE          │   │  ZONE 3: SANDBOXES                │
│  Postgres               │   │  (tainted node pool, no SA token) │
│  runs · events ·        │   │   agent sandbox  │ broker sandbox │
│  journal · audit chain  │   │   untrusted code │ trusted binary │
└─────────────────────────┘   └───────────────────────────────────┘
```

**Key property:** agent workers (Zone 1) hold **no credentials**. A fully
compromised worker can mint run tokens for runs it can enumerate, but it cannot
produce a tenant credential, because it has never had one.

---

## 4. Attack trees

### AT-1 — Exfiltrate tenant data (T1, T2)

```
GOAL: get workspace contents to an attacker-controlled endpoint
│
├── 1. HTTP tool to an attacker domain
│      ├── tool not granted                      → DENIED (grant check)
│      └── tool granted but host not allowlisted → DENIED (parameter policy)
│
├── 2. Network from inside a sandbox
│      ├── direct connect()                      → ENETUNREACH (empty netns)
│      ├── DNS exfiltration                      → no resolver, no route
│      └── raw socket                            → no CAP_NET_RAW, no netns
│
├── 3. Via a credentialed CLI
│      ├── `gh api` to an attacker URL           → policy on argv
│      └── shared /work as a channel             → same tenant only
│
├── 4. Via the shared workspace
│      └── write to another tenant's workspace   → path traversal blocked twice
│                                                   (lexical + symlink resolution);
│                                                   different host uid; bind mount
│                                                   exposes only this run's dir
│
└── 5. Via a tool result (echo data back to a colluding service)
       └── requires an allowlisted host          → collusion only with an
                                                    allowlisted domain
```

**Residual risk:** an agent granted a tool to an allowlisted host that is itself
attacker-controlled. Mitigation is organisational (who approves allowlists), not
technical. **Detection:** egress-proxy byte-volume anomalies.

### AT-2 — Escape the sandbox to the host (T1, T2)

```
GOAL: execute code on the node
│
├── 1. Kernel exploit
│      ├── syscall is on the denylist            → EPERM (seccomp)
│      ├── syscall allowed, kernel bug exists    → gVisor: reaches the Sentry,
│      │                                            not the host kernel
│      └── gVisor bug exists                     → node has no creds; tainted pool;
│                                                   runtime detection
│
├── 2. Abuse a capability
│      └── any capability                        → bounding set is EMPTY; no execve
│                                                   can acquire one
│
├── 3. setuid binary
│      ├── in the image                          → image has none (stripped at build)
│      └── planted in /work                      → no_new_privs; /work is nosuid
│
├── 4. Escape the mount namespace
│      ├── chroot-style fd escape                → pivot_root, old root unmounted
│      ├── open_by_handle_at                     → seccomp EPERM
│      └── mount(2)                              → seccomp EPERM
│
└── 5. Join another namespace
       ├── setns / unshare                       → seccomp EPERM
       └── clone3 with namespace flags           → ENOSYS forces filterable clone
```

**Residual risk:** a gVisor Sentry escape. **Detection:** eBPF runtime rules
(Falco/Tetragon) on the sandbox pool. **Containment:** no ServiceAccount token,
tainted pool, credentials expire in ≤60 s.

### AT-3 — Steal a credential (T1, T2)

```
GOAL: obtain a usable tenant credential
│
├── 1. Read it from the agent's environment      → it is not there
├── 2. Read the broker's /proc/<pid>/environ     → different pid namespace
├── 3. ptrace the broker                         → seccomp + namespace
├── 4. Ask the CLI to print it                   → policy denies `auth token`;
│                                                   CLI also refuses
├── 5. Recover it from the event log             → never written; asserted by test
├── 6. Recover it from a log line                → Secret type redacts everywhere
├── 7. Get it echoed in a tool result            → gateway scrubs the value
└── 8. Steal it in transit                       → in-cluster TLS ⚠ GAP: no mTLS
```

**Residual risk:** #8. See [Workload identity](../01-concepts/05-secrets.md#7-workload-identity--the-gap).
Even on success, the credential expires in ≤60 s and is bound to one tenant and
one reference.

### AT-4 — Cross-tenant access (T2, T3)

| Path | Control |
|---|---|
| API: read another tenant's run | Every route resolves caller → tenant and scopes the query. Cross-tenant returns **404, not 403** |
| Store: query without a tenant filter | No function in `store` takes two tenant ids |
| Workspace: traverse to another tenant | `<root>/<tenant>/<run>`, resolved lexically **and** through symlinks |
| Credentials: use tenant A's token as B | Per-tenant root secrets; verification fails |
| Sandbox: read another tenant's files | Per-tenant host uid ranges; only this run's dir is mounted |
| Audit: read another tenant's chain | Per-tenant chains; tenant-scoped queries |
| **LLM quota: starve another tenant** | Weighted max-min fairness; guaranteed share is arithmetic |

### AT-5 — Tamper with the audit log (T4)

```
GOAL: hide what an agent did
│
├── 1. UPDATE a record        → hash mismatch; chain breaks at that seq
├── 2. DELETE a record        → sequence gap detected by VerifyChain
├── 3. Rewrite the whole tail → possible with DB access ⚠ RESIDUAL
│      └── mitigation: publish the head hash externally (transparency log,
│          another account's object store, customer-held receipt)
└── 4. Backdate a record      → timestamp is inside the hash
```

**Honest limit:** hash chaining is *tamper-evident*, not tamper-proof. An
attacker with write access can rewrite the tail unless the head is anchored
somewhere they do not control. That anchoring is not implemented — see
[Audit](../01-concepts/07-audit.md).

### AT-6 — Denial of service (T2)

| Vector | Control |
|---|---|
| Spawn unlimited runs | Per-tenant `MaxConcurrentRuns`, enforced at admission with `429` + `retry_after` |
| Burn all LLM quota | Per-tenant token bucket; guaranteed share for everyone else |
| Fork bomb | `pids.max` cgroup — *measured*: refused at 23 children against a limit of 24 |
| Memory bomb | `memory.max` cgroup — *measured*: OOM-killed, confirmed via `memory.oom_control` |
| Infinite loop | Wall-clock deadline held by the parent, which the payload cannot influence |
| Fill the disk | `ephemeral-storage` limits, `emptyDir` `sizeLimit`, tmpfs size caps, `RLIMIT_FSIZE` |
| Flood the event log | Per-run step and tool-call budgets |
| Occupy every worker | **Backpressure is expressed as not-scheduling**, never as a blocked worker |

That last row is the most important one in the table, and the easiest to get
wrong. If a worker blocked waiting for a throttled tenant's quota, one noisy
tenant would occupy the entire pool.

---

## 5. Explicit non-goals

Stating these matters as much as stating the controls.

| Not defended against | Why |
|---|---|
| **A malicious platform operator** | Root on the cluster is game over. Mitigated organisationally (RBAC, audit, separation of duties), not architecturally. |
| **Model weight extraction / provider compromise** | Outside the boundary. |
| **Side channels** (Spectre-class, cache timing) | gVisor and per-tenant node pools help; full defence needs dedicated hardware. |
| **The model being wrong** | Correctness of the agent's *reasoning* is the agent author's problem. We contain the blast radius of a wrong decision, not the wrongness. |
| **Physical / hypervisor compromise** | Cloud provider's responsibility. |
| **Denial of wallet via legitimate use** | Budgets bound it; a tenant that genuinely wants to spend its quota may. |

---

## 6. Detection, where prevention is not possible

Prevention is preferred everywhere it exists. Where it does not, these are the
signals that matter:

| Signal | Indicates | Response |
|---|---|---|
| `agentorch_tool_calls_total{decision="DENY"}` rising for one run | Injected or misbehaving agent | Inspect the run; consider freezing the tenant |
| Egress proxy denials | Exfiltration attempt | Investigate the agent's inputs |
| Falco/Tetragon on the sandbox pool | Escape attempt in progress | Kill, cordon, snapshot, page |
| gVisor blocked-syscall log | Payload probing the boundary | Correlate with the run |
| `agentorch_audit_write_failures_total > 0` | **We are executing without recording** | Page immediately |
| Audit chain verification failure | Tampering, or a bug in our own hashing | Freeze and investigate both |
| `agentorch_quota_denied_total` sustained | A tenant is being starved, or is abusive | Check weights |
| High-risk tool call immediately after ingesting untrusted content | Possible injection | Provenance tagging — **not built**, noted as a gap |

---

## 7. What keeps me up at night

Honest ranking of residual risk:

1. **No mTLS between platform components.** The gateway authenticates the *run
   token*, not the *caller*. A compromised pod in the platform namespace with
   the signing secret can impersonate any worker.
   → [SPIFFE/SPIRE](../01-concepts/05-secrets.md#7-workload-identity--the-gap).
2. **The audit chain head is not anchored externally.** Tamper-*evident* only
   against an attacker without sustained write access.
3. **gVisor is one implementation.** Its own bugs are a single point of failure
   for the escape story; the node-pool and credential-TTL controls are what
   bound that.
4. **The egress proxy is designed, not built.** The agent sandbox's empty netns
   covers the demonstrated path; the credentialed path is less constrained than
   the design describes.
5. **No provenance tagging on ingested content.** We cannot currently say "this
   tool call happened right after the agent read an untrusted document", which
   is the highest-signal injection heuristic available.

---

**Next:** [Linux isolation](../01-concepts/01-linux-isolation.md) — how the
sandbox controls in AT-2 actually work.
