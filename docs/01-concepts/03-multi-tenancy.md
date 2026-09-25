# Multi-tenancy from first principles

> **Prerequisite:** [Threat model](../00-problem/04-threat-model.md)
> **Read next:** [Authorization](04-authorization.md)

---

## 1. What enterprises actually mean

A tenant is a customer. When an enterprise buys a multi-tenant platform, the
assumption they are making — usually without writing it down — is:

> *Nothing we put into this system is observable by any other customer, under any
> failure mode, including your bugs.*

That last clause is the hard one. Isolation that depends on every developer
remembering to add a `WHERE tenant_id = ?` will eventually fail, because
codebases outlive the people who understood them.

## 2. The design principle: make the bug unwritable

There is a spectrum of how isolation can be enforced:

| Level | Mechanism | Failure mode |
|---|---|---|
| 1. Convention | "remember to filter by tenant" | someone forgets |
| 2. Review | a checklist | a reviewer misses it |
| 3. Runtime check | assert in a middleware | the check has a hole |
| 4. **Type/API shape** | **the function cannot express the bug** | requires a deliberate API change |
| 5. Physical | separate clusters per tenant | cost |

This system aims for **level 4** wherever it can, and states honestly where it
only reaches level 3.

The organising rule:

> **There is no function in this system that takes two tenant IDs.**

To leak tenant A's data into tenant B's response, you would first have to add a
parameter. That is a reviewable change, not an oversight.

---

## 3. The seven enforcement points

Isolation is not one check. It is the same invariant expressed seven times, so
that no single mistake is sufficient.

### 3.1 API — every route is tenant-scoped

Every handler goes through a resolver that maps the caller to a principal:

```go
type Principal struct {
    TenantID string
    User     string
    Operator bool   // may read across tenants; audited like everything else
}
```

and every per-run route goes through `mustRun`, which re-checks:

```go
if !p.Operator && run.TenantID != p.TenantID {
    // 404, not 403: a caller should not be able to probe which run ids
    // exist in another tenant.
    writeErr(w, http.StatusNotFound, "run not found")
    return types.Run{}, false
}
```

**Why 404 and not 403.** A `403 Forbidden` says *"this exists, and you may not
see it."* That is an oracle: an attacker enumerating IDs learns which ones are
real, which leaks volume, naming patterns and activity. A `404` says nothing at
all.

The same reasoning applies to error messages: a denial tells the agent *why* in
plain language (so it stops retrying), but never reveals anything about other
tenants.

**Verified:**

```
globex reading acme run -> HTTP 404
no key at all           -> HTTP 401
```

### 3.2 Store — the API shape forbids it

```go
GetRun(ctx, id string) (types.Run, error)
ListRuns(ctx, f RunFilter) ([]types.Run, error)   // RunFilter.TenantID
ListAudit(ctx, tenantID, runID string, limit int)
```

`RunFilter` has one `TenantID` field. There is no `ListRunsAcrossTenants`. The
operator view passes `""` deliberately and only after `Principal.Operator` was
checked at the API layer — which is level 3, not level 4, and is the weakest
link in this list. In a larger system this would become a distinct
`OperatorStore` type so the ordinary store *cannot* return cross-tenant rows.

### 3.3 Agent definitions — tenant-owned and digest-pinned

```go
type AgentDefinition struct {
    Digest   string   // sha256 of the canonical spec
    TenantID string
    Name     string
    Spec     Spec
}
```

Two properties:

1. A definition belongs to exactly one tenant; running another tenant's agent is
   rejected at run creation.
