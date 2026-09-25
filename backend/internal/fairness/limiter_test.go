package fairness_test

import (
	"sync"
	"testing"
	"time"

	"github.com/gaurasha/agent-orch/backend/internal/fairness"
	"github.com/gaurasha/agent-orch/backend/internal/types"
)

func tenant(id string, weight int, tpm int64) types.Tenant {
	return types.Tenant{ID: id, Name: id, Weight: weight, TokensPerMinute: tpm, MaxConcurrentRuns: 1000}
}

// The headline claim: one tenant hammering the platform cannot stop another
// tenant from making progress. This is the noisy-neighbour test.
func TestFairness_NoisyTenantCannotStarveQuietTenant(t *testing.T) {
	l := fairness.New(fairness.Config{
		ProviderTokensPerMinute: 120_000, // 2000 tok/s total, so 1000 tok/s each
		BurstSeconds:            5,
		InteractiveReservePct:   20,
	})
	l.SetTenants([]types.Tenant{
		tenant("noisy", 1, 1_000_000),
		tenant("quiet", 1, 1_000_000),
	})

	// 100 tokens per call, so each tenant's entitlement over the window is
	// large enough for the grant counts to be statistically meaningful:
	// 5000 burst / 100 = 50 calls, plus 1000 tok/s / 100 = 10 calls per second.
	const est = 100
	const window = 2 * time.Second

	var wg sync.WaitGroup
	var mu sync.Mutex
	granted := map[string]int{}
	record := func(id string) {
		mu.Lock()
		granted[id]++
		mu.Unlock()
	}

	stop := make(chan struct{})

	// "noisy": 50 goroutines spinning as fast as they can for the whole window.
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if r, ok := l.Reserve("noisy", types.PriorityBatch, est); ok {
					record("noisy")
					l.Settle(r, est)
				}
			}
		}()
	}

	// "quiet": one polite caller asking every 5ms.
	wg.Add(1)
	go func() {
		defer wg.Done()
		tick := time.NewTicker(5 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				if r, ok := l.Reserve("quiet", types.PriorityBatch, est); ok {
					record("quiet")
					l.Settle(r, est)
				}
			}
		}
	}()

	time.Sleep(window)
	close(stop)
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	n, q := granted["noisy"], granted["quiet"]
	t.Logf("grants over %s: noisy(50 spinning goroutines)=%d quiet(1 polite ticker)=%d", window, n, q)

	if q == 0 {
		t.Fatal("the quiet tenant was completely starved by the noisy one")
	}
	// THE PROPERTY: throughput is decided by the tenant's weighted share, not
	// by how hard it pushes. With a single shared bucket, 50 spinning
	// goroutines would take essentially all of it and the ratio below would be
	// near zero.
	ratio := float64(q) / float64(n)
	if ratio < 0.7 {
		t.Fatalf("quiet tenant got only %.0f%% of the noisy tenant's throughput "+
			"(quiet=%d noisy=%d); equal weights must give equal share regardless of offered load",
			ratio*100, q, n)
	}
	// Each tenant should be held near its own entitlement (~50 burst + ~20
	// refill = ~70 calls), not the whole provider budget (~140).
	const entitlement = 5000/est + 2*1000/est
	if n > entitlement*2 {
		t.Fatalf("noisy tenant got %d grants against an entitlement of ~%d; "+
			"the per-tenant bucket is not binding", n, entitlement)
	}
	t.Logf("quiet tenant received %.0f%% of the noisy tenant's throughput "+
		"while offering ~1/50th of the load", ratio*100)
}

// Weight must actually translate into proportional throughput when both
// tenants are saturating. Otherwise "weight" is decoration.
func TestFairness_WeightsAllocateProportionally(t *testing.T) {
	l := fairness.New(fairness.Config{
		ProviderTokensPerMinute: 180_000, // 3000 tok/s
		BurstSeconds:            1,
		InteractiveReservePct:   0,
	})
	l.SetTenants([]types.Tenant{
		tenant("big", 2, 10_000_000),
		tenant("small", 1, 10_000_000),
	})

	// Drain both buckets so we measure the refill rates, not the initial burst.
	for l.SpareTokens() > 0 {
		if _, ok := l.Reserve("big", types.PriorityBatch, 1000); !ok {
			break
		}
	}
	for {
		if _, ok := l.Reserve("big", types.PriorityBatch, 100); !ok {
			break
		}
	}
	for {
		if _, ok := l.Reserve("small", types.PriorityBatch, 100); !ok {
			break
		}
	}

	time.Sleep(600 * time.Millisecond)

	count := func(id string) int {
		n := 0
		for {
			r, ok := l.Reserve(id, types.PriorityBatch, 100)
			if !ok {
				return n
			}
			n++
			l.Settle(r, 100)
			if n > 100000 {
				return n
			}
		}
	}
	big, small := count("big"), count("small")
	t.Logf("refilled grants after 600ms: big(weight 2)=%d small(weight 1)=%d", big, small)
	if small == 0 {
		t.Fatal("the lower-weighted tenant got nothing")
	}
	ratio := float64(big) / float64(small)
	if ratio < 1.5 || ratio > 2.6 {
		t.Fatalf("weight 2:1 should give roughly 2x throughput, got %.2fx (big=%d small=%d)",
			ratio, big, small)
	}
}

