# Fairness and rate limiting from first principles

> **Prerequisite:** [Multi-tenancy](03-multi-tenancy.md)
> **Read next:** [Audit](07-audit.md)
> **Code:** [`internal/fairness/limiter.go`](../../backend/internal/fairness/limiter.go)

---

## 1. The problem

The LLM provider caps you at some tokens-per-minute, and it is the largest line
on the bill. Tenant A starts 300 agents at 09:00. What happens to tenant B?

This is not a theoretical concern. It is the brief's stated traffic shape:
*"Traffic is bursty. A tenant may spin 300 agents at 9am and none at noon."*

---

## 2. Token buckets

The primitive. A bucket holds tokens, refills at a constant rate, and has a
maximum (the burst size).

```go
type bucket struct {
    capacity float64   // burst size
    rate     float64   // refill per second
    tokens   float64
    last     time.Time
}

func (b *bucket) refill(now time.Time) {
    b.tokens = math.Min(b.capacity, b.tokens + b.rate*now.Sub(b.last).Seconds())
    b.last = now
}

func (b *bucket) take(n float64) bool {
    if b.tokens < n { return false }
    b.tokens -= n
    return true
}
```

Why token buckets rather than a fixed window:

| | Fixed window | Sliding window | **Token bucket** |
|---|---|---|---|
| Burst handling | **boundary spike** — 2× at the edge | smooth | smooth, with explicit burst |
| Memory | O(1) | O(requests) | **O(1)** |
| Configurable burst | no | no | **yes** |

The boundary spike matters: with a fixed 60-second window, a tenant can spend its
whole allowance at 11:59:59 and again at 12:00:00, giving 2× the intended rate
at exactly the moment the fleet is bursting.

---

## 3. Three allocation strategies

### 3a. One global bucket — first come, first served

```
provider: 2000 tok/s
   └── everyone draws from here
```

Tenant A's 300 agents take everything. Tenant B stops. **This is the failure the
brief describes**, and it is what you get by default.

### 3b. Fixed per-tenant quotas

```
tenant A: 1000 tok/s        tenant B: 1000 tok/s
```

No starvation. But when B is idle overnight, its 1000 tok/s is **wasted** — and
at $3–15 per million tokens, wasted provider capacity is wasted money.

### 3c. Weighted max-min fairness + a shared spare pool ← chosen

```
guaranteed_rate(i) = weight(i) / Σ weights × provider_rate

  tenant A (weight 2): 1333 tok/s guaranteed
  tenant B (weight 1):  667 tok/s guaranteed
  spare pool:           whatever nobody's guarantee claims
```

Each tenant has its **own** bucket refilling at its guaranteed rate, and a
shared bucket collects capacity that guarantees do not claim.

**The key property:** a tenant can *always* draw at its guaranteed rate, whatever
anyone else is doing. This is **arithmetic**, not a scheduling heuristic that
might mis-tune. Starvation is not unlikely — it is impossible, because A's
bucket and B's bucket are different objects.

And guarantees do not cost utilisation, because unclaimed capacity flows into
the spare pool that anyone may burst into.

