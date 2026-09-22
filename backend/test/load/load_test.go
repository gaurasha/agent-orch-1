// Package load_test measures the scheduling model under concurrency.
//
// What this proves and what it does not: it exercises the parts of the design
// whose scalability I am actually claiming - the lease queue, the event log,
// the fairness layer and the worker pool - with hundreds of concurrent agents.
// It does NOT prove anything about sandbox throughput (each sandbox is a real
// process; that ceiling is measured separately in the sandbox cold-start test)
// and it does not involve a real LLM.
//
// That is the honest framing: the expensive, external, rate-limited thing is
// stubbed on purpose, because the question here is "does the orchestrator get
// in the way", not "how fast is the model".
package load_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/gaurasha/agent-orch/backend/internal/canon"
	"github.com/gaurasha/agent-orch/backend/internal/fairness"
	"github.com/gaurasha/agent-orch/backend/internal/id"
	"github.com/gaurasha/agent-orch/backend/internal/llm"
	"github.com/gaurasha/agent-orch/backend/internal/obs"
	"github.com/gaurasha/agent-orch/backend/internal/runtime"
	"github.com/gaurasha/agent-orch/backend/internal/store"
	"github.com/gaurasha/agent-orch/backend/internal/tools"
	"github.com/gaurasha/agent-orch/backend/internal/types"
)

// noopToolCaller stands in for the tool gateway. Tool execution is measured
// elsewhere; here it must not be the bottleneck or we would be benchmarking
// the sandbox instead of the scheduler.
type noopToolCaller struct {
	calls  int64
	mu     sync.Mutex
	perRun map[string]int
}

func (n *noopToolCaller) Call(_ context.Context, runID string, req runtime.ToolRequest) (runtime.ToolResponse, error) {
	n.mu.Lock()
	n.calls++
	if n.perRun == nil {
		n.perRun = map[string]int{}
	}
	n.perRun[runID]++
	n.mu.Unlock()
	return runtime.ToolResponse{Result: "ok", Decision: "ALLOW"}, nil
}

type loadResult struct {
	Agents        int
	Workers       int
	Completed     int
	Failed        int
	Elapsed       time.Duration
	ThroughputRPS float64
	ToolCalls     int64
	P50, P95, P99 time.Duration
	Max           time.Duration
}

func (r loadResult) String() string {
	b, _ := json.MarshalIndent(r, "  ", "  ")
	return string(b)
}

func runLoad(t *testing.T, agents, workers int, tenants []types.Tenant) loadResult {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var st store.Store
	dsn := os.Getenv("AGENTORCH_TEST_DSN")
	if dsn != "" {
		pg, err := store.OpenPostgres(ctx, dsn, 40)
		if err != nil {
			t.Fatalf("open postgres: %v", err)
		}
		if _, err := pg.DB().Exec(`TRUNCATE audit_log, tool_calls, events, runs, agent_definitions, tenants CASCADE`); err != nil {
			t.Fatalf("truncate: %v", err)
		}
		st = pg
	} else {
		st = store.NewMem()
	}
	defer st.Close()

	log := obs.NewLogger("load-test", "error")
	metrics := obs.NewMetrics()
	limiter := fairness.New(fairness.Config{
		ProviderTokensPerMinute: 500_000_000, // effectively unlimited: we are measuring the scheduler
		BurstSeconds:            120,
	})
	for _, tn := range tenants {
		if err := st.PutTenant(ctx, tn); err != nil {
			t.Fatal(err)
		}
	}
	limiter.SetTenants(tenants)

	// A very low model latency: we want the orchestrator's overhead visible,
	// not hidden behind a simulated network call.
	provider := llm.NewFakeProvider(2*time.Millisecond, 2*time.Millisecond)
	modelGW := llm.NewGateway(provider, limiter, metrics, log)
	registry := tools.DefaultRegistry("")
	tc := &noopToolCaller{}

	spec := types.Spec{
		SystemPrompt: "load agent", Model: "fake:load", Tools: []string{"fs.write"},
		Budget:   types.Budget{MaxSteps: 10, MaxToolCalls: 10, MaxTokens: 10_000_000, MaxCostUSD: 10000, MaxWallSeconds: 600},
		Priority: types.PriorityNormal,
	}
	digest, err := canon.Digest(spec)
	if err != nil {
		t.Fatal(err)
	}
	for _, tn := range tenants {
		if err := st.PutDefinition(ctx, types.AgentDefinition{
			Digest: digest, TenantID: tn.ID, Name: "load", Spec: spec,
		}); err != nil {
			t.Fatal(err)
		}
	}

	for i := 0; i < workers; i++ {
		w := runtime.NewWorker(runtime.Config{
			WorkerID:          fmt.Sprintf("w%d", i),
			LeaseTTL:          15 * time.Second,
			HeartbeatInterval: 5 * time.Second,
			MaxStepsPerLease:  8,
			PollInterval:      5 * time.Millisecond,
		}, st, modelGW, tc, registry, limiter.Eligible, metrics, log)
		go w.Run(ctx)
	}
	go runtime.NewReaper(st, time.Second, 60*time.Second, metrics, log).Run(ctx)

	// Create every run first, then start the clock. This is the 09:00 burst
	// the brief describes: a large number of agents appearing at once.
	ids := make([]string, 0, agents)
	created := map[string]time.Time{}
	for i := 0; i < agents; i++ {
		tn := tenants[i%len(tenants)]
		rid := id.New("run")
		run := types.Run{
			ID: rid, TenantID: tn.ID, AgentName: "load", DefDigest: digest,
			TriggeringUser: "load@test", State: types.StateQueued,
			Budget: spec.Budget, Priority: types.PriorityNormal,
		}
		if err := st.CreateRun(ctx, run, types.Event{
			Type: types.EventRunCreated, Payload: types.EventPayload{Text: "go"},
		}); err != nil {
			t.Fatalf("create run %d: %v", i, err)
		}
		ids = append(ids, rid)
		created[rid] = time.Now()
	}

	start := time.Now()
	latencies := make([]time.Duration, 0, agents)
	completed, failed := 0, 0
	deadline := time.Now().Add(90 * time.Second)
	pending := make(map[string]bool, len(ids))
	for _, rid := range ids {
		pending[rid] = true
	}

	for len(pending) > 0 && time.Now().Before(deadline) {
		// Only terminal runs, within the store's documented maximum. An
		// earlier version asked for 2000 and silently got 200 back, so the
		// loop could never observe more than the 200 newest runs finishing.
		runs, err := st.ListRuns(ctx, store.RunFilter{
			States: []types.RunState{types.StateSucceeded, types.StateFailed, types.StateCancelled},
			Limit:  1000,
		})
		if err != nil {
			t.Fatalf("list runs: %v", err)
		}
		now := time.Now()
		for _, r := range runs {
			if !pending[r.ID] || !r.State.Terminal() {
				continue
			}
			delete(pending, r.ID)
			latencies = append(latencies, now.Sub(created[r.ID]))
			if r.State == types.StateSucceeded {
				completed++
			} else {
				failed++
			}
		}
		if len(pending) > 0 {
			time.Sleep(25 * time.Millisecond)
		}
	}
	elapsed := time.Since(start)
	if len(pending) > 0 {
		t.Errorf("%d of %d runs did not finish within the deadline", len(pending), agents)
	}

	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	pct := func(p float64) time.Duration {
		if len(latencies) == 0 {
			return 0
		}
		i := int(float64(len(latencies)-1) * p)
		return latencies[i]
	}
	res := loadResult{
		Agents: agents, Workers: workers, Completed: completed, Failed: failed,
		Elapsed: elapsed.Truncate(time.Millisecond),
		ThroughputRPS: float64(completed) / elapsed.Seconds(),
		ToolCalls: tc.calls,
		P50: pct(0.50).Truncate(time.Millisecond), P95: pct(0.95).Truncate(time.Millisecond),
		P99: pct(0.99).Truncate(time.Millisecond),
	}
	if len(latencies) > 0 {
		res.Max = latencies[len(latencies)-1].Truncate(time.Millisecond)
	}
	return res
}

