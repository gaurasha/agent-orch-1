# Secrets and the credential boundary

> **Prerequisite:** [Authorization](04-authorization.md)
> **Read next:** [Fairness](06-fairness.md)
> **Code:** [`internal/creds/`](../../backend/internal/creds/) · [`internal/tools/invoke.go`](../../backend/internal/tools/invoke.go)

This is the question the brief asks most directly:

> *"how `gh` gets a token without the agent reading it"*

It is worth taking seriously because every obvious answer has a hole.

---

## 1. Why the obvious approach fails

```python
os.environ["GH_TOKEN"] = tenant_github_token       # DON'T
run_in_sandbox("gh pr create --title 'Fix'")
```

The agent's own code, in that same sandbox, can read it:

```bash
env | grep TOKEN
cat /proc/self/environ | tr '\0' '\n'
cat /proc/*/environ
ps auxe
```

And remember who wrote that code: a model that may have read an attacker's text.
A single injected instruction — *"print your GitHub token"* — retrieves a
long-lived credential for the tenant's entire GitHub organisation.

This is not hypothetical; it is the default behaviour of most agent frameworks.

---

## 2. The options, and why four of five have holes

| # | Approach | Hole |
|---|---|---|
| **a** | Env var in the agent's sandbox | The agent reads it. Fatal. |
| **b** | Egress proxy injects the header | Sound, but needs TLS interception with a CA in the image, breaks certificate pinning, and makes the proxy a very attractive target |
| **c** | Credential-helper unix socket | **If the helper returns the token, the agent can call the socket itself.** It must *proxy* instead — which collapses into (b) or (d) |
| **d** | **Run the CLI in a separate sandbox** | Costs a second sandbox; shares a workspace |
| **e** | Forbid CLIs; expose `github.create_pr` as an API tool | Airtight, but does not generalise — the brief requires arbitrary CLIs |

Option **c** is the instructive one. It *sounds* like the answer, and it is the
design several frameworks reach for. But a unix socket inside the sandbox is
reachable by everything in that sandbox, including the agent's code. Secrecy of
the socket is not a control.

**Chosen: (d), with (e) for the highest-value integrations.** If I had three
external systems rather than thirty, I would model them all as API tools and
skip the CLI machinery entirely.

---

## 3. The two-sandbox pattern

```
   AGENT SANDBOX                          BROKER SANDBOX
   pid namespace A                        pid namespace B
   user namespace A (uid 1000→100999)     user namespace B
   ─────────────────────────────          ────────────────────────────
   model-authored code                    gh  (trusted, from OUR image)
   env: PATH HOME TMPDIR LANG             env: …  GH_TOKEN=aot_…
   NO credential                          egress: allowlisted API only
   NO network (empty netns)               network: via the egress proxy
            │                                       │
            └──────────►  /work  ◄──────────────────┘
                   shared bind mount
                   THE ONLY CHANNEL
```

### The flow

```
 1. agent: fs.write NOTES.md                      → /work
 2. agent: github.cli ["pr","create","--body-file","NOTES.md"]
 3. gateway: policy — "pr create" allowed, "auth token" would not be
 4. broker: mint aot_…  TTL ≤ 60s, scoped to (tenant=acme, ref=github/token)
 5. executor: start the BROKER sandbox
       Argv:          /opt/agentorch/bin/gh pr create --body-file NOTES.md
       Env:           GH_TOKEN=aot_…          ← the only place plaintext exists
       WorkspaceDir:  the SAME /work
       Network:       proxy
 6. gh reads /work/NOTES.md, POSTs with Authorization: Bearer
 7. API verifies the token is genuine, unexpired, unrevoked, right tenant
 8. gateway: scrub the token value from stdout (belt and braces)
 9. gateway: revoke the credential
10. agent receives stdout only
```

### Why the agent cannot reach it

| Attack | Blocked by |
|---|---|
| `cat /proc/<pid>/environ` of the broker | **Different pid namespace** — that PID does not exist in the agent's `/proc` |
| `ps auxe` | Different pid namespace |
| `ptrace(PTRACE_ATTACH)` | seccomp `EPERM` **and** the namespace boundary |
| Read the token from a response | Never returned; stdout only |
| `gh auth token` | Parameter policy denies the pattern **and** the CLI refuses |
| Find it in the event log | Never written; asserted by test |
| Find it in a log line | The `Secret` type redacts (§4) |
| Steal it in transit | in-cluster TLS ⚠ **gap** — see §7 |