> [Max-min fairness](https://en.wikipedia.org/wiki/Max-min_fairness) is the same
> idea as [DRR](https://en.wikipedia.org/wiki/Deficit_round_robin) in packet
> scheduling, Linux [CFS](https://docs.kernel.org/scheduler/sched-design-CFS.html)
> for CPU, and [Envoy's rate-limit service](https://www.envoyproxy.io/docs/envoy/latest/intro/arch_overview/other_features/global_rate_limiting)
> for HTTP.

### Recomputation on membership change

```go
share := l.providerRate * float64(t.Weight) / float64(total)
if own := float64(t.TokensPerMinute) / 60.0; own > 0 && own < share {
    share = own                 // a small plan does not get a big share
}
```

Adding a tenant **must** reduce everyone else's guarantee, or the guarantees sum
to more than the provider gives you and the entire argument collapses. Asserted:

```
4 equal tenants each guaranteed 15000 TPM of 60000
```

---

## 4. Reserve then settle

You cannot know a call's token cost before making it. Three options:

| Strategy | Failure |
|---|---|
| Charge afterwards only | One enormous request blows past the quota before anyone notices |
| Charge the estimate only | Error accumulates; the bucket drifts from reality forever |
| **Reserve an estimate, reconcile after** | Correct |

```go
est := EstimateTokens(req)                       // crude: ~4 chars per token
res, ok := g.limiter.Reserve(tenantID, pri, est)
if !ok { return ErrQuotaUnavailable }            // caller must NOT block
resp, err := provider.Complete(ctx, req)
g.limiter.Settle(res, resp.InputTokens+resp.OutputTokens)
```

The estimator is deliberately crude, because it only has to be *approximately*
right — the settle corrects it. Getting the estimate wrong costs a little
scheduling accuracy for one call, not correctness.

### Both directions matter

| Direction | Behaviour | Why |
|---|---|---|
| **Over-estimate** | refunded | Otherwise a conservative estimator permanently shrinks a tenant's throughput |
| **Under-estimate** | charged, **allowing the bucket to go negative** | The tenant then waits until it refills — exactly the right response to having used more than it claimed |

```
reserved 5000, used 200, refunded 4800
under-estimate charged; tenant now in deficit at 0 tokens
```

The second line is the important one: after a 1000× under-estimate the tenant is
**refused** its next reservation. Without the negative-balance behaviour, a
tenant could under-report its way past the quota indefinitely.

### Settling exactly once

```go
settled := false
settle := func(actual int64) {
    if !settled { settled = true; g.limiter.Settle(res, actual) }
}
defer func() { settle(est) }()
```

A reservation that is never settled is quota **permanently lost** to that tenant
— a slow leak that would eventually strangle them. The `defer` guarantees it
happens on every path: success, permanent error, retry exhaustion, context
cancellation.

Note the different settle values: a **permanent** error settles `0` (refund
everything; we never made the call), whereas an unknown path settles `est`
(pessimistic).

---

## 5. Priority lanes

A person watching a spinner should not queue behind a nightly batch job.

```go
floor := 0.0
if pri != types.PriorityInteractive {
    floor = b.capacity * l.interactiveReserve      // default 20%
}
if b.tokens - need >= floor {
    b.tokens -= need
    return reservation, true
}
```

Batch and normal work may drain the bucket down to the reserve; **only
interactive work may go below it**. The reserve refills like everything else, so
it is a standing allocation rather than a one-off.

```
batch drained after 15 grants; interactive still admitted
```

Same idea as Kubernetes
[PriorityClass](https://kubernetes.io/docs/concepts/scheduling-eviction/pod-priority-preemption/)
and API server
[flow control](https://kubernetes.io/docs/concepts/cluster-administration/flow-control/).

---

## 6. Backpressure must be expressed as *not scheduling*

**This is the most important paragraph in the document**, and the easiest thing
to get wrong.

When a tenant is out of quota, there are two possible designs:

```go
// WRONG — one noisy tenant occupies every worker in the pool
func (w *Worker) step() {
    <-limiter.Wait(tenantID)      // block until quota is available
    ...
}

// RIGHT — ask who can run, and schedule only those
eligible := limiter.Eligible(typicalTokens)
if len(eligible) == 0 { return false, nil }     // nothing to do; sleep briefly
run, err := store.AcquireLease(ctx, workerID, ttl, eligible)
```

The eligible set is pushed **into the SQL query**:

```sql
AND ($1::text[] IS NULL OR cardinality($1::text[])=0 OR tenant_id = ANY($1))
```

A throttled tenant's runs are simply not candidates. They sit in `QUEUED`, no
worker is occupied, and everyone else makes progress.

If instead a worker blocked, 16 workers × one noisy tenant = a fully stalled
pool, and the fairness layer would have *caused* the outage it exists to
prevent.

> **This generalises well beyond LLMs:** when a shared resource is exhausted,
> remove the work from the schedule rather than parking a worker on it.

---

## 7. The evidence

### Noisy neighbour

50 goroutines spinning flat out vs. one caller ticking every 5 ms, both tenants
weight 1, over 2 seconds:

```
grants over 2s: noisy(50 spinning goroutines)=60 quiet(1 polite ticker)=59
quiet tenant received 98% of the noisy tenant's throughput
while offering ~1/50th of the load
```

Throughput is decided by **weighted share**, not by how hard you push. With a
single shared bucket the ratio would be near zero.

### Weights are real

```
refilled grants after 600ms: big(weight 2)=12  small(weight 1)=6
```

Exactly 2:1. A "weight" that did not produce proportional throughput would be
decoration.

### Guarantees sum correctly

```
4 equal tenants each guaranteed 15000 TPM of 60000
```

### Fail closed

An unconfigured tenant gets **nothing**:

```go
b, ok := l.buckets[tenantID]
if !ok {
    // Unknown tenant: fail closed. An unconfigured tenant must not get
    // unlimited access to the most expensive resource in the system.
    return Reservation{}, false
}
```

---

## 8. Operator visibility

Fairness that cannot be observed will be blamed for every latency complaint.

```go
type TenantQuota struct {
    TenantID       string
    Weight         int
    GuaranteedTPM  float64
    AvailableNow   float64
    CapacityTokens float64
    UtilizationPct float64
    InFlight       int
    Throttled      bool     // ← the question an operator is actually asking
}
```

Surfaced in the console's **Quota & fairness** tab: per-tenant bucket level with
a colour that crosses to warn at 50% and error at 25%, in-flight count, an
explicit throttled badge, and the shared spare pool. "Which tenant is being
throttled right now, and why" should be one glance during an incident, not a log
dig.

---

## 9. What this does not do

| Gap | Consequence |
|---|---|
| **Not distributed** | The limiter is per-process. With N control-plane replicas, the effective quota is N× the configured value. Production needs Redis or a dedicated sharded service. **The most important gap in this component.** |
| No RPM limit | Only tokens are governed; providers also cap requests/minute |
| No cost-aware weighting | A tenant using an expensive model gets the same token share as one using a cheap model, despite costing 5× more. Weighting by *dollars* would be more honest |
| No reserved capacity tier | Fair shares only; some customers want a contractual floor independent of other tenants' weights |
| No predictive admission | A tenant near its limit is throttled reactively rather than smoothed |

### On the first row

It is worth being precise: with 3 control-plane replicas each holding an
independent limiter configured for 400k TPM, the fleet can emit 1.2M TPM. The
fix is standard (a shared counter in Redis with a local batch/lease to avoid a
round trip per call), and the *algorithm* does not change — only where the
buckets live. But the current code would over-admit in a multi-replica
deployment, and that is a real limitation rather than a rough edge.

---

## References

- [Max-min fairness](https://en.wikipedia.org/wiki/Max-min_fairness)
- [Deficit round robin](https://en.wikipedia.org/wiki/Deficit_round_robin)
- [Linux CFS](https://docs.kernel.org/scheduler/sched-design-CFS.html)
- [Envoy global rate limiting](https://www.envoyproxy.io/docs/envoy/latest/intro/arch_overview/other_features/global_rate_limiting)
- [Kubernetes API Priority and Fairness](https://kubernetes.io/docs/concepts/cluster-administration/flow-control/)
- AWS Architecture Blog, [*Exponential Backoff And Jitter*](https://aws.amazon.com/blogs/architecture/exponential-backoff-and-jitter/)

---

**Next:** [Audit](07-audit.md) — making a log into evidence.
