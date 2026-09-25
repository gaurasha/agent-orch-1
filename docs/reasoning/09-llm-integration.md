# 09 · LLM integration — one gateway, reserve/settle, jittered retries, no streaming

> Decision: **every model call goes through `llm.Gateway.Complete()`: reserve
> fairness quota from an estimate, up to three attempts with full-jitter
> exponential backoff (base 250 ms), a fallback provider on the last attempt,
> settle against actual usage exactly once, account cost from a price table
> in code; a provider outage re-queues the run rather than failing it; tool
> schemas only — never backend details — are sent to the model; no streaming
> in the PoC.**
>
> Diagrams: [06-model-plane](../architecture/06-model-plane.md) · Related: [07-fairness](07-fairness.md), [10-prompt-injection](10-prompt-injection.md)

## Problem

The model provider is the largest cost line, the largest latency, the most
rate-limited dependency, and the one whose output is untrusted. The
constraints it imposes on the integration layer:

1. One place must own quota, retries, cost and provider failover — otherwise
   every worker reimplements them differently and the fairness argument has
   holes.
2. Retries must not amplify an outage (synchronised retries from 500 agents
   are a self-inflicted DDoS).
3. A provider outage must be a *delay*, never a *failed run*; the event log
   has no partial step to corrupt.
4. The model must see **only what it needs**: tool schemas, not binaries,
   credential references, timeouts or "dangerous" flags.
5. Cost must be attributable per run and per tenant at the moment it is
   incurred.

## Options

| # | Option | Strongest case | Where it breaks | Who does it |
|---|---|---|---|---|
| A | **Call the provider SDK directly from the worker** | least code; SDKs have retries built in | constraint 1 fails: fairness and cost accounting are scattered; SDK retries are per call with no cross-tenant view; provider switching is a rewrite | most first versions |
| B | **An in-process model gateway package** (chosen) | one code path; the limiter and the metrics live next to it; no extra hop; provider adapters behind one interface | per-process limiter state (the multi-replica gap); the gateway is only as good as its adapters | this repository; many in-house platforms |
| C | **An external LLM proxy** (LiteLLM, Portkey, OpenRouter, Helicone, Kong/Envoy AI Gateway, Cloudflare AI Gateway) | provider abstraction, caching, budgets, logging, retries and fallbacks as a product; one config for 100 models | a hop and a dependency on every step; per-tenant *weighted max-min with an interactive reserve* is not a standard feature (budgets and simple rate limits are); the proxy sees every prompt (a data-handling question); some are heavy Python services on the hot path | widely used in 2024–26 platforms; Cloudflare AI Gateway for edge |
| D | **Provider-native batch APIs** for batch-priority work | 50 % cheaper (Anthropic/OpenAI batch), higher limits | hours of latency; a different code path; only for genuinely offline agents | offline evaluation pipelines |
| E | **Streaming responses** | lower time-to-first-token for chat UIs | the agent loop needs the *whole* response (tool calls arrive at the end); streaming complicates idempotent commits and the event log; the console can show progress from events instead | chat products; not needed for an orchestrator's step |
| F | **Self-hosted models** (vLLM, TGI, Ollama) | no rate limits from a vendor; data stays in | you become the provider: GPU capacity planning, batching, model quality; the gateway is unchanged — it is just another adapter | organisations with data-residency requirements or scale |

## Deep dive

### Reserve then settle

Constraint 5 plus [07-fairness](07-fairness.md): the limiter must admit
before the call from `EstimateTokens(req)` — a character-count heuristic over
system prompt, messages, tool calls and tool schemas plus an output
allowance — and correct after the call with the provider's actual token
counts. `Settle` is idempotent (a `settled` flag) and a failed call settles to
the estimate so no quota is permanently lost.

### Three attempts, full jitter, then give up

```
backoff = 250 ms × 2^(attempt−1);  sleep = rand(0, backoff)
```

Full jitter (sleep uniformly in `[0, backoff]`) rather than fixed backoff or
"equal jitter": in AWS's simulation it produces the fewest total calls and
the least contention when many clients retry the same failure at once, which
is exactly the 500-agents-hit-a-429 case. Three attempts, not "until it
works": an unbounded retry loop against a degraded provider is how a partial
outage becomes a full one. Durability makes giving up cheap — the run
re-queues with `wake_at = now + 10 s` and nothing is lost.

### Fallback on the last attempt only

If a fallback provider is configured it is tried on the third attempt when
the error is retryable. Not on the first: the primary is the *intended* model
(quality, cost, data-handling); the fallback is for degradation, not load
balancing.

### Classifying errors

`isRetryable`: rate limits (429), server errors (5xx), timeouts and
connection errors retry; client errors (4xx other than 429 — a bad request, a
too-long context) do not, because retrying them is pointless and the run
should surface the problem.

### Schemas only

`Tool.MarshalJSON` serialises `Schema` and nothing else. The model receives
`{name, description, parameters, required}`; it cannot learn that
`github.cli` runs a binary called `gh`, that `http.post` is `Dangerous`, or
which credential reference a tool uses. This is constraint 4 and a small but
real reduction of what an injection can *ask for*.

### Context construction