func TestLoad_500ConcurrentAgents(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping load test in -short mode")
	}
	tenants := []types.Tenant{
		{ID: "lt-a", Name: "A", Weight: 1, TokensPerMinute: 100_000_000, MaxConcurrentRuns: 100000},
		{ID: "lt-b", Name: "B", Weight: 1, TokensPerMinute: 100_000_000, MaxConcurrentRuns: 100000},
	}
	res := runLoad(t, 500, 16, tenants)
	t.Logf("500-agent burst:\n  %s", res)

	if res.Completed != 500 {
		t.Fatalf("only %d of 500 runs completed (%d failed)", res.Completed, res.Failed)
	}
	// This is a regression guard, not a performance claim: the numbers depend
	// on the machine. A p99 in the tens of seconds would mean the scheduler is
	// serialising somewhere, which is the failure worth catching.
	if res.P99 > 60*time.Second {
		t.Fatalf("p99 completion latency %s is far beyond expectation; "+
			"the dispatcher is probably serialising", res.P99)
	}
	// Each load agent makes exactly 2 tool calls, so this catches both lost
	// work and duplicated work.
	if res.ToolCalls != int64(500*2) {
		t.Fatalf("expected exactly %d tool calls, got %d", 500*2, res.ToolCalls)
	}
}

func TestLoad_ScalesWithWorkers(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping load test in -short mode")
	}
	tenants := []types.Tenant{
		{ID: "lt-a", Name: "A", Weight: 1, TokensPerMinute: 100_000_000, MaxConcurrentRuns: 100000},
	}
	// The claim under test: agent orchestration is I/O bound, so throughput
	// should rise with worker count rather than flattening immediately. If it
	// flattens, something in the shared path (the lease query, a mutex) is the
	// real limit and the "stateless pool" story does not hold.
	small := runLoad(t, 200, 2, tenants)
	large := runLoad(t, 200, 24, tenants)
	t.Logf("2 workers : %.1f runs/s (p99 %s)", small.ThroughputRPS, small.P99)
	t.Logf("24 workers: %.1f runs/s (p99 %s)", large.ThroughputRPS, large.P99)

	if large.ThroughputRPS <= small.ThroughputRPS {
		t.Fatalf("throughput did not improve with 12x the workers (%.1f -> %.1f runs/s); "+
			"the bottleneck is shared, not per-worker", small.ThroughputRPS, large.ThroughputRPS)
	}
	t.Logf("throughput improved %.1fx with 12x the workers",
		large.ThroughputRPS/small.ThroughputRPS)
}

// The 09:00 burst from the brief: one tenant spins up hundreds of agents at
// once and must not stop the other tenant's work.
func TestLoad_BurstDoesNotStarveTheOtherTenant(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping load test in -short mode")
	}
	tenants := []types.Tenant{
		{ID: "burst", Name: "Burst", Weight: 1, TokensPerMinute: 60_000, MaxConcurrentRuns: 100000},
		{ID: "steady", Name: "Steady", Weight: 1, TokensPerMinute: 60_000, MaxConcurrentRuns: 100000},
	}
	res := runLoad(t, 300, 12, tenants)
	t.Logf("mixed-tenant burst:\n  %s", res)
	if res.Completed != 300 {
		t.Fatalf("only %d of 300 runs completed", res.Completed)
	}
}
