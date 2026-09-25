// Package fairness governs the LLM provider quota.
//
// The provider is rate limited and is the largest cost line, so it is the one
// resource where "first come, first served" produces a visibly unfair system:
// a tenant that spins up 300 agents at 09:00 would consume the whole quota and
// every other tenant's agents would simply stop.
//
// The model here is max-min fairness with weights, implemented as one token
// bucket per tenant plus a shared bucket for spare capacity:
//
//	guaranteed_rate(i) = weight(i) / sum(weights) * provider_rate
//
// A tenant can ALWAYS draw at its guaranteed rate, whatever everyone else is
// doing. That is what makes starvation impossible rather than unlikely - it is
// a property of the arithmetic, not of a scheduling heuristic that might
// mis-tune. Capacity nobody is using flows into a shared spare bucket that any
// tenant may burst into, so guarantees do not cost utilisation.
//
// Two further mechanisms matter as much as the arithmetic:
//
//   - RESERVE THEN SETTLE. Quota is reserved from an estimate before the call
//     and reconciled against the provider's reported usage after. Charging only
//     afterwards would let a tenant exceed its share by one very large request;
//     charging only the estimate would drift.
//
//   - BACKPRESSURE IS EXPRESSED AS NOT SCHEDULING. A tenant that is out of
//     quota is simply excluded from the dispatcher's candidate set; its runs
//     sit in QUEUED. Nothing blocks and no worker is occupied. Blocking a
//     worker on a full bucket is how one noisy tenant takes down the pool for
//     everybody - the failure this design exists to avoid.
package fairness

import (
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/gaurasha/agent-orch/backend/internal/types"
)

// bucket is a standard token bucket, in units of LLM tokens per second.
type bucket struct {
	capacity float64 // burst size
	rate     float64 // refill per second
	tokens   float64
	last     time.Time
}

func newBucket(rate, burstSeconds float64, now time.Time) *bucket {
	cap := rate * burstSeconds
	return &bucket{capacity: cap, rate: rate, tokens: cap, last: now}
}

func (b *bucket) refill(now time.Time) {
	if !now.After(b.last) {
		return
	}
	b.tokens = math.Min(b.capacity, b.tokens+b.rate*now.Sub(b.last).Seconds())
	b.last = now
}

func (b *bucket) take(n float64) bool {
	if b.tokens < n {
		return false
	}
	b.tokens -= n
	return true
}

// give returns tokens without exceeding capacity. Used to refund the
// difference between an estimate and actual usage.
func (b *bucket) give(n float64) {
	b.tokens = math.Min(b.capacity, b.tokens+n)
}

func (b *bucket) setRate(rate, burstSeconds float64) {
	b.rate = rate
	b.capacity = rate * burstSeconds
	b.tokens = math.Min(b.tokens, b.capacity)
}

// Reservation is a claim on quota, to be settled once actual usage is known.
type Reservation struct {
	TenantID  string
	Estimated int64
	Priority  types.Priority
	settled   bool
}

// Limiter shares provider quota across tenants.
type Limiter struct {
	mu sync.Mutex

	providerRate float64 // total tokens/second available from the provider
	burstSeconds float64
	// interactiveReserve is the fraction of every tenant's bucket that only
	// human-blocking work may consume. Background work throttles first, so a
	// person waiting on an agent does not queue behind a batch backlog.
	interactiveReserve float64

	weights map[string]int
	buckets map[string]*bucket
	spare   *bucket

	// inFlight tracks unsettled reservations per tenant, purely for the
	// operator view: "this tenant has N calls out right now".
	inFlight map[string]int

	now func() time.Time
}

