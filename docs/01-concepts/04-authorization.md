# Authorization from first principles

> **Prerequisite:** [Multi-tenancy](03-multi-tenancy.md)
> **Read next:** [Secrets](05-secrets.md)
> **Code:** [`internal/authz/`](../../backend/internal/authz/) · [`internal/gateway/`](../../backend/internal/gateway/)

---

## 1. The confused deputy

In 1988 Norm Hardy described
[the confused deputy problem](https://cap-lore.com/CapTheory/ConfusedDeputy.html):
a program with legitimate authority is tricked into using it on someone else's
behalf. His example was a compiler that could write to a billing file; a user
asked it to write output *to* that file, and the compiler — having the right to
do so — complied.

An agent platform is a confused deputy generator. The platform holds real
authority (tenant credentials, cluster access), and takes instructions from a
model that takes instructions from *anything in its context*, including a PDF
uploaded by an attacker.

Classic access control asks *"does this **user** have permission?"*. That is the
wrong question here, because the agent is acting for a user who really does have
permission — and is being steered by someone who does not.

The right question is:

> *"Is this **specific action**, with **these specific arguments**, within the
> capability set this **specific run** was granted at creation?"*

That reframing is the whole design.

---

## 2. Capability security

A [capability](https://en.wikipedia.org/wiki/Capability-based_security) is an
unforgeable reference that *is* the authority — possession is permission. The
relevant properties:

| Property | Consequence here |
|---|---|
| Unforgeable | An agent cannot invent a capability it was not given |
| No ambient authority | There is no "the platform can do X" that an agent inherits |
| Attenuable | A capability can be narrowed when delegated |
| Revocable | Withdrawn without touching an ACL somewhere else |

The contrast with ACLs matters. With an ACL, the question is *"who are you, and
what may you do?"* — and the deputy's identity is what gets checked, so the
deputy's authority is what gets used. With capabilities, the question is *"what
did you present?"*, and the agent can only present what it was handed.

In this system the capability is the **grant set pinned in the run's agent
definition digest**.

---

## 3. Content addressing: the grant set cannot move

```go
type AgentDefinition struct {
    Digest   string  // sha256 of the canonical spec
    TenantID string
    Name     string
    Spec     Spec    // system prompt, model, TOOLS, tool params, budget, priority
}
```

`digest = sha256(canonical_json(spec))`. Registering the same spec twice is a
no-op — the digest *is* the content, so a second write of the same digest would
mean a hash collision, not an update.

A run stores `def_digest`, and **the gateway authorizes against that digest**,
never against "the current definition named X".

### Why this matters

```
10:00  agent "deploy-bot" is defined with tools [fs.read]
10:05  a run starts, pinning digest sha256:abc…
10:07  someone edits deploy-bot to add [kubectl.apply]  → new digest sha256:def…
10:09  the 10:05 run asks for kubectl.apply             → DENIED
```

Without content addressing, that last line is **allowed**, and an auditor asked
"what was agent X permitted to do at 10:05?" has no answer, because the
definition was mutated.

This is the container-image model applied to agents, and it buys the same
property: *what ran is reconstructible*.

### Canonicalisation is where this goes wrong

`{"a":1,"b":2}` and `{"b":2,"a":1}` are the same value and must hash
identically, or one agent silently becomes two. [`internal/canon`](../../backend/internal/canon/canon.go)
sorts keys recursively, normalises number formatting, and round-trips through
`json.Number` so `1` and `1.0` do not diverge. Compare
[JCS (RFC 8785)](https://www.rfc-editor.org/rfc/rfc8785).

---

## 4. Where the check lives — three layers, one authority

| Layer | Mechanism | Is it security? |
|---|---|---|
| 1. Model context | Only granted schemas are sent | **No** — cost and noise reduction |
| 2. **Gateway** | **Checked against the pinned digest** | **Yes — authoritative** |
| 3. Provider | Fine-grained token scoped to one repo; IAM role with one action | Blast-radius bound |

### Layer 1 is not security, and saying so matters

Sending the model only the tools it may call is worth doing: fewer tokens, less
audit noise, fewer pointless round trips. But a model can emit **any** tool name,
and a prompt-injected one actively will. Anything that relies on the model not
trying is not a control.

### Layer 2 is authoritative because it cannot be bypassed

Not because we ask nicely — because of NetworkPolicy:

```yaml
# agentd-egress: workers may reach Postgres and the tool gateway. Nothing else.
egress:
  - to: [{podSelector: {matchLabels: {app.kubernetes.io/name: postgres}}}]
    ports: [{protocol: TCP, port: 5432}]
  - to: [{podSelector: {matchLabels: {app.kubernetes.io/name: toolgateway}}}]
    ports: [{protocol: TCP, port: 8081}]
```

A *fully compromised worker* has no route to GitHub, to the internet, or to the
cloud metadata service. "Skip the gateway" is not an available action.

### Layer 3 bounds the damage when layer 2 is wrong

A [GitHub fine-grained token](https://docs.github.com/en/authentication/keeping-your-account-and-data-secure/managing-your-personal-access-tokens)
scoped to one repository means that a policy bug in our gateway costs one
repository, not the organisation. Necessary, but **not sufficient alone**: it
gives no central audit trail and delegates correctness to N third parties
configured by N different people.

---

## 5. The evaluation order, and why each step is where it is

```go
func (e *Engine) Evaluate(req Request) Decision
```

| # | Check | Why here |
|---|---|---|
| 1 | Run is not terminal | A cancelled run must not keep reaching the outside world through a straggler worker |
| 2 | **Budget** | Stop a runaway *before* doing parsing work on its behalf |
| 3 | Tool exists | An unknown name is usually a hallucination, occasionally a probe for undocumented internal tools |
| 4 | **Tool is granted** by the pinned spec | The core capability check |
| 5 | Required arguments present | Cheap structural validation |
| 6 | **Parameter policy** | Where tool-level grants stop being enough |
| 7 | Human approval | The last gate for high-blast-radius actions |

Ordered by cheapness and by how badly a failure would hurt. Note that **no
credential has been touched** by the time a decision is reached — a denied call
never causes a secret to be minted at all.

### Fail closed, everywhere

Unknown tool → deny. Unparsable URL → deny. Missing definition → deny. Expired
token → deny. Unknown tenant in the limiter → deny. There is no path where an
error produces an allow.

---

## 6. Parameter policy: why tool-level grants are insufficient

"May call `http.get`" must not mean "may GET anything", or the first prompt
injection exfiltrates the workspace to an attacker's domain.

```go
type ParamPolicy struct {
    AllowedHosts      []string  // exact, or ".suffix.match"
    AllowedCommands   []string  // argv[0] allowlist for exec/CLI tools
    DeniedArgPatterns []string  // substrings forbidden anywhere in the arguments
    RequiresApproval  bool      // park the run for a human
}
```

### The `github.cli` example — the whole point in one policy

```go
"github.cli": {
    DeniedArgPatterns: []string{"auth token", "auth status", "secret", "variable"},
},
```

This is what makes a **broad** CLI grant safe. The agent may open pull requests
(the job it exists to do) and may not ask the CLI for its own credential or
touch repository secrets. Without parameter policy the choice would be binary:
grant `gh` entirely, or not at all.

`flattenArgs` renders nested values recursively before matching, so a denied
pattern cannot be hidden one level down inside an object or array.

### SSRF defence, layered

Even when an allowlist would already cover it:

```go
if ip := net.ParseIP(host); ip != nil && isBlockedIP(ip) {
    return "", deny("params.ssrf",
        "%s is a link-local, loopback or private address …", host)
}
if host == "metadata.google.internal" || strings.HasSuffix(host, ".internal") { … }
```

Blocked: loopback, link-local (**including 169.254.169.254**), RFC1918, CGNAT,
IPv6 unique-local, and metadata hostnames. Scheme is restricted to `http`/`https`
— without that, `file://` and `gopher://` become read primitives.

**Why duplicate what the allowlist does?** Because a misconfigured allowlist
entry like `*` should not immediately expose the node's metadata service. Defence
in depth means the layers overlap on purpose.

### Redirects are not followed

```go
CheckRedirect: func(req *http.Request, via []*http.Request) error {
    return http.ErrUseLastResponse
},
```

Following redirects would let an *allowed* host bounce a call to a *denied* one,
defeating the host allowlist entirely. This is a common and easily-missed hole.

---

## 7. Telling the model why

```go
// Reason is shown to the operator in the audit log AND returned to the agent.
// Telling the model precisely why it was denied is deliberate: it stops the
// loop where an agent retries the same forbidden call twenty times.
```

A real denial:

```
tool "http.post" is not in this agent's granted tool set (granted: fs.write, fs.read, exec.bash)
```

This is a deliberate information-disclosure trade:

| Disclosing the reason | Withholding it |
|---|---|
| Agent stops retrying — real cost and availability saving | Agent retries until its budget dies |
| Agent can explain the limitation to its user | User sees an opaque failure |
| Reveals this agent's own grant set | Reveals nothing |

The disclosed information is the agent's **own** configuration, which it could
infer from which calls succeed anyway. Nothing about other tenants, other
agents, or platform internals crosses that boundary.

---

## 8. Human-in-the-loop

Some actions should not be autonomous regardless of policy.

```
model emits a tool call marked RequiresApproval / Dangerous
   → gateway returns 202 Accepted + NEEDS_APPROVAL
   → worker appends APPROVAL_NEEDED, sets state WAITING_APPROVAL,
     RELEASES the lease
   → … agent costs nothing while parked …
   → operator POSTs /v1/runs/{id}/approve
   → APPROVAL_GIVEN event records WHO approved and WHEN
   → run returns to QUEUED; the call proceeds with approved=true
```

Two things worth noting: the approval is itself an **audited event** with an
attributed human, and the parked run holds no resources — the same property that
makes [`WAITING_HUMAN`](02-durable-execution.md) cheap.

---

## 9. Defence in depth, summarised

For a single tool call, the controls that must *all* fail:

```
1. the tool was never granted in the pinned spec        → DENIED
2. the worker's local pre-check                          → DENIED
3. the gateway's grant check                             → DENIED
4. the tool's JSON schema validation                     → DENIED
5. parameter policy (host / argv / denied patterns)      → DENIED
6. SSRF address and scheme checks                        → DENIED
7. human approval, where required                        → PARKED
8. NetworkPolicy (worker cannot reach anything else)     → NO ROUTE
9. the sandbox's empty netns (for exec tools)            → ENETUNREACH
10. provider-side token scoping                          → 403 from them
```

**Verified** in the injected-agent scenario: four attack routes, all refused,
with the run still completing cleanly rather than crashing.

---

## 10. What is not built

| Gap | Impact |
|---|---|
| **A real policy language** (OPA/Rego, Cedar) | Policy is Go structs. Fine for a PoC; production wants externally-authored, reviewable, testable policy with its own lifecycle |
| **Attenuated delegation** | No sub-agent spawning, so no need yet — but if an agent can spawn an agent, the child's grants must be a *subset* and the quota must be inherited. Non-trivial (cycle detection, transitive authorization) |
| **Per-call rate limits per tool** | A granted tool can be called up to the budget; no "5 emails per hour" |
| **Time-of-day / approval-window constraints** | No temporal dimension in policy |
| **Provenance tagging** | We cannot currently say "this call happened right after ingesting untrusted content", which is the highest-signal injection heuristic available |

---

## References

- Hardy, [*The Confused Deputy*](https://cap-lore.com/CapTheory/ConfusedDeputy.html)
- [Capability-based security](https://en.wikipedia.org/wiki/Capability-based_security)
- [OWASP Top 10 for LLM Applications](https://owasp.org/www-project-top-10-for-large-language-model-applications/) — LLM06 Excessive Agency
- [Google Zanzibar](https://research.google/pubs/pub48190/) — centralised authorization at scale
- [JCS, RFC 8785](https://www.rfc-editor.org/rfc/rfc8785) — canonical JSON

---

**Next:** [Secrets](05-secrets.md) — how a credential is used without ever being
disclosed.
