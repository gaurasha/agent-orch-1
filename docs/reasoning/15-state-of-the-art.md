# 15 · State of the art — what production agent infrastructure converges on (2025–26)

> A survey of the systems that run agents in production today, grouped by the
> layer of this design they correspond to, with what each does and the public
> source for it. The purpose is calibration: where this repository's choices
> match the field, where they are deliberately different, and where the field
> has moved past them.

## The convergence, in one table

| Layer | What the field converges on | This repository |
|---|---|---|
| **execution isolation** | a hardware or user-space kernel boundary per agent/task: Firecracker microVMs where the vendor owns the metal, gVisor on Kubernetes; network off by default with domain allowlists through a proxy | namespaces + seccomp in the PoC (8.4 ms), gVisor `RuntimeClass` in production; empty netns; proxy designed |
| **durable execution** | the agent's progress lives in a durable log/journal and is replayed; Temporal is the reference; "Postgres as the engine" (DBOS) is the lightweight variant | event-sourced runs in Postgres, stateless workers, fenced commits |
| **tool access** | a gateway or platform between the model and tools that holds credentials, applies policy and logs; MCP as the protocol; approval for destructive tools | the tool gateway; deny-by-default; parameter policy; `Dangerous` → human |
| **credentials** | short-lived, scoped tokens vended to the tool layer, never to the model; "auth for agents" products | broker-minted ≤ 60 s tokens; two-sandbox pattern; `Secret` type |
| **prompt injection** | containment (network, permissions, approvals) as the boundary; detection as telemetry; CaMeL as the research direction | contain, don't detect |
| **fairness / cost** | per-key budgets and rate limits in an LLM gateway; provider tiers | weighted max-min with an interactive reserve, reserve/settle, backpressure in scheduling |
| **observability** | OpenTelemetry GenAI semantic conventions; trace-per-run; cost per token | event log as the trace; 11 metrics; audit chain; OTel designed |
| **evaluation** | task suites with programmatic checks; safety suites (AgentDojo); statistical treatment; LLM-as-judge for triage only | platform tests assert mechanisms; agent evals recommended in [13](13-evals-and-testing.md) |

## 1. Sandboxes and execution environments

| System | Isolation | Cold start (vendor-stated) | Network model | Notes |
|---|---|---|---|---|
| **E2B** | Firecracker microVMs | ~150 ms; snapshot/restore for persistence | outbound allowed by default, configurable | the most-cited "sandbox for agents" API; open-source infra; used by Manus and many coding agents |
| **Modal Sandboxes** | gVisor | seconds for cold containers, warm pools; filesystem snapshots | egress configurable, CIDR/domain allowlists | GPU-capable; the same runtime as Modal functions |
| **Fly Machines** | Firecracker | sub-second start/stop of stopped machines | per-app private networking (6PN) | "machines" as a primitive; Fly's own `fly agent` guidance |
| **Daytona** | container/microVM tiers | sub-100 ms claimed | configurable | positioned for agent code execution at scale |
| **Vercel Sandbox** (2025) | Firecracker microVMs | seconds; up to 45 min–5 h runs | outbound allowed | for AI-generated code in Vercel's platform |
| **Cloudflare Sandbox SDK / Containers** (2025) | containers on Cloudflare's runtime, driven from Workers/Durable Objects | seconds | Workers egress | pairs with the Agents SDK |
| **Azure Container Apps dynamic sessions** (2024) | Hyper-V isolated sessions | pool-based, warm | configurable egress | Microsoft's "code interpreter as a service" |
| **AWS Bedrock AgentCore Code Interpreter / Browser** (2025) | microVM session isolation | managed | configurable | part of the AgentCore suite (Runtime, Gateway, Identity, Memory, Observability) |
| **Google Vertex AI Agent Engine** | managed runtime with code-execution tooling | managed | managed | pairs with ADK |
| **OpenAI Codex (cloud)** | isolated containers per task; network disabled by default, allowlist optional | — | off by default | the docs are explicit that network isolation is the injection defence |
| **Anthropic Claude Code** + `sandbox-runtime` | bubblewrap (Linux) / seatbelt (macOS); domain-allowlist proxy | — | proxy only | open-source runtime; the design pattern this repository's broker sandbox follows |
| **Anthropic Managed Agents / Agent SDK** | server-hosted sessions with a managed sandbox | — | managed | the vendor-hosted variant of the same loop |

