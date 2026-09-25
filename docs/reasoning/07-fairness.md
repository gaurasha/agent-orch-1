# 07 · Fairness — sharing one provider rate limit across tenants

> Decision: **weighted max-min fairness: one token bucket per tenant with a
> guaranteed rate `w_i/Σw × provider_rate` (capped by the tenant's own plan),
> a shared spare bucket for capacity nobody claims, a 20 % interactive reserve
> inside each bucket, reserve-then-settle accounting, and backpressure
> expressed as *not scheduling* the tenant's runs.** Not first-come-first-served,
> not fixed quotas, not dominant-resource fairness, not a separate rate-limit
> service (yet).
>
> Diagrams: [06-model-plane](../architecture/06-model-plane.md) · Concept: [fairness](../01-concepts/06-fairness.md) · Related: [04-scheduling](04-scheduling.md), [09-llm-integration](09-llm-integration.md)

## Problem

The provider gives the platform one limit — say 1 000 000 tokens/minute
across everything. Tenant A starts 300 agents at 09:00. Tenant B has one
interactive agent with a human waiting. Physical constraints:

1. The limit is enforced by the provider *after* the request; if we send too
   much we get 429s, and the retries make it worse. So admission must happen
   **before** the call, in our process.
2. Tokens are only known **after** the call (the provider bills actual
   usage). So admission must work from an estimate and be corrected later.
3. The human waiting on B must not be behind A's 300 batch agents.
4. Capacity nobody is using should not be wasted on ceremony.
5. A tenant we have not configured must get **nothing**, not everything.

## Options

| # | Option | Strongest case | Where it breaks | Who does it |
|---|---|---|---|---|
| A | **One global bucket, first come first served** | trivial; the provider's own model | A's burst drains it; B waits behind 300 agents; no notion of tenant at all | many single-tenant deployments |
| B | **Fixed per-tenant quotas** (A gets 400 k tpm, B 100 k) | predictable; a contract | idle capacity is wasted (B's unused 90 k cannot help A); sum of quotas must be ≤ provider rate, so onboarding a tenant means shrinking everyone; no priority within a tenant | most "enterprise plan" tiers |
| C | **Weighted max-min fairness + spare pool** (chosen) | every tenant is *guaranteed* its share and may *borrow* what others leave idle; guarantees always sum to the provider rate; weights encode plans; interactive reserve encodes "a human is waiting" | more state (a bucket per tenant); recomputation on membership change; per-process today | network schedulers (WFQ), Linux CFS, Kubernetes ResourceQuota-ish, YARN/Mesos fair schedulers |
| D | **Dominant Resource Fairness** (Ghodsi et al.) | the right answer when tenants contend for *multiple* resources with different mixes (tokens, sandbox CPU, concurrent runs) | there is one dominant resource here (provider tokens); DRF's extra machinery buys nothing until sandbox CPU becomes the bottleneck | Mesos, YARN, Kubernetes scheduling research |
| E | **Priority queues only** (interactive > normal > batch) | simple; matches the human-waiting constraint | says nothing about *tenants*: A's interactive work starves B's interactive work; no isolation | many job systems |
| F | **A central rate-limit service** (Envoy ratelimit + Redis, Redis GCRA/`redis-cell`, Stripe-style) | correct across N replicas; well-understood | a network round trip per admission decision on the hot path (or a local lease/batch); another service; the *algorithm* is still C or B | Stripe, Envoy users, most API gateways |
| G | **Let the provider's per-key limits do it** (one API key per tenant) | provider-enforced; zero code | keys per tenant is a procurement problem; limits are per key not weighted; the burst still hits the provider and comes back as 429s to retry; no interactive reserve | small multi-tenant SaaS |

## Deep dive

### Max-min fairness in one paragraph

Give every tenant the smallest of (its demand, its fair share); redistribute
what is left over to those still wanting, repeatedly. The result: no tenant
can increase its allocation by taking from a tenant with a smaller
allocation. Weights generalise it: shares are proportional to `w_i`. This is
Jaffe's 1981 definition and the property fair queueing (Demers, Keshav,
Shenker 1989) implements for packets; Linux CFS implements it for CPU time.
For tokens per minute it is exactly constraint 3 and 4: guaranteed shares,
idle capacity redistributed.

### Token buckets as the implementation

A bucket per tenant with `rate = share` and `capacity = rate × burstSeconds`
lets a tenant burst up to `capacity` after being idle and then sustain
`rate`. The spare bucket's rate is `provider_rate − Σ shares`, which is
non-zero only when plan caps leave capacity unclaimed; its *tokens* are what
accumulates while nobody borrows. `Reserve` tries the tenant's bucket first,
then the spare — so borrowing never comes out of another tenant's guarantee.

### The interactive reserve

Inside each tenant's bucket, batch and normal work may only draw down to 20 %
of capacity; interactive work may take the rest. The effect: a tenant's own
batch burst cannot starve that tenant's human. `Eligible()` uses the batch
floor, so a tenant is "eligible" only if even its lowest-priority work could
proceed — interactive work that needs the reserve still succeeds at
`Reserve` time.

### Reserve then settle

Admission uses `EstimateTokens(req)`, a character-count heuristic. After the
call, `Settle(reservation, actual)` refunds an over-estimate (so a
conservative estimator does not permanently shrink a tenant's throughput)
and charges an under-estimate, allowing the bucket to go negative (so a tenant
that used more than it claimed waits for refill — the right response to
having been wrong in its favour). `Settle` is idempotent via a `settled`
flag, and a failed call settles to the estimate (a full refund), because a
reservation never settled is quota permanently lost to that tenant.

### Backpressure as not scheduling

The critical design property: an out-of-quota tenant costs **zero** worker
time. `Eligible()` is computed once per tick and pushed into the lease query.
The alternatives — a worker blocking on `Reserve`, or a run failing and being
retried — both convert one tenant's over-demand into everyone's latency.
[04-scheduling](04-scheduling.md) has the query.

### Fail closed

An unknown tenant has no bucket, so `Reserve` returns `false`. A
misconfiguration produces *no* model access rather than *unlimited* access to
the most expensive resource in the system. `TestFairness_UnknownTenantFailsClosed`.

### The per-process gap

Each worker replica has its own limiter. N replicas each believe they may
admit the full share: up to N× over-admission. The design's fix is a
Redis-backed bucket with a *local lease* (each replica reserves a slice of
the tenant's rate for a few seconds and admits against it locally), which
keeps the hot path off the network. This is the same pattern Stripe describes
for its rate limiters and Envoy's ratelimit service implements. The limiter
interface already isolates the change.

## State of the art

* **Provider rate limits** are per organisation and per model, in tokens and
  requests per minute (Anthropic and OpenAI both document tiered limits and
  `429` with `retry-after`). Everything above exists because those limits are
  *shared* by our tenants.
* **LLM gateways** (LiteLLM, Portkey, Kong AI Gateway, Envoy AI Gateway) offer
  per-key/per-team token budgets and rate limits — option B or a simple C
  without the spare pool; most run the limiter in Redis (F).
* **Kubernetes** uses weights and reserves for CPU (`requests` guarantee,
  `limits` cap, CFS shares) — the same shape for a different resource.
* **Stripe's rate limiters** (2017 blog): request rate limiter + concurrent
  request limiter + fleet usage load shedder + worker utilization load
  shedder, with Redis and local decisions — the canonical production
  reference for "several limiters, each for a different failure".
* **YARN/Mesos fair schedulers** and **DRF** for cluster resources — the
  reference for when a second dominant resource appears.

## Documented issues

* **Retry storms after 429**: the provider's own guidance and AWS's *Exponential
  Backoff and Jitter* post document that synchronised retries prolong an
  outage; the model gateway's full-jitter backoff ([09](09-llm-integration.md))
  is the mitigation, and admission-before-call is why most 429s never happen.
* **Token estimation error**: providers' tokenizers differ; a character
  heuristic can be off by 2× on code or non-Latin text. Settle corrects it
  after one call; the bounded overshoot is `max_tokens`.
* **Over-admission across replicas** — this repository's documented gap;
  Envoy's ratelimit docs and Stripe's post both describe the Redis + local
  window pattern that fixes it.
* **Starvation of batch under sustained interactive load** — a consequence of
  the priority lane design; the capacity doc's answer is pool sizing, and
  ageing is the "would reverse" item.

## Evidence in this repository

* `TestFairness_NoisyTenantCannotStarveQuietTenant`,
  `_WeightsAllocateProportionally`, `_InteractiveReserveProtectsHumanBlockingWork`,
  `_SettleRefundsOverEstimate`, `_SettleChargesUnderEstimate`,
  `_UnknownTenantFailsClosed`, `_GuaranteesSumToProviderCapacity`.
* `TestLoad_BurstDoesNotStarveTheOtherTenant` — a mixed-tenant burst end to
  end through the scheduler.
* [benchmarks §4–5](../04-evidence/01-benchmarks.md): weights 2:1 produce
  12:6 grants after refill; the quiet tenant's latency is unaffected by the
  noisy one.

## Would reverse if

* a second worker replica goes to production → Redis-backed buckets, first;
* sandbox CPU or concurrent runs become a contended resource with a different
  per-tenant mix than tokens → DRF;
* batch SLAs are contractual → add ageing or a dedicated batch pool.

## References

* Jaffe, *Bottleneck flow control* (max-min fairness, 1981) — https://ieeexplore.ieee.org/document/1095152
* Demers, Keshav, Shenker, *Analysis and simulation of a fair queueing algorithm* (1989) — https://dl.acm.org/doi/10.1145/75246.75248
* Ghodsi et al., *Dominant Resource Fairness* (NSDI 2011) — https://www.usenix.org/conference/nsdi11/dominant-resource-fairness-fair-allocation-multiple-resource-types
* Stripe, *Scaling your API with rate limiters* — https://stripe.com/blog/rate-limiters
* AWS Architecture Blog, *Exponential Backoff And Jitter* — https://aws.amazon.com/blogs/architecture/exponential-backoff-and-jitter/
* Envoy, *Global rate limiting* — https://www.envoyproxy.io/docs/envoy/latest/intro/arch_overview/other_features/global_rate_limiting · ratelimit service — https://github.com/envoyproxy/ratelimit
* `redis-cell` (GCRA) — https://github.com/brandur/redis-cell
* Anthropic, *Rate limits* — https://docs.claude.com/en/api/rate-limits · OpenAI, *Rate limits* — https://platform.openai.com/docs/guides/rate-limits
* Linux, *CFS scheduler* — https://docs.kernel.org/scheduler/sched-design-CFS.html
* Kubernetes, *Resource management (requests, limits, QoS)* — https://kubernetes.io/docs/concepts/configuration/manage-resources-containers/
* LiteLLM, *Budgets and rate limits* — https://docs.litellm.ai/docs/proxy/users