// Human-blocking work must keep flowing when background work has drained the
// tenant's bucket. A person watching a spinner should not queue behind a
// nightly batch job.
func TestFairness_InteractiveReserveProtectsHumanBlockingWork(t *testing.T) {
	l := fairness.New(fairness.Config{
		ProviderTokensPerMinute: 60_000, // 1000 tok/s
		BurstSeconds:            10,
		InteractiveReservePct:   25,
	})
	l.SetTenants([]types.Tenant{tenant("t1", 1, 1_000_000)})

	// Batch work consumes everything it is allowed to.
	batchGrants := 0
	for {
		r, ok := l.Reserve("t1", types.PriorityBatch, 500)
		if !ok {
			break
		}
		batchGrants++
		l.Settle(r, 500)
		if batchGrants > 10000 {
			t.Fatal("batch reservations did not converge")
		}
	}
	if batchGrants == 0 {
		t.Fatal("batch work could not start at all")
	}

	// Batch is now blocked...
	if _, ok := l.Reserve("t1", types.PriorityBatch, 500); ok {
		t.Fatal("batch work should be throttled once it reaches the interactive reserve")
	}
	// ...but interactive work still gets through, from the reserve.
	if _, ok := l.Reserve("t1", types.PriorityInteractive, 500); !ok {
		t.Fatalf("interactive work was blocked even though %.0f%% of the bucket is reserved for it", 25.0)
	}
	t.Logf("batch drained after %d grants; interactive still admitted", batchGrants)
}

// The estimate is crude by design, so the reconciliation has to be right or a
// conservative estimator permanently shrinks a tenant's throughput.
func TestFairness_SettleRefundsOverEstimate(t *testing.T) {
	l := fairness.New(fairness.Config{
		ProviderTokensPerMinute: 60_000, BurstSeconds: 10, InteractiveReservePct: 0,
	})
	l.SetTenants([]types.Tenant{tenant("t1", 1, 1_000_000)})

	before := l.Snapshot()[0].AvailableNow
	r, ok := l.Reserve("t1", types.PriorityNormal, 5000)
	if !ok {
		t.Fatal("first reservation should succeed")
	}
	afterReserve := l.Snapshot()[0].AvailableNow
	if afterReserve > before-4900 {
		t.Fatalf("reservation did not deduct: before=%.0f after=%.0f", before, afterReserve)
	}
	// The call actually used far less than estimated.
	l.Settle(r, 200)
	afterSettle := l.Snapshot()[0].AvailableNow
	refunded := afterSettle - afterReserve
	if refunded < 4700 {
		t.Fatalf("expected ~4800 tokens refunded, got %.0f", refunded)
	}
	t.Logf("reserved 5000, used 200, refunded %.0f", refunded)
}

// Under-estimating must be charged, or a tenant could under-report its way past
// the quota indefinitely.
func TestFairness_SettleChargesUnderEstimate(t *testing.T) {
	l := fairness.New(fairness.Config{
		ProviderTokensPerMinute: 60_000, BurstSeconds: 1, InteractiveReservePct: 0,
	})
	l.SetTenants([]types.Tenant{tenant("t1", 1, 1_000_000)})

	r, ok := l.Reserve("t1", types.PriorityNormal, 100)
	if !ok {
		t.Fatal("reservation should succeed")
	}
	avail := l.Snapshot()[0].AvailableNow
	// The call turned out to be enormous.
	l.Settle(r, 100_000)
	after := l.Snapshot()[0].AvailableNow
	if after >= avail {
		t.Fatalf("a massive under-estimate was not charged: %.0f -> %.0f", avail, after)
	}
	if _, ok := l.Reserve("t1", types.PriorityNormal, 100); ok {
		t.Fatal("tenant should be in deficit and refused after using 1000x its estimate")
	}
	t.Logf("under-estimate charged; tenant now in deficit at %.0f tokens", after)
}

// An unconfigured tenant must never be able to spend.
func TestFairness_UnknownTenantFailsClosed(t *testing.T) {
	l := fairness.New(fairness.Config{ProviderTokensPerMinute: 60_000})
	l.SetTenants([]types.Tenant{tenant("known", 1, 1_000)})
	if _, ok := l.Reserve("not-configured", types.PriorityInteractive, 1); ok {
		t.Fatal("an unknown tenant was granted quota; the limiter must fail closed")
	}
}

// Adding a tenant must reduce everyone else's guarantee, or the guarantees sum
// to more than the provider actually gives us.
func TestFairness_GuaranteesSumToProviderCapacity(t *testing.T) {
	l := fairness.New(fairness.Config{ProviderTokensPerMinute: 60_000, BurstSeconds: 5})
	l.SetTenants([]types.Tenant{tenant("a", 1, 1_000_000), tenant("b", 1, 1_000_000)})
	var sum float64
	for _, q := range l.Snapshot() {
		sum += q.GuaranteedTPM
	}
	if sum > 60_000*1.01 {
		t.Fatalf("guaranteed rates sum to %.0f TPM, more than the provider's 60000", sum)
	}

	l.SetTenants([]types.Tenant{
		tenant("a", 1, 1_000_000), tenant("b", 1, 1_000_000),
		tenant("c", 1, 1_000_000), tenant("d", 1, 1_000_000),
	})
	sum = 0
	for _, q := range l.Snapshot() {
		sum += q.GuaranteedTPM
	}
	if sum > 60_000*1.01 {
		t.Fatalf("after adding tenants, guarantees sum to %.0f TPM", sum)
	}
	for _, q := range l.Snapshot() {
		if q.GuaranteedTPM > 15_100 {
			t.Fatalf("tenant %s still guaranteed %.0f TPM; shares were not recomputed",
				q.TenantID, q.GuaranteedTPM)
		}
	}
	t.Logf("4 equal tenants each guaranteed %.0f TPM of 60000", l.Snapshot()[0].GuaranteedTPM)
}
