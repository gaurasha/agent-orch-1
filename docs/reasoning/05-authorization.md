# 05 · Authorization — where "may this agent do this" is decided

> Decision: **every tool call passes through a separate gateway process that
> reloads the run and its pinned agent definition from the store, evaluates a
> deny-by-default policy (lifecycle → budget → tool exists → tool granted →
> required args → parameter policy → human approval), and only then touches a
> credential or a sandbox.** Not a library in the worker, not a sidecar, not a
> policy engine bolted onto the model, not the model's own judgement.
>
> Diagrams: [03-tool-gateway](../architecture/03-tool-gateway.md) · Concept: [authorization](../01-concepts/04-authorization.md) · Related: [06-credentials](06-credentials.md), [10-prompt-injection](10-prompt-injection.md)

## Problem

The model emits `{"tool": "http.get", "args": {"url": "…"}}`. Something must
decide whether that happens. The constraints:

1. **The model's output carries no authority.** It was shaped by every byte in
   the context, some of which an attacker wrote (see [10](10-prompt-injection.md)).
   So the decision cannot be made by the model, and cannot trust anything the
   model says about itself.
2. **The worker process runs the loop and is therefore exposed** to the
   model's output; a bug there (a deserialisation flaw, a prompt that
   confuses the parsing) must not be an authorisation bypass.
3. **The decision must be un-bypassable**: if there is any path from "model
   said X" to "X happened" that skips the decision, the decision is
   decorative.