**Where this repository sits:** the same primitives as `sandbox-runtime` and
bubblewrap in the PoC, gVisor in production; the honest difference is that
E2B/Fly/Vercel's Firecracker boundary is stronger than gVisor when you own
the metal, and the "would reverse" in [02](02-isolation.md) says so.

## 2. Durable execution and orchestration

| System | Model | Storage | Notable for agents |
|---|---|---|---|
| **Temporal** | workflows + activities, deterministic replay, signals, timers, heartbeats | Cassandra / MySQL / Postgres (+ Elasticsearch) | OpenAI Codex runs on it; Temporal's own "durable AI agents" material; the reference implementation of the pattern |
| **Restate** | durable functions/virtual objects with a journal; single binary | embedded RocksDB, replicated | lighter operational footprint than Temporal; explicit agent SDK guides |
| **DBOS** | "durable execution as a library" on Postgres | Postgres | the closest published design to this repository's substrate choice |
| **Inngest** | event-driven durable functions, steps, sleeps | managed / self-hosted | AgentKit for agent networks |
| **Hatchet** | Postgres-backed task queue + durable workflows | Postgres | explicit "we chose Postgres over Kafka" rationale |
| **Trigger.dev** | long-running tasks with checkpoints | managed | agent-oriented examples |
| **Cloudflare Workflows + Durable Objects** | durable steps on the edge; one DO per agent with SQLite | Cloudflare storage | the Agents SDK runs on it |
| **AWS Step Functions / Azure Durable Functions** | state machines / orchestrator functions | managed | the cloud-native workflow engines; less used for agent loops |
| **LangGraph Platform** | graph checkpointing per super-step, human-in-the-loop, threads | Postgres | the most-used agent framework's hosted runtime |
| **Vercel Workflow (2025)** | durable TypeScript functions on Vercel | managed | announced 2025 for agent workloads |

**Where this repository sits:** DBOS/Hatchet's substrate choice (Postgres,
no second cluster) with Temporal's semantics re-implemented at the scale
this problem needs (leases, fencing, replay, idempotent activities). The
reversal condition in [01](01-runtime-model.md) is "the organisation already
runs Temporal".

## 3. Tool access, gateways and protocols

| System | What it is | Relevance |
|---|---|---|
| **Model Context Protocol (MCP)** | the de-facto protocol for tool servers; 2025 spec adds OAuth 2.1 authorization and tool annotations | the interface this gateway would speak to external tool servers; the spec is explicit that annotations are not a security boundary |
| **AWS Bedrock AgentCore Gateway** | managed gateway fronting tools/MCP with authorization and credential injection | the same choke-point shape as a product |
| **Docker MCP Gateway / Catalog** (2025) | run MCP servers in containers behind a gateway with secrets management | the same containment idea for third-party tool servers |
| **Composio, Arcade, Nango, Toolhouse** | "auth + tools for agents" platforms holding OAuth tokens and exposing actions | option E in [06](06-credentials.md) as a product |
| **Kong AI Gateway, Envoy AI Gateway, LiteLLM, Portkey, Cloudflare AI Gateway, OpenRouter** | LLM proxies: provider abstraction, budgets, rate limits, fallbacks, caching, logging | option C in [09](09-llm-integration.md); the natural home for multi-application platforms |
| **Anthropic Claude Code permissions / OpenAI Codex approval modes** | client-side allow/deny rules with argument patterns and approval prompts | parameter-level policy at the client, the same idea as the gateway's `ParamPolicy` |
| **Google CaMeL** (research, 2025) | capability-based control flow separating planner from quarantined data processing | the direction for second-order injection ([10](10-prompt-injection.md)) |