type Config struct {
	// ProviderTokensPerMinute is the total budget across all tenants. In
	// production this comes from the provider contract, and should be set
	// slightly BELOW the real limit so that we shed load before the provider
	// does - our 429 is graceful, theirs is not.
	ProviderTokensPerMinute int64
	// BurstSeconds is how much unused quota may accumulate. Too large and one
	// tenant's burst can still monopolise a window; too small and normal
	// bursty traffic is throttled unnecessarily.
	BurstSeconds float64
	// InteractiveReservePct (0-100) is held back for human-blocking work.
	InteractiveReservePct float64
}

func New(cfg Config) *Limiter {
	if cfg.BurstSeconds <= 0 {
		cfg.BurstSeconds = 10
	}
	if cfg.InteractiveReservePct <= 0 {
		cfg.InteractiveReservePct = 20
	}
	now := time.Now()
	rate := float64(cfg.ProviderTokensPerMinute) / 60.0
	l := &Limiter{
		providerRate:       rate,
		burstSeconds:       cfg.BurstSeconds,
		interactiveReserve: cfg.InteractiveReservePct / 100.0,
		weights:            map[string]int{},
		buckets:            map[string]*bucket{},
		inFlight:           map[string]int{},
		now:                time.Now,
	}
	// The spare bucket starts empty: capacity only becomes "spare" once it has
	// gone unused for a while.
	l.spare = newBucket(0, cfg.BurstSeconds, now)
	l.spare.tokens = 0
	return l
}

// SetTenants installs the weight set and recomputes every guaranteed rate.
//
// This runs whenever the tenant set changes. Recomputing all shares is correct
// and cheap: adding a tenant must reduce everyone else's guarantee, or the
// guarantees would sum to more than the provider gives us and the whole
// argument collapses.
func (l *Limiter) SetTenants(ts []types.Tenant) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()

	total := 0
	for _, t := range ts {
		if t.Weight > 0 {
			total += t.Weight
		}
	}
	if total == 0 {
		return
	}
	live := make(map[string]bool, len(ts))
	var allocated float64
	for _, t := range ts {
		live[t.ID] = true
		l.weights[t.ID] = t.Weight
		share := l.providerRate * float64(t.Weight) / float64(total)
		// A tenant's contractual own limit also applies: a tenant with a small
		// plan does not get a large share just because the cluster is quiet.
		if own := float64(t.TokensPerMinute) / 60.0; own > 0 && own < share {
			share = own
		}
		allocated += share
		if b, ok := l.buckets[t.ID]; ok {
			b.refill(now)
			b.setRate(share, l.burstSeconds)
		} else {
			l.buckets[t.ID] = newBucket(share, l.burstSeconds, now)
		}
	}
	for id := range l.buckets {
		if !live[id] {
			delete(l.buckets, id)
			delete(l.weights, id)
		}
	}
	// Whatever the guarantees do not claim (because a tenant's own plan limit
	// is below its fair share) becomes burstable spare capacity.
	spareRate := math.Max(0, l.providerRate-allocated)
	l.spare.refill(now)
	l.spare.setRate(spareRate, l.burstSeconds)
}

// Reserve claims quota for one model call.
//
// Returns ok=false when the tenant should wait. The caller must NOT block on
// this; it should leave the run queued and try a different one.
func (l *Limiter) Reserve(tenantID string, pri types.Priority, estTokens int64) (Reservation, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()

	b, ok := l.buckets[tenantID]
	if !ok {
		// Unknown tenant: fail closed. An unconfigured tenant must not get
		// unlimited access to the most expensive resource in the system.
		return Reservation{}, false
	}
	b.refill(now)
	l.spare.refill(now)

	need := float64(estTokens)
	// Human-blocking work may consume the whole bucket. Everything else must
	// leave the interactive reserve untouched.
	floor := 0.0
	if pri != types.PriorityInteractive {
		floor = b.capacity * l.interactiveReserve
	}
	if b.tokens-need >= floor {
		b.tokens -= need
		l.inFlight[tenantID]++
		return Reservation{TenantID: tenantID, Estimated: estTokens, Priority: pri}, true
	}
	// Guaranteed share is exhausted; try to borrow idle capacity.
	if l.spare.take(need) {
		l.inFlight[tenantID]++
		return Reservation{TenantID: tenantID, Estimated: estTokens, Priority: pri}, true
	}
	return Reservation{}, false
}