4. **Grants must not widen retroactively**: editing an agent's definition
   must not change what an already-running agent may do (the running agent
   may be the attacker's tool).
5. **Tool-level grants are not enough.** "May call `http.get`" must not mean
   "may GET anything": the first injection exfiltrates the workspace to an
   attacker's domain. Authorisation has to reach into *parameters*.
6. **Some calls need a human.** The blast radius of `http.post` to a payment
   API is not something a policy can bound; a person must see it.

## Options

| # | Option | Strongest case | Where it breaks | Who does it |
|---|---|---|---|---|
| A | **The model decides** ("system prompt says only use tools when appropriate") | zero engineering; the model is quite good at it | violates constraint 1 outright; every injection paper is a demonstration of this failing | far too many agent demos |
| B | **Authorization library inside the worker** | no network hop; simplest deployment | violates 2 and 3: the worker holds the credential to call the tool, so a compromised or buggy worker skips the check; nothing at the network layer prevents the worker from calling GitHub directly | most frameworks' "tool permissions" (a Python decorator is a suggestion) |
| C | **Sidecar / service mesh policy** (Envoy ext_authz, Istio AuthorizationPolicy) | the mesh enforces it; language-agnostic | mesh policies are per-*service*, not per-*run*; they cannot see "this run's pinned definition grants `pr` but not `auth`"; parameter policy is out of scope; the credential injection still needs a component | API-gateway-level coarse authz in many companies |
| D | **A general policy engine** (OPA/Rego, Cedar, Casbin, OpenFGA/Zanzibar) evaluated by the worker or a sidecar | expressive, auditable policy language; battle-tested | still constraint 2/3 unless it sits at a choke point the worker cannot bypass; Rego for "argv[0] ∈ allowlist and no denied substring and host suffix-matches" is more policy language than the problem needs; Zanzibar-style relationship authz answers "who can access what object", not "may this argv run" | Netflix (OPA), AWS (Cedar in Verified Permissions), Auth0/Okta FGA |
| E | **A separate gateway process on the hot path** (chosen) — the only component with egress and credentials; reloads truth from the store; deny by default; parameter policy; approval | constraints 1–6 all hold structurally: the worker has no route to third parties (NetworkPolicy), so the gateway is the *only* way a tool call can happen; policy is ~200 lines of Go with tests; the same process journals and audits | +1 RPC per tool call; the gateway is a hot-path dependency (PDB, 2 replicas); policy is code, not a DSL — changing it is a deploy | Bedrock AgentCore Gateway, MCP gateways (Docker MCP Gateway, Lasso, Portkey), this repository |
| F | **MCP server permissions / tool-server-side checks** | each tool server enforces its own rules | the tool server sees a request from "the agent", not the run's pinned grants; per-run policy needs the gateway anyway; MCP's authorization spec (OAuth 2.1) addresses *who* the client is, not *what this run may do with which parameters* | MCP ecosystem 2025 |
| G | **Capability tokens per tool call** (macaroons, biscuits, object capabilities) | the token *is* the authority; no central check at call time; attenuable | minting the token is the decision, so a minting service is the gateway by another name; verification at every tool server; caveat languages are their own policy DSL; excellent theory, heavier engineering | Google's internal systems (capability-ish), Fly.io (macaroons), CapTP-style research |

## Deep dive

### Why a network hop is the price of un-bypassability

Constraint 3 says the decision must be on every path. In-process (B) means
"on every path *the code takes*". A network hop plus a NetworkPolicy means "on
every path *that exists*": `agentd-egress` permits Postgres and the gateway
and nothing else, so a worker that wanted to skip the gateway has no route.
The hop costs ~1 ms in-cluster on a step that costs seconds; it buys the
difference between a policy and a boundary.

### Why the gateway reloads instead of trusting the request

The worker sends a JWT naming the run. The gateway *could* trust claims in the
token (tenant, grants). It does not: it reloads the run row and the
definition by the run's **pinned digest**. Three consequences:

* a cancelled run's straggler worker is refused (`run.terminal`) even though
  its token is still valid;
* a budget exhausted a second ago is enforced now, not at the token's expiry;
* editing the agent definition creates a *new* digest; the running agent
  keeps calling with the old one, so grants cannot widen retroactively
  (constraint 4). Content-addressing is what makes "pin" cheap: the digest is
  the identity.

Two point reads per call is the cost. A cache would reintroduce every one of
those windows.

### Why the order of checks matters

```
run.terminal → budget.exhausted → tool.unknown → tool.not_granted → args.missing
→ params.* → approval.required → args.invalid → default.granted
```

Cheapest and most-final first. A terminal run is refused before anything is
computed. Budget before grants because a poison agent looping on a *granted*
tool must be stopped without evaluating the grant. `tool.unknown` before
`tool.not_granted` so a hallucinated name is not reported as a policy denial
(which would confuse an operator reading the audit log). Argument validation
only for granted tools, so a denial is never masked as a schema error.
Approval last, because asking a human about a call that would have been
denied anyway is noise.

### Why parameter policy and not just grants

Constraint 5. The demo agent's policy:

```
http.get    allowed_hosts: [api.github.com]
github.cli  allowed_commands: [pr, issue, repo]   denied_arg_patterns: ["auth token"]
http.post   Dangerous → human approval, UnsafeRetry → "may have taken effect"
exec.*      no parameter policy — the sandbox IS the policy (no network, read-only root)
```

Plus always-on SSRF checks: scheme `http(s)` only; no loopback, link-local
(`169.254.169.254` is the cloud metadata service), private ranges or
`*.internal` hosts. These are the rules an injection has to defeat, and none
of them consults the model.

### Why "contain, don't detect"

A classifier that flags injections is option A with extra steps: it makes a
judgement on attacker-controlled text. It has a job — *observability* (the
audit log records denials; a spike in `tool_calls_total{decision=DENY}` is the
signal) — but not as the control. See [10](10-prompt-injection.md) for the
benchmarks (AgentDojo, InjecAgent) showing detection rates that no one would
accept as a security boundary.

### Why approval is a state, not a modal dialog

`NEEDS_APPROVAL` parks the run (`WAITING_APPROVAL`) and releases the worker.
The human approves via the API; the run re-queues; the worker retries the
**same** idempotency key with `approved = true`. The approval is an event in
the log, so it is auditable, replayable, and survives every process in the
system dying in between.

## State of the art

* **AWS Bedrock AgentCore Gateway** (2025): a managed gateway that fronts
  tools/MCP servers with authorization and credential injection — the same
  choke-point shape as here, as a product.
* **MCP authorization** (2025 spec): OAuth 2.1 between client and server;
  "tool annotations" (`destructiveHint`, `readOnlyHint`) are *hints* the
  server declares and the client may ignore — the spec is explicit that they
  are not a security boundary. Per-run parameter policy is out of its scope.
* **Anthropic Claude Code permissions**: allow/deny rules per tool with
  pattern matching on arguments (`Bash(git *)`), plus a sandbox and a network
  allowlist — parameter-level policy at the client.
* **OpenAI Agents SDK guardrails** and **Codex approval modes**: input/output
  guardrails as code, approval prompts for commands outside a sandbox.
* **Google CaMeL** (2025): capability-based control flow — a privileged model
  plans, a quarantined model reads untrusted data, and capabilities on data
  values determine what tools may receive them. The most principled published
  design; it is the *parameter policy* idea generalised to data provenance.
* **OPA at Netflix / Cedar at AWS**: general policy engines for service
  authorization; the natural home for the *tenant-level* policy if this
  gateway's Go rules grew past a few hundred lines.

## Documented issues

* **GitHub MCP prompt-injection data leak** (Invariant Labs, May 2025): an
  issue in a public repo instructed an agent with a broad-scope token to leak
  private repo data via a PR. Every element is a constraint above: model
  output carried authority (A), the token's scope exceeded the task, no
  parameter policy on where data could go.
* **Supabase MCP / Cursor SQL leak** (2025): an agent with a service-role key
  executed injected SQL. Same shape: credential in the agent's reach, no
  per-call policy.
* **Amazon Q Developer extension** (July 2025): a malicious PR injected a
  prompt into the released extension telling the agent to wipe the user's
  environment — a supply-chain path to the model's instructions, which no
  runtime authorisation can fix but which per-call parameter policy and
  approval for destructive tools bound.
* **Replit agent deleting a production database** (July 2025): the agent ran
  destructive commands during a code freeze; the vendor's response was
  dev/prod separation and one-click restore — i.e. *containment and
  approval*, not a smarter model.
* **EchoLeak (Microsoft 365 Copilot, CVE-2025-32711)**: zero-click
  exfiltration via a crafted email and an image-fetch — the exfiltration
  channel was an allowed outbound fetch; the fix was closing the channel.

Each of these is an argument for E over A/B and for parameter policy over
tool-level grants. [14-industry-learnings](14-industry-learnings.md) has the
links and the mapping.

## Evidence in this repository

* S14 (prompt injection contained: four rules exercised in one run), S15
  (budgets enforced), S16 (tenancy holds) in [safety proofs](../04-evidence/02-safety-proofs.md).
* Every denial is audited with `rule` and `args_sha256`; `TestAudit_ChainIsTamperEvident`.
* The `Tool.MarshalJSON` schema/backend split — the model cannot learn a
  backend detail because the type cannot serialise it.

## Would reverse if

* tool-call latency budgets fell below a few milliseconds end to end (the RPC
  would matter) — then the gateway becomes a *library with a network policy
  that blocks the worker's egress*, which keeps constraint 3 by other means;
* policy grew beyond what a few hundred lines of Go can express — move the
  tenant-level rules to OPA/Cedar, keep the choke point;
* tool servers became capability-verifying (macaroon-style) — the gateway
  becomes the minter, and tool servers verify; the choke point moves but
  does not disappear.

## References

* Dennis & Van Horn, *Programming semantics for multiprogrammed computations* (1966; capabilities) — https://dl.acm.org/doi/10.1145/365230.365252
* Hardy, *The Confused Deputy* (1988) — https://dl.acm.org/doi/10.1145/54289.871709
* Miller et al., *Capability myths demolished* — https://srl.cs.jhu.edu/pubs/SRL2003-02.pdf
* Debenedetti et al., *Defeating prompt injections by design* (CaMeL, 2025) — https://arxiv.org/abs/2503.18813
* Model Context Protocol, *Authorization* — https://modelcontextprotocol.io/specification/2025-06-18/basic/authorization · *Tool annotations* — https://modelcontextprotocol.io/specification/2025-06-18/server/tools
* Anthropic, *Claude Code permissions* — https://docs.claude.com/en/docs/claude-code/settings#permissions
* OpenAI, *Agents SDK guardrails* — https://openai.github.io/openai-agents-python/guardrails/
* AWS, *Bedrock AgentCore Gateway* — https://docs.aws.amazon.com/bedrock-agentcore/latest/devguide/gateway.html
* Open Policy Agent — https://www.openpolicyagent.org/docs/ · Cedar — https://www.cedarpolicy.com/ · OpenFGA — https://openfga.dev/
* Envoy, *External authorization* — https://www.envoyproxy.io/docs/envoy/latest/configuration/http/http_filters/ext_authz_filter
* Invariant Labs, *GitHub MCP exploited: accessing private repositories via MCP* — https://invariantlabs.ai/blog/mcp-github-vulnerability
* OWASP, *Top 10 for LLM Applications* (LLM01 Prompt Injection, LLM06 Excessive Agency) — https://owasp.org/www-project-top-10-for-large-language-model-applications/
