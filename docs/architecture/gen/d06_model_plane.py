from svgkit import Diagram, LINE, INK

PURPLE, GREEN, ORANGE, RED, BLUE, AMBER = "#7a3fd1", "#2e8b57", "#d9822b", "#c0392b", "#2f6fbf", "#b8860b"

def build():
    d = Diagram(1560, 1150, "06 · The model plane and fairness — one choke point for the most expensive resource",
                "llm.Gateway.Complete() is the only code path to a provider. It reserves quota before the call, retries with full jitter, settles against real usage, and never fails a run for a provider outage.")

    d.zone(40, 86, 560, 720, "MODEL GATEWAY · internal/llm/gateway.go", "platform",
           "called by a worker's step(); returns a Response or an error the worker turns into a requeue", subtitle_inside=True)
    d.zone(640, 86, 880, 720, "FAIRNESS LIMITER · internal/fairness/limiter.go", "state",
           "weighted max-min fairness = one token bucket per tenant + one shared spare bucket", subtitle_inside=True)

    X, W = 70, 500
    d.box("call", X, 130, W, 74, "Complete(tenant, priority, req)", kind="platform", mono=True,
          lines=["req = {model, system, messages (Rebuild), tools: SchemasFor(def.Tools), max_tokens}",
                 "est = EstimateTokens(req)  // character-count heuristic over system+messages+tools, + output"])
    d.box("reserve", X, 228, W, 74, "1 · limiter.Reserve(tenant, priority, est)", kind="state",
          lines=["ok=false → return ErrQuotaUnavailable immediately — the worker requeues the run with wake_at = now+2 s",
                 "the worker's goroutine is free within microseconds; it leases some other tenant's run"])
    d.box("attempt", X, 326, W, 150, "2 · for attempt in 1..3", kind="platform",
          lines=["resp, err = primary.Complete(ctx, req)                 (per-call ctx timeout)",
                 "success → break",
                 "not retryable (4xx other than 429) → break with the error",
                 "attempt == 3 and fallback configured and retryable → try fallback provider instead",
                 "backoff = 250 ms × 2^(attempt−1);  sleep = rand(0, backoff)   ← FULL jitter",
                 "  (AWS 'Exponential Backoff And Jitter': full jitter minimises retry collisions",
                 "   across 500 agents hitting the same 429 at the same second)",
                 "log: tenant, attempt, backoff_ms, err"])
    d.box("settle", X, 500, W, 74, "3 · limiter.Settle(reservation, actualTokens) — exactly once", kind="state",
          lines=["over-estimate → tokens refunded to the tenant's bucket · under-estimate → charged, may go negative",
                 "Reservation.settled guards double settlement; a failed call settles with 0 (full refund)"])
    d.box("account", X, 598, W, 74, "4 · account + return", kind="platform",
          lines=["metrics.ModelCall(tenant, model, in, out, costUSD) — cost from the price table in code",
                 "Response{content, tool_calls, stop_reason, tokens, cost} → the worker writes MODEL_RESPONSE"])
    d.box("fail", X, 696, W, 90, "after 3 failures → error to the worker", kind="external",
          lines=["the worker does NOT fail the run: requeue with wake_at = now+10 s, status_reason 'model provider unavailable'",
                 "durability means an outage is a delay, not a loss; the event log has no partial step",
                 "budget check happens BEFORE the call, so a retry storm can never overspend a run"])
    for a, b in [("call", "reserve"), ("reserve", "attempt"), ("attempt", "settle"), ("settle", "account")]:
        d.arrow(a, b, "", src_side="s", dst_side="n", color=BLUE, width=2)
    d.arrow("attempt", "fail", "", src_side="w", dst_side="w", src_off=40, dst_off=-20,
            via=[(56, 441), (56, 721)], color=RED, style="dashed")

    # limiter internals
    LX = 670
    d.box("bucket", LX, 130, 400, 118, "bucket — one per tenant", kind="state", mono=True,
          lines=["rate     = provider_rate × w_i / Σw   (tok/s)",
                 "         min(rate, tenant.tokens_per_minute/60)",
                 "capacity = rate × burstSeconds",
                 "refill(now): tokens = min(cap, tokens + rate×Δt)",
                 "take(n) / give(n)  · tokens may go NEGATIVE (deficit)"])
    d.box("spare", 1100, 130, 390, 118, "spare — one shared bucket", kind="state", mono=True,
          lines=["rate = provider_rate − Σ allocated shares",
                 "(only what plan caps leave unclaimed)",
                 "starts EMPTY: capacity is spare only",
                 "once it has actually gone unused",
                 "any tenant may borrow from it after its own bucket"])
    d.box("reserve_i", LX, 272, 400, 132, "Reserve(tenant, pri, need)", kind="platform", mono=True,
          lines=["b = buckets[tenant]; unknown tenant → false (fail closed)",
                 "floor = pri == interactive ? 0 : 0.20 × b.capacity",
                 "if b.tokens − need ≥ floor: b.tokens −= need → ok",
                 "elif spare.take(need):                      → ok",
                 "else → false  (caller must NOT block)",
                 "inFlight[tenant]++ for the operator view"])
    d.box("eligible", 1100, 272, 390, 132, "Eligible(typical=2000) → []tenant", kind="platform", mono=True,
          lines=["for each bucket: refill(now)",
                 "  if tokens − typical ≥ 0.20×cap",
                 "  or spare.tokens ≥ typical → eligible",
                 "sorted; passed to AcquireLease as $1",
                 "→ WHERE tenant_id = ANY($1)",
                 "backpressure = NOT scheduling, never a blocked worker"])
    d.box("settle_i", LX, 428, 400, 96, "Settle(r, actual)", kind="platform", mono=True,
          lines=["delta = r.Estimated − actual",
                 "delta > 0 → b.give(delta)       (refund)",
                 "delta < 0 → b.tokens += delta   (charge; deficit)",
                 "a tenant in deficit waits until refill catches up"])
    d.box("snapshot", 1100, 428, 390, 96, "Snapshot() → /v1/quota", kind="platform", mono=True,
          lines=["per tenant: weight, guaranteed tok/s,",
                 "  tokens, capacity, in_flight,",
                 "  throttled = tokens ≤ 20%·cap && spare empty",
                 "SpareTokens() for the shared pool gauge"])
    d.arrow("reserve", "reserve_i", "", src_side="e", dst_side="w", color=GREEN, style="dotted", via=[(630, 265), (630, 338)])
    d.arrow("settle", "settle_i", "", src_side="e", dst_side="w", color=GREEN, style="dotted", via=[(620, 537), (620, 476)])

    d.table(LX, 548, [130, 100, 110, 150, 140, 160], [
        [["Worked example"], ["weight"], ["plan tpm"], ["guaranteed"], ["idle → spare?"], ["interactive floor"]],
        [["provider: 1000 tok/s"], [""], [""], [""], [""], [""]],
        [["acme"], ["2"], ["∞"], ["500 tok/s"], ["no (share fully claimed)"], ["100 tok/s held back"]],
        [["beta"], ["1"], ["∞"], ["250 tok/s"], ["no"], ["50 tok/s"]],
        [["gamma"], ["1"], ["6 000"], ["min(250, 100) = 100"], ["150 tok/s → spare"], ["20 tok/s"]],
        [["Σ"], ["4"], [""], ["850 allocated"], ["spare rate 150 tok/s"], ["burst = rate × burstSeconds"]],
    ], kind="state", size=10)
    d.note(LX, 720, 820, kind="platform", title="Why max-min with weights, and not a global bucket or fixed quotas",
           lines=["a global bucket lets one tenant's 300-agent burst starve everyone (first come, first served) · fixed quotas waste idle capacity",
                  "weighted max-min: every tenant is GUARANTEED its share and may BORROW what others leave idle; guarantees always sum to the provider rate",
                  "tests: TestFairness_GuaranteesSumToProviderCapacity · _NoisyTenantCannotStarveQuietTenant · _WeightsAllocateProportionally",
                  "       _InteractiveReserveProtectsHumanBlockingWork · _SettleRefundsOverEstimate · _SettleChargesUnderEstimate · _UnknownTenantFailsClosed"])

    d.table(40, 830, [300, 340, 420, 440], [
        [["Failure / condition"], ["Detection"], ["Reaction"], ["Trade-off · gap"]],
        [["provider 429 / 5xx / timeout"], ["error class from the provider adapter"], ["≤3 attempts, full-jitter backoff, fallback on the last; then requeue +10 s"],
         ["a run's first step waits out the outage; nothing is lost, nothing is duplicated (no side effect in a model call)"]],
        [["tenant out of quota"], ["Reserve() false · Eligible() omits the tenant"], ["run requeued +2 s or simply not leased; workers serve other tenants"],
         ["interactive work can dip into the 20 % reserve; batch cannot — batch latency grows first, by design"]],
        [["estimate badly wrong"], ["Settle() delta"], ["refund or charge; deficits allowed so the tenant self-corrects next refill"],
         ["a huge under-estimate lets one call overshoot its share once — bounded by max_tokens"]],
        [["limiter process restarts / N replicas"], ["—"], ["buckets rebuilt from tenants table on start; each replica admits independently"],
         ["N replicas ⇒ up to N× over-admission — the documented reason Redis is in the multi-replica design"]],
        [["unknown tenant"], ["no bucket"], ["Reserve() false: fail closed"], ["a misconfigured tenant gets NO model access rather than unlimited access"]],
    ], kind="platform")

    d.legend = [("platform", "gateway / limiter code"), ("state", "quota state"), ("external", "failure path")]
    return d

if __name__ == "__main__":
    build().save("../svg/06-model-plane.svg")