// Settle reconciles a reservation against actual usage.
//
// Over-estimates are refunded so a conservative estimator does not permanently
// shrink a tenant's throughput. Under-estimates are charged, which can push the
// bucket negative - deliberately: the tenant then waits until it has refilled,
// which is exactly the right response to having used more than it claimed.
func (l *Limiter) Settle(r Reservation, actualTokens int64) {
	if r.TenantID == "" || r.settled {
		return
	}
	r.settled = true
	l.mu.Lock()
	defer l.mu.Unlock()
	if n := l.inFlight[r.TenantID]; n > 0 {
		l.inFlight[r.TenantID] = n - 1
	}
	b, ok := l.buckets[r.TenantID]
	if !ok {
		return
	}
	b.refill(l.now())
	delta := float64(r.Estimated - actualTokens)
	if delta > 0 {
		b.give(delta)
		return
	}
	// Used more than reserved: charge the difference, allowing a deficit.
	b.tokens += delta
}

// Eligible returns the tenants that can currently afford a typical call.
//
// This is what the dispatcher passes to the store's lease query, so tenants
// that are out of quota are never even considered - the backpressure is in the
// scheduling decision, not in a blocked worker.
func (l *Limiter) Eligible(typicalTokens int64) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.spare.refill(now)
	need := float64(typicalTokens)

	out := make([]string, 0, len(l.buckets))
	for id, b := range l.buckets {
		b.refill(now)
		// Use the batch floor here: a tenant is "eligible" if even its lowest
		// priority work could proceed. Interactive work that needs the reserve
		// will still succeed at Reserve() time.
		if b.tokens-need >= b.capacity*l.interactiveReserve || l.spare.tokens >= need {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// TenantQuota is the operator-facing view of one tenant's standing.
type TenantQuota struct {
	TenantID       string  `json:"tenant_id"`
	Weight         int     `json:"weight"`
	GuaranteedTPM  float64 `json:"guaranteed_tpm"`
	AvailableNow   float64 `json:"available_now"`
	CapacityTokens float64 `json:"capacity_tokens"`
	UtilizationPct float64 `json:"utilization_pct"`
	InFlight       int     `json:"in_flight"`
	Throttled      bool    `json:"throttled"`
}

// Snapshot powers the fairness panel in the UI. "Which tenant is being
// throttled right now, and why" should be one glance, not a log dig.
func (l *Limiter) Snapshot() []TenantQuota {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.spare.refill(now)
	out := make([]TenantQuota, 0, len(l.buckets))
	for id, b := range l.buckets {
		b.refill(now)
		used := b.capacity - b.tokens
		q := TenantQuota{
			TenantID: id, Weight: l.weights[id],
			GuaranteedTPM:  b.rate * 60,
			AvailableNow:   math.Max(0, b.tokens),
			CapacityTokens: b.capacity,
			InFlight:       l.inFlight[id],
		}
		if b.capacity > 0 {
			q.UtilizationPct = math.Max(0, math.Min(100, used/b.capacity*100))
		}
		q.Throttled = b.tokens <= b.capacity*l.interactiveReserve && l.spare.tokens <= 0
		out = append(out, q)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TenantID < out[j].TenantID })
	return out
}

// SpareTokens exposes the shared burst pool for the operator view.
func (l *Limiter) SpareTokens() float64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.spare.refill(l.now())
	return l.spare.tokens
}

// String is for debugging and test failure messages.
func (l *Limiter) String() string {
	return fmt.Sprintf("fairness.Limiter(provider=%.1f tok/s, tenants=%d, interactive_reserve=%.0f%%)",
		l.providerRate, len(l.buckets), l.interactiveReserve*100)
}