## 4. Identity for agents

| System | What it provides |
|---|---|
| **SPIFFE / SPIRE** | workload identity (SVIDs) — how the gateway proves itself to a vault or STS without a static secret |
| **AWS AgentCore Identity** | credential providers vending OAuth/API keys to tools on the agent's behalf |
| **Okta / Auth0 "Auth for GenAI" (Token Vault)** | per-user OAuth tokens stored and exchanged for agents, with consent |
| **Microsoft Entra Agent ID** (2025) | first-class directory identities for agents |
| **GitHub App installation tokens** | scoped, ~1 h tokens — the model for the `github/token` credential reference |
| **HashiCorp Vault dynamic secrets** | per-lease database/cloud credentials with TTL and revocation |

**Where this repository sits:** the broker interface (`Mint`/`Revoke`/`Verify`)
is the seam where any of these plugs in; the PoC's HMAC-derived tokens
demonstrate the *shape* (scope + TTL + revocation + verification).

## 5. Agent frameworks and SDKs (the layer above this platform)

| Framework | Shape | Relationship to this design |
|---|---|---|
| **OpenAI Agents SDK** | agents, handoffs, guardrails, tracing; Python/TS | an application-level loop that would run *inside* a worker step; its guardrails are prompt-level and complement, not replace, the gateway |
| **Anthropic Claude Agent SDK** | the Claude Code loop as a library; hooks, subagents, sandboxing | same; its sandbox and permission model mirror [02](02-isolation.md)/[05](05-authorization.md) |
| **Google ADK** | agents, tools, evaluation, deploy to Vertex Agent Engine | same |
| **Microsoft Agent Framework** (AutoGen + Semantic Kernel, 2025) | multi-agent orchestration, workflows | same |
| **LangGraph / LangChain** | graph-based control flow with checkpoints | the checkpointer is the piece this design implements as the event log |
| **CrewAI, PydanticAI, Mastra, Vercel AI SDK** | role-based / typed / TS-first agent libraries | application-level; need a substrate like this one to be durable and safe |

The distinction that matters: frameworks decide *what the agent does next*;
this platform decides *whether it may, how it survives, and who pays*. They
compose — a framework's loop runs inside `step()`, and its tool calls go
through the gateway.

## 6. Observability and evaluation

* **OpenTelemetry GenAI semantic conventions** (spans for model calls, tool
  calls, agents; token and cost attributes) — the standard this design's
  `run_id` correlation would map onto.
* **LangSmith, Braintrust, Langfuse, Arize Phoenix, Datadog LLM
  Observability** — trajectory tracing plus eval datasets and LLM-as-judge.
* **Inspect AI (UK AISI), promptfoo, DeepEval, OpenAI Evals** — eval
  frameworks; **AgentDojo, τ-bench, SWE-bench Verified, Terminal-Bench** — the
  benchmarks ([13](13-evals-and-testing.md)).

## 7. Where the field has moved past this design, and what to take from it

1. **Warm snapshot/restore of sandboxes** (E2B, Modal, Fly) makes per-run
   persistent sandboxes cheap; this design's per-call jail is right for
   `exec.*` tools at 8 ms but a stateful REPL session would want a snapshot.
2. **FQDN-based egress policy in the CNI** (Cilium) can replace the egress
   proxy for the broker sandbox.
3. **Agent identity as a first-class directory object** (Entra Agent ID,
   AgentCore Identity) formalises what `triggering_user` + `agent_name` +
   tenant do informally here.
4. **CaMeL-style data capabilities** address the second-order injection this
   design only bounds.
5. **Standard tool protocol (MCP)** — the gateway should speak MCP to external
   tool servers rather than only its built-in registry; the policy stage does
   not change.

## References