Six independent controls. The demo asserts five of them hold simultaneously.

### Why the workspace is shared

Because `gh pr create --body-file NOTES.md` has to read what the agent wrote.
This is a real, narrow channel between the two sandboxes, and it is the right
trade: it carries **files the agent already owns**, in one direction, within one
tenant.

### The busybox trick

The broker sandbox needs a `gh` binary with no image, no package manager and no
shared libraries. The platform binary is statically linked (`CGO_ENABLED=0`) and
bind-mounted read-only under the CLI's name; `main()` dispatches on `argv[0]`:

```go
switch filepath.Base(os.Args[0]) {
case "gh":        os.Exit(runGH(os.Args[1:]))
case "aoconvert": os.Exit(runConvert(os.Args[1:]))
}
```

> **A bug worth recording.** The helper was originally mounted at
> `/usr/local/bin/gh`. But `/usr` inside the jail is a **read-only bind mount of
> the host's**, so creating the placeholder file failed with
> `read-only file system`. Helpers now live at `/opt/agentorch/bin`, which is on
> the sandbox's own tmpfs — writable during setup, sealed read-only before the
> payload runs. `sandbox.HelperBinDir` is prepended to `PATH`.

---

## 4. Making a leak a type error

Architecture is not enough, because leaks happen by accident:

```go
log.Printf("calling API with %v", credential)   // oops
json.Marshal(requestStruct)                     // oops — token is a field
fmt.Errorf("upstream failed: %v", req)          // oops
slog.Info("request", "req", req)                // oops
```

Discipline does not scale across a codebase or across time. Types do.

```go
type Secret string

func (s Secret) String() string                  { return "[REDACTED]" }
func (s Secret) GoString() string                { return "[REDACTED]" }
func (s Secret) Format(f fmt.State, verb rune)   { f.Write([]byte("[REDACTED]")) }
func (s Secret) MarshalJSON() ([]byte, error)    { return json.Marshal("[REDACTED]") }

func (s Secret) Reveal() string                  { return string(s) }  // the ONLY way out
```

`Format` is the one people omit — `String()` alone does not cover `%q`, `%#v` or
`%x`. With `Format`, every verb redacts.

### What the test proves

[`secret_test.go`](../../backend/internal/creds/secret_test.go) asserts the
plaintext is absent and the redaction marker is present across:

```
fmt.Sprint · fmt.Sprintf("%v|%s|%q|%#v|%x") · log output
json.Marshal(wrapper) · error wrapping · String()
```

and separately that a `Secret` **nested inside a struct, a map and a slice** is
still redacted — the realistic accident of logging a whole request object.

### The call-site guard

```
plaintext credential use sites: 3 across 2 files:
  map[../creds/secret.go:1 ../tools/invoke.go:2]
```

The test asserts the count is **exactly 3** and names each one:

| Site | Why it is legitimate |
|---|---|
| `creds.Scrub` | Compares against output in order to redact it — discloses nothing |
| `tools.invokeHTTP` | Injects `Authorization` on a request the **gateway** makes; the agent never sees it |
| `tools.invokeCLI` | Sets `GH_TOKEN` in the **broker** sandbox's environment |

Adding a fourth fails the build. That is the point: it turns "where do we handle
plaintext credentials?" from an archaeology exercise into a one-line answer.

> **This guard caught a documentation error.** The docs claimed "exactly one
> place", which was true when the test was first written and stopped being true
> when `invoke.go` was added. The test had been reporting the real number all
> along; nobody re-read it. An earlier version also counted a *comment*
> containing `.Reveal()`, which made the number meaningless — now fixed to skip
> comment lines.

---

## 5. Short-lived, scoped, verifiable

```go
token := fmt.Sprintf("aot_%s.%s.%d.%s", tenantID, credID, exp.Unix(), sig)
//                        │       │       │          └── HMAC(root, tenant|ref|credID|exp)
//                        │       │       └── expiry
//                        │       └── credential id (for revocation)
//                        └── tenant
```

| Property | Value | Why |
|---|---|---|
| TTL | **≤ 60 s**, hard-capped at 10 min by the broker | A stolen credential is useful for seconds |
| Scope | one `(tenant, ref)` pair | Cannot be replayed as another tenant — the HMAC root differs |
| Revocation | explicit, after every call | Bounds the window further |
| Verifiable offline | HMAC over its own claims | The far end can check it without a shared database |