`Rebuild(events)` turns the event log into the message list on every step:
`RUN_CREATED` → the user's first message; `MODEL_RESPONSE` → an assistant
turn (with tool calls); `TOOL_RESULT`/`TOOL_DENIED` → tool results (a denial
is a tool result saying "refused: <reason>", so the model can re-plan);
`HUMAN_RESUME` → a user turn. `NOTE`s and worker ids are not sent. The system
prompt is built from the pinned definition. Because the log is the only
input, two workers rebuild identical contexts — which is what makes the
deterministic idempotency key valid.

Context growth is the real long-run cost ([04](04-scheduling.md)); the
designed answer is a checkpoint event carrying a summary, the same move as
Anthropic's context editing / compaction and LangGraph's summarisation
nodes.

### Cost

A price table in code (`$/Mtok` in and out per model) produces `cost_usd`
per call, committed with the step in `runs.usage`, exported per tenant and
model in `model_cost_usd_total`. Prompt caching (Anthropic/OpenAI) changes
the price of cached input tokens; the adapter reports cache tokens
separately and the table has a column for them — a production detail the PoC
leaves at the interface.

### The fake provider

The PoC ships `FakeProvider`: scripted scenarios (including the
prompt-injection scenario), configurable latency and jitter, and
`FailEvery(n)` so the retry and requeue paths are exercised by the demo, not
only by unit tests. Every adapter has the same three-method interface.

## State of the art

* **LiteLLM / Portkey / OpenRouter / Helicone / Cloudflare AI Gateway /
  Envoy AI Gateway / Kong AI Gateway**: the LLM-proxy category; per-key
  budgets, rate limits, fallbacks, caching, logging. Widely deployed; the
  natural evolution of option B when a platform serves many *applications*
  rather than many *tenants of one application*.
* **Provider SDK retries**: both Anthropic's and OpenAI's SDKs retry with
  exponential backoff and honour `retry-after` — the same policy, per call.
* **Prompt caching** (Anthropic cache control, OpenAI automatic caching):
  changes cost accounting and favours stable prefixes — the system prompt and
  tool schemas are placed first for that reason.
* **Anthropic's "Building effective agents"** and the Agent SDK's
  context-management features (compaction) — the reference for context
  growth.
* **Provider batch APIs** for offline work at half price.
* **Structured outputs / strict tool schemas**: providers now validate tool
  arguments against JSON Schema; the gateway still validates (`args.invalid`)
  because the model is not the trust boundary.

## Documented issues

* **Retry storms**: AWS's post; providers' own guidance on backoff; several
  public 2024–25 provider incidents where client retries extended recovery.
* **Rate-limit tiers and 429 semantics** change often; the adapters treat
  429 and `overloaded` as retryable and everything else as the run's problem.
* **Tokenizer mismatch**: estimates from characters are wrong by up to ~2× on
  code and non-Latin scripts; settle corrects; `max_tokens` bounds the
  overshoot.
* **Context-window exhaustion** produces a 4xx that is *not* retryable; the
  run fails with the reason, which is the correct outcome until checkpoints
  exist.
* **Proxy-on-the-hot-path incidents**: a self-hosted LLM proxy becoming the
  bottleneck or a single point of failure is a recurring theme in platform
  post-mortems; it is why option B keeps the gateway in-process and stateless.

## Evidence in this repository

* `llm/gateway_test.go` — retries, non-retryable errors, fallback, settle
  exactly once.
* The demo's `FailEvery` path: a provider failure mid-run produces a requeue
  with `status_reason` and the run still completes.
* `TestLoad_500ConcurrentAgents` runs with the fake provider at 2 ms — the
  orchestrator's overhead is measured *without* provider latency, and the
  benchmarks say so explicitly.

## Would reverse if

* the platform serves many applications with heterogeneous providers → an
  external LLM gateway (C) in front of the same `Provider` interface;
* time-to-first-token becomes a product metric → streaming (E) for the
  console, with the commit still happening on the complete response;
* data residency forbids the vendor → self-hosted (F) behind the same
  adapter.

## References

* AWS Architecture Blog, *Exponential Backoff And Jitter* — https://aws.amazon.com/blogs/architecture/exponential-backoff-and-jitter/
* Anthropic, *Rate limits* — https://docs.claude.com/en/api/rate-limits · *Prompt caching* — https://docs.claude.com/en/docs/build-with-claude/prompt-caching · *Tool use* — https://docs.claude.com/en/docs/agents-and-tools/tool-use/overview · *Batch processing* — https://docs.claude.com/en/docs/build-with-claude/batch-processing
* OpenAI, *Rate limits* — https://platform.openai.com/docs/guides/rate-limits · *Function calling* — https://platform.openai.com/docs/guides/function-calling · *Batch API* — https://platform.openai.com/docs/guides/batch
* Anthropic, *Building effective agents* — https://www.anthropic.com/engineering/building-effective-agents · *Effective context engineering for AI agents* — https://www.anthropic.com/engineering/effective-context-engineering-for-ai-agents
* LiteLLM — https://docs.litellm.ai/ · Portkey — https://portkey.ai/docs · OpenRouter — https://openrouter.ai/docs · Cloudflare AI Gateway — https://developers.cloudflare.com/ai-gateway/ · Envoy AI Gateway — https://aigateway.envoyproxy.io/
* vLLM — https://docs.vllm.ai/ · Ollama — https://github.com/ollama/ollama