2. A run **pins the digest**, so permissions are immutable for the life of the
   run. See [Authorization](04-authorization.md#3-content-addressing-the-grant-set-cannot-move).

### 3.4 Workspaces — path layout plus two independent checks

```
<root>/<tenant>/<run>/
```

Tenancy is in the *path*, so a run is handed a directory it cannot name its way
out of — but only if path resolution is done correctly. Both checks are needed:

```go
// Check 1 (lexical): after cleaning, are we still under the base?
if full != base && !strings.HasPrefix(full, base+string(os.PathSeparator)) {
    return "", fmt.Errorf("path %q escapes the workspace", rel)
}
// Check 2 (symlinks): the agent may have created a symlink in a PREVIOUS tool
// call pointing at /etc or another tenant's directory.
if realParent, err := filepath.EvalSymlinks(parent); err == nil { … }
```

**Why the second check exists.** The lexical check alone is defeated by an agent
that, on tool call *n*, writes `ln -s /etc secrets` into its own workspace, and
on call *n+1* reads `secrets/shadow`. The path `secrets/shadow` contains no
`..` and is lexically inside the workspace. `EvalSymlinks` on the parent catches
it. We check the *parent* because the leaf may legitimately not exist yet on a
write.

`safeSegment` additionally guarantees a tenant ID can never become `..`, an
absolute path, or contain a separator.

### 3.5 Credentials — per-tenant root secrets

Each tenant gets its own HMAC root per credential reference:

```go
key := make([]byte, 32)
rand.Read(key)
p.Broker.AddRoot(t.ID, "github/token", "github_token", key)
```

A token minted for `acme` **fails verification** as `globex`, because the HMAC
was computed with a different key. Cross-tenant credential replay does not fail
at a policy check we might have forgotten — it fails at the cryptography.

The far end enforces it too: the demo GitHub API extracts the tenant from the
token, verifies against that tenant's root, and scopes `GET /pulls` to that
tenant's pull requests.

### 3.6 Sandboxes — per-tenant host UID ranges

`sandbox.Config.UIDBase` is the host uid that container uid 1 maps to. Giving
different tenants different bases means that even a mount-level mistake cannot
make one tenant's files readable by another: the *filesystem* refuses, not our
code.

Kubernetes sandbox pods additionally carry:

```yaml
labels:
  agentorch.io/tenant: <tenant>
  agentorch.io/workload-class: untrusted
```

so NetworkPolicy, quota and forensic queries can all select on tenancy.

### 3.7 Audit — one hash chain per tenant

```sql
PRIMARY KEY (tenant_id, seq)
```

Per-tenant chains, not one global chain. Two reasons:

| Reason | Detail |
|---|---|
| **Throughput** | Appends serialise behind the chain head. A global chain would serialise the whole fleet; per tenant, each chain sees a fraction |
| **Exportability** | A tenant-scoped export is **self-verifying** — you can hand a customer their chain and they can check it alone |

---

## 4. The resource dimension: noisy neighbours

Data isolation is necessary but not sufficient. A tenant that consumes all the
shared capacity has denied service to everyone else without seeing a byte of
their data.

| Shared resource | Isolation mechanism |
|---|---|
| LLM quota | Weighted max-min fairness — [Fairness](06-fairness.md) |
| Worker pool | `MaxConcurrentRuns` at admission + backpressure as *not scheduling* |
| Sandbox capacity | `ResourceQuota` on the sandbox namespace + `LimitRange` per pod |
| Database | Connection pool limits; per-tenant query patterns are indexed |
| Node CPU/memory | Guaranteed QoS (requests == limits) per sandbox pod |

**The critical one** is the second: backpressure must be expressed as *declining
to schedule*, never as a *blocked worker*. If a worker blocked waiting for a
throttled tenant's quota, one noisy tenant would occupy the entire pool — the
exact outage the fairness layer exists to prevent.

```go
// tick(): if every tenant is out of quota, this is not an error - there is
// simply nothing to do.
eligible = w.eligible(w.cfg.TypicalTokens)
if len(eligible) == 0 {
    return false, nil
}
```

---

## 5. Soft vs hard multi-tenancy

| | Soft (this system today) | Hard (not built) |
|---|---|---|
| Boundary | namespaces, policy, uid ranges | dedicated nodes or clusters per tenant |
| Shared kernel | yes (gVisor narrows it) | no |
| Cost | shared fleet | per-tenant floor cost |
| Suitable for | most enterprise tenants | regulated tenants, or after an incident |

The design supports moving a tenant to hard isolation without an architectural
change: sandbox pods already carry a tenant label and target a node pool by
`nodeSelector`, so a per-tenant pool is a scheduling change, not a redesign.
That is called out in
[DESIGN.md "What I cut"](../../DESIGN.md#8-what-i-cut-and-why-it-was-right-to-cut-it).

---

## 6. What could still go wrong

Honest residual risk:

| Risk | Level reached | Note |
|---|---|---|
| A new store method forgets its tenant filter | 3 (runtime) | No compiler enforcement. A `TenantScopedStore` wrapper type would fix it |
| `Principal.Operator` set wrongly | 3 | Operator access is the one legitimate cross-tenant path, and it is only as good as the key management |
| A shared cache keyed without tenant | — | No shared caches exist today; this is the classic way isolation is lost later |
| Timing side channels across tenants | not addressed | Needs hard isolation |
| A gVisor escape reaching another tenant's sandbox | mitigated | Tainted pool, no SA token, per-tenant uids, ≤60 s credentials |

### The test that would catch the first row

Not written, and it should be: a test that enumerates every exported `Store`
method by reflection and asserts each either takes a tenant ID or is explicitly
listed as operator-scoped. That is the kind of invariant worth mechanising,
because it is exactly the thing a future contributor will not know about.

---

**Next:** [Authorization](04-authorization.md) — how "an agent cannot call a
tool it was not granted" becomes structurally true.