### Sandboxes
* E2B — https://e2b.dev/docs · Modal Sandboxes — https://modal.com/docs/guide/sandbox · Fly Machines — https://fly.io/docs/machines/ · Daytona — https://www.daytona.io/docs · Vercel Sandbox — https://vercel.com/docs/vercel-sandbox · Cloudflare Sandbox SDK — https://developers.cloudflare.com/sandbox/ · Azure Container Apps dynamic sessions — https://learn.microsoft.com/en-us/azure/container-apps/sessions · AWS AgentCore — https://docs.aws.amazon.com/bedrock-agentcore/latest/devguide/what-is-bedrock-agentcore.html · Vertex AI Agent Engine — https://cloud.google.com/vertex-ai/generative-ai/docs/agent-engine/overview · OpenAI Codex security — https://developers.openai.com/codex/security · Anthropic `sandbox-runtime` — https://github.com/anthropic-experimental/sandbox-runtime · Claude Code sandboxing — https://docs.claude.com/en/docs/claude-code/sandboxing
* gVisor — https://gvisor.dev/ · Firecracker — https://firecracker-microvm.github.io/

### Durable execution
* Temporal — https://docs.temporal.io/ · Restate — https://docs.restate.dev/ · DBOS — https://docs.dbos.dev/ · Inngest — https://www.inngest.com/docs · Hatchet — https://docs.hatchet.run/ · Trigger.dev — https://trigger.dev/docs · Cloudflare Workflows — https://developers.cloudflare.com/workflows/ · Cloudflare Agents SDK — https://developers.cloudflare.com/agents/ · AWS Step Functions — https://docs.aws.amazon.com/step-functions/ · Azure Durable Functions — https://learn.microsoft.com/en-us/azure/azure-functions/durable/durable-functions-overview · LangGraph Platform — https://langchain-ai.github.io/langgraph/concepts/langgraph_platform/

### Tools, gateways, identity
* Model Context Protocol — https://modelcontextprotocol.io/ · AgentCore Gateway — https://docs.aws.amazon.com/bedrock-agentcore/latest/devguide/gateway.html · AgentCore Identity — https://docs.aws.amazon.com/bedrock-agentcore/latest/devguide/identity.html · Docker MCP Gateway — https://docs.docker.com/ai/mcp-catalog-and-toolkit/ · Composio — https://docs.composio.dev/ · Arcade — https://docs.arcade.dev/ · Kong AI Gateway — https://docs.konghq.com/gateway/latest/ai-gateway/ · Envoy AI Gateway — https://aigateway.envoyproxy.io/ · LiteLLM — https://docs.litellm.ai/ · Portkey — https://portkey.ai/docs · Cloudflare AI Gateway — https://developers.cloudflare.com/ai-gateway/
* SPIFFE — https://spiffe.io/ · Auth0 for AI agents — https://auth0.com/ai · Microsoft Entra Agent ID — https://learn.microsoft.com/en-us/entra/agent-id/ · Vault dynamic secrets — https://developer.hashicorp.com/vault/docs/secrets/databases

### Frameworks, observability, evals
* OpenAI Agents SDK — https://openai.github.io/openai-agents-python/ · Claude Agent SDK — https://docs.claude.com/en/api/agent-sdk/overview · Google ADK — https://google.github.io/adk-docs/ · Microsoft Agent Framework — https://learn.microsoft.com/en-us/agent-framework/ · LangGraph — https://langchain-ai.github.io/langgraph/ · CrewAI — https://docs.crewai.com/ · PydanticAI — https://ai.pydantic.dev/ · Mastra — https://mastra.ai/docs · Vercel AI SDK — https://ai-sdk.dev/docs
* OpenTelemetry GenAI semantic conventions — https://opentelemetry.io/docs/specs/semconv/gen-ai/ · Inspect AI — https://inspect.aisi.org.uk/ · promptfoo — https://www.promptfoo.dev/ · Langfuse — https://langfuse.com/docs · Braintrust — https://www.braintrust.dev/docs · LangSmith — https://docs.smith.langchain.com/
* Debenedetti et al., *CaMeL* — https://arxiv.org/abs/2503.18813 · *AgentDojo* — https://arxiv.org/abs/2406.13352
