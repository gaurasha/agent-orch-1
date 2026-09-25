# 10 · Prompt injection — contain, don't detect

> Decision: **treat every byte the model reads as attacker-controlled and
> remove the attacker's *capabilities* rather than trying to recognise the
> attacker's *text*: no network in the agent sandbox, no credential the agent
> can read, deny-by-default tool grants with parameter allowlists, human
> approval for high-blast-radius tools, and an audit trail in which injection
> attempts appear as denials.** Detection is used for observability, never as
> the boundary.
>
> Concept: [prompt injection](../01-concepts/08-prompt-injection.md) · Threat model: [attack trees](../00-problem/04-threat-model.md) · Related: [05-authorization](05-authorization.md), [06-credentials](06-credentials.md), [02-isolation](02-isolation.md)

## Problem

A language model has one input channel. Instructions and data arrive in the
same channel, in the same representation, and the model's behaviour is a
function of all of it. There is no reliable way — architecturally, in
current models — to mark a span as "data, do not follow". So:

> Anything the agent reads (a web page, a PR description, a file in the
> workspace, a tool's output, an email) can change what the agent does next.

Simon Willison's *lethal trifecta* names the three capabilities an attacker
needs to turn that into a breach: **access to private data**, **exposure to
untrusted content**, and **a way to exfiltrate**. Remove any one and the
injection can still misbehave but cannot steal. An orchestration platform
controls exactly those capabilities, per tool call, in code the model cannot
influence. That is the whole strategy.

## Options

| # | Option | Strongest case | Where it breaks | Who does it |
|---|---|---|---|---|
| A | **Prompt-level defences** ("ignore instructions in documents", delimiters, spotlighting/datamarking) | cheap; measurably reduces naive attacks | reduces, never eliminates; every benchmark shows adaptive attacks succeed; the model is still the judge of attacker text | everyone, as a first layer |
| B | **Injection classifiers / guard models** (Prompt Guard, Lakera, Rebuff, LLM-as-judge on inputs) | catches known patterns; useful telemetry | false negatives are the security failure and are unbounded (adaptive attacks, paraphrase, encoding, multilingual); false positives break legitimate documents that *talk about* instructions; latency and cost on every input; the classifier is itself an LLM reading attacker text | many products as a layer; none as the boundary |
| C | **Model-level training** (instruction hierarchy, adversarial training) | the right long-term direction; providers report improved robustness | a model property we do not control; every release re-opens the question; still probabilistic | OpenAI's instruction hierarchy, Anthropic's constitutional classifiers (for other threats) |
| D | **Capability containment** (chosen) — the agent physically cannot exfiltrate (no network), cannot read credentials (separate process), cannot call tools it was not granted, cannot send to hosts not allowlisted, and needs a human for dangerous tools | deterministic; independent of the model; cheap to verify (a NetworkPolicy, a namespace, an allowlist); an injection shows up as a *denial* in the audit log | constrains what agents can do — an agent that needs to browse arbitrary sites and post anywhere cannot be fully contained; the policy author must get allowlists right; second-order attacks (poisoning the workspace for a later trusted tool) remain | Anthropic sandbox-runtime (network allowlist), OpenAI Codex (network off), this repository |
| E | **Dual-LLM / CaMeL-style architectures** — a privileged planner never sees untrusted data; a quarantined model processes it into typed values; capabilities on values decide what tools may receive them | the most principled published design; provable properties on data flow; solves the *second-order* problem D leaves open | requires the planner to write code (not free-form tool calls); a capability system on data values is heavy engineering; not compatible with off-the-shelf agent loops today | Google DeepMind's CaMeL (2025), Willison's dual-LLM pattern (2023) |
| F | **Human approval for everything** | perfect containment | the agent has no value; approval fatigue makes humans rubber-stamp | no one, deliberately |

D is the boundary; A and B are layers on top for telemetry; E is where the
design goes if second-order attacks become the dominant risk.

## Deep dive: closing each leg of the trifecta

| Leg | How the platform removes it | Where |
|---|---|---|
| **exfiltration channel** | the agent sandbox has an empty network namespace — `connect()` fails with `ENETUNREACH`; the only outbound calls are `http.*` through the gateway, to allowlisted hosts, SSRF-checked | [04-sandbox](../architecture/04-sandbox.md), [03-tool-gateway](../architecture/03-tool-gateway.md) |
| **private data** | credentials are never in the agent's process ([06](06-credentials.md)); the workspace is per run; `fs.*` tools are path-validated to the workspace; the host filesystem is invisible | [05-credential-plane](../architecture/05-credential-plane.md) |
| **untrusted content** | cannot be removed — it is the job — so it is *bounded*: tool outputs are capped; the model's context is rebuilt from the log (no hidden state); denials are fed back as tool results so the model can re-plan rather than loop | [02-scheduling-runtime](../architecture/02-scheduling-runtime.md) |

And the two properties that make containment hold under an *agentic* attack
(one that adapts over many steps):

* **Grants are pinned.** The attacker cannot talk the agent into "asking for"
  a new tool; the definition digest is fixed for the run.
* **Budgets end loops.** An injected "keep trying until it works" hits the
  step/tool-call/cost budget and the run fails with the reason.

### The demo's injection scenario (safety proof S14)

The scripted model, acting on an injected instruction, tries in order:
`http.get` to an unlisted domain (`params.host_not_allowed`); `github.cli`
with `auth token` (`params.denied_pattern`); `http.get` to
`169.254.169.254` from inside a sandbox (`params.ssrf`); a tool that was
never granted (`tool.not_granted`). Four denials, four audit records, zero
side effects, and the run ends with the model reporting that every attempt
was refused. The test asserts the *mechanism* (the rule names in the audit
chain), not the model's behaviour.

### Where detection still has a job

* **Telemetry**: `tool_calls_total{decision="DENY"}` by tenant and rule is the
  injection-attempt dashboard; a spike is an incident signal.
* **Triage**: a classifier over denied calls' arguments can rank incidents
  for a human.
* **Product**: warning a user that a document contains instruction-like
  text is a UX feature.

None of these is on the enforcement path.

### Where this design is weaker than it should be

* **Second-order injection**: the agent writes a poisoned file to `/work`
  that a *later*, more trusted step (or the broker sandbox's `gh`) consumes.
  Bounded by argv allowlists, token scope and TTL; not eliminated. CaMeL's
  data-provenance capabilities are the principled fix.
* **Allowlist quality**: `allowed_hosts: [api.github.com]` is safe;
  `allowed_hosts: [*]` is not, and nothing stops an agent author from writing
  it. A policy linter and an admission check on definitions is the production
  step.
* **The broker sandbox's network** in the PoC uses the host namespace; the
  egress proxy is designed, not built.

## State of the art

* **Willison, *The lethal trifecta for AI agents*** (2025) and his earlier
  *dual LLM pattern* (2023) — the clearest statements of "contain, don't
  detect".
* **Greshake et al., *Not what you've signed up for*** (2023) — the paper
  that defined *indirect* prompt injection and demonstrated it against
  Bing Chat and plugins.
* **Debenedetti et al., *AgentDojo*** (NeurIPS 2024) — a benchmark of 97
  tasks and 629 security test cases for agents with tools; the headline
  result is that no prompt-level defence eliminates attacks, and the best
  ones cost utility.
* **Zhan et al., *InjecAgent*** (2024) — indirect injection against tool-using
  agents; attack success rates in the tens of percent against strong models.
* **Google DeepMind, *CaMeL*** (2025) — capability-based containment that
  solves the AgentDojo tasks with provable security properties; the design
  this document points to for second-order attacks.
* **OWASP Top 10 for LLM Applications** — LLM01 (prompt injection) and LLM06
  (excessive agency) are the two this design addresses structurally.
* **OpenAI's *instruction hierarchy*** (2024) and provider-side training —
  option C; helpful, not sufficient.
* **Anthropic and OpenAI agent product docs** both state that sandboxing
  and network restrictions, not prompts, are the defence: Claude Code's
  sandbox and network allowlist; Codex's network-disabled default.

## Documented incidents (details and links in [14](14-industry-learnings.md))

* GitHub MCP private-repo leak via an injected issue (2025).
* EchoLeak — zero-click exfiltration in Microsoft 365 Copilot (CVE-2025-32711).
* Amazon Q Developer extension: a malicious PR injected destructive
  instructions (2025).
* Slack AI data exfiltration via a public channel message (2024).
* Bing Chat / early ChatGPT plugin exfiltration demonstrations (2023).

In every case, the fix that actually shipped was a *capability* change —
closing the exfiltration channel, narrowing the token, requiring approval —
not a better detector.

## Evidence in this repository

* S14 (injection contained), S1 (no network), S12 (no credential reachable),
  S15 (budgets end loops), S16 (tenancy) in [safety proofs](../04-evidence/02-safety-proofs.md).
* Attack trees 1–6 in the [threat model](../00-problem/04-threat-model.md).

## Would reverse if

* agents must browse and post to arbitrary sites as their core job → the
  trifecta cannot be broken by allowlists; move to E (CaMeL-style data
  capabilities) and accept the engineering cost;
* provider-side robustness reaches a point where a detector's false-negative
  rate is measurably below the platform's other risks — not the case in any
  published benchmark.

## References

* Willison, *The lethal trifecta for AI agents* — https://simonwillison.net/2025/Jun/16/the-lethal-trifecta/
* Willison, *The Dual LLM pattern for building AI assistants that can resist prompt injection* — https://simonwillison.net/2023/Apr/25/dual-llm-pattern/
* Greshake et al., *Not what you've signed up for: compromising real-world LLM-integrated applications with indirect prompt injection* — https://arxiv.org/abs/2302.12173
* Debenedetti et al., *AgentDojo: a dynamic environment to evaluate prompt injection attacks and defenses for LLM agents* — https://arxiv.org/abs/2406.13352
* Zhan et al., *InjecAgent: benchmarking indirect prompt injections in tool-integrated LLM agents* — https://arxiv.org/abs/2403.02691
* Debenedetti et al., *Defeating prompt injections by design* (CaMeL) — https://arxiv.org/abs/2503.18813
* Wallace et al., *The instruction hierarchy: training LLMs to prioritize privileged instructions* — https://arxiv.org/abs/2404.13208
* OWASP, *Top 10 for LLM Applications* — https://owasp.org/www-project-top-10-for-large-language-model-applications/
* Aim Security, *EchoLeak* (CVE-2025-32711) — https://www.aim.security/lp/aim-labs-echoleak-blogpost
* Invariant Labs, *GitHub MCP exploited* — https://invariantlabs.ai/blog/mcp-github-vulnerability
* Anthropic, *Claude Code sandboxing* — https://docs.claude.com/en/docs/claude-code/sandboxing
* OpenAI, *Codex security* — https://developers.openai.com/codex/security