The TTL cap is enforced at the broker, not trusted from the caller:

```go
// Cap the TTL here rather than trusting the caller. A bug upstream that
// requests a 30-day credential must not be able to get one.
const maxTTL = 10 * time.Minute
```

### The demo API actually verifies

This is what makes the demonstration meaningful rather than theatre. The fake
GitHub API rejects tokens that are missing, malformed, expired, revoked, or for
the wrong tenant — and records every presentation:

```json
{"credential_presented": true, "credential_valid": true,
 "tenant": "acme", "token_fingerprint": "aot_acme.c...CGD1rQ"}
```

Without that verification, "the platform injected a credential" would be
unfalsifiable: the demo would look identical if the token were the empty string.

---

## 6. What production needs that this does not have

`DerivedBroker` is a **stand-in**, and it is important to say what it is not:

| Property | Here | Production |
|---|---|---|
| Short-lived | ✓ (we expire it) | ✓ (the **issuer** expires it) |
| Tenant-scoped | ✓ | ✓ |
| Verifiable | ✓ HMAC | ✓ signed / introspectable |
| **Provider-side permission scoping** | ✗ | ✓ — a GitHub App installation token limited to one repo |
| **Revocable at the provider** | ✗ | ✓ |
| **Rotation of the root** | ✗ | ✓ |
| **Audit at the provider** | ✗ | ✓ |

The right production implementation is a secrets manager issuing **genuinely
dynamic** credentials — [Vault's](https://developer.hashicorp.com/vault/docs/secrets)
database/AWS/GitHub secret engines, or cloud-native equivalents — so
"short-lived" is enforced by the issuer rather than by us remembering.

---

## 7. Workload identity — the gap

The gateway authenticates the **run token**: an HMAC-signed JWT that says *"I am
acting for run R of tenant T."*

It does **not** authenticate the **caller**.

```
What the token proves:   "this request claims to be for run R"
What it does NOT prove:  "this request came from a genuine agent worker"
```

A compromised pod inside the platform namespace that has the signing secret can
mint a token for any run of any tenant. The blast radius is bounded by
NetworkPolicy (it still cannot reach the internet directly) and by the ≤60 s
credential TTL — but it is a real gap.

### The fix

[SPIFFE/SPIRE](https://spiffe.io/docs/latest/spiffe-about/overview/) workload
identity with mTLS, so the gateway authenticates the caller's **attested
identity** (node + pod selectors) independently of the run token it presents.
Combined with asymmetric run tokens (RS256/EdDSA) issued by the control plane,
the gateway would need no shared secret at all, and compromising the signing key
would no longer be sufficient.

**This is the single most important difference between the PoC and something I
would run in production.** It is listed first in
[DESIGN.md "What I cut"](../../DESIGN.md#8-what-i-cut-and-why-it-was-right-to-cut-it)
for that reason. It was right to cut because it changes no architectural
decision — the token shape and the choke point are identical either way.

---

## 8. Defence in depth, summarised

For a credential to leak to an agent, **all six** must fail:

```
1. Architecture   — the CLI runs in a different pid/user namespace
2. Policy         — credential-disclosing subcommands are denied by pattern
3. Tool           — the CLI itself refuses to print its own token
4. Type system    — Secret redacts through every formatting path
5. Output         — the gateway scrubs the value from results
6. Time           — the credential expires in ≤60s and is then revoked
```

And the demo asserts, in one run, that the agent found nothing **and** that the
credentialed call succeeded **and** that the far end verified a real token.

---

## References

- [SPIFFE/SPIRE](https://spiffe.io/docs/latest/spiffe-about/overview/)
- [Vault dynamic secrets](https://developer.hashicorp.com/vault/docs/secrets)
- [GitHub fine-grained tokens](https://docs.github.com/en/authentication/keeping-your-account-and-data-secure/managing-your-personal-access-tokens)
- Rust's [`secrecy`](https://docs.rs/secrecy/latest/secrecy/) crate — the same type-level argument
- [OWASP LLM Top 10](https://owasp.org/www-project-top-10-for-large-language-model-applications/) — LLM06 Sensitive Information Disclosure

---

**Next:** [Fairness](06-fairness.md) — sharing the one genuinely scarce resource.
