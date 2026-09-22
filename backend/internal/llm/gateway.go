package llm

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"time"

	"github.com/gaurasha/agent-orch/backend/internal/fairness"
	"github.com/gaurasha/agent-orch/backend/internal/obs"
	"github.com/gaurasha/agent-orch/backend/internal/types"
)

// ErrQuotaUnavailable means the tenant has no quota right now.
//
// This is NOT an error the caller should retry in a loop. It is a signal to put
// the run back in the queue and pick up different work, which is what keeps a
// throttled tenant from occupying workers. See fairness package docs.
var ErrQuotaUnavailable = errors.New("llm: tenant quota unavailable")

// Gateway is the single choke point for model calls.
//
// Everything expensive or externally-constrained about the LLM lives here:
// quota, retries, backoff, cost accounting and failover. A worker calls
// Complete and gets either a response or a clear reason it cannot proceed; it
// never talks to a provider directly, so there is exactly one place to change
// when the provider's behaviour changes.
type Gateway struct {
	primary  Provider
	fallback Provider
	limiter  *fairness.Limiter
	metrics  *obs.Metrics
	log      *obs.Logger

	maxAttempts int
	baseBackoff time.Duration
	rng         *rand.Rand
}

func NewGateway(primary Provider, limiter *fairness.Limiter, m *obs.Metrics, log *obs.Logger) *Gateway {
	return &Gateway{
		primary: primary, limiter: limiter, metrics: m, log: log,
		// Three attempts, not "until it works". An unbounded retry loop against
		// a struggling provider is how a partial outage becomes a total one -
		// every client hammering hardest exactly when the provider can cope
		// least. Durability means we can afford to give up and requeue.
		maxAttempts: 3,
		baseBackoff: 250 * time.Millisecond,
		rng:         rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

// WithFallback registers a secondary provider used when the primary is
// degraded. Model behaviour differs between providers, so this is opt-in per
// agent definition in a real system rather than silently automatic.
func (g *Gateway) WithFallback(p Provider) *Gateway { g.fallback = p; return g }

// Complete reserves quota, calls the provider with bounded retries, and
// settles the reservation against actual usage.
func (g *Gateway) Complete(ctx context.Context, tenantID string, pri types.Priority, req Request) (Response, error) {
	est := EstimateTokens(req)
	res, ok := g.limiter.Reserve(tenantID, pri, est)
	if !ok {
		g.metrics.QuotaDenied(tenantID)
		return Response{}, fmt.Errorf("%w: tenant %s estimated %d tokens", ErrQuotaUnavailable, tenantID, est)
	}
	// Settle exactly once, whatever path we leave by. A reservation that is
	// never settled is quota permanently lost to that tenant.
	settled := false
	settle := func(actual int64) {
		if !settled {
			settled = true
			g.limiter.Settle(res, actual)
		}
	}
	defer func() { settle(est) }()

	var lastErr error
	for attempt := 1; attempt <= g.maxAttempts; attempt++ {
		provider := g.primary
		// Last attempt on a degraded primary: try the fallback rather than
		// spending the final retry on something we already know is failing.
		if attempt == g.maxAttempts && g.fallback != nil && isRetryable(lastErr) {
			provider = g.fallback
		}

		resp, err := provider.Complete(ctx, req)
		if err == nil {
			actual := resp.InputTokens + resp.OutputTokens
			settle(actual)
			g.metrics.ModelCall(tenantID, resp.Model, resp.InputTokens, resp.OutputTokens, resp.CostUSD)
			return resp, nil
		}
		lastErr = err

		if ctx.Err() != nil {
			return Response{}, ctx.Err()
		}
		if !isRetryable(err) {
			// A permanent error is not worth quota. Refund it.
			settle(0)
			return Response{}, fmt.Errorf("model call failed permanently: %w", err)
		}
		if attempt == g.maxAttempts {
			break
		}
		// Exponential backoff with full jitter. Full jitter rather than a fixed
		// multiplier because synchronised retries from hundreds of workers are
		// themselves an outage; see the AWS Architecture Blog, "Exponential
		// Backoff And Jitter".
		backoff := time.Duration(float64(g.baseBackoff) * math.Pow(2, float64(attempt-1)))
		sleep := time.Duration(g.rng.Int63n(int64(backoff) + 1))
		g.log.Warn("model call failed; backing off",
			"tenant", tenantID, "attempt", attempt, "backoff_ms", sleep.Milliseconds(), "err", err)
		select {
		case <-ctx.Done():
			return Response{}, ctx.Err()
		case <-time.After(sleep):
		}
	}
	// Give the quota back: we never got a completion for it.
	settle(0)
	return Response{}, fmt.Errorf("model call failed after %d attempts: %w", g.maxAttempts, lastErr)
}

func isRetryable(err error) bool {
	return errors.Is(err, ErrRateLimited) || errors.Is(err, ErrOverloaded)
}
